package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

// mkCert — тестовый сертификат с настраиваемым шаблоном.
func mkCert(t *testing.T, edit func(*x509.Certificate), key any) (certPEM, keyPEM []byte) {
	t.Helper()
	if key == nil {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		key = k
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "nas.home"},
		NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"nas.home"},
	}
	if edit != nil {
		edit(tmpl)
	}
	var pub any
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		pub = &k.PublicKey
	case *rsa.PrivateKey:
		pub = &k.PublicKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
}

func TestValidate(t *testing.T) {
	good, goodKey := mkCert(t, nil, nil)
	if _, err := Validate(good, goodKey, t0); err != nil {
		t.Fatalf("годный: %v", err)
	}
	_, otherKey := mkCert(t, nil, nil)
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	weakC, weakK := mkCert(t, nil, weak)
	cases := map[string][2][]byte{
		"мусор":          {[]byte("x"), []byte("y")},
		"чужой ключ":     {good, otherKey},
		"истёк":          pair(mkCert(t, func(c *x509.Certificate) { c.NotAfter = t0.Add(-time.Minute) }, nil)),
		"ещё не начался": pair(mkCert(t, func(c *x509.Certificate) { c.NotBefore = t0.Add(time.Hour) }, nil)),
		"CA": pair(mkCert(t, func(c *x509.Certificate) {
			c.IsCA, c.BasicConstraintsValid, c.KeyUsage = true, true, x509.KeyUsageCertSign
		}, nil)),
		"clientAuth": pair(mkCert(t, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }, nil)),
		"без SAN":    pair(mkCert(t, func(c *x509.Certificate) { c.DNSNames = nil }, nil)),
		"RSA-1024":   {weakC, weakK},
		"большой":    {append(good, make([]byte, MaxPEM)...), goodKey},
	}
	for name, c := range cases {
		if _, err := Validate(c[0], c[1], t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ожидался ErrInvalid, получено %v", name, err)
		}
	}
	// Ключ в ошибке не печатается.
	if _, err := Validate(good, otherKey, t0); err != nil && strings.Contains(err.Error(), "PRIVATE") {
		t.Errorf("ключ в тексте ошибки: %v", err)
	}
}

func pair(a, b []byte) [2][]byte { return [2][]byte{a, b} }

func TestStoreInstallRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	names := func() Names { return Names{Hosts: []string{"nas"}, IPs: []net.IP{net.ParseIP("10.0.0.2")}} }
	now := func() time.Time { return t0 }
	s, warn, err := NewStore(dir, "", "", names, now)
	if err != nil || warn != nil {
		t.Fatal(err, warn)
	}
	self := s.Info()
	if self.Source != SourceSelfSigned || !self.Editable || !self.SelfSigned {
		t.Fatalf("самоподписанный: %+v", self)
	}
	c, k := mkCert(t, nil, nil)
	in, err := s.Install(c, k)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetCertificate(nil)
	if in.Source != SourceCustom || in.Subject != "nas.home" || got.Leaf.Subject.CommonName != "nas.home" {
		t.Fatalf("после загрузки: %+v", in)
	}
	if perm(t, filepath.Join(dir, CustomName)) != 0o600 {
		t.Error("права custom.pem")
	}
	// Негодный не заменяет действующий.
	if _, err := s.Install([]byte("x"), []byte("y")); !errors.Is(err, ErrInvalid) || s.Info().Fingerprint != in.Fingerprint {
		t.Errorf("негодный: %v", err)
	}
	// Перезапуск подхватывает загруженный.
	s2, _, err := NewStore(dir, "", "", names, now)
	if err != nil || s2.Info().Fingerprint != in.Fingerprint {
		t.Fatalf("перезапуск: %v %+v", err, s2.Info())
	}
	// Возврат к самоподписанному — тот же, что был (не перевыпуск).
	back, err := s.Remove()
	if err != nil || back.Source != SourceSelfSigned || back.Fingerprint != self.Fingerprint {
		t.Fatalf("возврат: %v %+v", err, back)
	}
	if _, err := os.Stat(filepath.Join(dir, CustomName)); !os.IsNotExist(err) {
		t.Error("custom.pem не удалён")
	}
	// Испорченный загруженный файл — запуск с самоподписанным и предупреждением.
	os.WriteFile(filepath.Join(dir, CustomName), []byte("мусор"), 0o600)
	s3, warn, err := NewStore(dir, "", "", names, now)
	if err != nil || warn == nil || s3.Info().Source != SourceSelfSigned {
		t.Fatalf("испорченный: %v %v", err, warn)
	}
}

func TestStoreConfigReadOnly(t *testing.T) {
	d := t.TempDir()
	c, k := mkCert(t, nil, nil)
	os.WriteFile(filepath.Join(d, "c"), c, 0o600)
	os.WriteFile(filepath.Join(d, "k"), k, 0o600)
	s, _, err := NewStore(filepath.Join(d, "tls"), filepath.Join(d, "c"), filepath.Join(d, "k"), LocalNames, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	if in := s.Info(); in.Source != SourceConfig || in.Editable {
		t.Fatalf("%+v", in)
	}
	if _, err := s.Install(c, k); !errors.Is(err, ErrReadOnly) {
		t.Error(err)
	}
	if _, err := s.Remove(); !errors.Is(err, ErrReadOnly) {
		t.Error(err)
	}
}

// Смена имени хоста или нового адреса — перевыпуск самоподписанного при
// запуске; пропавший адрес — нет.
func TestReissueOnNamesChange(t *testing.T) {
	dir := t.TempDir()
	n1 := Names{Hosts: []string{"nas"}, IPs: []net.IP{net.ParseIP("10.0.0.2"), net.ParseIP("10.0.0.3")}}
	r1, err := LoadOrCreate(dir, n1, t0)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := LoadOrCreate(dir, Names{Hosts: []string{"NAS"}, IPs: n1.IPs[:1]}, t0); r.Generated {
		t.Error("перевыпуск без новых имён")
	}
	r2, _ := LoadOrCreate(dir, Names{Hosts: []string{"nas"}, IPs: []net.IP{net.ParseIP("10.0.0.9")}}, t0)
	r3, _ := LoadOrCreate(dir, Names{Hosts: []string{"nas2"}, IPs: []net.IP{net.ParseIP("10.0.0.9")}}, t0)
	if !r2.Generated || !r3.Generated || r2.Fingerprint == r1.Fingerprint {
		t.Errorf("нет перевыпуска: новый адрес %v, новое имя %v", r2.Generated, r3.Generated)
	}
}

// Замена действует на новые соединения без перезапуска сервера.
func TestHotSwapHandshake(t *testing.T) {
	s, _, err := NewStore(t.TempDir(), "", "", func() Names { return Names{Hosts: []string{"nas"}} }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerConfig(s.GetCertificate))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	peer := func() string {
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.ConnectionState().PeerCertificates[0].Subject.CommonName
	}
	if cn := peer(); cn != "nas" {
		t.Fatalf("до замены: %s", cn)
	}
	c, k, _ := Generate(Names{Hosts: []string{"nas.home"}}, time.Now())
	if _, err := s.Install(c, k); err != nil {
		t.Fatal(err)
	}
	if cn := peer(); cn != "nas.home" {
		t.Fatalf("после замены: %s", cn)
	}
}
