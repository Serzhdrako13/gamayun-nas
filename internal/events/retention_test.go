package events

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetentionPruneAge(t *testing.T) {
	l := newLog(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	ago := func(d int) time.Time { return now.Add(-time.Duration(d) * 24 * time.Hour) }
	emit := func(sev, subj string, days int) int64 {
		t.Helper()
		ev, err := l.Emit(ctx, Event{Type: "x", Severity: sev, Subject: subj, Time: ago(days)}, true)
		if err != nil {
			t.Fatal(err)
		}
		return ev.ID
	}
	// Без предмета: удаляются только старше срока своего уровня.
	oldInfo := emit(SevInfo, "", 91)
	newInfo := emit(SevInfo, "", 89)
	oldWarn := emit(SevWarning, "", 100)
	oldCrit := emit(SevCritical, "", 366)
	keepCrit := emit(SevCritical, "", 200)
	odd := emit("strange", "", 95) // неизвестный уровень — как info
	// Предмет: старые события удаляются, последнее сохраняется при любом возрасте.
	diskOld := emit(SevWarning, "disk:a", 500)
	diskLast := emit(SevWarning, "disk:a", 400)
	poolLast := emit(SevCritical, "pool:p", 1000)

	n, err := l.PruneAge(ctx, DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("удалено %d, ожидалось 5", n)
	}
	left := map[int64]bool{}
	all, _ := l.Since(ctx, 0, 100)
	for _, e := range all {
		left[e.ID] = true
	}
	for id, want := range map[int64]bool{oldInfo: false, newInfo: true, oldWarn: false, oldCrit: false,
		keepCrit: true, odd: false, diskOld: false, diskLast: true, poolLast: true} {
		if left[id] != want {
			t.Errorf("событие %d: осталось=%v, ожидалось %v", id, left[id], want)
		}
	}
	// Повтор ничего не меняет.
	if n, err := l.PruneAge(ctx, DefaultRetention(), now); err != nil || n != 0 {
		t.Errorf("повтор: %d %v", n, err)
	}
}

func TestRetentionSettings(t *testing.T) {
	l := newLog(t)
	ctx := context.Background()
	r, err := LoadRetention(ctx, l.db)
	if err != nil || r != DefaultRetention() || r.CriticalDays != 365 || r.InfoDays != 90 {
		t.Fatalf("по умолчанию: %+v %v", r, err)
	}
	want := Retention{InfoDays: 30, WarningDays: 60, CriticalDays: 730}
	if err := SaveRetention(ctx, l.db, want); err != nil {
		t.Fatal(err)
	}
	if r, err := LoadRetention(ctx, l.db); err != nil || r != want {
		t.Errorf("сохранено: %+v %v", r, err)
	}
	for _, bad := range []Retention{{0, 90, 365}, {90, 90, MaxRetentionDays + 1}, {90, -1, 365}} {
		if err := SaveRetention(ctx, l.db, bad); !errors.Is(err, ErrInvalidRetention) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if r, _ := LoadRetention(ctx, l.db); r != want {
		t.Errorf("после неверных: %+v", r)
	}
	// Испорченная запись — сроки по умолчанию и ошибка.
	l.db.Exec("UPDATE settings SET value = '{\"info_days\":0}' WHERE key = ?", RetentionKey)
	if r, err := LoadRetention(ctx, l.db); err == nil || r != DefaultRetention() {
		t.Errorf("испорченная: %+v %v", r, err)
	}
}
