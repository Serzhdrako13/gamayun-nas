package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"gamayun-nas/internal/config"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
)

func start(t *testing.T, cfg config.Config, demo time.Duration) (string, context.CancelFunc, chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() {
		done <- Run(ctx, cfg, Options{
			Logger: slog.New(slog.DiscardHandler), Listener: ln, DemoSleep: demo,
			Ready: func(a net.Addr) { ready <- a },
		})
	}()
	select {
	case a := <-ready:
		return "http://" + a.String(), cancel, done
	case err := <-done:
		cancel()
		t.Fatalf("Run завершился: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("демон не запустился")
	}
	return "", cancel, done
}

// plainConfig — прежний режим тестов каркаса: HTTP на loopback без входа.
func plainConfig(dir string) config.Config {
	cfg := config.Default()
	cfg.Database = filepath.Join(dir, "gamayun.db")
	cfg.HelperSocket = filepath.Join(dir, "нет.sock")
	cfg.TLS.Disable = true
	cfg.Auth.Disable = true
	return cfg
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if v != nil {
		json.Unmarshal(b, v)
	}
	return resp.StatusCode
}

func stop(t *testing.T, cancel context.CancelFunc, done chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("демон не остановился")
	}
}

// TestRunRestartInterruptsTask: задача, выполнявшаяся при остановке
// демона, после повторного запуска остаётся interrupted и не перезапускается.
func TestRunRestartInterruptsTask(t *testing.T) {
	cfg := plainConfig(t.TempDir())

	base, cancel, done := start(t, cfg, time.Minute)
	var h map[string]string
	if code := getJSON(t, base+"/api/v1/health", &h); code != 200 || h["status"] != "ok" {
		t.Fatalf("health: %d %v", code, h)
	}
	if code := getJSON(t, base+"/api/v1/disks", nil); code != 503 {
		t.Errorf("disks без helper: %d", code)
	}
	// SSE-поток не должен мешать остановке.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, base+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var list struct{ Tasks []tasks.Task }
	deadline := time.Now().Add(5 * time.Second)
	for {
		getJSON(t, base+"/api/v1/tasks", &list)
		if len(list.Tasks) == 1 && list.Tasks[0].State == tasks.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("demo.sleep не запустилась: %+v", list.Tasks)
		}
		time.Sleep(10 * time.Millisecond)
	}
	id := list.Tasks[0].ID
	stop(t, cancel, done)

	base, cancel, done = start(t, cfg, 0)
	defer stop(t, cancel, done)
	var task tasks.Task
	if code := getJSON(t, base+"/api/v1/tasks/"+id, &task); code != 200 || task.State != tasks.Interrupted {
		t.Fatalf("после перезапуска: %d %+v", code, task)
	}
	getJSON(t, base+"/api/v1/tasks", &list)
	if len(list.Tasks) != 1 {
		t.Errorf("задача перезапущена или продублирована: %+v", list.Tasks)
	}
}

func TestRunRefusesRemoteWithoutFlag(t *testing.T) {
	for _, mod := range []func(*config.Config){
		func(c *config.Config) { c.TLS.Disable = true },
		func(c *config.Config) { c.Auth.Disable = true },
	} {
		cfg := config.Default()
		cfg.Listen = "0.0.0.0:0"
		cfg.Database = filepath.Join(t.TempDir(), "d.db")
		mod(&cfg)
		if err := Run(context.Background(), cfg, Options{Logger: slog.New(slog.DiscardHandler)}); err == nil {
			t.Fatal("не-loopback адрес без TLS или без входа и без insecure_allow_remote должен отклоняться")
		}
	}
}

func TestPruneOnStart(t *testing.T) {
	cfg := plainConfig(t.TempDir())
	db, err := store.Open(context.Background(), cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	// Свежие сверх предела числа, затем (с большими ID) старые:
	// info старше 90 дней удаляются по сроку, critical 200 дней — остаются.
	now := time.Now()
	daysAgo := func(d int) int64 { return now.Add(-time.Duration(d) * 24 * time.Hour).UnixMilli() }
	tx, _ := db.Begin()
	for i := 0; i < EventsKeep+5; i++ {
		tx.Exec("INSERT INTO events(ts, type, data) VALUES (?, 'x', 'null')", now.UnixMilli())
	}
	for i := 0; i < 3; i++ {
		tx.Exec("INSERT INTO events(ts, type, severity, data) VALUES (?, 'old', 'info', 'null')", daysAgo(100))
	}
	for i := 0; i < 2; i++ {
		tx.Exec("INSERT INTO events(ts, type, severity, data) VALUES (?, 'crit', 'critical', 'null')", daysAgo(200))
	}
	tx.Commit()
	db.Close()

	_, cancel, done := start(t, cfg, 0)
	stop(t, cancel, done)

	db, _ = store.Open(context.Background(), cfg.Database)
	defer db.Close()
	var n, old, crit int
	db.QueryRow("SELECT COUNT(*) FROM events").Scan(&n)
	db.QueryRow("SELECT COUNT(*) FROM events WHERE type = 'old'").Scan(&old)
	db.QueryRow("SELECT COUNT(*) FROM events WHERE type = 'crit'").Scan(&crit)
	if n != EventsKeep || old != 0 || crit != 2 {
		t.Errorf("после обрезки: всего %d (ждали %d), старых info %d, critical %d", n, EventsKeep, old, crit)
	}
}
