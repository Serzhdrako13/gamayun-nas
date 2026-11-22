package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeCaddy — admin API на unix-сокете: хранит конфиг, считает /load.
type fakeCaddy struct {
	mu    sync.Mutex
	cfg   []byte
	loads int
}

func (f *fakeCaddy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/config/":
		if f.cfg == nil {
			w.Write([]byte("null"))
			return
		}
		w.Write(f.cfg)
	case r.Method == "POST" && r.URL.Path == "/load":
		f.cfg, _ = io.ReadAll(r.Body)
		f.loads++
	case r.URL.Path == "/pki/ca/local":
		w.Write([]byte(`{"root_certificate":"-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"}`))
	default:
		http.NotFound(w, r)
	}
}

func startFake(t *testing.T) (*fakeCaddy, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCaddy{}
	srv := &http.Server{Handler: f}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f, sock
}

func TestSyncIdempotent(t *testing.T) {
	f, sock := startFake(t)
	routes := []Route{{App: "jf", Name: "jelly", Port: 8096}}
	s := New(Options{Logger: slog.New(slog.DiscardHandler), AdminSocket: sock, AuthSocket: "/run/gamayund/a.sock",
		Hostname: func() string { return "NAS1" }, PortalPort: "8780",
		Routes: func(context.Context) []Route { return routes }})
	fp := &fakePub{}
	s.mdns.pub, s.mdns.addr = fp, func() []ifAddr { return []ifAddr{{2, "192.168.1.5"}, {2, "fd00::5"}} }
	ctx := context.Background()
	s.Sync(ctx)
	s.Sync(ctx)
	if f.loads != 1 {
		t.Fatalf("повторная сверка без изменений: loads=%d", f.loads)
	}
	st := s.Status()
	if !st.Available || st.Error != "" || st.Domain != "nas1.local" || len(st.Routes) != 1 ||
		st.Routes[0].URL != "https://jelly-nas1.local/" {
		t.Fatalf("статус: %+v", st)
	}
	if !s.Allowed("jelly-nas1.local") || s.Allowed("x-nas1.local") {
		t.Fatal("Allowed")
	}
	if p := s.Portal(""); p != "https://nas1.local:8780" {
		t.Fatalf("портал: %s", p)
	}
	cfg := string(f.cfg)
	for _, want := range []string{`"listen":"unix//run/gamayund/a.sock"`, `"dial":"unix//run/gamayund/a.sock"`,
		`"dial":"127.0.0.1:8096"`, `"jelly-nas1.local"`, `"module":"internal"`, `"install_trust":false`,
		`"listen":"unix/` + sock + `|0660"`} {
		if want == `"listen":"unix//run/gamayund/a.sock"` {
			continue // сокет auth — только dial
		}
		if !strings.Contains(cfg, want) {
			t.Errorf("в конфиге нет %s", want)
		}
	}
	if n := len(fp.cur); n != 2 || fp.calls != 1 {
		t.Fatalf("публикаций mDNS: %d (вызовов %d), ожидалось 2 записи за 1 вызов", n, fp.calls)
	}
	// Изменение маршрутов — новый /load; снятие всех — сервер http убран.
	routes = nil
	s.Sync(ctx)
	if len(fp.cur) != 0 {
		t.Fatalf("публикации mDNS не сняты: %d", len(fp.cur))
	}
	if f.loads != 2 || strings.Contains(string(f.cfg), `"http"`) {
		t.Fatalf("снятие публикации: loads=%d cfg=%s", f.loads, f.cfg)
	}
	// Caddy потерял конфиг (перезапуск без autosave) — восстановить.
	f.mu.Lock()
	f.cfg = nil
	f.mu.Unlock()
	s.Sync(ctx)
	if f.loads != 3 {
		t.Fatalf("восстановление: loads=%d", f.loads)
	}
	if ca, err := s.RootCA(ctx); err != nil || !strings.Contains(string(ca), "BEGIN CERTIFICATE") {
		t.Fatalf("RootCA: %v", err)
	}
}

func TestNoCaddy(t *testing.T) {
	s := New(Options{Logger: slog.New(slog.DiscardHandler), AdminSocket: filepath.Join(t.TempDir(), "none.sock"),
		Hostname: func() string { return "nas" }})
	s.Sync(context.Background())
	if st := s.Status(); st.Available || st.Error != "" {
		t.Fatalf("без Caddy: %+v", st)
	}
	if _, err := s.RootCA(context.Background()); !ErrNoCaddy(err) {
		t.Fatalf("RootCA без Caddy: %v", err)
	}
}

// Порядок обработчиков: удаление подделанного X-Gamayun-User — до forward_auth.
func TestBuildOrder(t *testing.T) {
	b, _ := json.Marshal(Build("/a.sock", "/b.sock", []Hosted{{Route: Route{Name: "x", Port: 1}, Host: "x.n.local"}}))
	s := string(b)
	del := strings.Index(s, `"delete":["X-Gamayun-User"]`)
	fa := strings.Index(s, `"uri":"/verify"`)
	if del < 0 || fa < 0 || del > fa {
		t.Fatalf("порядок обработчиков: %s", s)
	}
	for _, n := range []string{"jelly", "a1", "my-app"} {
		if !ValidName(n) {
			t.Errorf("ValidName(%q)", n)
		}
	}
	for _, n := range []string{"", "-a", "a-", "A", "a.b", strings.Repeat("a", 33)} {
		if ValidName(n) {
			t.Errorf("ValidName(%q) принят", n)
		}
	}
}

// fakePub — подделка avahi: текущий набор, число вызовов, сбой.
type fakePub struct {
	cur     []entry
	calls   int
	fail    error
	unhealt error
}

func (f *fakePub) Publish(_ context.Context, es []entry) error {
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	f.cur = append([]entry(nil), es...)
	return nil
}
func (f *fakePub) Healthy() error { return f.unhealt }
func (f *fakePub) Close()         { f.cur = nil }

func TestMDNSRepublish(t *testing.T) {
	fp := &fakePub{}
	m := newMDNS(slog.New(slog.DiscardHandler), fp)
	m.addr = func() []ifAddr { return []ifAddr{{3, "10.0.0.1"}} }
	ctx := context.Background()
	m.sync(ctx, []string{"a-n.local"})
	m.sync(ctx, []string{"a-n.local"})
	if fp.calls != 1 || len(fp.cur) != 1 || fp.cur[0].Iface != 3 {
		t.Fatalf("повтор без изменений: calls=%d cur=%v", fp.calls, fp.cur)
	}
	fp.unhealt = errors.New("конфликт имени mDNS")
	m.sync(ctx, []string{"a-n.local"})
	if fp.calls != 2 {
		t.Fatalf("конфликт — переопубликовать: calls=%d", fp.calls)
	}
	fp.unhealt = nil
	m.addr = func() []ifAddr { return []ifAddr{{3, "10.0.0.2"}} }
	m.sync(ctx, []string{"a-n.local"})
	if fp.calls != 3 || fp.cur[0].Addr != "10.0.0.2" {
		t.Fatalf("смена адреса: calls=%d cur=%v", fp.calls, fp.cur)
	}
	fp.fail = errors.New("avahi: нет службы")
	if e := m.sync(ctx, []string{"b-n.local"}); !strings.Contains(e, "нет службы") {
		t.Fatalf("ошибка не передана: %q", e)
	}
	fp.fail = nil
	if e := m.sync(ctx, []string{"b-n.local"}); e != "" || fp.cur[0].Host != "b-n.local" {
		t.Fatalf("после сбоя повтор: %q %v", e, fp.cur)
	}
}

func TestSkipIface(t *testing.T) {
	for _, n := range []string{"docker0", "veth12", "br-ab", "lo"} {
		if !skipIface(n) {
			t.Errorf("%s не исключён", n)
		}
	}
	if skipIface("ens18") || skipIface("eth0") {
		t.Error("LAN исключён")
	}
}

func TestCheckName(t *testing.T) {
	s := New(Options{Hostname: func() string { return strings.Repeat("h", 40) }})
	if s.CheckName("short") != nil || s.CheckName(strings.Repeat("a", 30)) == nil {
		t.Fatal("CheckName: предел 63 символа метки")
	}
	if s.Host("app") != "app-"+strings.Repeat("h", 40)+".local" {
		t.Fatal(s.Host("app"))
	}
}
