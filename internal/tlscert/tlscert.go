// Package tlscert — сертификат HTTPS gamayund: свой (cert/key из конфигурации)
// или самоподписанный, создаваемый в каталоге данных.
//
// Самоподписанный: ECDSA P-256, SAN — имя хоста, localhost и IP-адреса
// интерфейсов, срок 397 дней (не больше предела браузеров для серверных
// сертификатов), перевыпуск при запуске, если до истечения меньше 30 дней.
// Каталог 0700, ключ и сертификат 0600. Отпечаток SHA-256 пишется в журнал
// при старте, чтобы оператор сверил его с тем, что показывает браузер.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gamayun-nas/internal/brand"
)

const (
	// Validity — срок самоподписанного сертификата.
	Validity = 397 * 24 * time.Hour
	// RenewBefore — перевыпуск, если до истечения осталось меньше.
	RenewBefore = 30 * 24 * time.Hour
	// CertName, KeyName — файлы в каталоге самоподписанного сертификата.
	CertName = "cert.pem"
	KeyName  = "key.pem"
)

// Names — имена и адреса для SAN.
type Names struct {
	Hosts []string
	IPs   []net.IP
}

// LocalNames — имя хоста, localhost, адреса loopback и всех интерфейсов
// (кроме link-local IPv6: браузеры не обращаются по ним без зоны).
func LocalNames() Names {
	n := Names{Hosts: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}}
	if h, err := os.Hostname(); err == nil && h != "" && h != "localhost" {
		n.Hosts = append([]string{h}, n.Hosts...)
		if short, _, ok := strings.Cut(h, "."); ok && short != "" {
			n.Hosts = append(n.Hosts, short)
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		n.IPs = append(n.IPs, ipn.IP)
	}
	return n
}

// Result — загруженный сертификат.
type Result struct {
	Cert        tls.Certificate
	Leaf        *x509.Certificate
	Fingerprint string // SHA-256 DER, hex через ':'
	Generated   bool   // создан при этом запуске
}

// Load читает свой сертификат и ключ (PEM).
func Load(certFile, keyFile string) (Result, error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return Result{}, fmt.Errorf("TLS: %w", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return Result{}, fmt.Errorf("TLS: %w", err)
	}
	c.Leaf = leaf
	return Result{Cert: c, Leaf: leaf, Fingerprint: Fingerprint(leaf)}, nil
}

// LoadOrCreate загружает самоподписанный сертификат из dir или создаёт
// новый (нет файлов, не читается, истекает в течение RenewBefore, не
// покрывает текущие имя хоста или адреса).
func LoadOrCreate(dir string, names Names, now time.Time) (Result, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("TLS: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("TLS: %w", err)
	}
	certPath, keyPath := filepath.Join(dir, CertName), filepath.Join(dir, KeyName)
	if r, err := Load(certPath, keyPath); err == nil && r.Leaf.NotAfter.Sub(now) > RenewBefore && Covers(r.Leaf, names) {
		for _, p := range []string{certPath, keyPath} {
			if err := os.Chmod(p, 0o600); err != nil {
				return Result{}, fmt.Errorf("TLS: %w", err)
			}
		}
		return r, nil
	}
	certPEM, keyPEM, err := Generate(names, now)
	if err != nil {
		return Result{}, err
	}
	// Ключ пишется первым: при сбое между записями пара просто перевыпустится.
	if err := writeFile(keyPath, keyPEM); err != nil {
		return Result{}, err
	}
	if err := writeFile(certPath, certPEM); err != nil {
		return Result{}, err
	}
	r, err := Load(certPath, keyPath)
	r.Generated = true
	return r, err
}

// Generate создаёт самоподписанный сертификат ECDSA P-256 (PEM).
func Generate(names Names, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	cn := "localhost"
	if len(names.Hosts) > 0 {
		cn = names.Hosts[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{brand.Name + " (самоподписанный)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names.Hosts,
		IPAddresses:           names.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
}

func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("TLS: %w", err)
	}
	defer os.Remove(f.Name())
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("TLS: %s: %w", path, err)
	}
	return nil
}

// Covers — все ли имена и адреса names есть в SAN сертификата. Лишние имена
// в сертификате (адрес, которого больше нет) перевыпуска не требуют.
func Covers(c *x509.Certificate, names Names) bool {
	for _, h := range names.Hosts {
		if !slices.ContainsFunc(c.DNSNames, func(d string) bool { return strings.EqualFold(d, h) }) {
			return false
		}
	}
	for _, ip := range names.IPs {
		if !slices.ContainsFunc(c.IPAddresses, ip.Equal) {
			return false
		}
	}
	return true
}

// Fingerprint — SHA-256 от DER сертификата в виде AA:BB:...
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// ServerConfig — настройки TLS сервера: не ниже TLS 1.2 (1.3 выбирается
// при поддержке клиентом; его наборы шифров Go не настраиваются). Для 1.2 —
// только ECDHE с AEAD (прямая секретность, без CBC и SHA-1); TLS 1.2 оставлен
// ради старых клиентов (ТВ-приставки, старые Android).
// Сертификат берётся через get при каждом рукопожатии — замена из веба
// (Store.Install) действует без перезапуска.
func ServerConfig(get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: get,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}
}
