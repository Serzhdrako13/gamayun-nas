package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gamayun-nas/internal/tasks"
)

func TestExhausted(t *testing.T) {
	arr := func(level string, n, missing int) PoolStatus {
		h := HealthOK
		if missing > 0 {
			h = HealthDegraded
		}
		return PoolStatus{Health: h, Arrays: []ArrayStatus{{Name: "p1-0", Level: level, RaidDevices: n, Missing: missing}}}
	}
	for _, c := range []struct {
		name string
		st   PoolStatus
		want bool
	}{
		{"raid1 ok", arr(LevelRAID1, 2, 0), false},
		{"raid1 2/1", arr(LevelRAID1, 2, 1), true},
		{"raid1 3/2 запас есть", arr(LevelRAID1, 3, 1), false},
		{"raid5 -1", arr(LevelRAID5, 3, 1), true},
		{"raid6 -1 запас есть", arr(LevelRAID6, 4, 1), false},
		{"raid6 -2", arr(LevelRAID6, 4, 2), true},
		{"raid0 исправен", arr(LevelRAID0, 2, 0), false},
		{"failed", PoolStatus{Health: HealthFailed}, true},
		{"missing", PoolStatus{Health: HealthMissing}, false},
		{"btrfs без устройства", PoolStatus{Health: HealthOK, Btrfs: &BtrfsUsage{DeviceMissing: 1}}, true},
	} {
		if got, _ := Exhausted(c.st); got != c.want {
			t.Errorf("%s: %v, ожидалось %v", c.name, got, c.want)
		}
	}
}

type fakeShares struct{ calls []bool }

func (f *fakeShares) SetPoolReadOnly(_ context.Context, _ string, ro bool) error {
	f.calls = append(f.calls, ro)
	return nil
}

func degraded(ro bool) PoolStatus {
	st := okStatus(LevelRAID1, 2, gib)
	st.Health, st.Mounted, st.ReadOnly = HealthDegraded, true, ro
	st.Arrays[0].Missing = 1
	return st
}

func TestProtectionReadOnlyAndOverride(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	sh := &fakeShares{}
	e.s.Shares = sh

	// Исправный пул: ничего не делается.
	ok := okStatus(LevelRAID1, 2, gib)
	ok.Mounted = true
	e.h.setStatus(ok)
	if err := e.s.EnforceAll(ctx); err != nil || e.h.called(OpSetReadOnly) != 0 {
		t.Fatalf("%v %d", err, e.h.called(OpSetReadOnly))
	}

	// Запас исчерпан → read-only (по умолчанию включено).
	e.h.setStatus(degraded(false))
	if err := e.s.EnforceAll(ctx); err != nil {
		t.Fatal(err)
	}
	var p ReadOnlyParams
	json.Unmarshal(e.h.params[OpSetReadOnly], &p)
	if e.h.called(OpSetReadOnly) != 1 || !p.ReadOnly || len(sh.calls) != 1 || !sh.calls[0] {
		t.Fatalf("read-only не применён: %d %+v %v", e.h.called(OpSetReadOnly), p, sh.calls)
	}
	e.h.setStatus(degraded(true))
	v, err := e.s.Get(ctx, "p1")
	if err != nil || !v.Protection.WriteBlocked || !v.Protection.Enforced || !v.Protection.ReadOnly {
		t.Fatalf("%v %+v", err, v.Protection)
	}

	// Ручное разрешение: проверки и журнал.
	if _, err := e.s.SetWriteOverride(ctx, "p1", "admin", "", time.Hour); !errors.Is(err, ErrInvalid) {
		t.Fatalf("без причины: %v", err)
	}
	if _, err := e.s.SetWriteOverride(ctx, "p1", "admin", "копирую данные", 30*24*time.Hour); !errors.Is(err, ErrInvalid) {
		t.Fatalf("слишком долго: %v", err)
	}
	pr, err := e.s.SetWriteOverride(ctx, "p1", "admin", "копирую данные", time.Hour)
	if err != nil || pr.Override == nil || pr.WriteBlocked || pr.Enforced {
		t.Fatalf("%v %+v", err, pr)
	}
	json.Unmarshal(e.h.params[OpSetReadOnly], &p)
	if p.ReadOnly {
		t.Fatal("запись не возвращена")
	}
	e.h.setStatus(degraded(false))

	// Срок истёк, запас всё ещё исчерпан → снова read-only.
	e.now = e.now.Add(61 * time.Minute)
	if err := e.s.EnforceAll(ctx); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(e.h.params[OpSetReadOnly], &p)
	if !p.ReadOnly || e.h.called(OpSetReadOnly) != 3 {
		t.Fatalf("после истечения: %+v %d", p, e.h.called(OpSetReadOnly))
	}
	e.h.setStatus(degraded(true))

	// Восстановление запаса → запись возвращается.
	ok.ReadOnly = true
	e.h.setStatus(ok)
	if err := e.s.EnforceAll(ctx); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(e.h.params[OpSetReadOnly], &p)
	if p.ReadOnly || e.h.called(OpSetReadOnly) != 4 {
		t.Fatalf("после восстановления: %+v", p)
	}

	log, err := e.s.ProtectionLog(ctx, "p1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var acts []string
	for i := len(log) - 1; i >= 0; i-- {
		acts = append(acts, log[i].Action+"/"+log[i].Actor)
	}
	want := []string{"auto_readonly/system", "override/admin", "auto_readwrite/system", "override_expired/system", "auto_readonly/system", "auto_readwrite/system"}
	if len(acts) != len(want) {
		t.Fatalf("журнал %v", acts)
	}
	for i := range want {
		if acts[i] != want[i] {
			t.Fatalf("журнал %v", acts)
		}
	}
	if log[4].Action != ActOverride || log[4].ExpiresAt == nil {
		t.Fatal("у разрешения нет срока в журнале")
	}
}

func TestProtectionDisabled(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	if _, err := e.s.SetProtectionEnabled(ctx, "p1", false, "admin", "тест"); err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(degraded(false))
	if err := e.s.EnforceAll(ctx); err != nil || e.h.called(OpSetReadOnly) != 0 {
		t.Fatalf("защита отключена, но применена: %v", err)
	}
	v, _ := e.s.Get(ctx, "p1")
	if v.Protection.Enabled || v.Protection.RedundancyExhausted == nil || !*v.Protection.RedundancyExhausted {
		t.Fatalf("%+v", v.Protection)
	}
}

func TestDeletePool(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	if _, err := e.s.Delete(ctx, "p1", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("без подтверждения: %v", err)
	}
	if _, err := e.s.Delete(ctx, "p1", "p2"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("неверное имя: %v", err)
	}
	if _, err := e.s.Delete(ctx, "nope", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет пула: %v", err)
	}
	if e.h.called(OpDestroy) != 0 {
		t.Fatal("helper вызван до подтверждения")
	}
	// Сбой helper: запись остаётся.
	e.h.fail = map[string]error{OpDestroy: errors.New("refused")}
	tk, err := e.s.Delete(ctx, "p1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if tk = e.wait(t, tk.ID); tk.State != tasks.Failed {
		t.Fatalf("%+v", tk)
	}
	if _, err := e.s.getRecord(ctx, "p1"); err != nil {
		t.Fatalf("запись удалена после сбоя: %v", err)
	}
	e.h.fail = nil
	tk, _ = e.s.Delete(ctx, "p1", "p1")
	if tk = e.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk)
	}
	if _, err := e.s.getRecord(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("запись не удалена: %v", err)
	}
}

// Сигнатура mdstat не меняется от прогресса, меняется от отказа.
func TestMdstatSignature(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "mdstat")
	w := func(s string) string { os.WriteFile(p, []byte(s), 0o644); return mdstatSignature(p) }
	base := "md127 : active raid1 sdc1[1] sdb1[0]\n      1045504 blocks super 1.2 [2/2] [UU]\n"
	a := w(base + "      [=>...]  resync = 10.0% (1/2) finish=1.0min speed=1K/sec\n      bitmap: 1/1 pages\n")
	b := w(base + "      [===>.]  resync = 60.0% (1/2) finish=0.3min speed=2K/sec\n      bitmap: 0/1 pages\n")
	if a != b {
		t.Fatalf("прогресс меняет сигнатуру:\n%s\n---\n%s", a, b)
	}
	c := w("md127 : active raid1 sdc1[1](F) sdb1[0]\n      1045504 blocks super 1.2 [2/1] [U_]\n")
	if c == b || w(base) == a {
		t.Fatal("отказ или конец синхронизации не меняют сигнатуру")
	}
}

// Изменение mdstat вызывает проход защиты без ожидания interval.
func TestRunProtectionReactsToMdstat(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "raid1", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	p := filepath.Join(t.TempDir(), "mdstat")
	os.WriteFile(p, []byte("a"), 0o644)
	e.s.MdstatPath, e.s.MdstatPoll = p, 10*time.Millisecond
	var changes atomic.Int32 // сигнал коллектору
	e.s.OnMdstatChange = func() { changes.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.s.RunProtection(ctx, time.Hour, nil)
	time.Sleep(50 * time.Millisecond)
	if changes.Load() != 0 {
		t.Fatal("сигнал без изменения mdstat")
	}
	n0 := e.h.called(OpStatus)
	// атомарно: WriteFile обрезает файл, опрос мог увидеть пустой — два изменения
	os.WriteFile(p+".new", []byte("b"), 0o644)
	os.Rename(p+".new", p)
	deadline := time.Now().Add(2 * time.Second)
	for e.h.called(OpStatus) == n0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.h.called(OpStatus) == n0 {
		t.Fatal("нет прохода после изменения mdstat")
	}
	if changes.Load() != 1 {
		t.Fatalf("сигналов коллектору %d, ждали 1", changes.Load())
	}
}

// После перезагрузки ФС уже rw, запас восстановлен, в базе ещё
// enforced — read-only снимается и с общих папок.
func TestProtectionClearsSharesWhenFSAlreadyRW(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	sh := &fakeShares{}
	e.s.Shares = sh
	e.h.setStatus(degraded(false))
	if err := e.s.EnforceAll(ctx); err != nil || len(sh.calls) != 1 || !sh.calls[0] {
		t.Fatalf("%v %v", err, sh.calls)
	}
	ok := okStatus(LevelRAID1, 2, gib)
	ok.Mounted = true // смонтирована заново rw
	e.h.setStatus(ok)
	n := e.h.called(OpSetReadOnly)
	if err := e.s.EnforceAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sh.calls) != 2 || sh.calls[1] || e.h.called(OpSetReadOnly) != n {
		t.Fatalf("папки: %v, set_read_only: %d→%d", sh.calls, n, e.h.called(OpSetReadOnly))
	}
	if v, _ := e.s.Get(ctx, "p1"); v.Protection.Enforced {
		t.Fatal("enforced не снят")
	}
}

// Перед read-only защиты — хук (остановка Docker); сбой хука —
// RO в этом проходе не применяется. Пул, занятый приложениями, не удаляется.
func TestProtectionBeforeReadOnlyAndInUse(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	var hooks []string
	fail := errors.New("docker не остановлен")
	e.s.BeforeReadOnly = func(_ context.Context, pool string) error {
		hooks = append(hooks, pool)
		if len(hooks) == 1 {
			return fail
		}
		return nil
	}
	e.h.setStatus(degraded(false))
	if err := e.s.EnforceAll(ctx); err == nil || e.h.called(OpSetReadOnly) != 0 {
		t.Fatalf("сбой хука: %v, remount %d", err, e.h.called(OpSetReadOnly))
	}
	if err := e.s.EnforceAll(ctx); err != nil || e.h.called(OpSetReadOnly) != 1 || len(hooks) != 2 || hooks[1] != "p1" {
		t.Fatalf("%v %d %v", err, e.h.called(OpSetReadOnly), hooks)
	}
	e.s.InUse = func(_ context.Context, pool string) string {
		if pool == "p1" {
			return "приложения"
		}
		return ""
	}
	if _, err := e.s.Delete(ctx, "p1", "p1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("удаление пула приложений: %v", err)
	}
}

// Выключение защиты у пула, уже переведённого в ro защитой, сразу
// возвращает запись; включение при исчерпанном запасе сразу ставит ro.
// Причина обязательна, оба действия — в журнале с автором.
func TestProtectionToggleAppliesImmediately(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.setStatus(degraded(false))
	if err := e.s.EnforceAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(degraded(true))
	if _, err := e.s.SetProtectionEnabled(ctx, "p1", false, "admin", " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("без причины: %v", err)
	}
	p, err := e.s.SetProtectionEnabled(ctx, "p1", false, "admin", "срочно восстановить базу")
	var rp ReadOnlyParams
	json.Unmarshal(e.h.params[OpSetReadOnly], &rp)
	if err != nil || p.Enabled || p.WriteBlocked || p.Enforced || p.ReadOnly || rp.ReadOnly || e.h.called(OpSetReadOnly) != 2 {
		t.Fatalf("выключение: %v %+v %+v", err, p, rp)
	}
	e.h.setStatus(degraded(false))
	if err := e.s.EnforceAll(ctx); err != nil || e.h.called(OpSetReadOnly) != 2 {
		t.Fatalf("при выключенной защите ro применён: %v", err)
	}
	p, err = e.s.SetProtectionEnabled(ctx, "p1", true, "admin", "закончил")
	json.Unmarshal(e.h.params[OpSetReadOnly], &rp)
	if err != nil || !p.Enabled || !p.WriteBlocked || !p.Enforced || !rp.ReadOnly || e.h.called(OpSetReadOnly) != 3 {
		t.Fatalf("включение: %v %+v %+v", err, p, rp)
	}
	log, _ := e.s.ProtectionLog(ctx, "p1", 0)
	var acts []string
	for i := len(log) - 1; i >= 0; i-- {
		acts = append(acts, log[i].Action+"/"+log[i].Actor)
	}
	want := "[auto_readonly/system disable/admin auto_readwrite/system enable/admin auto_readonly/system]"
	if got := fmt.Sprint(acts); got != want {
		t.Fatalf("журнал %s", got)
	}
}
