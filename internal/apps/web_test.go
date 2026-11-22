package apps

import (
	"context"
	"errors"
	"testing"

	"gamayun-nas/internal/apps/compose"
)

func TestSetWeb(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	calls := 0
	r.s.OnRoutes = func() { calls++ }
	ports := []compose.Port{{Service: "web", Host: 8096, Container: 8096, Proto: "tcp"},
		{Service: "web", Host: 9000, Container: 9000, Proto: "tcp", Network: true},
		{Service: "web", Host: 7000, Container: 7000, Proto: "udp"}}
	for _, p := range []Project{{ID: "jf", Name: "jf", Ports: ports}, {ID: "b", Name: "b", Ports: ports},
		{ID: "gone", Removed: true}} {
		if err := r.s.putProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	bad := []struct {
		id  string
		w   WebRoute
		err error
	}{
		{"jf", WebRoute{Name: "Jelly", Port: 8096}, ErrInvalidSvc},
		{"jf", WebRoute{Name: "jelly", Port: 9000}, ErrInvalidSvc}, // «в сети»
		{"jf", WebRoute{Name: "jelly", Port: 7000}, ErrInvalidSvc}, // udp
		{"jf", WebRoute{Name: "jelly", Port: 1}, ErrInvalidSvc},
		{"gone", WebRoute{Name: "x", Port: 8096}, ErrNotFound},
		{"none", WebRoute{Name: "x", Port: 8096}, ErrNotFound},
	}
	for _, b := range bad {
		w := b.w
		if _, err := r.s.SetWeb(ctx, b.id, &w, "admin"); !errors.Is(err, b.err) {
			t.Errorf("%s %+v: %v, ожидалось %v", b.id, b.w, err, b.err)
		}
	}
	if calls != 0 {
		t.Fatalf("отказы вызвали OnRoutes: %d", calls)
	}
	if _, err := r.s.SetWeb(ctx, "jf", &WebRoute{Name: "jelly", Port: 8096}, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.SetWeb(ctx, "b", &WebRoute{Name: "jelly", Port: 8096}, "admin"); !errors.Is(err, ErrConflictSvc) {
		t.Fatalf("занятый поддомен: %v", err)
	}
	rs := r.s.Routes(ctx)
	if len(rs) != 1 || rs[0].App != "jf" || rs[0].Name != "jelly" || rs[0].Port != 8096 || calls != 1 {
		t.Fatalf("маршруты: %+v calls=%d", rs, calls)
	}
	// Задача приложения — публикация недоступна.
	if err := r.s.lockApp("jf"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.SetWeb(ctx, "jf", nil, "admin"); !errors.Is(err, ErrConflictSvc) {
		t.Fatalf("во время задачи: %v", err)
	}
	r.s.unlockApp("jf")
	if _, err := r.s.SetWeb(ctx, "jf", nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if rs := r.s.Routes(ctx); len(rs) != 0 || calls != 2 {
		t.Fatalf("после снятия: %+v calls=%d", rs, calls)
	}
	n := 0
	for _, ty := range r.types(t) {
		if ty == EvAppWeb {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("событий %s: %d", EvAppWeb, n)
	}
}

func TestKeepWeb(t *testing.T) {
	w := &WebRoute{Name: "a", Port: 80}
	if keepWeb(w, []compose.Port{{Host: 80, Proto: "tcp"}}) == nil {
		t.Fatal("порт остался — публикация должна сохраниться")
	}
	if keepWeb(w, []compose.Port{{Host: 81, Proto: "tcp"}}) != nil || keepWeb(nil, nil) != nil {
		t.Fatal("порт исчез — публикация снимается")
	}
}
