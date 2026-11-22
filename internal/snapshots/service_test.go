package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
)

// fakeHelper — снапшоты в памяти: папка → снапшоты.
type fakeHelper struct {
	mu       sync.Mutex
	now      func() time.Time
	snaps    map[string][]Snapshot
	noSubvol map[string]bool
	free     float64 // % свободного места
	ro       bool
	ops      []string
	restore  chan struct{} // не nil — restore ждёт закрытия
}

func (f *fakeHelper) Call(_ context.Context, op string, params, result any) error {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	var v any
	switch op {
	case OpList:
		p := params.(ShareParams)
		l := ListResult{Subvolume: !f.noSubvol[p.Share], ReadOnly: f.ro, Total: 100, Avail: uint64(f.free), Snapshots: []Snapshot{}}
		l.Snapshots = append(l.Snapshots, f.snaps[p.Share]...)
		sort.Slice(l.Snapshots, func(i, j int) bool { return l.Snapshots[i].Name > l.Snapshots[j].Name })
		v = l
	case OpCreate:
		p := params.(CreateParams)
		if f.noSubvol[p.Share] {
			f.mu.Unlock()
			return errors.New("не подтом")
		}
		r := CreateResult{FreePct: f.free}
		switch {
		case p.Auto && f.ro:
			r.Skipped = SkipReadOnly
		case p.Auto && f.free < float64(p.MinFreePct):
			r.Skipped = SkipLowSpace
		default:
			tm := f.now().UTC().Truncate(time.Second)
			r.Name = tm.Format(nas.SnapshotGoLayout)
			f.snaps[p.Share] = append(f.snaps[p.Share], Snapshot{Name: r.Name, Time: tm, Auto: p.Auto})
		}
		v = r
	case OpDelete:
		p := params.(SnapParams)
		l := f.snaps[p.Share]
		for i, s := range l {
			if s.Name == p.Name {
				f.snaps[p.Share] = append(l[:i:i], l[i+1:]...)
			}
		}
		v = struct{}{}
	case OpRestore:
		p := params.(PathParams)
		ch := f.restore
		f.mu.Unlock()
		if ch != nil {
			<-ch
		}
		v = RestoreResult{Path: p.Path + " (копия)", Files: 1}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, result)
	default:
		f.mu.Unlock()
		return errors.New("неизвестная операция " + op)
	}
	f.mu.Unlock()
	if result == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

func (f *fakeHelper) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.ops {
		if o == op {
			n++
		}
	}
	return n
}

type senv struct {
	s   *Service
	h   *fakeHelper
	log *events.Log
	now time.Time
	mgr *tasks.Manager
}

func newSvc(t *testing.T) *senv {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &senv{now: time.Date(2026, 11, 18, 10, 0, 30, 0, time.UTC)}
	e.h = &fakeHelper{now: func() time.Time { return e.now }, snaps: map[string][]Snapshot{}, noSubvol: map[string]bool{}, free: 50}
	e.log = events.NewLog(db)
	e.mgr = tasks.New(db, e.log, tasks.Options{})
	t.Cleanup(func() { e.mgr.Shutdown(context.Background()) })
	e.s = &Service{DB: db, Helper: e.h, Events: e.log, Tasks: e.mgr, Location: time.UTC,
		Now: func() time.Time { return e.now },
		Targets: func(context.Context) ([]Target, error) {
			return []Target{{Share: "docs", Pool: "p1"}, {Share: "old", Pool: "p1"}}, nil
		}}
	e.h.noSubvol["old"] = true
	if err := e.s.Init(); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *senv) events(t *testing.T, typ string) []events.Event {
	t.Helper()
	l, err := e.log.Query(context.Background(), events.Filter{Type: typ, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestScheduleCreatesHourlyAndRotates(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	if _, err := e.s.SetPolicy(ctx, "docs", Policy{Enabled: true, Hourly: 3}); err != nil {
		t.Fatal(err)
	}
	// Ручной снапшот не ротируется.
	if _, err := e.s.Create(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Second)
	for i := 0; i < 6; i++ {
		if err := e.s.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		// Повтор в том же часу — ничего.
		e.now = e.now.Add(20 * time.Minute)
		if err := e.s.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		e.now = e.now.Add(40 * time.Minute)
	}
	if n := e.h.count(OpCreate); n != 7 {
		t.Fatalf("создано %d (ожидалось 1 ручной + 6 по расписанию)", n)
	}
	info, err := e.s.Info(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	auto, manual := 0, 0
	for _, s := range info.Snapshots {
		if s.Auto {
			auto++
		} else {
			manual++
		}
	}
	if auto != 3 || manual != 1 {
		t.Fatalf("после ротации: авто %d, ручных %d: %+v", auto, manual, info.Snapshots)
	}
	if info.Policy.Hourly != 3 || info.FreePct != 50 || !info.Subvolume {
		t.Fatalf("сведения: %+v", info)
	}
	// Старая папка (не подтом): попытки создания нет, событий сбоя нет.
	if len(e.h.snaps["old"]) != 0 || len(e.events(t, EvFailed)) != 0 {
		t.Fatalf("old: %v, события: %v", e.h.snaps["old"], e.events(t, EvFailed))
	}
	if len(e.events(t, EvCreated)) != 1 {
		t.Fatal("событие — только о ручном снапшоте")
	}
}

func TestScheduleSkipEventOncePerReason(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	e.h.free = 5
	for i := 0; i < 3; i++ {
		if err := e.s.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		e.now = e.now.Add(time.Hour)
	}
	ev := e.events(t, EvSkipped)
	if len(ev) != 1 || ev[0].Severity != events.SevWarning || ev[0].Subject != "share:docs" {
		t.Fatalf("события пропуска: %+v", ev)
	}
	if len(e.h.snaps["docs"]) != 0 {
		t.Fatal("при нехватке места снапшот по расписанию не создаётся")
	}
	// Место освободилось — снапшот; новая нехватка — снова одно событие.
	e.h.free = 50
	e.s.RunOnce(ctx)
	e.now = e.now.Add(time.Hour)
	e.h.free = 5
	e.s.RunOnce(ctx)
	if len(e.events(t, EvSkipped)) != 2 || len(e.h.snaps["docs"]) != 1 {
		t.Fatalf("после восстановления места: %d событий, %d снапшотов", len(e.events(t, EvSkipped)), len(e.h.snaps["docs"]))
	}
}

func TestPolicyDisabledNoSnapshots(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	if _, err := e.s.SetPolicy(ctx, "docs", Policy{Enabled: false, Hourly: 24}); err != nil {
		t.Fatal(err)
	}
	e.s.RunOnce(ctx)
	if len(e.h.snaps["docs"]) != 0 {
		t.Fatal("выключено — снапшотов нет")
	}
	if _, err := e.s.SetPolicy(ctx, "docs", Policy{Enabled: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("включено без уровней: %v", err)
	}
	if _, err := e.s.SetPolicy(ctx, "nope", DefaultPolicy()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет папки: %v", err)
	}
	if _, err := e.s.Info(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Info нет папки: %v", err)
	}
}

func TestRestoreTaskBlocksDelete(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	sn, err := e.s.Create(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Restore(ctx, "docs", sn.Name, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("пустой путь: %v", err)
	}
	if _, err := e.s.Restore(ctx, "docs", sn.Name, "../etc"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("..: %v", err)
	}
	e.h.restore = make(chan struct{})
	task, err := e.s.Restore(ctx, "docs", sn.Name, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Пока идёт восстановление, снапшот не удаляется.
	deadline := time.Now().Add(5 * time.Second)
	for !e.s.isBusy("docs", sn.Name) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.s.Delete(ctx, "docs", sn.Name); !errors.Is(err, ErrConflict) {
		t.Fatalf("удаление во время восстановления: %v", err)
	}
	close(e.h.restore)
	for time.Now().Before(deadline) {
		x, _ := e.mgr.Get(ctx, task.ID)
		if x.State == tasks.Succeeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	x, _ := e.mgr.Get(ctx, task.ID)
	if x.State != tasks.Succeeded {
		t.Fatalf("задача: %+v", x)
	}
	ev := e.events(t, EvRestored)
	if len(ev) != 1 || !json.Valid(ev[0].Data) {
		t.Fatalf("событие восстановления: %+v", ev)
	}
	var d map[string]any
	json.Unmarshal(ev[0].Data, &d)
	if d["restored_as"] != "a.txt (копия)" || d["snapshot"] != sn.Name {
		t.Fatalf("данные события: %v", d)
	}
	if err := e.s.Delete(ctx, "docs", sn.Name); err != nil {
		t.Fatalf("удаление после восстановления: %v", err)
	}
	if len(e.events(t, EvDeleted)) != 1 {
		t.Fatal("нет события удаления")
	}
}
