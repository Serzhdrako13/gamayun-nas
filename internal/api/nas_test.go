package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
)

func TestNasUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.get(t, "/api/v1/nas/users"); r.status != 503 || errCode(r) != "nas_unavailable" {
		t.Errorf("%d %s", r.status, r.raw)
	}
}

func TestNasFlow(t *testing.T) {
	sh := &storageHelper{}
	e := newEnv(t, func(o *Options) { o.NAS = nas.NewService(sh, o.DB) })
	if _, err := e.db.Exec("INSERT INTO pools(name, type, layout, created_at, updated_at) VALUES ('p1','shr','{}',0,0)"); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/v1/nas/users", `{"name":"ann","password":"password-1"}`, 201},
		{"POST", "/api/v1/nas/users", `{"name":"ann","password":"password-1"}`, 409},
		{"POST", "/api/v1/nas/users", `{"name":"B","password":"password-1"}`, 400},
		{"PUT", "/api/v1/nas/users/ann/ssh", `{"ssh":true}`, 204},
		{"PUT", "/api/v1/nas/users/ann/ssh", `{}`, 400},
		{"PUT", "/api/v1/nas/users/zz/ssh", `{"ssh":true}`, 404},
		{"POST", "/api/v1/nas/users", `{"name":"boss","password":"password-1","admin":true,"ssh":false}`, 409},
		{"PUT", "/api/v1/nas/users/boss/ssh", `{"ssh":true}`, 404}, // при отказе пользователь не создан
		{"POST", "/api/v1/nas/groups", `{"name":"staff"}`, 201},
		{"PUT", "/api/v1/nas/groups/staff/members", `{"members":["ann"]}`, 204},
		{"POST", "/api/v1/nas/shares", `{"name":"docs","pool":"p1"}`, 201},
		{"POST", "/api/v1/nas/shares", `{"name":"x","pool":"zz"}`, 404},
		{"PUT", "/api/v1/nas/shares/docs/access", `{"access":[{"kind":"group","subject":"staff","level":"rw"}]}`, 200},
		{"PUT", "/api/v1/nas/shares/docs/access", `{"access":[{"kind":"group","subject":"staff","level":"x"}]}`, 400},
		{"POST", "/api/v1/nas/shares/apply", `{}`, 200},
		{"GET", "/api/v1/nas/shares/docs/dirs", ``, 200},
		{"GET", "/api/v1/nas/shares/docs/dirs?path=a/b&depth=2", ``, 200},
		{"GET", "/api/v1/nas/shares/docs/dirs?path=../etc", ``, 400},
		{"GET", "/api/v1/nas/shares/docs/dirs?path=/etc", ``, 400},
		{"GET", "/api/v1/nas/shares/docs/dirs?depth=9", ``, 400},
		{"GET", "/api/v1/nas/shares/docs/dirs?depth=x", ``, 400},
		{"GET", "/api/v1/nas/shares/nope/dirs", ``, 404},
		{"DELETE", "/api/v1/nas/shares/docs", ``, 204},
		{"DELETE", "/api/v1/nas/users/ann", ``, 204},
		{"DELETE", "/api/v1/nas/users/ann", ``, 404},
	}
	for _, s := range steps {
		if r := e.do(t, s.method, s.path, "application/json", s.body); r.status != s.status {
			t.Errorf("%s %s: %d, ожидалось %d: %s", s.method, s.path, r.status, s.status, r.raw)
		}
	}
}

// Мастер, имя сервера, домашние папки, защита SSH.
type recHelper struct {
	mu  sync.Mutex
	ops []string
}

func (f *recHelper) Call(_ context.Context, op string, params, result any) error {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.mu.Unlock()
	switch op {
	case nas.OpIdentityGet:
		return json.Unmarshal([]byte(`{"hostname":"nas","timezone":"UTC","ntp":true,"ntp_synced":true,"timezones":["UTC","Europe/Moscow"]}`), result)
	case nas.OpGuardStatus:
		return json.Unmarshal([]byte(`{"installed":true,"active":false,"banned":null}`), result)
	}
	return nil
}

func TestSettingsAPI(t *testing.T) {
	h := &recHelper{}
	e := newEnv(t, func(o *Options) { o.NAS = nas.NewService(h, o.DB) })
	if _, err := e.db.Exec("INSERT INTO pools(name, type, layout, created_at, updated_at) VALUES ('p1','shr','{}',0,0)"); err != nil {
		t.Fatal(err)
	}
	if r := e.get(t, "/api/v1/setup/status"); r.status != 200 || r.body["done"] != false || r.body["product"] == "" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if r := e.get(t, "/api/v1/system/identity"); r.status != 200 || r.body["hostname"] != "nas" {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	steps := []struct {
		method, path, body string
		status             int
	}{
		{"PUT", "/api/v1/system/identity", `{"hostname":"Bad Name","timezone":"UTC","ntp":true}`, 400},
		{"PUT", "/api/v1/system/identity", `{"hostname":"nas2","timezone":"Europe/Moscow","ntp":true}`, 200},
		{"PUT", "/api/v1/nas/homes", `{"enabled":true,"pool":""}`, 400},
		{"PUT", "/api/v1/nas/homes", `{"enabled":true,"pool":"zz"}`, 404},
		{"PUT", "/api/v1/nas/homes", `{"enabled":true,"pool":"p1"}`, 200},
		{"POST", "/api/v1/nas/shares", `{"name":"homes","pool":"p1"}`, 400},
		{"PUT", "/api/v1/nas/homes", `{"enabled":false}`, 200},
		{"PUT", "/api/v1/system/ssh-guard", `{"enabled":true,"maxretry":0,"findtime":600,"bantime":600}`, 400},
		{"PUT", "/api/v1/system/ssh-guard", `{"enabled":true,"maxretry":3,"findtime":300,"bantime":900}`, 200},
		{"POST", "/api/v1/system/ssh-guard/unban", `{"ip":"nonsense"}`, 400},
		{"POST", "/api/v1/system/ssh-guard/unban", `{"ip":"192.0.2.7"}`, 204},
		{"POST", "/api/v1/setup/complete", `{}`, 200},
		{"POST", "/api/v1/setup/complete", `{}`, 409}, // второго прохода нет
		{"PUT", "/api/v1/system/ssh", `{}`, 400},      // enabled обязателен
		{"PUT", "/api/v1/system/ssh", `{"enabled":true}`, 200},
		{"PUT", "/api/v1/system/ssh", `{"enabled":false,"kill_sessions":true}`, 200},
	}
	for _, s := range steps {
		if r := e.do(t, s.method, s.path, "application/json", s.body); r.status != s.status {
			t.Errorf("%s %s: %d, ожидалось %d: %s", s.method, s.path, r.status, s.status, r.raw)
		}
	}
	// Выключение и перезагрузка — только с confirm = action.
	for _, s := range []struct {
		body   string
		status int
	}{
		{`{"action":"reboot"}`, 400},
		{`{"action":"reboot","confirm":"poweroff"}`, 400},
		{`{"action":"halt","confirm":"halt"}`, 400},
		{`{"action":"reboot","confirm":"reboot"}`, 202},
		{`{"action":"poweroff","confirm":"poweroff"}`, 202},
	} {
		if r := e.do(t, "POST", "/api/v1/system/power", "application/json", s.body); r.status != s.status {
			t.Errorf("power %s: %d, ожидалось %d: %s", s.body, r.status, s.status, r.raw)
		}
	}
	n := 0
	for _, op := range h.ops {
		if op == nas.OpPower {
			n++
		}
	}
	if n != 2 {
		t.Errorf("вызовов %s: %d, ожидалось 2", nas.OpPower, n)
	}
	if r := e.get(t, "/api/v1/setup/status"); r.body["done"] != true {
		t.Fatalf("%s", r.raw)
	}
	if r := e.get(t, "/api/v1/system/ssh"); r.status != 200 || r.body["enabled"] != false || r.body["ssh_users"] != float64(0) {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	// после мастера: выключено (мастер), включено, выключено с разрывом сессий
	var sshOps []string
	for _, op := range h.ops {
		if op == nas.OpSSHApply {
			sshOps = append(sshOps, op)
		}
	}
	if len(sshOps) != 3 {
		t.Errorf("вызовов %s: %d, ожидалось 3", nas.OpSSHApply, len(sshOps))
	}
	r := e.get(t, "/api/v1/system/ssh-guard")
	st, _ := r.body["settings"].(map[string]any)
	if r.status != 200 || st["enabled"] != true || st["maxretry"] != float64(3) {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

// Вложенные каталоги — создание, переименование, удаление и
// сопоставление кодов ошибок helper со статусами HTTP.
type dirHelper struct {
	mu   sync.Mutex
	fail map[string]*helper.RemoteError // op → ошибка helper
	last map[string]string              // op → параметры последнего вызова
}

func (f *dirHelper) Call(_ context.Context, op string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(params)
	f.last[op] = string(b)
	if e := f.fail[op]; e != nil {
		return e
	}
	switch op {
	case nas.OpShareMkdir, nas.OpShareRename, nas.OpShareRmdir, nas.OpShareMove:
		return json.Unmarshal([]byte(`{"path":"a/new","changed":true}`), result)
	}
	return nil
}

func TestNasShareDirOps(t *testing.T) {
	h := &dirHelper{fail: map[string]*helper.RemoteError{}, last: map[string]string{}}
	e := newEnv(t, func(o *Options) { o.NAS = nas.NewService(h, o.DB) })
	if _, err := e.db.Exec("INSERT INTO pools(name, type, layout, created_at, updated_at) VALUES ('p1','shr','{}',0,0)"); err != nil {
		t.Fatal(err)
	}
	if r := e.post(t, "/api/v1/nas/shares", `{"name":"docs","pool":"p1"}`); r.status != 201 {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	steps := []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"a","name":"new"}`, 201, ""},
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"","name":"top"}`, 201, ""},
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"../etc","name":"x"}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"/etc","name":"x"}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"a","name":".."}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"a","name":"b/c"}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"a","name":""}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs", `{"path":"a","name":"x","extra":1}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/nope/dirs", `{"path":"","name":"x"}`, 404, "not_found"},
		{"PATCH", "/api/v1/nas/shares/docs/dirs", `{"path":"a/old","name":"new"}`, 200, ""},
		{"PATCH", "/api/v1/nas/shares/docs/dirs", `{"path":"","name":"new"}`, 400, "invalid_request"},
		{"PATCH", "/api/v1/nas/shares/docs/dirs", `{"path":"a","name":"x/y"}`, 400, "invalid_request"},
		{"DELETE", "/api/v1/nas/shares/docs/dirs?path=a/b", ``, 204, ""},
		{"DELETE", "/api/v1/nas/shares/docs/dirs", ``, 400, "invalid_request"},
		{"DELETE", "/api/v1/nas/shares/docs/dirs?path=../x", ``, 400, "invalid_request"},
		{"DELETE", "/api/v1/nas/shares/nope/dirs?path=a", ``, 404, "not_found"},
		// Перенос.
		{"POST", "/api/v1/nas/shares/docs/dirs/move", `{"path":"a/b","to":"c"}`, 200, ""},
		{"POST", "/api/v1/nas/shares/docs/dirs/move", `{"path":"a","to":"a/b"}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs/move", `{"path":"","to":"c"}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs/move", `{"path":"a","to":"../x"}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/docs/dirs/move", `{"path":"a","to":"c","name":"x"}`, 400, "invalid_request"},
		{"POST", "/api/v1/nas/shares/nope/dirs/move", `{"path":"a","to":""}`, 404, "not_found"},
	}
	for _, s := range steps {
		r := e.do(t, s.method, s.path, "application/json", s.body)
		if r.status != s.status || (s.code != "" && errCode(r) != s.code) {
			t.Errorf("%s %s %s: %d %s, ожидалось %d %s", s.method, s.path, s.body, r.status, r.raw, s.status, s.code)
		}
	}
	// Windows-несовместимые имена — 400 с текстом причины, до helper.
	before := h.last[nas.OpShareMkdir] + h.last[nas.OpShareRename]
	for _, c := range []struct{ method, body, msg string }{
		{"POST", `{"path":"a","name":"a:b"}`, "символ «:» в имени папки недопустим"},
		{"POST", `{"path":"","name":"CON.txt"}`, "CON — зарезервированное имя устройства Windows"},
		{"POST", `{"path":"a","name":"отчёт."}`, "не может оканчиваться точкой"},
		{"POST", `{"path":"a","name":"x "}`, "не может оканчиваться пробелом"},
		{"PATCH", `{"path":"a/old","name":"lpt1"}`, "LPT1 — зарезервированное имя устройства Windows"},
		{"PATCH", `{"path":"a/old","name":"что?"}`, "символ «?» в имени папки недопустим"},
	} {
		r := e.do(t, c.method, "/api/v1/nas/shares/docs/dirs", "application/json", c.body)
		if r.status != 400 || errCode(r) != "invalid_request" || !strings.Contains(r.raw, c.msg) {
			t.Errorf("%s %s: %d %s, ждали 400 «%s»", c.method, c.body, r.status, r.raw, c.msg)
		}
	}
	// Существующий каталог с таким именем можно переименовать в допустимое.
	if r := e.do(t, "PATCH", "/api/v1/nas/shares/docs/dirs", "application/json", `{"path":"a:b","name":"ab"}`); r.status != 200 {
		t.Errorf("переименование a:b → ab: %d %s", r.status, r.raw)
	}
	if after := h.last[nas.OpShareMkdir] + h.last[nas.OpShareRename]; !strings.Contains(after, `"path":"a:b"`) || strings.Contains(before+after, "CON.txt") {
		t.Errorf("до helper дошло лишнее: %s", after)
	}
	if r := e.post(t, "/api/v1/nas/shares", `{"name":"nul","pool":"p1"}`); r.status != 400 || !strings.Contains(r.raw, "NUL — зарезервированное имя устройства Windows") {
		t.Errorf("папка nul: %d %s", r.status, r.raw)
	}
	if !strings.Contains(h.last[nas.OpShareMkdir], `"pool":"p1"`) || !strings.Contains(h.last[nas.OpShareMkdir], `"name":"top"`) {
		t.Errorf("параметры mkdir: %s", h.last[nas.OpShareMkdir])
	}
	if h.last[nas.OpShareRmdir] != `{"pool":"p1","share":"docs","path":"a/b"}` {
		t.Errorf("параметры rmdir: %s", h.last[nas.OpShareRmdir])
	}
	// Коды helper → HTTP.
	for _, c := range []struct {
		code   string
		status int
		api    string
	}{
		{helper.CodeConflict, 409, "conflict"},
		{helper.CodeNotFound, 404, "not_found"},
		{helper.CodeInvalidParams, 400, "invalid_request"},
		{helper.CodeRefused, 400, "refused"},
		{helper.CodeFailed, 502, "helper_error"},
	} {
		for _, op := range []string{nas.OpShareMkdir, nas.OpShareRename, nas.OpShareRmdir, nas.OpShareMove} {
			h.mu.Lock()
			h.fail = map[string]*helper.RemoteError{op: {Code: c.code, Message: "сообщение"}}
			h.mu.Unlock()
			var r result
			switch op {
			case nas.OpShareMkdir:
				r = e.do(t, "POST", "/api/v1/nas/shares/docs/dirs", "application/json", `{"path":"","name":"x"}`)
			case nas.OpShareRename:
				r = e.do(t, "PATCH", "/api/v1/nas/shares/docs/dirs", "application/json", `{"path":"a","name":"x"}`)
			case nas.OpShareMove:
				r = e.do(t, "POST", "/api/v1/nas/shares/docs/dirs/move", "application/json", `{"path":"a","to":"c"}`)
			default:
				r = e.do(t, "DELETE", "/api/v1/nas/shares/docs/dirs?path=a", "application/json", ``)
			}
			if r.status != c.status || errCode(r) != c.api {
				t.Errorf("%s %s: %d %s, ожидалось %d %s", op, c.code, r.status, r.raw, c.status, c.api)
			}
		}
	}
}
