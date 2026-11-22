package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestLoadOrCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	names := Names{Hosts: []string{"nas", "localhost"}, IPs: []net.IP{net.ParseIP("192.168.1.5"), net.IPv6loopback}}
	r, err := LoadOrCreate(dir, names, now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Generated {
		t.Error("Generated=false для нового сертификата")
	}
	if perm(t, dir) != 0o700 || perm(t, filepath.Join(dir, CertName)) != 0o600 || perm(t, filepath.Join(dir, KeyName)) != 0o600 {
		t.Errorf("права: каталог %o, cert %o, key %o", perm(t, dir), perm(t, filepath.Join(dir, CertName)), perm(t, filepath.Join(dir, KeyName)))
	}
	leaf := r.Leaf
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("ключ: %T", leaf.PublicKey)
	}
	if !slices.Equal(leaf.DNSNames, names.Hosts) || len(leaf.IPAddresses) != 2 || !leaf.IPAddresses[0].Equal(names.IPs[0]) {
		t.Errorf("SAN: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if leaf.Subject.CommonName != "nas" {
		t.Errorf("CN: %s", leaf.Subject.CommonName)
	}
	if d := leaf.NotAfter.Sub(now); d != Validity {
		t.Errorf("срок: %v", d)
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || leaf.IsCA {
		t.Errorf("назначение: %v, CA=%v", leaf.ExtKeyUsage, leaf.IsCA)
	}
	// Проверка цепочки самим сертификатом по имени и по IP.
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	for _, n := range []string{"nas", "192.168.1.5", "::1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: n, CurrentTime: now}); err != nil {
			t.Errorf("проверка по %s: %v", n, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other", CurrentTime: now}); err == nil {
		t.Error("чужое имя прошло проверку")
	}
	if len(r.Fingerprint) != 32*3-1 {
		t.Errorf("отпечаток: %q", r.Fingerprint)
	}

	// Повторный запуск — тот же сертификат; права исправляются.
	os.Chmod(filepath.Join(dir, KeyName), 0o644)
	r2, err := LoadOrCreate(dir, names, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Generated || r2.Fingerprint != r.Fingerprint {
		t.Error("сертификат перевыпущен без причины")
	}
	if perm(t, filepath.Join(dir, KeyName)) != 0o600 {
		t.Error("права ключа не исправлены")
	}
	// Близко к истечению — перевыпуск.
	r3, err := LoadOrCreate(dir, names, now.Add(Validity-RenewBefore+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !r3.Generated || r3.Fingerprint == r.Fingerprint {
		t.Error("истекающий сертификат не перевыпущен")
	}
	// Испорченный файл — перевыпуск.
	os.WriteFile(filepath.Join(dir, CertName), []byte("мусор"), 0o600)
	if r4, err := LoadOrCreate(dir, names, now); err != nil || !r4.Generated {
		t.Errorf("испорченный сертификат: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("лишние файлы в каталоге: %v", entries)
	}
}

func TestLocalNames(t *testing.T) {
	n := LocalNames()
	if !slices.Contains(n.Hosts, "localhost") {
		t.Errorf("нет localhost: %v", n.Hosts)
	}
	var v4, v6 bool
	for _, ip := range n.IPs {
		v4 = v4 || ip.Equal(net.IPv4(127, 0, 0, 1))
		v6 = v6 || ip.Equal(net.IPv6loopback)
		if ip.IsLinkLocalUnicast() {
			t.Errorf("link-local в SAN: %v", ip)
		}
	}
	if !v4 || !v6 {
		t.Errorf("нет loopback: %v", n.IPs)
	}
	if h, _ := os.Hostname(); h != "" && h != "localhost" && n.Hosts[0] != h {
		t.Errorf("имя хоста не первое: %v", n.Hosts)
	}
}
