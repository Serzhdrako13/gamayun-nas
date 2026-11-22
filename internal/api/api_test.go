package api

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
)

type fakeHelper struct {
	results map[string]string
	err     error
}

func (f *fakeHelper) Call(_ context.Context, op string, params, result any) error {
	if f.err != nil {
		return f.err
	}
	if params != nil {
		return errors.New("неожиданные параметры")
	}
	res, ok := f.results[op]
	if !ok {
		return &helper.RemoteError{Code: helper.CodeUnknownOp, Message: op}
	}
	return json.Unmarshal([]byte(res), result)
}

type env struct {
	db    *sql.DB
	log   *events.Log
	tasks *tasks.Manager
	help  *fakeHelper
	srv   *httptest.Server
}

func newEnv(t *testing.T, mod func(*Options)) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, log: events.NewLog(db)}
	e.tasks = tasks.New(db, e.log, tasks.Options{Logger: slog.New(slog.DiscardHandler)})
	e.tasks.Register(tasks.DemoSleepKind, tasks.DemoSleep)
	e.help = &fakeHelper{results: map[string]string{
		"system.info":   `{"hostname":"nas","arch":"amd64"}`,
		"disks.observe": `{"disks":[],"complete":true,"issues":[]}`,
	}}
	o := Options{
		Helper: e.help, Tasks: e.tasks, Events: e.log, DB: db,
		Logger: slog.New(slog.DiscardHandler), Heartbeat: time.Hour,
	}
	if mod != nil {
		mod(&o)
	}
	e.srv = httptest.NewServer(New(o))
	t.Cleanup(func() {
		e.srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.tasks.Shutdown(ctx)
		db.Close()
	})
	return e
}

type result struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (e *env) do(t *testing.T, method, path, ctype, body string) result {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := result{status: resp.StatusCode, header: resp.Header, raw: string(b)}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(b, &r.body); err != nil {
			t.Fatalf("%s %s: не JSON: %s", method, path, b)
		}
	}
	return r
}

func (e *env) get(t *testing.T, path string) result { return e.do(t, http.MethodGet, path, "", "") }

func errCode(r result) string {
	if e, ok := r.body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		if msg, _ := e["message"].(string); msg == "" {
			return code + " (без message)"
		}
		return code
	}
	return ""
}

func TestHealth(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get(t, "/api/v1/health")
	if r.status != 200 || r.body["status"] != "ok" || r.body["database"] != "ok" || r.body["version"] == nil {
		t.Fatalf("health: %d %s", r.status, r.raw)
	}
	if r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("заголовки безопасности: %v", r.header)
	}
	e.db.Close()
	if r := e.get(t, "/api/v1/health"); r.status != 503 || r.body["status"] != "degraded" {
		t.Errorf("health без базы: %d %s", r.status, r.raw)
	}
}

func TestSystemAndDisks(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get(t, "/api/v1/system")
	host, _ := r.body["host"].(map[string]any)
	daemon, _ := r.body["daemon"].(map[string]any)
	if r.status != 200 || host["hostname"] != "nas" || daemon["version"] == nil {
		t.Fatalf("system: %d %s", r.status, r.raw)
	}
	r = e.get(t, "/api/v1/disks")
	if r.status != 200 || r.body["complete"] != true {
		t.Fatalf("disks: %d %s", r.status, r.raw)
	}
}

// Метрики (без Sampler — 503) и последние события.
func TestMetricsAndRecentEvents(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/system/metrics"); r.status != 503 {
		t.Fatalf("без Sampler: %d %s", r.status, r.raw)
	}
	for i := 0; i < 3; i++ {
		e.log.Emit(context.Background(), events.Event{Type: fmt.Sprintf("test.e%d", i)}, true)
	}
	r := e.get(t, "/api/v1/events/recent?limit=2")
	evs, _ := r.body["events"].([]any)
	if r.status != 200 || len(evs) != 2 || evs[0].(map[string]any)["type"] != "test.e2" {
		t.Fatalf("recent: %d %s", r.status, r.raw)
	}
	for _, q := range []string{"0", "201", "x"} {
		if r := e.get(t, "/api/v1/events/recent?limit="+q); r.status != 400 {
			t.Errorf("limit=%s: %d", q, r.status)
		}
	}
}

func TestHelperErrors(t *testing.T) {
	e := newEnv(t, nil)
	e.help.err = fmt.Errorf("%w: connect: no such file", helper.ErrUnavailable)
	for _, p := range []string{"/api/v1/system", "/api/v1/disks"} {
		if r := e.get(t, p); r.status != 503 || errCode(r) != "helper_unavailable" {
			t.Errorf("%s без helper: %d %s", p, r.status, r.raw)
		}
	}
	e.help.err = &helper.RemoteError{Code: helper.CodeFailed, Message: "источник недоступен"}
	if r := e.get(t, "/api/v1/disks"); r.status != 502 || errCode(r) != "helper_error" {
		t.Errorf("ошибка helper: %d %s", r.status, r.raw)
	}
}

const tb = int64(1_000_000_000_000)

func TestPlan(t *testing.T) {
	e := newEnv(t, nil)
	body := fmt.Sprintf(`{"type":"shr","disks":[{"id":"a","size_bytes":%d},{"id":"b","size_bytes":%d},{"id":"c","size_bytes":%d}]}`, 2*tb, 4*tb, 4*tb)
	r := e.do(t, http.MethodPost, "/api/v1/storage/plan", "application/json", body)
	if r.status != 200 {
		t.Fatalf("plan: %d %s", r.status, r.raw)
	}
	usable := r.body["usable_bytes"].(float64)
	if usable < float64(6*tb)-float64(4<<20) || usable > float64(6*tb) {
		t.Errorf("usable_bytes = %.0f, ожидалось ~6 TB", usable)
	}
	if r.body["tolerance"].(float64) != 1 || len(r.body["arrays"].([]any)) != 2 || len(r.body["transitions"].([]any)) == 0 {
		t.Errorf("план: %s", r.raw)
	}
	if _, ok := r.body["warnings"].([]any); !ok {
		t.Errorf("warnings должен быть массивом: %s", r.raw)
	}

	cases := []struct {
		name, ctype, body string
		status            int
		code              string
	}{
		{"лишнее поле", "application/json", `{"type":"raid1","disks":[],"force":true}`, 400, "invalid_request"},
		{"лишнее поле диска", "application/json", `{"type":"raid1","disks":[{"id":"a","size_bytes":1,"path":"/dev/sda"}]}`, 400, "invalid_request"},
		{"не JSON", "application/json", `{`, 400, "invalid_request"},
		{"два объекта", "application/json", `{} {}`, 400, "invalid_request"},
		{"не тот Content-Type", "text/plain", `{}`, 400, "invalid_request"},
		{"без Content-Type", "", `{"type":"basic","disks":[{"id":"a","size_bytes":1000000000}]}`, 400, "invalid_request"},
		{"нулевой размер", "", `{"type":"basic","disks":[{"id":"a","size_bytes":0}]}`, 400, "invalid_request"},
		{"пустой id", "", `{"type":"basic","disks":[{"id":"","size_bytes":1}]}`, 400, "invalid_request"},
		{"отрицательный резерв", "", `{"type":"basic","disks":[{"id":"a","size_bytes":1}],"system_reserve_bytes":-1}`, 400, "invalid_request"},
		{"неизвестный тип", "", `{"type":"raid7","disks":[{"id":"a","size_bytes":1000000000}]}`, 422, "plan_rejected"},
		{"мало дисков", "", `{"type":"raid5","disks":[{"id":"a","size_bytes":1000000000}]}`, 422, "plan_rejected"},
		{"дубликат", "", `{"type":"raid1","disks":[{"id":"a","size_bytes":1000000000},{"id":"a","size_bytes":1000000000}]}`, 422, "plan_rejected"},
		{"слишком большое тело", "", `{"type":"basic","disks":[{"id":"` + strings.Repeat("x", 70<<10) + `","size_bytes":1}]}`, 400, "invalid_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctype := c.ctype
			if ctype == "" && c.name != "без Content-Type" {
				ctype = "application/json; charset=utf-8"
			}
			r := e.do(t, http.MethodPost, "/api/v1/storage/plan", ctype, c.body)
			if r.status != c.status || errCode(r) != c.code {
				t.Errorf("%d %s, ожидалось %d %s", r.status, r.raw, c.status, c.code)
			}
		})
	}
}

func TestRoutingErrors(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get(t, "/api/v1/storage/plan")
	if r.status != 405 || errCode(r) != "method_not_allowed" || r.header.Get("Allow") != "POST" {
		t.Errorf("GET plan: %d %v %s", r.status, r.header.Get("Allow"), r.raw)
	}
	r = e.do(t, http.MethodDelete, "/api/v1/tasks/abc", "", "")
	if r.status != 405 || !strings.Contains(r.header.Get("Allow"), "GET") {
		t.Errorf("DELETE task: %d %s", r.status, r.raw)
	}
	r = e.do(t, http.MethodPost, "/api/v1/health", "", "")
	if r.status != 405 || errCode(r) != "method_not_allowed" {
		t.Errorf("POST health: %d %s", r.status, r.raw)
	}
	for _, p := range []string{"/api/v1/nope", "/", "/api/v2/health"} {
		if r := e.get(t, p); r.status != 404 || errCode(r) != "not_found" {
			t.Errorf("%s: %d %s", p, r.status, r.raw)
		}
	}
}

func TestTasksEndpoints(t *testing.T) {
	e := newEnv(t, nil)
	r := e.get(t, "/api/v1/tasks")
	if r.status != 200 || len(r.body["tasks"].([]any)) != 0 {
		t.Fatalf("пустой список: %d %s", r.status, r.raw)
	}
	task, err := e.tasks.Submit(context.Background(), tasks.DemoSleepKind, json.RawMessage(`{"duration_ms":10}`))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r = e.get(t, "/api/v1/tasks/"+task.ID)
		if r.status != 200 {
			t.Fatalf("задача: %d %s", r.status, r.raw)
		}
		if r.body["state"] == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("задача не завершилась: %s", r.raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.body["kind"] != "demo.sleep" || r.body["progress"].(float64) != 100 {
		t.Errorf("задача: %s", r.raw)
	}
	r = e.get(t, "/api/v1/tasks?limit=5")
	if len(r.body["tasks"].([]any)) != 1 {
		t.Errorf("список: %s", r.raw)
	}
	for _, q := range []string{"0", "1001", "x"} {
		if r := e.get(t, "/api/v1/tasks?limit="+q); r.status != 400 || errCode(r) != "invalid_request" {
			t.Errorf("limit=%s: %d %s", q, r.status, r.raw)
		}
	}
	if r := e.get(t, "/api/v1/tasks/нет"); r.status != 404 || errCode(r) != "not_found" {
		t.Errorf("неизвестная задача: %d %s", r.status, r.raw)
	}
}

func TestAuthHook(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Auth = func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Test-Auth") != "ok" {
					writeError(w, http.StatusUnauthorized, "unauthorized", "нужен вход")
					return
				}
				h.ServeHTTP(w, r)
			})
		}
	})
	if r := e.get(t, "/api/v1/health"); r.status != 200 {
		t.Errorf("health должен быть открыт: %d", r.status)
	}
	for _, p := range []string{"/api/v1/system", "/api/v1/disks", "/api/v1/tasks", "/api/v1/events", "/api/v1/nope"} {
		if r := e.get(t, p); r.status != 401 || errCode(r) != "unauthorized" {
			t.Errorf("%s без входа: %d %s", p, r.status, r.raw)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/system", nil)
	req.Header.Set("X-Test-Auth", "ok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("с входом: %d", resp.StatusCode)
	}
}

// --- SSE ---

type sseEvent struct {
	id, event, data string
}

type sseReader struct {
	t      *testing.T
	resp   *http.Response
	sc     *bufio.Scanner
	cancel context.CancelFunc
}

func openSSE(t *testing.T, url, lastID string) *sseReader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("SSE: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &sseReader{t: t, resp: resp, sc: bufio.NewScanner(resp.Body), cancel: cancel}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return s
}

// next возвращает следующее событие или комментарий (event=":comment").
func (s *sseReader) next() sseEvent {
	s.t.Helper()
	var ev sseEvent
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case line == "":
			if ev != (sseEvent{}) {
				return ev
			}
		case strings.HasPrefix(line, ":"):
			return sseEvent{event: ":comment", data: line}
		case strings.HasPrefix(line, "id: "):
			ev.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			ev.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			ev.data = line[6:]
		case strings.HasPrefix(line, "retry: "):
		}
	}
	s.t.Fatalf("поток SSE оборвался: %v", s.sc.Err())
	return ev
}

func (s *sseReader) nextEvent() sseEvent {
	s.t.Helper()
	for {
		if ev := s.next(); ev.event != ":comment" {
			return ev
		}
	}
}

func TestEventsStream(t *testing.T) {
	e := newEnv(t, nil)
	s := openSSE(t, e.srv.URL+"/api/v1/events", "")
	task, err := e.tasks.Submit(context.Background(), tasks.DemoSleepKind, json.RawMessage(`{"duration_ms":20}`))
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	progress := 0
	for len(states) < 3 {
		ev := s.nextEvent()
		var payload events.Event
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			t.Fatalf("data не JSON: %q", ev.data)
		}
		if payload.TaskID != task.ID || payload.Type != ev.event {
			t.Fatalf("событие: %+v", ev)
		}
		switch ev.event {
		case tasks.EventState:
			if ev.id == "" {
				t.Error("у события состояния нет id")
			}
			var tk tasks.Task
			json.Unmarshal(payload.Data, &tk)
			states = append(states, string(tk.State))
		case tasks.EventProgress:
			if ev.id != "" {
				t.Error("у события прогресса не должно быть id")
			}
			progress++
		}
	}
	if strings.Join(states, ",") != "queued,running,succeeded" || progress == 0 {
		t.Errorf("состояния %v, событий прогресса %d", states, progress)
	}
}

func TestEventsReplay(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		ev, err := e.log.Emit(ctx, events.Event{Type: "test.event", Data: json.RawMessage(fmt.Sprint(i))}, true)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID)
	}
	s := openSSE(t, e.srv.URL+"/api/v1/events", fmt.Sprint(ids[0]))
	for _, want := range ids[1:] {
		if ev := s.nextEvent(); ev.id != fmt.Sprint(want) {
			t.Fatalf("досылка: %+v, ожидался id %d", ev, want)
		}
	}
	// Новое событие после досылки приходит один раз.
	live, _ := e.log.Emit(ctx, events.Event{Type: "test.event"}, true)
	if ev := s.nextEvent(); ev.id != fmt.Sprint(live.ID) {
		t.Fatalf("живое событие: %+v", ev)
	}

	if r := e.do(t, http.MethodGet, "/api/v1/events?last_event_id=-1", "", ""); r.status != 400 {
		t.Errorf("неверный last_event_id: %d", r.status)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Heartbeat = 20 * time.Millisecond })
	s := openSSE(t, e.srv.URL+"/api/v1/events", "")
	if ev := s.next(); ev.event != ":comment" || ev.data != ": ping" {
		t.Errorf("ожидался пинг, получено %+v", ev)
	}
}

// TestRealHelper — API через настоящий helper.Client/Server на Unix-сокете.
func TestRealHelper(t *testing.T) {
	dir, err := os.MkdirTemp("", "dna")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "h.sock")
	ln, err := helper.Listen(sock, -1)
	if err != nil {
		t.Fatal(err)
	}
	hs := &helper.Server{
		Registry:   helper.DefaultRegistry(helper.DefaultDeps(helper.DefaultConfig())),
		AllowedUID: 1000,
		PeerCred:   func(*net.UnixConn) (helper.Cred, error) { return helper.Cred{UID: 1000}, nil },
		Logger:     slog.New(slog.DiscardHandler),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hs.Serve(ctx, ln); close(done) }()
	defer func() { cancel(); <-done }()

	e := newEnv(t, func(o *Options) { o.Helper = &helper.Client{Socket: sock, Timeout: 10 * time.Second} })
	r := e.get(t, "/api/v1/system")
	host, _ := r.body["host"].(map[string]any)
	if r.status != 200 || host["arch"] == nil {
		t.Fatalf("system через helper: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/disks"); r.status != 200 {
		t.Fatalf("disks через helper: %d %s", r.status, r.raw)
	}
}

// TestOpenAPIMatchesRoutes — каждый путь и метод из api/openapi.yaml
// обслуживается API (не 404/405 маршрутизатора), и наоборот.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, nil)
	authEnv := newEnv(t, nil)
	authEnv.srv.Config.Handler = New(Options{Helper: authEnv.help, Tasks: authEnv.tasks, Events: authEnv.log, DB: authEnv.db,
		Logger: slog.New(slog.DiscardHandler), AuthRoutes: auth.New(authEnv.db, auth.Options{Logger: slog.New(slog.DiscardHandler)}).Routes})
	type route struct{ method, path string }
	var spec []route
	var cur string
	for _, line := range strings.Split(string(b), "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		key := strings.TrimSuffix(strings.TrimSpace(line), ":")
		switch {
		case line == "":
		case indent == 0:
			cur = "" // следующий раздел верхнего уровня
		case indent == 2 && strings.HasPrefix(key, "/api/"):
			cur = key
		case indent == 4 && cur != "" && strings.Contains("get post put patch delete", key):
			spec = append(spec, route{strings.ToUpper(key), cur})
		}
	}
	want := []route{
		{"GET", "/api/v1/health"},
		{"POST", "/api/v1/auth/login"}, {"POST", "/api/v1/auth/logout"}, {"GET", "/api/v1/auth/session"},
		{"POST", "/api/v1/auth/password"}, {"POST", "/api/v1/auth/setup"},
		{"GET", "/api/v1/system"}, {"GET", "/api/v1/system/metrics"}, {"GET", "/api/v1/events/recent"}, {"POST", "/api/v1/disks/{id}/release"}, {"GET", "/api/v1/disks"},
		{"POST", "/api/v1/storage/plan"},
		{"POST", "/api/v1/storage/pools/plans"}, {"POST", "/api/v1/storage/pools/plans/{id}/confirm"},
		{"GET", "/api/v1/storage/pools"}, {"GET", "/api/v1/storage/pools/{name}"},
		{"GET", "/api/v1/tasks"}, {"GET", "/api/v1/tasks/{id}"},
		{"GET", "/api/v1/events"},
	}
	if fmt.Sprint(spec) != fmt.Sprint(want) {
		t.Fatalf("пути OpenAPI %v, ожидались %v", spec, want)
	}
	for _, r := range spec {
		if r.path == "/api/v1/events" {
			continue // поток; проверен в TestEventsStream
		}
		path := strings.NewReplacer("{id}", "x", "{name}", "x").Replace(r.path)
		env := e
		if strings.HasPrefix(r.path, "/api/v1/auth/") {
			env = authEnv
		}
		res := env.do(t, r.method, path, "application/json", "{}")
		if code := errCode(res); code == "method_not_allowed" || (res.status == 404 && !strings.Contains(r.path, "{")) {
			t.Errorf("%s %s не обслуживается: %d %s", r.method, r.path, res.status, res.raw)
		}
	}
}

// TestRealAuth — API с настоящей аутентификацией: SSE и прочие
// эндпоинты без сессии — 401, поток закрывается после выхода, заголовки
// безопасности.
func TestRealAuth(t *testing.T) {
	var svc *auth.Service
	e := newEnv(t, func(o *Options) {
		svc = auth.New(o.DB, auth.Options{Logger: slog.New(slog.DiscardHandler), Events: o.Events,
			Params: auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}})
		o.Auth, o.AuthRoutes, o.SessionCheck = svc.Middleware, svc.Routes, svc.StillValid
		o.Heartbeat = 20 * time.Millisecond
	})
	if r := e.get(t, "/api/v1/health"); r.status != 200 {
		t.Fatalf("health: %d", r.status)
	}
	for _, p := range []string{"/api/v1/system", "/api/v1/tasks", "/api/v1/events", "/api/v1/storage/pools", "/api/v1/nope", "/"} {
		if r := e.get(t, p); r.status != 401 || errCode(r) != "setup_required" {
			t.Errorf("%s без сессии: %d %s", p, r.status, r.raw)
		}
	}
	if r := e.do(t, "POST", "/api/v1/storage/pools/plans", "application/json", "{}"); r.status != 401 {
		t.Errorf("план пула без сессии: %d", r.status)
	}
	h, _ := auth.HashPassword("correct horse battery", auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	if _, err := (auth.Store{DB: e.db}).CreateUser(context.Background(), "admin", h, true, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := e.get(t, "/api/v1/events"); r.status != 401 || errCode(r) != "unauthorized" {
		t.Errorf("SSE без сессии: %d %s", r.status, r.raw)
	}

	r := e.do(t, "POST", "/api/v1/auth/login", "application/json", `{"username":"admin","password":"correct horse battery"}`)
	if r.status != 200 {
		t.Fatalf("вход: %d %s", r.status, r.raw)
	}
	var cookie string
	for _, sc := range strings.Split(strings.Join(r.header.Values("Set-Cookie"), "\n"), "\n") {
		if strings.HasPrefix(sc, auth.SessionCookie+"=") {
			cookie = strings.SplitN(sc, ";", 2)[0]
		}
	}
	csrf, _ := r.body["csrf_token"].(string)

	// SSE с сессией работает; после выхода поток закрывается на ближайшем пинге.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/v1/events", nil)
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("SSE с сессией: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", e.srv.URL+"/api/v1/auth/logout", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set(auth.CSRFHeader, csrf)
	lr, err := http.DefaultClient.Do(req)
	if err != nil || lr.StatusCode != 204 {
		t.Fatalf("выход: %v %v", lr.StatusCode, err)
	}
	lr.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("поток SSE не закрылся сервером после выхода: %v", err)
	}

	hd := e.get(t, "/api/v1/health").header
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
	} {
		if hd.Get(k) != want {
			t.Errorf("%s: %q", k, hd.Get(k))
		}
	}
	if csp := hd.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP: %q", csp)
	}
	if hd.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS без TLS")
	}
}

// Фильтры журнала, last_event у дисков, absent — пропавшие.
func TestEventFiltersAndDiskEvents(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.help.results["disks.observe"] = `{"disks":[{"id":"A","name":"Диск 1"},{"id":"C","name":"Диск 2"},{"name":"без ID"}],"complete":true,"issues":[]}`
	e.log.Emit(ctx, events.Event{Type: "task.state"}, true)
	e.log.Emit(ctx, events.Event{Type: events.DiskRemoved, Severity: events.SevCritical, Subject: "disk:A"}, true)
	e.log.Emit(ctx, events.Event{Type: events.DiskReturned, Subject: "disk:A"}, true)
	e.log.Emit(ctx, events.Event{Type: events.DiskRemoved, Severity: events.SevCritical, Subject: "disk:B"}, true)
	e.log.Emit(ctx, events.Event{Type: events.DiskAdded, Subject: "disk:D"}, true) // добавлен и снова исчез без события — не absent

	r := e.get(t, "/api/v1/events/recent?type=disk.&severity=critical")
	evs, _ := r.body["events"].([]any)
	if r.status != 200 || len(evs) != 2 || evs[0].(map[string]any)["subject"] != "disk:B" {
		t.Fatalf("фильтр: %d %s", r.status, r.raw)
	}
	r = e.get(t, "/api/v1/events/recent?subject=disk:A&before=3")
	evs, _ = r.body["events"].([]any)
	if r.status != 200 || len(evs) != 1 || evs[0].(map[string]any)["type"] != events.DiskRemoved {
		t.Fatalf("предмет+страница: %d %s", r.status, r.raw)
	}
	for _, q := range []string{"severity=bad", "before=0", "before=x"} {
		if r := e.get(t, "/api/v1/events/recent?"+q); r.status != 400 {
			t.Errorf("%s: %d", q, r.status)
		}
	}

	r = e.get(t, "/api/v1/disks")
	if r.status != 200 {
		t.Fatalf("disks: %d %s", r.status, r.raw)
	}
	ds := r.body["disks"].([]any)
	a := ds[0].(map[string]any)
	if le, _ := a["last_event"].(map[string]any); le == nil || le["type"] != events.DiskReturned {
		t.Errorf("last_event A: %v", a)
	}
	if _, ok := ds[1].(map[string]any)["last_event"]; ok {
		t.Errorf("у C нет событий: %v", ds[1])
	}
	abs, _ := r.body["absent"].([]any)
	if len(abs) != 1 || abs[0].(map[string]any)["subject"] != "disk:B" {
		t.Errorf("absent: %s", r.raw)
	}
}
