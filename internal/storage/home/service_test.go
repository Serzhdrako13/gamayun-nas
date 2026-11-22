package home

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
)

// fakeHelper — подмена gamayun-helper для Service.
type fakeHelper struct {
	mu       sync.Mutex
	snap     disks.Snapshot
	statuses []PoolStatus    // очередь ответов home.pool.status; последний повторяется
	assemble *AssembleResult // ответ home.pool.assemble; nil — Result
	calls    []string
	params   map[string]json.RawMessage
	fail     map[string]error
	hook     func(op string)   // вызывается до ответа
	results  map[string]Result // ответ op вместо Result по умолчанию
	scrub    []ScrubStatus     // очередь ответов home.pool.scrub_status; последний повторяется
}

func (f *fakeHelper) Call(_ context.Context, op string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
	if f.params == nil {
		f.params = map[string]json.RawMessage{}
	}
	raw, _ := json.Marshal(params)
	f.params[op] = raw
	if f.hook != nil {
		f.hook(op)
	}
	if err := f.fail[op]; err != nil {
		return err
	}
	var v any
	switch op {
	case "disks.observe":
		v = f.snap
	case OpStatus:
		if len(f.statuses) == 0 {
			v = PoolStatus{Health: HealthMissing}
		} else {
			v = f.statuses[0]
			if len(f.statuses) > 1 {
				f.statuses = f.statuses[1:]
			}
		}
	case OpScrubStatus:
		if len(f.scrub) == 0 {
			v = ScrubStatus{}
		} else {
			v = f.scrub[0]
			if len(f.scrub) > 1 {
				f.scrub = f.scrub[1:]
			}
		}
	case OpAssemble:
		if f.assemble != nil {
			v = *f.assemble
		} else {
			v = Result{Pool: "p1", Commands: []string{op}}
		}
	default:
		v = Result{Pool: "p1", Commands: []string{op}}
		if r, ok := f.results[op]; ok {
			v = r
		}
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

func (f *fakeHelper) setStatus(s ...PoolStatus) {
	f.mu.Lock()
	f.statuses = s
	f.mu.Unlock()
}

func (f *fakeHelper) called(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

func snapDisk(kname, id, role string) disks.Disk {
	size := gib
	roles := []string{}
	if role != disks.SummaryUnknown {
		roles = []string{"mounted"}
	}
	return disks.Disk{Name: "Диск " + kname, KernelName: kname, ID: id, SizeBytes: &size, SystemRole: role, SystemRoles: roles}
}

func freeSnap() disks.Snapshot {
	return disks.Snapshot{Complete: true, Issues: []disks.Issue{}, Disks: []disks.Disk{
		snapDisk("sda", "", disks.SummaryRoot),
		snapDisk("sdb", ref101.ID, disks.SummaryUnknown),
		snapDisk("sdc", ref102.ID, disks.SummaryUnknown),
		snapDisk("sdd", ref103.ID, disks.SummaryUnknown),
	}}
}

type svcEnv struct {
	s   *Service
	h   *fakeHelper
	m   *tasks.Manager
	now time.Time
}

func newSvc(t *testing.T) *svcEnv {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := tasks.New(db, events.NewLog(db), tasks.Options{Logger: slog.New(slog.DiscardHandler)})
	h := &fakeHelper{snap: freeSnap()}
	s, err := NewService(h, db, m)
	if err != nil {
		t.Fatal(err)
	}
	s.Poll = 5 * time.Millisecond
	e := &svcEnv{s: s, h: h, m: m, now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	s.SetClock(func() time.Time { return e.now })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx)
		db.Close()
	})
	return e
}

func (e *svcEnv) wait(t *testing.T, id string) tasks.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tk, err := e.m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.State != tasks.Queued && tk.State != tasks.Running {
			return tk
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("задача не завершилась")
	return tasks.Task{}
}

var createReq = PlanRequest{Op: PlanCreate, Pool: "p1", Type: "shr", DiskIDs: []string{ref101.ID, ref102.ID}, Integrity: true}

func TestPlanCreateAndConfirm(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	p, err := e.s.Plan(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	if p.PlanID == "" || len(p.ConfirmToken) != 64 || !p.ExpiresAt.Equal(e.now.Add(10*time.Minute)) {
		t.Fatalf("план %+v", p)
	}
	if len(p.Arrays) != 1 || p.Arrays[0].Level != LevelRAID1 || p.Arrays[0].SliceBytes != gib-PartitionReserve ||
		p.UsableBytes != gib-PartitionReserve || p.Tolerance != 1 || !p.Disks[0].Erase {
		t.Fatalf("раскладка %+v", p)
	}
	if e.h.called(OpCreate) != 0 {
		t.Fatal("расчёт плана не должен ничего создавать")
	}
	if p.IntegrityMode != DefaultIntegrityMode {
		t.Fatalf("режим в плане %q", p.IntegrityMode)
	}
	// Создание завершается, как только пул смонтирован; начальная
	// синхронизация (пересчёт тегов, resync md) — состояние пула, а не задача.
	half, rb := 50.0, int64(1<<29)
	arr := ArrayStatus{Index: 0, Level: "raid1", RaidDevices: 2, State: []string{"clean"}, DevSizeBytes: 1 << 30}
	resync := arr
	resync.Action, resync.Progress = "resync", &half
	recalc := []IntegrityStatus{{Name: "a", ProvidedBytes: 1 << 30, RecalcBytes: &rb, Recalculating: true}}
	e.h.setStatus(PoolStatus{Pool: "p1", Health: HealthResyncing, Mounted: true, Arrays: []ArrayStatus{resync}, Integrity: recalc})
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Kind != TaskCreate {
		t.Fatalf("вид задачи %s", tk.Kind)
	}
	if done := e.wait(t, tk.ID); done.State != tasks.Succeeded || done.Progress != 100 {
		t.Fatalf("задача %+v", done)
	}
	if rec, _ := e.s.getRecord(ctx, "p1"); !rec.InitialSync {
		t.Fatal("признак начальной синхронизации снят до её окончания")
	}
	st, err := e.s.status(ctx, "p1")
	if err != nil || st.Sync == nil || st.Sync.Stage != SyncStageIntegrity || st.Sync.Percent != 50 || st.Sync.EtaMin != nil {
		t.Fatalf("ход синхронизации %+v %v", st.Sync, err)
	}
	if _, err := e.s.Scrub(ctx, "p1", "manual"); err == nil || !strings.Contains(err.Error(), "идёт начальная синхронизация") {
		t.Fatalf("проверка данных во время синхронизации: %v", err)
	}
	var cp CreateParams
	json.Unmarshal(e.h.params[OpCreate], &cp)
	if cp.Pool != "p1" || !cp.Integrity || cp.IntegrityMode != DefaultIntegrityMode || len(cp.Disks) != 2 || cp.Disks[0].SizeBytes != gib {
		t.Fatalf("параметры helper %+v", cp)
	}
	rec, err := e.s.getRecord(ctx, "p1")
	if err != nil || rec.Type != "shr" || !rec.Integrity || rec.IntegrityMode != DefaultIntegrityMode || len(rec.Disks) != 2 {
		t.Fatalf("запись %+v %v", rec, err)
	}
	// Токен одноразовый.
	if _, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("повтор: %v", err)
	}
	// Пул уже существует.
	if _, err := e.s.Plan(ctx, createReq); !errors.Is(err, ErrConflict) {
		t.Fatalf("повторное создание: %v", err)
	}
}

func TestConfirmRejections(t *testing.T) {
	ctx := context.Background()
	t.Run("неверный токен аннулирует план", func(t *testing.T) {
		e := newSvc(t)
		p, _ := e.s.Plan(ctx, createReq)
		if _, err := e.s.Confirm(ctx, p.PlanID, "00"+p.ConfirmToken[2:]); !errors.Is(err, ErrBadToken) {
			t.Fatalf("%v", err)
		}
		if _, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken); !errors.Is(err, ErrPlanNotFound) {
			t.Fatalf("после неверного токена: %v", err)
		}
		if e.h.called(OpCreate) != 0 {
			t.Fatal("создание вызвано")
		}
	})
	t.Run("истёк", func(t *testing.T) {
		e := newSvc(t)
		p, _ := e.s.Plan(ctx, createReq)
		e.now = e.now.Add(10 * time.Minute)
		if _, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken); !errors.Is(err, ErrPlanExpired) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("неизвестный план", func(t *testing.T) {
		e := newSvc(t)
		if _, err := e.s.Confirm(ctx, "nope", "x"); !errors.Is(err, ErrPlanNotFound) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("состояние диска изменилось", func(t *testing.T) {
		e := newSvc(t)
		p, _ := e.s.Plan(ctx, createReq)
		e.h.snap.Disks[2] = snapDisk("sdc", ref102.ID, disks.SummaryInUse)
		if _, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken); !errors.Is(err, ErrPlanStale) {
			t.Fatalf("%v", err)
		}
		if e.h.called(OpCreate) != 0 {
			t.Fatal("создание вызвано")
		}
	})
	t.Run("размер диска изменился", func(t *testing.T) {
		e := newSvc(t)
		p, _ := e.s.Plan(ctx, createReq)
		big := 2 * gib
		e.h.snap.Disks[1].SizeBytes = &big
		if _, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken); !errors.Is(err, ErrPlanStale) {
			t.Fatalf("%v", err)
		}
	})
}

func TestPlanCreateRejections(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		mod  func(e *svcEnv, r *PlanRequest)
		want error
	}{
		"имя пула":     {func(_ *svcEnv, r *PlanRequest) { r.Pool = "../x" }, ErrInvalid},
		"операция":     {func(_ *svcEnv, r *PlanRequest) { r.Op = "destroy" }, ErrInvalid},
		"нет дисков":   {func(_ *svcEnv, r *PlanRequest) { r.DiskIDs = nil }, ErrInvalid},
		"повтор диска": {func(_ *svcEnv, r *PlanRequest) { r.DiskIDs = []string{ref101.ID, ref101.ID} }, ErrInvalid},
		"неизвестный":  {func(_ *svcEnv, r *PlanRequest) { r.DiskIDs = []string{ref101.ID, "wwn-0x1"} }, ErrInvalid},
		"системный диск": {func(e *svcEnv, r *PlanRequest) {
			e.h.snap.Disks[0].ID = "wwn-0x9"
			r.DiskIDs = []string{ref101.ID, "wwn-0x9"}
		}, ErrConflict},
		"занятый диск":    {func(e *svcEnv, _ *PlanRequest) { e.h.snap.Disks[1] = snapDisk("sdb", ref101.ID, disks.SummaryInUse) }, ErrConflict},
		"неполный снимок": {func(e *svcEnv, _ *PlanRequest) { e.h.snap.Complete = false }, ErrConflict},
		"мало дисков":     {func(_ *svcEnv, r *PlanRequest) { r.Type = "raid5" }, ErrInvalid},
		"тип":             {func(_ *svcEnv, r *PlanRequest) { r.Type = "raid7" }, ErrInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSvc(t)
			r := createReq
			r.DiskIDs = append([]string{}, createReq.DiskIDs...)
			c.mod(e, &r)
			if _, err := e.s.Plan(ctx, r); !errors.Is(err, c.want) {
				t.Fatalf("ожидалось %v, получено %v", c.want, err)
			}
		})
	}
}

func TestPlanJBODSlices(t *testing.T) {
	e := newSvc(t)
	r := createReq
	r.Type, r.Integrity = "jbod", false
	p, err := e.s.Plan(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Arrays) != 2 || p.Arrays[0].Level != LevelSingle || p.Arrays[0].SliceBytes != gib-PartitionReserve {
		t.Fatalf("%+v", p.Arrays)
	}
}

// seedPool — запись пула p1 (SHR из двух дисков) и его исправное состояние.
func seedPool(t *testing.T, e *svcEnv, typ string, integrity bool) {
	t.Helper()
	rec := PoolRecord{Name: "p1", Type: typ, Integrity: integrity, Disks: []DiskRef{ref101, ref102},
		Arrays: []ArraySpec{{Level: LevelRAID1, SliceBytes: gib - PartitionReserve, Members: []string{ref101.ID, ref102.ID}}}}
	if err := e.s.saveRecord(context.Background(), rec, true); err != nil {
		t.Fatal(err)
	}
	e.h.snap.Disks[1] = snapDisk("sdb", ref101.ID, disks.SummaryInUse)
	e.h.snap.Disks[2] = snapDisk("sdc", ref102.ID, disks.SummaryInUse)
}

func okStatus(level string, n int, size int64) PoolStatus {
	return PoolStatus{Pool: "p1", Health: HealthOK, Mounted: true, DiskIDs: []string{ref101.ID, ref102.ID},
		Arrays: []ArrayStatus{{Index: 0, Name: "p1-0", Level: level, RaidDevices: n, ActiveDevices: n,
			DevSizeBytes: gib - 3*Align, ArraySizeBytes: size}}}
}

func TestAddDiskPlanAndTask(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align))
	p, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID})
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != "shr" || p.Arrays[0].Level != LevelRAID5 || len(p.Arrays[0].Members) != 3 ||
		p.UsableBytes != 2*(gib-PartitionReserve) || p.PrevUsableBytes != gib-PartitionReserve || !p.TransitionChecked {
		t.Fatalf("план %+v", p)
	}
	reshaping := okStatus(LevelRAID5, 3, gib-3*Align)
	reshaping.Health, reshaping.Arrays[0].Action = HealthReshaping, "reshape"
	pct := 40.0
	reshaping.Arrays[0].Progress = &pct
	// Сразу после grow массив ещё в покое (reshape не начался) — задача ждёт.
	early := okStatus(LevelRAID5, 3, gib-3*Align)
	done := okStatus(LevelRAID5, 3, 2*(gib-3*Align))
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align)) // для сверки при подтверждении
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(early, reshaping, reshaping, done)
	if r := e.wait(t, tk.ID); r.State != tasks.Succeeded {
		t.Fatalf("задача %+v", r)
	}
	if e.h.called(OpAddDisk) != 1 || e.h.called(OpGrow) != 1 {
		t.Fatalf("вызовы %v", e.h.calls)
	}
	var ap AddDiskParams
	json.Unmarshal(e.h.params[OpAddDisk], &ap)
	if ap.Disk != ref103 || len(ap.Arrays) != 1 || ap.Arrays[0].RaidDevices != 3 || ap.Arrays[0].Level != LevelRAID5 {
		t.Fatalf("параметры %+v", ap)
	}
	rec, _ := e.s.getRecord(ctx, "p1")
	if len(rec.Disks) != 3 || rec.Arrays[0].Level != LevelRAID5 {
		t.Fatalf("запись %+v", rec)
	}
}

func TestAddDiskPlanRejections(t *testing.T) {
	ctx := context.Background()
	t.Run("пул не в покое", func(t *testing.T) {
		e := newSvc(t)
		seedPool(t, e, "shr", false)
		st := okStatus(LevelRAID1, 2, gib)
		st.Health = HealthResyncing
		e.h.setStatus(st)
		if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("меньший диск меняет слои", func(t *testing.T) {
		e := newSvc(t)
		seedPool(t, e, "shr", false)
		e.h.setStatus(okStatus(LevelRAID1, 2, gib))
		small := gib / 2
		e.h.snap.Disks[3].SizeBytes = &small
		if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("недопустимый переход", func(t *testing.T) {
		e := newSvc(t)
		seedPool(t, e, "raid1", false)
		e.h.setStatus(okStatus(LevelRAID1, 2, gib))
		if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", Type: "raid6", DiskID: ref103.ID}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("нет пула", func(t *testing.T) {
		e := newSvc(t)
		if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v", err)
		}
	})
}

func TestReplacePlanAndTask(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", true)
	degraded := okStatus(LevelRAID1, 2, gib)
	degraded.Health, degraded.DiskIDs = HealthDegraded, []string{ref101.ID}
	degraded.Arrays[0].Missing, degraded.Arrays[0].ActiveDevices = 1, 1
	// Исправный пул заменять нельзя.
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("исправный пул: %v", err)
	}
	e.h.setStatus(degraded)
	// Диск, всё ещё входящий в пул, — не замена.
	if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref101.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("диск пула: %v", err)
	}
	p, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	rebuilding := degraded
	rebuilding.Arrays = []ArrayStatus{degraded.Arrays[0]}
	rebuilding.Health, rebuilding.Arrays[0].Action = HealthRebuilding, "recovery"
	healthy := replacedStatus(ref101.ID, ref103.ID, "")
	e.h.setStatus(rebuilding, rebuilding, healthy)
	if r := e.wait(t, tk.ID); r.State != tasks.Succeeded {
		t.Fatalf("задача %+v", r)
	}
	var rp ReplaceParams
	json.Unmarshal(e.h.params[OpReplace], &rp)
	// Запись без integrity_mode (старый пул) открывалась в режиме журнала:
	// новый участник открывается так же, режим в записи сохраняется.
	if rp.Disk != ref103 || !rp.Integrity || rp.IntegrityMode != LegacyIntegrityMode || len(rp.Arrays) != 1 || rp.Arrays[0].SliceBytes != gib-PartitionReserve {
		t.Fatalf("параметры %+v", rp)
	}
	rec, _ := e.s.getRecord(ctx, "p1")
	if rec.IntegrityMode != LegacyIntegrityMode {
		t.Fatalf("режим в записи %q", rec.IntegrityMode)
	}
	if len(rec.Disks) != 2 || rec.Disks[0].ID != ref101.ID || rec.Disks[1].ID != ref103.ID ||
		len(rec.Arrays[0].Members) != 2 || rec.Arrays[0].Members[1] != ref103.ID {
		t.Fatalf("запись %+v", rec)
	}
}

func TestTaskFailsOnHelperRefusal(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	e.h.fail = map[string]error{OpCreate: errors.New("gamayun-helper: refused: на диске sdb есть раздел")}
	p, _ := e.s.Plan(ctx, createReq)
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	r := e.wait(t, tk.ID)
	if r.State != tasks.Failed || r.Error == nil {
		t.Fatalf("%+v", r)
	}
	if _, err := e.s.getRecord(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("запись пула не должна появляться при отказе")
	}
}

func TestTooManyPlans(t *testing.T) {
	e := newSvc(t)
	for i := 0; i < maxPending; i++ {
		if _, err := e.s.Plan(context.Background(), createReq); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.Plan(context.Background(), createReq); !errors.Is(err, ErrTooMany) {
		t.Fatalf("%v", err)
	}
	e.now = e.now.Add(11 * time.Minute) // истёкшие вычищаются
	if _, err := e.s.Plan(context.Background(), createReq); err != nil {
		t.Fatal(err)
	}
}
