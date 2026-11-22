package events

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"gamayun-nas/internal/store"
)

func newLog(t *testing.T) *Log {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewLog(db)
}

func TestEmitPersistAndSince(t *testing.T) {
	l := newLog(t)
	ctx := context.Background()
	sub := l.Subscribe(8)
	defer sub.Close()

	a, err := l.Emit(ctx, Event{Type: "task.state", TaskID: "t1", Data: json.RawMessage(`{"x":1}`)}, true)
	if err != nil || a.ID == 0 {
		t.Fatalf("emit: %+v, %v", a, err)
	}
	p, _ := l.Emit(ctx, Event{Type: "task.progress", TaskID: "t1"}, false)
	if p.ID != 0 {
		t.Errorf("эфемерное событие получило ID %d", p.ID)
	}
	b, _ := l.Emit(ctx, Event{Type: "task.state"}, true)
	if b.ID <= a.ID {
		t.Errorf("ID не возрастает: %d после %d", b.ID, a.ID)
	}

	got := []Event{<-sub.C, <-sub.C, <-sub.C}
	if got[0].ID != a.ID || got[1].ID != 0 || got[2].ID != b.ID {
		t.Errorf("порядок рассылки: %+v", got)
	}

	since, err := l.Since(ctx, 0, 10)
	if err != nil || len(since) != 2 {
		t.Fatalf("журнал: %+v, %v", since, err)
	}
	if string(since[0].Data) != `{"x":1}` || since[0].TaskID != "t1" {
		t.Errorf("данные журнала: %+v", since[0])
	}
	since, _ = l.Since(ctx, a.ID, 10)
	if len(since) != 1 || since[0].ID != b.ID {
		t.Errorf("Since(after) = %+v", since)
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	l := newLog(t)
	sub := l.Subscribe(1)
	ctx := context.Background()
	l.Emit(ctx, Event{Type: "a"}, false)
	l.Emit(ctx, Event{Type: "b"}, false) // переполнение → отключение
	if _, ok := <-sub.C; !ok {
		t.Fatal("первое событие должно быть доставлено")
	}
	if _, ok := <-sub.C; ok {
		t.Fatal("канал отстающего подписчика должен быть закрыт")
	}
	sub.Close() // повторное закрытие безопасно
}

func TestPrune(t *testing.T) {
	l := newLog(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		l.Emit(ctx, Event{Type: "x"}, true)
	}
	n, err := l.Prune(ctx, 2)
	if err != nil || n != 3 {
		t.Fatalf("prune: %d, %v", n, err)
	}
	left, _ := l.Since(ctx, 0, 10)
	if len(left) != 2 || left[1].ID != 5 {
		t.Errorf("осталось: %+v", left)
	}
}

func TestSeveritySubject(t *testing.T) {
	l := newLog(t)
	ctx := context.Background()
	a, _ := l.Emit(ctx, Event{Type: "task.state"}, true)
	if a.Severity != SevInfo {
		t.Errorf("уровень по умолчанию: %q", a.Severity)
	}
	l.Emit(ctx, Event{Type: DiskRemoved, Severity: SevCritical, Subject: "disk:A"}, true)
	l.Emit(ctx, Event{Type: DiskAdded, Subject: "disk:B"}, true)
	l.Emit(ctx, Event{Type: DiskReturned, Severity: SevWarning, Subject: "disk:A"}, true)

	ev, ok, err := l.LastBySubject(ctx, "disk:A")
	if err != nil || !ok || ev.Type != DiskReturned || ev.Severity != SevWarning || ev.Subject != "disk:A" {
		t.Fatalf("последнее по disk:A: %+v %v %v", ev, ok, err)
	}
	if _, ok, _ := l.LastBySubject(ctx, "disk:Z"); ok {
		t.Error("нашлось событие несуществующего предмета")
	}
	all, _ := l.Since(ctx, 0, 10)
	if len(all) != 4 || all[1].Severity != SevCritical || all[0].Subject != "" {
		t.Errorf("журнал: %+v", all)
	}
}

func TestQueryAndLastPerSubject(t *testing.T) {
	l := newLog(t)
	ctx := context.Background()
	l.Emit(ctx, Event{Type: "task.state"}, true)                                            // 1
	l.Emit(ctx, Event{Type: DiskRemoved, Severity: SevCritical, Subject: "disk:A"}, true)   // 2
	l.Emit(ctx, Event{Type: ArrayDegraded, Severity: SevCritical, Subject: "pool:p"}, true) // 3
	l.Emit(ctx, Event{Type: DiskReturned, Subject: "disk:A"}, true)                         // 4
	l.Emit(ctx, Event{Type: DiskRemoved, Severity: SevWarning, Subject: "disk:AB"}, true)   // 5
	ids := func(f Filter) []int64 {
		f.Limit = 10
		evs, err := l.Query(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, e := range evs {
			out = append(out, e.ID)
		}
		return out
	}
	check := func(name string, got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %v, ждали %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: %v, ждали %v", name, got, want)
			}
		}
	}
	check("тип-префикс", ids(Filter{Type: "disk."}), 5, 4, 2)
	check("тип точно", ids(Filter{Type: DiskRemoved}), 5, 2)
	check("предмет точно", ids(Filter{Subject: "disk:A"}), 4, 2)
	check("предмет-префикс", ids(Filter{Subject: "disk:"}), 5, 4, 2)
	check("warning+", ids(Filter{Severity: SevWarning}), 5, 3, 2)
	check("critical", ids(Filter{Severity: SevCritical}), 3, 2)
	check("страница", ids(Filter{Before: 4, Type: "disk."}), 2)
	m, err := l.LastPerSubject(ctx, "disk:")
	if err != nil || len(m) != 2 || m["disk:A"].ID != 4 || m["disk:AB"].ID != 5 {
		t.Fatalf("последние: %+v %v", m, err)
	}
}
