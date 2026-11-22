package home

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"gamayun-nas/internal/tasks"
)

// Вывод btrfs-progs 6.14 (2026-11-14).
const btrfsNoStats = `UUID:             53125a8a-0dd1-44d9-b01f-fb223095be3c
	no stats available
	data_extents_scrubbed: 0
	tree_extents_scrubbed: 0
	data_bytes_scrubbed: 0
	tree_bytes_scrubbed: 0
	read_errors: 0
	csum_errors: 0
	verify_errors: 0
	no_csum: 0
	csum_discards: 0
	super_errors: 0
	malloc_errors: 0
	uncorrectable_errors: 0
	unverified_errors: 0
	corrected_errors: 0
	last_physical: 0
`

const btrfsFinished = `UUID:             53125a8a-0dd1-44d9-b01f-fb223095be3c
Scrub started:    Sat Nov 14 09:58:05 2026
Status:           finished
Duration:         0:00:00
	data_extents_scrubbed: 0
	tree_extents_scrubbed: 9
	data_bytes_scrubbed: 4096
	tree_bytes_scrubbed: 147456
	read_errors: 0
	csum_errors: 3
	verify_errors: 0
	no_csum: 0
	csum_discards: 0
	super_errors: 0
	malloc_errors: 0
	uncorrectable_errors: 3
	unverified_errors: 0
	corrected_errors: 0
	last_physical: 22020096
`

func TestParseBtrfsScrubStatus(t *testing.T) {
	b, err := ParseBtrfsScrubStatus(btrfsNoStats)
	if err != nil || b.Status != "none" || b.Errors() != 0 {
		t.Fatalf("нет статистики: %+v %v", b, err)
	}
	b, err = ParseBtrfsScrubStatus(btrfsFinished)
	if err != nil || b.Status != "finished" || b.CsumErrors != 3 || b.UncorrectableErrors != 3 ||
		b.DataBytesScrubbed != 4096 || b.TreeBytesScrubbed != 147456 || b.Errors() != 3 {
		t.Fatalf("finished: %+v %v", b, err)
	}
	if _, err := ParseBtrfsScrubStatus("ERROR: not a btrfs filesystem"); err == nil {
		t.Fatal("вывод без счётчиков — ошибка")
	}
}

func TestScrubSeverity(t *testing.T) {
	cases := []struct {
		r    ScrubResult
		want string
	}{
		{ScrubResult{Btrfs: &BtrfsScrub{}}, "info"},
		{ScrubResult{IntegrityErrorsDelta: 2, Btrfs: &BtrfsScrub{}}, "warning"},
		{ScrubResult{MismatchCnt: 128}, "warning"},
		{ScrubResult{Btrfs: &BtrfsScrub{CsumErrors: 1, CorrectedErrors: 1}}, "warning"},
		{ScrubResult{Btrfs: &BtrfsScrub{CsumErrors: 1, UncorrectableErrors: 1}}, "critical"},
	}
	for i, c := range cases {
		if got := c.r.Severity(); got != c.want {
			t.Errorf("%d: %s, ожидалось %s", i, got, c.want)
		}
	}
}

func pf(v float64) *float64 { return &v }
func pi(v int64) *int64     { return &v }

type scrubSink struct {
	mu  sync.Mutex
	res []ScrubResult
	err []error
}

func (k *scrubSink) on(r ScrubResult, err error) {
	k.mu.Lock()
	k.res, k.err = append(k.res, r), append(k.err, err)
	k.mu.Unlock()
}

func TestScrubTaskFull(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", true)
	st := okStatus(LevelRAID1, 2, gib)
	st.Mounted = true
	after := st
	after.IntegrityErrors = 2 // dm-integrity нашла и md перезаписал
	e.h.setStatus(st)
	starts := 0
	e.h.hook = func(op string) { // под f.mu
		if op == OpScrubStart {
			if starts++; starts == 2 {
				e.h.statuses = []PoolStatus{after}
			}
		}
	}
	e.h.scrub = []ScrubStatus{
		{Pool: "p1", Arrays: []ScrubArray{{Name: "p1-0", Action: "check", Progress: pf(50)}}, Btrfs: &BtrfsScrub{Status: "none"}},
		{Pool: "p1", Arrays: []ScrubArray{{Name: "p1-0", Action: "idle", MismatchCnt: pi(0)}}, Btrfs: &BtrfsScrub{Status: "none"}},
		{Pool: "p1", Arrays: []ScrubArray{{Name: "p1-0", Action: "idle", MismatchCnt: pi(0)}}, Btrfs: &BtrfsScrub{Status: "running"}},
		{Pool: "p1", Arrays: []ScrubArray{{Name: "p1-0", Action: "idle", MismatchCnt: pi(0)}}, Btrfs: &BtrfsScrub{Status: "finished", DataBytesScrubbed: 10}},
	}
	var sink scrubSink
	e.s.OnScrub = sink.on
	tk, err := e.s.Scrub(ctx, "p1", ScrubManual)
	if err != nil {
		t.Fatal(err)
	}
	// Пока идёт проверка, пул занят: другие операции — 409.
	if _, err := e.s.Scrub(ctx, "p1", ScrubManual); !errors.Is(err, ErrConflict) {
		t.Errorf("повторная проверка: %v", err)
	}
	r := e.wait(t, tk.ID)
	if r.State != tasks.Succeeded {
		t.Fatalf("задача %s: %+v", r.State, r.Error)
	}
	var res ScrubResult
	json.Unmarshal(r.Result, &res)
	if res.Cancelled || res.MismatchCnt != 0 || res.Btrfs == nil || res.Btrfs.Status != "finished" || res.IntegrityErrorsDelta != 2 {
		t.Fatalf("итог %+v", res)
	}
	if res.Severity() != "warning" {
		t.Errorf("уровень %s", res.Severity())
	}
	if e.h.called(OpScrubStart) != 2 {
		t.Errorf("стадий запущено %d", e.h.called(OpScrubStart))
	}
	if len(sink.res) != 1 || sink.err[0] != nil {
		t.Errorf("OnScrub: %+v %v", sink.res, sink.err)
	}
	if e.s.Busy("p1") != "" {
		t.Error("пул не освобождён")
	}
}

func TestScrubRefusedUnhealthy(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "shr", true)
	st := okStatus(LevelRAID1, 2, gib)
	st.Mounted, st.Health = true, HealthDegraded
	e.h.setStatus(st)
	if _, err := e.s.Scrub(context.Background(), "p1", ScrubSchedule); !errors.Is(err, ErrConflict) {
		t.Fatalf("деградированный пул: %v", err)
	}
	if _, err := e.s.Scrub(context.Background(), "nope", ScrubManual); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет пула: %v", err)
	}
	if err := e.s.ScrubCancel(context.Background(), "p1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("отмена без проверки: %v", err)
	}
}

func TestScrubInterruptedByRecovery(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "shr", true)
	st := okStatus(LevelRAID1, 2, gib)
	st.Mounted = true
	e.h.setStatus(st)
	e.h.scrub = []ScrubStatus{{Pool: "p1", Arrays: []ScrubArray{{Name: "p1-0", Action: "recovery"}}}}
	var sink scrubSink
	e.s.OnScrub = sink.on
	tk, err := e.s.Scrub(context.Background(), "p1", ScrubManual)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.wait(t, tk.ID); r.State != tasks.Failed {
		t.Fatalf("ожидался failed: %s", r.State)
	}
	if len(sink.err) != 1 || sink.err[0] == nil {
		t.Fatalf("OnScrub с ошибкой: %v", sink.err)
	}
}

func TestScrubCancel(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "shr", true)
	st := okStatus(LevelRAID1, 2, gib)
	st.Mounted = true
	e.h.setStatus(st)
	e.h.scrub = []ScrubStatus{{Pool: "p1", Arrays: []ScrubArray{{Name: "p1-0", Action: "check", Progress: pf(10)}}}}
	started := make(chan struct{}, 1)
	e.h.hook = func(op string) {
		if op == OpScrubStatus {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	}
	tk, err := e.s.Scrub(context.Background(), "p1", ScrubManual)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := e.s.ScrubCancel(context.Background(), "p1"); err != nil {
		t.Fatal(err)
	}
	r := e.wait(t, tk.ID)
	var res ScrubResult
	json.Unmarshal(r.Result, &res)
	if r.State != tasks.Succeeded || !res.Cancelled {
		t.Fatalf("отмена: %s %+v", r.State, res)
	}
	if e.h.called(OpScrubCancel) < 1 {
		t.Error("helper не получил отмену")
	}
	if e.h.called(OpScrubStart) != 1 {
		t.Error("после отмены btrfs scrub не должен запускаться")
	}
}

// Остановка gamayund во время md check: задача отменяется, проверка в ядре
// останавливается (scrub_cancel проходит, хотя изменяющие вызовы при
// остановке запрещены).
func TestScrubStopOnShutdown(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "shr", true)
	st := okStatus(LevelRAID1, 2, gib)
	st.Mounted = true
	e.h.setStatus(st)
	e.h.scrub = []ScrubStatus{{Pool: "p1", Arrays: []ScrubArray{{Name: "p1-0", Action: "check", Progress: pf(10)}}}}
	started := make(chan struct{}, 1)
	e.h.hook = func(op string) {
		if op == OpScrubStatus {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	}
	var sink scrubSink
	e.s.OnScrub = sink.on
	tk, err := e.s.Scrub(context.Background(), "p1", ScrubManual)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	e.s.Stop()
	if err := e.m.Cancel(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	e.wait(t, tk.ID)
	if e.h.called(OpScrubCancel) != 1 {
		t.Fatalf("scrub_cancel при остановке: %d вызовов", e.h.called(OpScrubCancel))
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.err) != 1 || sink.err[0] == nil {
		t.Fatalf("OnScrub с ошибкой: %v", sink.err)
	}
}

func TestScrubAction(t *testing.T) {
	// Отложенный check соседнего массива: mdstat — «resync=DELAYED», sysfs — check.
	act, p := scrubAction("check", MdstatArray{Action: "resync", Delayed: true}, true)
	if act != "check" || p != nil {
		t.Errorf("отложенный check: %s %v", act, p)
	}
	act, p = scrubAction("check", MdstatArray{Action: "check", Progress: pf(40)}, true)
	if act != "check" || p == nil || *p != 40 {
		t.Errorf("идущий check: %s %v", act, p)
	}
	if act, _ := scrubAction("idle", MdstatArray{}, true); act != "idle" {
		t.Errorf("покой: %s", act)
	}
	if act, _ := scrubAction("", MdstatArray{Action: "recovery", Progress: pf(5)}, true); act != "recovery" {
		t.Errorf("без sysfs: %s", act)
	}
	if act, _ := scrubAction("", MdstatArray{}, false); act != "idle" {
		t.Errorf("нет данных: %s", act)
	}
}
