package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Список блокировок и ручная разблокировка администратором.
func TestLockoutsListUnlock(t *testing.T) {
	e := newTEnv(t, func(o *Options) {
		o.IPPolicy = LimitPolicy{Free: 12, Base: time.Minute, Max: time.Hour, Window: time.Hour}
		o.AccountPolicy = LimitPolicy{Free: 5, Base: time.Minute, Max: time.Hour, Window: time.Hour}
	})
	e.addAdmin(t, "admin", goodPW)
	owner := e.client(t)
	if r := owner.login("admin", goodPW); r.status != 200 {
		t.Fatal(r.raw)
	}
	att := e.client(t)
	for i := 0; i < 6; i++ {
		att.login("admin", "guess")
		att.login("Secret-Typed-As-Name", "x")
	}
	att.login("other", "x") // 13-я неудача — блокировка адреса
	if r := att.login("admin", goodPW); r.status != 429 {
		t.Fatalf("ожидалась блокировка: %d", r.status)
	}
	// Без сессии — 401.
	if r := att.do("GET", "/api/v1/auth/lockouts", ""); r.status != 401 {
		t.Fatalf("без сессии: %d", r.status)
	}
	r := owner.do("GET", "/api/v1/auth/lockouts", "")
	if r.status != 200 {
		t.Fatalf("список: %d %s", r.status, r.raw)
	}
	if strings.Contains(strings.ToLower(r.raw), "secret-typed") {
		t.Error("несуществующее имя выдано в списке")
	}
	ls, _ := r.body["lockouts"].([]any)
	found := map[string]map[string]any{}
	for _, x := range ls {
		m := x.(map[string]any)
		k := m["scope"].(string)
		if u, _ := m["user"].(string); u != "" {
			k += ":" + u
		} else if m["unknown_user"] == true {
			k += ":?"
		}
		found[k] = m
	}
	if len(ls) != 3 || found["ip"] == nil || found["account:admin"] == nil || found["account:?"] == nil {
		t.Fatalf("блокировки: %s", r.raw)
	}
	if found["ip"]["address"] != "127.0.0.1" || found["account:admin"]["fails"].(float64) != 6 || found["account:admin"]["until"] == "" {
		t.Errorf("поля: %v", found)
	}
	// Снятие блокировки учётки: адрес всё ещё заблокирован.
	id := found["account:admin"]["id"].(string)
	if r := owner.do("DELETE", "/api/v1/auth/lockouts/"+id, ""); r.status != 204 {
		t.Fatalf("разблокировка: %d %s", r.status, r.raw)
	}
	if r := owner.do("DELETE", "/api/v1/auth/lockouts/"+id, ""); r.status != 404 {
		t.Errorf("повторная разблокировка: %d", r.status)
	}
	if r := att.login("admin", goodPW); r.status != 429 {
		t.Errorf("адрес должен оставаться заблокированным: %d", r.status)
	}
	// Снять всё — вход проходит.
	if r := owner.do("DELETE", "/api/v1/auth/lockouts", ""); r.status != 200 || r.body["unlocked"].(float64) != 2 {
		t.Fatalf("снять всё: %d %s", r.status, r.raw)
	}
	if r := att.login("admin", goodPW); r.status != 200 {
		t.Fatalf("после разблокировки: %d", r.status)
	}
	evs, _ := e.log.Since(context.Background(), 0, 1000)
	n := 0
	for _, ev := range evs {
		if ev.Type == EventUnlock {
			n++
			if !strings.Contains(string(ev.Data), `"actor":"admin"`) {
				t.Errorf("событие без actor: %s", ev.Data)
			}
		}
	}
	if n != 3 {
		t.Errorf("событий auth.unlock: %d", n)
	}
	// Без CSRF-токена — отказ.
	owner.csrf = ""
	if r := owner.do("DELETE", "/api/v1/auth/lockouts", ""); r.status != 403 {
		t.Errorf("без CSRF: %d", r.status)
	}
}
