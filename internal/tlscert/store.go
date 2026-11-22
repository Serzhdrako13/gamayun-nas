package tlscert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Свой сертификат, загружаемый из веба, и выбор источника.
//
// Приоритет: файлы из конфигурации (tls.cert_file/key_file) → загруженный
// через API (<каталог>/custom.pem) → самоподписанный (<каталог>/cert.pem).
// Загруженный хранится одним файлом (цепочка + ключ), поэтому замена
// атомарна: при сбое остаётся либо старая пара, либо новая.

// Источники сертификата.
const (
	SourceConfig     = "config"
	SourceCustom     = "custom"
	SourceSelfSigned = "self-signed"
	// CustomName — файл загруженного сертификата (цепочка + ключ, PEM).
	CustomName = "custom.pem"
	// MaxPEM — предел размера загружаемых PEM (цепочка + ключ).
	MaxPEM = 48 << 10
)

// Ошибки проверки и состояния (для API — 400/409).
var (
	ErrInvalid  = errors.New("сертификат не принят")
	ErrReadOnly = errors.New("сертификат задан в конфигурации (tls.cert_file) — замена из веба отключена")
)

// Info — сведения о действующем сертификате для веба.
type Info struct {
	Source      string    `json:"source"`
	Editable    bool      `json:"editable"` // можно загрузить свой или вернуть самоподписанный
	Fingerprint string    `json:"fingerprint_sha256"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	DNSNames    []string  `json:"dns_names"`
	IPs         []string  `json:"ips"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	SelfSigned  bool      `json:"self_signed"`
	// Warnings — истекает скоро, не покрывает имя хоста или адрес.
	Warnings []string `json:"warnings"`
}

// Store — действующий сертификат сервера; потокобезопасен.
type Store struct {
	dir               string
	certFile, keyFile string
	names             func() Names
	now               func() time.Time

	mu     sync.Mutex // Install/Remove
	cur    Result
	source string
	rw     sync.RWMutex // cur, source
}

// NewStore выбирает и загружает сертификат по приоритету. Испорченный
// загруженный файл не мешает запуску: используется самоподписанный, а
// ошибка возвращается в warn (для журнала).
func NewStore(dir, certFile, keyFile string, names func() Names, now func() time.Time) (s *Store, warn error, err error) {
	s = &Store{dir: dir, certFile: certFile, keyFile: keyFile, names: names, now: now}
	if certFile != "" {
		r, err := Load(certFile, keyFile)
		if err != nil {
			return nil, nil, err
		}
		s.set(r, SourceConfig)
		return s, nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("TLS: %w", err)
	}
	custom := filepath.Join(dir, CustomName)
	if _, serr := os.Stat(custom); serr == nil {
		r, lerr := Load(custom, custom)
		if lerr == nil {
			s.set(r, SourceCustom)
			return s, nil, nil
		}
		warn = fmt.Errorf("загруженный сертификат %s не читается, используется самоподписанный: %w", custom, lerr)
	}
	r, err := LoadOrCreate(dir, names(), now())
	if err != nil {
		return nil, warn, err
	}
	s.set(r, SourceSelfSigned)
	return s, warn, nil
}

func (s *Store) set(r Result, src string) {
	s.rw.Lock()
	s.cur, s.source = r, src
	s.rw.Unlock()
}

// Current — действующий сертификат и его источник.
func (s *Store) Current() (Result, string) {
	s.rw.RLock()
	defer s.rw.RUnlock()
	return s.cur, s.source
}

// GetCertificate — для tls.Config.
func (s *Store) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r, _ := s.Current()
	c := r.Cert
	return &c, nil
}

// Info — сведения о действующем сертификате.
func (s *Store) Info() Info {
	r, src := s.Current()
	return s.info(r, src)
}

func (s *Store) info(r Result, src string) Info {
	l := r.Leaf
	in := Info{
		Source: src, Editable: src != SourceConfig, Fingerprint: r.Fingerprint,
		Subject: l.Subject.CommonName, Issuer: l.Issuer.CommonName,
		DNSNames: append([]string{}, l.DNSNames...), IPs: []string{},
		NotBefore: l.NotBefore, NotAfter: l.NotAfter,
		SelfSigned: bytes.Equal(l.RawIssuer, l.RawSubject) && l.CheckSignature(l.SignatureAlgorithm, l.RawTBSCertificate, l.Signature) == nil, Warnings: []string{},
	}
	for _, ip := range l.IPAddresses {
		in.IPs = append(in.IPs, ip.String())
	}
	now := s.now()
	switch left := l.NotAfter.Sub(now); {
	case left <= 0:
		in.Warnings = append(in.Warnings, "сертификат истёк")
	case left < RenewBefore:
		in.Warnings = append(in.Warnings, fmt.Sprintf("сертификат истекает через %d дн.", int(left.Hours()/24)+1))
	}
	if src == SourceSelfSigned && !Covers(l, s.names()) {
		in.Warnings = append(in.Warnings, "имя или адрес сервера изменились — новый самоподписанный сертификат будет выпущен при перезапуске")
	}
	return in
}

// Validate проверяет загружаемую пару: PEM разбирается, ключ подходит к
// сертификату, срок действует, сертификат — серверный (не CA), ключ не
// слабее RSA-2048/P-256. Цепочка до корня не проверяется: доверие — дело
// браузера (свой домашний CA тоже допустим).
func Validate(certPEM, keyPEM []byte, now time.Time) (Result, error) {
	if len(certPEM)+len(keyPEM) > MaxPEM {
		return Result{}, fmt.Errorf("%w: больше %d КиБ", ErrInvalid, MaxPEM>>10)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	c.Leaf = leaf
	switch {
	case now.Before(leaf.NotBefore):
		return Result{}, fmt.Errorf("%w: срок действия ещё не начался (%s)", ErrInvalid, leaf.NotBefore.Format(time.DateOnly))
	case !now.Before(leaf.NotAfter):
		return Result{}, fmt.Errorf("%w: срок действия истёк (%s)", ErrInvalid, leaf.NotAfter.Format(time.DateOnly))
	case leaf.IsCA:
		return Result{}, fmt.Errorf("%w: это сертификат центра сертификации, нужен сертификат сервера", ErrInvalid)
	case len(leaf.ExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) &&
		!slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny):
		return Result{}, fmt.Errorf("%w: сертификат не предназначен для сервера (нет serverAuth)", ErrInvalid)
	case len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0:
		return Result{}, fmt.Errorf("%w: нет имён и адресов (SAN) — браузеры такой не примут", ErrInvalid)
	}
	switch k := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return Result{}, fmt.Errorf("%w: ключ RSA короче 2048 бит", ErrInvalid)
		}
	case *ecdsa.PublicKey:
		if k.Curve.Params().BitSize < 256 {
			return Result{}, fmt.Errorf("%w: кривая ECDSA слабее P-256", ErrInvalid)
		}
	case ed25519.PublicKey:
	default:
		return Result{}, fmt.Errorf("%w: неподдерживаемый тип ключа %T", ErrInvalid, k)
	}
	return Result{Cert: c, Leaf: leaf, Fingerprint: Fingerprint(leaf)}, nil
}

// Install проверяет и сохраняет свой сертификат; действует сразу (новые
// соединения). Ключ в ответ и журналы не попадает.
func (s *Store) Install(certPEM, keyPEM []byte) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, src := s.Current(); src == SourceConfig {
		return Info{}, ErrReadOnly
	}
	r, err := Validate(certPEM, keyPEM, s.now())
	if err != nil {
		return Info{}, err
	}
	data := append(append(append([]byte{}, certPEM...), '\n'), keyPEM...)
	if err := writeFile(filepath.Join(s.dir, CustomName), data); err != nil {
		return Info{}, err
	}
	s.set(r, SourceCustom)
	return s.info(r, SourceCustom), nil
}

// Remove удаляет загруженный сертификат и возвращает самоподписанный
// (создаёт или перевыпускает при необходимости).
func (s *Store) Remove() (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, src := s.Current(); src == SourceConfig {
		return Info{}, ErrReadOnly
	}
	r, err := LoadOrCreate(s.dir, s.names(), s.now())
	if err != nil {
		return Info{}, err
	}
	if err := os.Remove(filepath.Join(s.dir, CustomName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("TLS: %w", err)
	}
	s.set(r, SourceSelfSigned)
	return s.info(r, SourceSelfSigned), nil
}
