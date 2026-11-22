package snapshots

import (
	"slices"
	"testing"
	"time"

	"gamayun-nas/internal/nas"
)

func TestPolicyValidate(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Policy{
		{Enabled: true},              // ни одного уровня
		{Enabled: true, Hourly: -1},  // отрицательное
		{Enabled: true, Daily: 1000}, // выше предела
	} {
		if p.Validate() == nil {
			t.Errorf("должно быть отвергнуто: %+v", p)
		}
	}
	if err := (Policy{Enabled: false}).Validate(); err != nil {
		t.Fatalf("выключенное расписание без уровней допустимо: %v", err)
	}
}

func TestPolicyDue(t *testing.T) {
	loc := time.UTC
	at := func(h, m int) time.Time { return time.Date(2026, 11, 18, h, m, 0, 0, loc) }
	p := DefaultPolicy()
	if !p.Due(at(10, 0), time.Time{}, false, loc) {
		t.Fatal("первый снапшот — сразу")
	}
	if p.Due(at(10, 59), at(10, 1), true, loc) {
		t.Fatal("тот же час — рано")
	}
	if !p.Due(at(11, 0), at(10, 59), true, loc) {
		t.Fatal("новый час — пора")
	}
	if p.Due(at(9, 0), at(10, 0), true, loc) {
		t.Fatal("часы назад — не пора")
	}
	daily := Policy{Enabled: true, Daily: 7}
	if daily.Due(at(23, 0), at(0, 5), true, loc) || !daily.Due(at(0, 1).Add(24*time.Hour), at(23, 0), true, loc) {
		t.Fatal("дневной шаг")
	}
	if (Policy{Hourly: 24}).Due(at(12, 0), time.Time{}, false, loc) {
		t.Fatal("выключено — никогда")
	}
}

// auto — снапшоты по расписанию каждый час от start назад n часов.
func hourly(start time.Time, n int) []Snapshot {
	var out []Snapshot
	for i := 0; i < n; i++ {
		tm := start.Add(-time.Duration(i) * time.Hour)
		out = append(out, Snapshot{Name: tm.UTC().Format(nas.SnapshotGoLayout), Time: tm, Auto: true})
	}
	return out
}

func TestPolicyExpired(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, loc)
	snaps := hourly(now, 72) // трое суток каждый час: 18-е 10:00 … 15-е 11:00
	manual := Snapshot{Name: "@GMT-2020.01.01-00.00.00", Time: time.Date(2020, 1, 1, 0, 0, 0, 0, loc)}
	snaps = append(snaps, manual)
	p := Policy{Enabled: true, Hourly: 24, Daily: 7}
	exp := p.Expired(snaps, loc)
	if slices.Contains(exp, manual.Name) {
		t.Fatal("ручной снапшот не ротируется")
	}
	// Остаются: 24 последних часа (18-е 10:00 … 17-е 11:00) + самый новый в
	// днях 16-е и 15-е (18-е и 17-е 23:00 уже среди часовых) = 26.
	if keep := 72 - len(exp); keep != 26 {
		t.Fatalf("оставлено %d, удалено %v", keep, len(exp))
	}
	for _, s := range []string{"@GMT-2026.11.17-23.00.00", "@GMT-2026.11.16-23.00.00", "@GMT-2026.11.15-23.00.00", "@GMT-2026.11.17-11.00.00"} {
		if slices.Contains(exp, s) {
			t.Errorf("%s должен остаться", s)
		}
	}
	if !slices.Contains(exp, "@GMT-2026.11.17-10.00.00") {
		t.Error("17-е 10:00 — вне 24 часов и не последний за день: удаляется")
	}
	if (Policy{Enabled: false, Hourly: 1}).Expired(snaps, loc) != nil {
		t.Fatal("выключенное расписание ничего не удаляет")
	}
}
