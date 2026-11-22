package daemon

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/config"
)

// Веб-интерфейс отдаётся без входа, а API за ним остаётся закрытым.
func TestWebUIServedAPIStaysProtected(t *testing.T) {
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
		done <- Run(ctx, cfg, Options{Logger: slog.New(slog.DiscardHandler), Listener: ln, Ready: func(a net.Addr) { ready <- a }})
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

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr}
	get := func(p string) (*http.Response, string) {
		t.Helper()
		resp, err := c.Get("https://" + addr.String() + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, body := get("/")
	if resp.StatusCode != 200 || !strings.Contains(body, `id="app"`) {
		t.Fatalf("/: %d %.80q", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Error("нет CSP на странице интерфейса")
	}
	if resp, _ := get("/pools"); resp.StatusCode != 200 {
		t.Errorf("SPA-маршрут: %d", resp.StatusCode)
	}
	// ссылки на ресурсы из index.html существуют и отдаются
	for _, f := range strings.Split(body, `"`) {
		if strings.HasPrefix(f, "/assets/") {
			if r, _ := get(f); r.StatusCode != 200 {
				t.Errorf("%s: %d", f, r.StatusCode)
			}
		}
	}
	for _, p := range []string{"/api/v1/storage/pools", "/api/v1/nas/users", "/api/v1/unknown"} {
		if r, _ := get(p); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s без входа: %d, ожидался 401", p, r.StatusCode)
		}
	}
}
