package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/auth"
	"gamayun-nas/internal/config"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tlscert"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TestRunTLSAuth — демон по умолчанию: HTTPS с самоподписанным
// сертификатом, вход обязателен, первичная настройка без пароля по умолчанию.
func TestRunTLSAuth(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Database = filepath.Join(dir, "gamayun.db")
	cfg.HelperSocket = filepath.Join(dir, "нет.sock")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() {
		done <- Run(ctx, cfg, Options{Logger: slog.New(slog.DiscardHandler), Listener: ln,
			Ready: func(a net.Addr) { ready <- a }})
	}()
	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("демон не запустился")
	}
	defer stop(t, cancel, done)

	// Сертификат: файлы 0600, каталог 0700; проверяем цепочку по нему же
	// и имя localhost из SAN (без InsecureSkipVerify).
	tlsDir := filepath.Join(dir, "tls")
	if m := mode(t, tlsDir); m != 0o700 {
		t.Errorf("каталог tls: %o", m)
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		if m := mode(t, filepath.Join(tlsDir, f)); m != 0o600 {
			t.Errorf("%s: %o", f, m)
		}
	}
	pemBytes, _ := os.ReadFile(filepath.Join(tlsDir, "cert.pem"))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("cert.pem не читается")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	base := "https://" + addr.String()

	// Токен установки: файл 0600; пароля по умолчанию нет.
	tokPath := filepath.Join(dir, SetupTokenName)
	if m := mode(t, tokPath); m != 0o600 {
		t.Errorf("токен установки: %o", m)
	}

	do := func(method, path, body string, hdr map[string]string) (*http.Response, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		return resp, m
	}
	code := func(m map[string]any) string {
		e, _ := m["error"].(map[string]any)
		c, _ := e["code"].(string)
		return c
	}

	if r, _ := do("GET", "/api/v1/health", "", nil); r.StatusCode != 200 {
		t.Fatalf("health: %d", r.StatusCode)
	} else if r.Header.Get("Strict-Transport-Security") == "" {
		t.Error("нет HSTS по TLS")
	}
	if r, m := do("GET", "/api/v1/tasks", "", nil); r.StatusCode != 401 || code(m) != "setup_required" {
		t.Fatalf("без входа и без администратора: %d %v", r.StatusCode, m)
	}
	if r, _ := do("GET", "/api/v1/events", "", nil); r.StatusCode != 401 {
		t.Fatalf("SSE без сессии: %d", r.StatusCode)
	}
	for _, pw := range []string{"admin", "password", ""} {
		if r, _ := do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+pw+`"}`, nil); r.StatusCode != 401 {
			t.Fatalf("вход admin/%q на пустой базе: %d", pw, r.StatusCode)
		}
	}

	// Первый администратор — по токену установки.
	tok, _ := os.ReadFile(tokPath)
	r, m := do("POST", "/api/v1/auth/setup",
		`{"setup_token":"`+strings.TrimSpace(string(tok))+`","username":"owner","password":"correct horse battery"}`, nil)
	if r.StatusCode != 201 {
		t.Fatalf("setup: %d %v", r.StatusCode, m)
	}
	if _, err := os.Stat(tokPath); !os.IsNotExist(err) {
		t.Error("файл токена установки не удалён")
	}

	r, m = do("POST", "/api/v1/auth/login", `{"username":"owner","password":"correct horse battery"}`, nil)
	if r.StatusCode != 200 {
		t.Fatalf("вход: %d %v", r.StatusCode, m)
	}
	var sess string
	for _, c := range r.Cookies() {
		if c.Name == auth.SessionCookie {
			sess = c.Value
		}
	}
	csrf, _ := m["csrf_token"].(string)
	cookie := map[string]string{"Cookie": auth.SessionCookie + "=" + sess}
	if r, m := do("GET", "/api/v1/tasks", "", cookie); r.StatusCode != 200 {
		t.Fatalf("задачи с сессией: %d %v", r.StatusCode, m)
	}
	if r, _ := do("POST", "/api/v1/auth/logout", "", cookie); r.StatusCode != 403 {
		t.Errorf("выход без CSRF: %d", r.StatusCode)
	}
	cookie[auth.CSRFHeader] = csrf
	if r, _ := do("POST", "/api/v1/auth/logout", "", cookie); r.StatusCode != 204 {
		t.Errorf("выход: %d", r.StatusCode)
	}
	if r, _ := do("GET", "/api/v1/tasks", "", cookie); r.StatusCode != 401 {
		t.Errorf("после выхода: %d", r.StatusCode)
	}
}

// TestRunCustomCert — свой сертификат из конфигурации.
func TestRunCustomCert(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := testCert(t)
	cfg := config.Default()
	cfg.Database = filepath.Join(dir, "gamayun.db")
	cfg.HelperSocket = filepath.Join(dir, "нет.sock")
	cfg.TLS.CertFile, cfg.TLS.KeyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cfg.TLS.CertFile, certPEM, 0o600)
	os.WriteFile(cfg.TLS.KeyFile, keyPEM, 0o600)
	st, tc, err := loadTLS(cfg, dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if tc.MinVersion != tls.VersionTLS12 || tc.GetCertificate == nil {
		t.Errorf("tls.Config: %+v", tc)
	}
	if in := st.Info(); in.Source != tlscert.SourceConfig || in.Editable {
		t.Errorf("источник: %+v", in)
	}
	if _, err := os.Stat(filepath.Join(dir, "tls")); !os.IsNotExist(err) {
		t.Error("при своём сертификате самоподписанный создаваться не должен")
	}
	os.WriteFile(cfg.TLS.KeyFile, []byte("мусор"), 0o600)
	if _, _, err := loadTLS(cfg, dir, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("испорченный ключ должен давать ошибку")
	}
}

func testCert(t *testing.T) ([]byte, []byte) {
	t.Helper()
	c, k, err := tlscert.Generate(tlscert.Names{Hosts: []string{"nas.example"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c, k
}

func TestRedirectHandler(t *testing.T) {
	for _, c := range []struct{ port, host, uri, want string }{
		{"8780", "nas.local", "/api/v1/health?x=1", "https://nas.local:8780/api/v1/health?x=1"},
		{"8780", "192.168.1.5:80", "/", "https://192.168.1.5:8780/"},
		{"443", "nas:80", "/a", "https://nas/a"},
		{"443", "[fe80::1]:80", "/", "https://[fe80::1]/"},
		{"8780", "[::1]", "/", "https://[::1]:8780/"},
	} {
		req := httptest.NewRequest("POST", c.uri, nil)
		req.Host = c.host
		w := httptest.NewRecorder()
		RedirectHandler(c.port).ServeHTTP(w, req)
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != c.want {
			t.Errorf("%s%s → %d %q, ожидалось %q", c.host, c.uri, w.Code, w.Header().Get("Location"), c.want)
		}
	}
}

// TestNoSetupTokenWhenAdminExists — администратор есть: токен не создаётся, старый удаляется.
func TestNoSetupTokenWhenAdminExists(t *testing.T) {
	dir := t.TempDir()
	cfg := plainConfig(dir)
	cfg.Auth.Disable = false
	db, err := store.Open(context.Background(), cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := auth.HashPassword("correct horse battery", auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	if _, err := (auth.Store{DB: db}).CreateUser(context.Background(), "owner", h, true, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	stale := filepath.Join(dir, SetupTokenName)
	os.WriteFile(stale, []byte("old\n"), 0o600)
	_, cancel, done := start(t, cfg, 0)
	stop(t, cancel, done)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("устаревший токен установки не удалён")
	}
}
