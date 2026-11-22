package collector

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/store"
)

func snap(ids ...string) *disks.Snapshot {
	s := &disks.Snapshot{Complete: true}
	for _, id := range ids {
		s.Disks = append(s.Disks, disks.Disk{ID: id, Name: "Диск " + id})
	}
	return s
}

func pool(name, health string, diskIDs []string, faulty ...string) home.PoolView {
	v := home.PoolView{}
	v.Name = name
	for _, id := range diskIDs {
		v.Disks = append(v.Disks, home.DiskRef{ID: id})
	}
	if health == "" {
		return v // Status nil — не прочитан
	}
	st := &home.PoolStatus{Pool: name, Health: health}
	a := home.ArrayStatus{}
	for _, id := range diskIDs {
		state := "active sync"
		for _, f := range faulty {
			if f == id {
				state = "faulty"
			}
		}
		a.Members = append(a.Members, home.MemberStatus{DiskID: id, State: state})
	}
	st.Arrays = []home.ArrayStatus{a}
	v.Status = st
	return v
}

func types(evs []events.Event) []string {
	var t []string
	for _, e := range evs {
		t = append(t, e.Type+"/"+e.Severity+"/"+e.Subject)
	}
	return t
}

func eq(t *testing.T, got []events.Event, want ...string) {
	t.Helper()
	g := types(got)
	if len(g) != len(want) {
		t.Fatalf("события %v, ждали %v", g, want)
	}
	for i := range g {
		if g[i] != want[i] {
			t.Fatalf("события %v, ждали %v", g, want)
		}
	}
}

func TestFirstRunSilent(t *testing.T) {
	cur := Build(State{}, Observation{Disks: snap("A", "B"), Pools: []home.PoolView{pool("p", "degraded", []string{"A", "B"})}})
	eq(t, Diff(State{}, cur))
}

func TestDiskRemoveReturnAdd(t *testing.T) {
	ps := []home.PoolView{pool("p", "ok", []string{"A", "B"})}
	s0 := Build(State{}, Observation{Disks: snap("A", "B", "C"), Pools: ps})
	s1 := Build(s0, Observation{Disks: snap("A", "C"), Pools: ps})
	eq(t, Diff(s0, s1), "disk.removed/critical/disk:B")
	s2 := Build(s1, Observation{Disks: snap("A"), Pools: ps})
	eq(t, Diff(s1, s2), "disk.removed/warning/disk:C") // не член пула
	s3 := Build(s2, Observation{Disks: snap("A", "B", "D"), Pools: ps})
	eq(t, Diff(s2, s3), "disk.returned/info/disk:B", "disk.added/info/disk:D")
	eq(t, Diff(s3, Build(s3, Observation{Disks: snap("A", "B", "D"), Pools: ps}))) // без изменений — тишина
}

func TestPoolTransitions(t *testing.T) {
	d := snap("A", "B")
	ids := []string{"A", "B"}
	s0 := Build(State{}, Observation{Disks: d, Pools: []home.PoolView{pool("p", "ok", ids)}})
	s1 := Build(s0, Observation{Disks: d, Pools: []home.PoolView{pool("p", "degraded", ids, "B")}})
	eq(t, Diff(s0, s1), "disk.failed/critical/disk:B", "array.degraded/critical/pool:p")
	s2 := Build(s1, Observation{Disks: d, Pools: []home.PoolView{pool("p", "rebuilding", ids)}})
	eq(t, Diff(s1, s2), "array.recovering/warning/pool:p")
	s3 := Build(s2, Observation{Disks: d, Pools: []home.PoolView{pool("p", "ok", ids)}})
	eq(t, Diff(s2, s3), "array.recovered/info/pool:p")
	// resyncing (начальная синхронизация) — не восстановление после отказа
	s4 := Build(s3, Observation{Disks: d, Pools: []home.PoolView{pool("p", "resyncing", ids)}})
	eq(t, Diff(s3, s4))
}

func TestUnknownKeepsPrevious(t *testing.T) {
	ids := []string{"A", "B"}
	s0 := Build(State{}, Observation{Disks: snap("A", "B"), Pools: []home.PoolView{pool("p", "ok", ids)}})
	// диски не прочитаны, пул unknown → ни «диск пропал», ни «пул деградировал»
	s1 := Build(s0, Observation{Pools: []home.PoolView{pool("p", home.HealthUnknown, ids)}})
	eq(t, Diff(s0, s1))
	s2 := Build(s1, Observation{Disks: snap("A", "B"), Pools: []home.PoolView{pool("p", "", ids)}}) // Status nil
	eq(t, Diff(s1, s2))
	s3 := Build(s2, Observation{Disks: snap("A", "B"), PoolsErr: context.DeadlineExceeded})
	eq(t, Diff(s2, s3))
	if s3.Pools["p"].Health != "ok" || s3.Members["B"] != "p" {
		t.Errorf("прежнее состояние потеряно: %+v", s3)
	}
	// после неизвестности реальный переход виден относительно последнего известного
	s4 := Build(s3, Observation{Disks: snap("A", "B"), Pools: []home.PoolView{pool("p", "degraded", ids, "A")}})
	eq(t, Diff(s3, s4), "disk.failed/critical/disk:A", "array.degraded/critical/pool:p")
}

func TestIntegrityErrors(t *testing.T) {
	mk := func(btrfs, integ int64) []home.PoolView {
		v := pool("p", "ok", []string{"A"})
		v.Status.BtrfsErrors = map[string]int64{"corruption_errs": btrfs}
		v.Status.Integrity = []home.IntegrityStatus{{Name: "i0", Mismatches: integ}}
		return []home.PoolView{v}
	}
	s0 := Build(State{}, Observation{Disks: snap("A"), Pools: mk(0, 0)})
	s1 := Build(s0, Observation{Disks: snap("A"), Pools: mk(0, 3)})
	eq(t, Diff(s0, s1), "integrity.errors/warning/pool:p")
	s2 := Build(s1, Observation{Disks: snap("A"), Pools: mk(1, 3)})
	eq(t, Diff(s1, s2), "integrity.errors/warning/pool:p")
	s3 := Build(s2, Observation{Disks: snap("A"), Pools: mk(0, 0)}) // сброс счётчиков (перезагрузка) — не событие
	eq(t, Diff(s2, s3))
}

// Отказ во время простоя gamayund виден после «перезапуска» (новый коллектор, та же база).
func TestSyncAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "c.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	obs := Observation{Disks: snap("A", "B"), Pools: []home.PoolView{pool("p", "ok", []string{"A", "B"})}}
	c := New(db, events.NewLog(db), func(context.Context) Observation { return obs })
	if evs, err := c.Sync(ctx); err != nil || len(evs) != 0 {
		t.Fatalf("первая сверка: %v %v", types(evs), err)
	}
	db.Close()

	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := events.NewLog(db)
	obs = Observation{Disks: snap("A"), Pools: []home.PoolView{pool("p", "degraded", []string{"A", "B"})}}
	c = New(db, log, func(context.Context) Observation { return obs })
	evs, err := c.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, evs, "disk.removed/critical/disk:B", "array.degraded/critical/pool:p")
	if evs, _ := c.Sync(ctx); len(evs) != 0 {
		t.Fatalf("повтор: %v", types(evs))
	}
	if e, ok, _ := log.LastBySubject(ctx, "disk:B"); !ok || e.Type != events.DiskRemoved || e.ID == 0 {
		t.Fatalf("в журнале: %+v %v", e, ok)
	}
}

func TestRelevantUevent(t *testing.T) {
	ok := []string{
		"remove@/devices/x/block/sdb\x00ACTION=remove\x00DEVPATH=/devices/x/block/sdb\x00SUBSYSTEM=block\x00DEVNAME=sdb",
		"change@/devices/virtual/block/md0\x00ACTION=change\x00SUBSYSTEM=block",
		"add@/x\x00SUBSYSTEM=block\x00ACTION=add",
	}
	no := []string{
		"bind@/x\x00ACTION=bind\x00SUBSYSTEM=block",
		"add@/x\x00ACTION=add\x00SUBSYSTEM=usb",
		"libudev\x00garbage",
		"",
	}
	for _, m := range ok {
		if !RelevantUevent([]byte(m)) {
			t.Errorf("не принято: %q", m)
		}
	}
	for _, m := range no {
		if RelevantUevent([]byte(m)) {
			t.Errorf("принято лишнее: %q", m)
		}
	}
}

// Пачка Trigger во время паузы — одна внеочередная сверка.
func TestTriggerCoalesced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n atomic.Int32
	c := New(db, events.NewLog(db), func(context.Context) Observation { n.Add(1); return Observation{Disks: snap("A")} })
	c.Settle = 50 * time.Millisecond
	go c.Run(ctx, time.Hour, nil)
	waitFor(t, func() bool { return n.Load() == 1 }) // сверка при старте
	for i := 0; i < 5; i++ {
		c.Trigger()
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, func() bool { return n.Load() == 2 })
	time.Sleep(150 * time.Millisecond)
	if got := n.Load(); got != 2 {
		t.Fatalf("сверок %d, ждали 2", got)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("не дождались")
}

// Пул «создаётся» (или создание прервано) в состоянии failed —
// не отказ массива: событий нет ни при появлении, ни повторно.
func TestCreatingPoolSilent(t *testing.T) {
	d := snap("A", "B")
	ids := []string{"A", "B"}
	cr := pool("p", "failed", ids)
	cr.Creating = true
	s0 := Build(State{}, Observation{Disks: d})
	s1 := Build(s0, Observation{Disks: d, Pools: []home.PoolView{cr}})
	eq(t, Diff(s0, s1))
	s2 := Build(s1, Observation{Disks: d, Pools: []home.PoolView{cr}})
	eq(t, Diff(s1, s2))
	if s2.Members["A"] != "p" {
		t.Fatal("диски создаваемого пула должны оставаться участниками")
	}
}

// Удаление пула через API — разборка массива (missing) не даёт
// array.degraded; после удаления записи пула событий тоже нет.
func TestDestroyingPoolSilent(t *testing.T) {
	d := snap("A", "B")
	ids := []string{"A", "B"}
	s0 := Build(State{}, Observation{Disks: d, Pools: []home.PoolView{pool("p", "ok", ids)}})
	del := pool("p", "missing", ids)
	del.Busy = home.BusyDestroying
	s1 := Build(s0, Observation{Disks: d, Pools: []home.PoolView{del}})
	eq(t, Diff(s0, s1))
	s2 := Build(s1, Observation{Disks: d})
	eq(t, Diff(s1, s2))
	// без задачи удаления та же картина — отказ (контроль)
	s3 := Build(s0, Observation{Disks: d, Pools: []home.PoolView{pool("p", "missing", ids)}})
	eq(t, Diff(s0, s3), "array.degraded/critical/pool:p")
}

// Аварийный read-only Btrfs — событие при входе и при выходе.
func TestEmergencyReadOnly(t *testing.T) {
	mk := func(ro bool) []home.PoolView {
		h := "ok"
		if ro { // health при аварийном RO — warning; лишних событий о массиве нет
			h = "warning"
		}
		v := pool("p", h, []string{"A"})
		v.Status.EmergencyReadOnly = ro
		return []home.PoolView{v}
	}
	s0 := Build(State{}, Observation{Disks: snap("A"), Pools: mk(false)})
	s1 := Build(s0, Observation{Disks: snap("A"), Pools: mk(true)})
	eq(t, Diff(s0, s1), "pool.emergency_ro/critical/pool:p")
	s2 := Build(s1, Observation{Disks: snap("A"), Pools: mk(true)})
	eq(t, Diff(s1, s2))
	s3 := Build(s2, Observation{Disks: snap("A"), Pools: mk(false)})
	eq(t, Diff(s2, s3), "pool.emergency_ro_cleared/info/pool:p")
}

// Запас исчерпан при выключенной защите — отдельное critical
// (однократно, на переходе), снятие — info; при включённой защите — тишина.
func TestProtectionBypassed(t *testing.T) {
	mk := func(enabled, exhausted bool) []home.PoolView {
		h := "ok"
		if exhausted {
			h = "degraded"
		}
		v := pool("p", h, []string{"A"})
		v.Protection.Enabled = enabled
		v.Protection.RedundancyExhausted = &exhausted
		return []home.PoolView{v}
	}
	obs := func(en, ex bool) Observation { return Observation{Disks: snap("A"), Pools: mk(en, ex)} }
	s0 := Build(State{}, obs(false, false))
	s1 := Build(s0, obs(false, true))
	eq(t, Diff(s0, s1), "array.degraded/critical/pool:p", "pool.protection_bypassed/critical/pool:p")
	s2 := Build(s1, obs(false, true))
	eq(t, Diff(s1, s2))
	s3 := Build(s2, obs(true, true)) // защиту включили обратно
	eq(t, Diff(s2, s3), "pool.protection_bypassed_cleared/info/pool:p")
	s4 := Build(s3, obs(true, true))
	eq(t, Diff(s3, s4))
	// Включённая защита при исчерпании — без нового события (только array.*).
	e0 := Build(State{}, obs(true, false))
	e1 := Build(e0, obs(true, true))
	eq(t, Diff(e0, e1), "array.degraded/critical/pool:p")
	// Выключили защиту при уже исчерпанном запасе — critical сразу.
	e2 := Build(e1, obs(false, true))
	eq(t, Diff(e1, e2), "pool.protection_bypassed/critical/pool:p")
}
