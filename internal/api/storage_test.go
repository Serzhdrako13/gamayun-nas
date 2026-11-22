package api

import (
	"context"
	"encoding/json"
	"fmt"
	"gamayun-nas/internal/events"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/storage/home"
)

// storageHelper — подмена helper для пулов: снимок дисков и состояние пула.
type storageHelper struct {
	mu     sync.Mutex
	snap   disks.Snapshot
	status home.PoolStatus
	err    error
}

func (f *storageHelper) Call(_ context.Context, op string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	var v any
	switch op {
	case "disks.observe":
		v = f.snap
	case home.OpStatus:
		v = f.status
	default:
		v = home.Result{Pool: "p1"}
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

func storageEnv(t *testing.T) (*env, *storageHelper, *home.Service, *time.Time) {
	t.Helper()
	size := int64(1 << 30)
	sh := &storageHelper{snap: disks.Snapshot{Complete: true, Disks: []disks.Disk{
		{Name: "Диск 1", KernelName: "sdb", ID: "wwn-0x1", SizeBytes: &size, SystemRole: "unknown", SystemRoles: []string{}},
		{Name: "Диск 2", KernelName: "sdc", ID: "wwn-0x2", SizeBytes: &size, SystemRole: "unknown", SystemRoles: []string{}},
		{Name: "Диск 3", KernelName: "sda", ID: "wwn-0x3", SizeBytes: &size, SystemRole: "root", SystemRoles: []string{"root"}},
	}}, status: home.PoolStatus{Pool: "p1", Health: home.HealthOK, Mounted: true, Arrays: []home.ArrayStatus{{Index: 0, Level: "raid1", RaidDevices: 2}}}}
	var svc *home.Service
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	e := newEnv(t, func(o *Options) {
		var err error
		if svc, err = home.NewService(sh, o.DB, o.Tasks); err != nil {
			t.Fatal(err)
		}
		svc.SetClock(func() time.Time { return now })
		o.Storage = svc
	})
	return e, sh, svc, &now
}

const createBody = `{"op":"create","pool":"p1","type":"raid1","disk_ids":["wwn-0x1","wwn-0x2"],"integrity":true}`

func (e *env) post(t *testing.T, path, body string) result {
	return e.do(t, "POST", path, "application/json", body)
}

func TestStorageUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	for _, r := range []result{e.post(t, "/api/v1/storage/pools/plans", createBody), e.get(t, "/api/v1/storage/pools")} {
		if r.status != 503 || errCode(r) != "storage_unavailable" {
			t.Errorf("%d %s", r.status, r.raw)
		}
	}
}

func TestStoragePlanConfirm(t *testing.T) {
	e, sh, _, now := storageEnv(t)
	plan := func() (string, string) {
		r := e.post(t, "/api/v1/storage/pools/plans", createBody)
		if r.status != 200 {
			t.Fatalf("план: %d %s", r.status, r.raw)
		}
		return r.body["plan_id"].(string), r.body["confirm_token"].(string)
	}
	confirm := func(id, tok string) result {
		return e.post(t, "/api/v1/storage/pools/plans/"+id+"/confirm", fmt.Sprintf(`{"confirm_token":%q}`, tok))
	}

	id, tok := plan()
	if r := confirm(id, "bad"); r.status != 403 || errCode(r) != "bad_token" {
		t.Fatalf("неверный токен: %d %s", r.status, r.raw)
	}
	if r := confirm(id, tok); r.status != 404 || errCode(r) != "plan_not_found" {
		t.Fatalf("после неверного токена: %d %s", r.status, r.raw)
	}

	id, tok = plan()
	*now = now.Add(time.Hour)
	if r := confirm(id, tok); r.status != 410 || errCode(r) != "plan_expired" {
		t.Fatalf("истёкший: %d %s", r.status, r.raw)
	}

	id, tok = plan()
	sh.mu.Lock()
	sh.snap.Disks[1].SystemRole = "in_use"
	sh.mu.Unlock()
	if r := confirm(id, tok); r.status != 409 || errCode(r) != "plan_stale" {
		t.Fatalf("устаревший: %d %s", r.status, r.raw)
	}
	if r := e.post(t, "/api/v1/storage/pools/plans", createBody); r.status != 409 || errCode(r) != "conflict" {
		t.Fatalf("занятый диск: %d %s", r.status, r.raw)
	}
	sh.mu.Lock()
	sh.snap.Disks[1].SystemRole = "unknown"
	sh.mu.Unlock()

	id, tok = plan()
	r := confirm(id, tok)
	if r.status != 202 {
		t.Fatalf("подтверждение: %d %s", r.status, r.raw)
	}
	task := r.body["task"].(map[string]any)
	if task["kind"] != home.TaskCreate {
		t.Fatalf("задача %v", task)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := e.get(t, "/api/v1/tasks/"+task["id"].(string))
		if r.body["state"] == "succeeded" {
			break
		}
		if time.Now().After(deadline) || r.body["state"] == "failed" {
			t.Fatalf("задача: %s", r.raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
	r = e.get(t, "/api/v1/storage/pools")
	pools, _ := r.body["pools"].([]any)
	if r.status != 200 || len(pools) != 1 {
		t.Fatalf("список: %d %s", r.status, r.raw)
	}
	r = e.get(t, "/api/v1/storage/pools/p1")
	st, _ := r.body["status"].(map[string]any)
	if r.status != 200 || r.body["type"] != "raid1" || st["health"] != "ok" {
		t.Fatalf("пул: %d %s", r.status, r.raw)
	}
	if _, ok := r.body["last_event"]; ok {
		t.Fatalf("событий пула нет, а last_event есть: %s", r.raw)
	}
	e.log.Emit(context.Background(), events.Event{Type: events.ArrayDegraded, Severity: events.SevCritical, Subject: "pool:p1"}, true)
	r = e.get(t, "/api/v1/storage/pools/p1")
	if le, _ := r.body["last_event"].(map[string]any); le == nil || le["type"] != events.ArrayDegraded || r.body["type"] != "raid1" {
		t.Fatalf("last_event пула: %s", r.raw)
	}
	r = e.get(t, "/api/v1/storage/pools")
	if p := r.body["pools"].([]any)[0].(map[string]any); p["last_event"] == nil || p["name"] != "p1" {
		t.Fatalf("last_event в списке: %s", r.raw)
	}
	if r := e.get(t, "/api/v1/storage/pools/nope"); r.status != 404 {
		t.Fatalf("нет пула: %d %s", r.status, r.raw)
	}
	// Ошибка helper не скрывает запись пула.
	sh.mu.Lock()
	sh.err = &helper.RemoteError{Code: helper.CodeFailed, Message: "x"}
	sh.mu.Unlock()
	if r := e.get(t, "/api/v1/storage/pools/p1"); r.status != 200 || r.body["status"] != nil || r.body["status_error"] == nil {
		t.Fatalf("пул без helper: %d %s", r.status, r.raw)
	}
	if r := e.post(t, "/api/v1/storage/pools/plans", strings.Replace(createBody, `"p1"`, `"p2"`, 1)); r.status != 502 {
		t.Fatalf("план без helper: %d %s", r.status, r.raw)
	}
}

func TestStoragePlanErrors(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	cases := []struct {
		body   string
		status int
		code   string
	}{
		{`{"op":"create","pool":"p1","type":"raid1","disk_ids":["wwn-0x1"],"x":1}`, 400, "invalid_request"},
		{`{"op":"destroy","pool":"p1"}`, 400, "invalid_request"},
		{`{"op":"create","pool":"P 1","type":"raid1","disk_ids":["wwn-0x1","wwn-0x2"]}`, 400, "invalid_request"},
		{`{"op":"create","pool":"p1","type":"raid1","disk_ids":["wwn-0x1","wwn-0x3"]}`, 409, "conflict"},
		{`{"op":"add_disk","pool":"p9","disk_id":"wwn-0x1"}`, 404, "not_found"},
	}
	for _, c := range cases {
		r := e.post(t, "/api/v1/storage/pools/plans", c.body)
		if r.status != c.status || errCode(r) != c.code {
			t.Errorf("%s: %d %s", c.body, r.status, r.raw)
		}
	}
	if r := e.post(t, "/api/v1/storage/pools/plans/x/confirm", `{}`); r.status != 400 {
		t.Errorf("пустой токен: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/storage/pools/plans", "text/plain", createBody); r.status != 400 {
		t.Errorf("Content-Type: %d", r.status)
	}
}

func TestStorageDeleteAndOverride(t *testing.T) {
	e, _, svc, _ := storageEnv(t)
	_ = svc
	if r := e.do(t, "DELETE", "/api/v1/storage/pools/p1", "application/json", `{"confirm_name":"p1"}`); r.status != 404 {
		t.Fatalf("нет пула: %d %s", r.status, r.raw)
	}
	if _, err := e.db.Exec(`INSERT INTO pools(name, type, integrity, layout, created_at, updated_at) VALUES ('p1','raid1',0,'{"disks":[],"arrays":[]}',0,0)`); err != nil {
		t.Fatal(err)
	}
	if r := e.do(t, "DELETE", "/api/v1/storage/pools/p1", "application/json", `{"confirm_name":"x"}`); r.status != 400 {
		t.Fatalf("неверное подтверждение: %d %s", r.status, r.raw)
	}
	if r := e.post(t, "/api/v1/storage/pools/p1/write-override", `{"reason":"","duration_minutes":60}`); r.status != 400 {
		t.Fatalf("без причины: %d %s", r.status, r.raw)
	}
	if r := e.post(t, "/api/v1/storage/pools/p1/write-override", `{"reason":"копия","duration_minutes":60}`); r.status != 200 {
		t.Fatalf("разрешение: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/storage/pools/p1/protection/log"); r.status != 200 || !strings.Contains(string(r.raw), `"action":"override"`) {
		t.Fatalf("журнал: %d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/storage/pools/p1"); r.status != 200 || !strings.Contains(string(r.raw), `"protection"`) {
		t.Fatalf("состояние: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "DELETE", "/api/v1/storage/pools/p1", "application/json", `{"confirm_name":"p1"}`); r.status != 202 {
		t.Fatalf("удаление: %d %s", r.status, r.raw)
	}
}

// «Продолжить замену»: нет пула — 404, нечего
// продолжать — 409, незавершённый рост — 202 и задача storage.pool.resume.
func TestStorageResume(t *testing.T) {
	e, _, _, _ := storageEnv(t)
	if r := e.post(t, "/api/v1/storage/pools/p1/resume", `{}`); r.status != 404 {
		t.Fatalf("нет пула: %d %s", r.status, r.raw)
	}
	if _, err := e.db.Exec(`INSERT INTO pools(name, type, integrity, layout, created_at, updated_at) VALUES ('p1','raid1',0,'{"disks":[],"arrays":[]}',0,0)`); err != nil {
		t.Fatal(err)
	}
	if r := e.post(t, "/api/v1/storage/pools/p1/resume", `{}`); r.status != 409 || errCode(r) != "conflict" {
		t.Fatalf("нечего продолжать: %d %s", r.status, r.raw)
	}
	if _, err := e.db.Exec(`UPDATE pools SET layout='{"disks":[],"arrays":[],"grow_pending":true}' WHERE name='p1'`); err != nil {
		t.Fatal(err)
	}
	r := e.post(t, "/api/v1/storage/pools/p1/resume", `{}`)
	if r.status != 202 || !strings.Contains(string(r.raw), `"storage.pool.resume"`) {
		t.Fatalf("продолжение: %d %s", r.status, r.raw)
	}
}
