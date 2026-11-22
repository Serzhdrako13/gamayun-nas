package apps

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
)

// fakeHelper — состояние helper apps.* в памяти.
type fakeHelper struct {
	mu        sync.Mutex
	st        Status
	ops       []string
	fail      map[string]error
	createdBy bool
	evs       chan EventsResult
	busy      PoolBusyResult
}

func (f *fakeHelper) Call(ctx context.Context, op string, params, result any) error {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	if err := f.fail[op]; err != nil {
		f.mu.Unlock()
		return err
	}
	var v any
	switch op {
	case OpStatus:
		v = f.st
	case OpConfigure:
		f.st.Pool = params.(PoolParams).Pool
		if !f.st.AppsExists {
			f.createdBy, f.st.AppsExists = true, true
		}
		v = ConfigureResult{Pool: f.st.Pool}
	case OpUnconfigure:
		if params.(UnconfigureParams).PurgeCreated && f.createdBy {
			f.st.AppsExists = false
		}
		f.st.Pool = ""
		v = UnconfigureResult{}
	case OpStart:
		if f.st.PoolReadOnly || !f.st.PoolMounted {
			f.mu.Unlock()
			return errors.New("refused: пул")
		}
		f.st.Docker, f.st.Containerd, f.st.StartFlag = "active", "active", true
		v = StartResult{Version: "29.1.0"}
	case OpStop:
		f.st.Docker, f.st.Containerd, f.st.StartFlag, f.st.Leftover = "inactive", "inactive", false, 0
		v = StopResult{}
	case OpRemount:
		f.st.PoolReadOnly = false
		v = RemountResult{}
	case OpPoolBusy:
		v = f.busy
	case OpEvents:
		ch := f.evs
		f.mu.Unlock()
		select {
		case r := <-ch:
			b, _ := json.Marshal(r)
			return json.Unmarshal(b, result)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Unlock()
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

func (f *fakeHelper) set(fn func(st *Status)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.st)
}

type fakePools struct{ v home.PoolView }

func (p *fakePools) Get(_ context.Context, name string) (home.PoolView, error) {
	if name != "p1" {
		return home.PoolView{}, errors.New("нет пула")
	}
	return p.v, nil
}

type svcRig struct {
	s   *Service
	h   *fakeHelper
	p   *fakePools
	log *events.Log
	mgr *tasks.Manager
}

func newSvcRig(t *testing.T) *svcRig {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := events.NewLog(db)
	mgr := tasks.New(db, log, tasks.Options{})
	h := &fakeHelper{st: Status{Installed: true, PoolMounted: true, Docker: "inactive", Containerd: "inactive"}, fail: map[string]error{}, evs: make(chan EventsResult)}
	p := &fakePools{v: home.PoolView{Status: &home.PoolStatus{Mounted: true}}}
	s := &Service{DB: db, Helper: h, Events: log, Tasks: mgr, Pools: p}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return &svcRig{s: s, h: h, p: p, log: log, mgr: mgr}
}

func (r *svcRig) wait(t *testing.T, id string) tasks.Task {
	t.Helper()
	for i := 0; i < 200; i++ {
		tk, err := r.mgr.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.State == tasks.Succeeded || tk.State == tasks.Failed {
			return tk
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("задача не завершилась")
	return tasks.Task{}
}

func (r *svcRig) types(t *testing.T) []string {
	evs, err := r.log.Recent(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		if strings.HasPrefix(evs[i].Type, "apps.") {
			out = append(out, evs[i].Type)
		}
	}
	return out
}

func TestEnableReactDisable(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	if v := r.s.Refresh(ctx); v.State != StateDisabled {
		t.Fatalf("%+v", v)
	}
	if _, err := r.s.Enable(ctx, "zz"); err == nil {
		t.Fatal("нет пула — отказ")
	}
	tk, err := r.s.Enable(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk)
	}
	if v := r.s.View(); v.State != StateRunning || v.Pool != "p1" {
		t.Fatalf("%+v", v)
	}
	if _, err := r.s.Enable(ctx, "p2"); !errors.Is(err, ErrConflictSvc) {
		t.Fatalf("другой пул: %v", err)
	}
	// Пул пропал: Docker остановлен, «недоступно», событие — один раз.
	r.h.set(func(st *Status) { st.PoolMounted = false })
	for i := 0; i < 2; i++ {
		if v := r.s.Refresh(ctx); v.State != StateUnavailable || v.Reason != "Приложения недоступны: пул p1 не подключён" {
			t.Fatalf("%+v", v)
		}
	}
	if r.h.count(OpStop) != 1 {
		t.Fatalf("stop: %d", r.h.count(OpStop))
	}
	// Пул вернулся — запуск сам.
	r.h.set(func(st *Status) { st.PoolMounted = true })
	if v := r.s.Refresh(ctx); v.State != StateRunning {
		t.Fatalf("%+v", v)
	}
	// Пул ушёл в RO: стоп, пауза; после возврата записи — пауза до resume.
	r.h.set(func(st *Status) { st.PoolReadOnly = true })
	if v := r.s.Refresh(ctx); v.State != StatePaused || v.ReasonCode != ReasonReadOnly {
		t.Fatalf("%+v", v)
	}
	if v := r.s.Refresh(ctx); v.State != StatePaused || r.h.st.Docker != "inactive" {
		t.Fatalf("%+v", v)
	}
	// resume: перемонтирование и запуск.
	tk, err = r.s.Resume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded || r.h.count(OpRemount) != 1 {
		t.Fatalf("%+v remount=%d", tk, r.h.count(OpRemount))
	}
	if v := r.s.View(); v.State != StateRunning {
		t.Fatalf("%+v", v)
	}
	// Выключение: стоп и снятие настройки.
	tk, _ = r.s.Disable(ctx)
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded || r.h.st.Pool != "" || r.h.st.Docker != "inactive" {
		t.Fatalf("%+v %+v", tk, r.h.st)
	}
	want := []string{EvEnabled, EvUnavailable, EvStarted, EvPaused, EvResumed, EvDisabled}
	if got := r.types(t); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("события %v, ожидалось %v", got, want)
	}
}

func TestEnableRollback(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	r.h.fail[OpStart] = errors.New("dockerd упал")
	tk, err := r.s.Enable(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	tk = r.wait(t, tk.ID)
	if tk.State != tasks.Failed || tk.Error == nil || !strings.Contains(tk.Error.Message, "исходном состоянии") {
		t.Fatalf("%+v", tk)
	}
	if r.h.st.Pool != "" || r.h.st.AppsExists || r.h.count(OpUnconfigure) != 1 {
		t.Fatalf("откат: %+v", r.h.st)
	}
	if c, _ := r.s.load(ctx); c.Enabled {
		t.Fatal("после сбоя включено")
	}
	// Защита запрещает запись — отказ до задачи.
	r.p.v.Protection.WriteBlocked = true
	if _, err := r.s.Enable(ctx, "p1"); !errors.Is(err, ErrConflictSvc) {
		t.Fatalf("защита: %v", err)
	}
}

func TestStartFailureSticky(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	tk, _ := r.s.Enable(ctx, "p1")
	r.wait(t, tk.ID)
	r.h.set(func(st *Status) { st.Docker, st.Containerd = "failed", "inactive" })
	r.h.fail[OpStart] = errors.New("нет места")
	if v := r.s.Refresh(ctx); v.State != StateError || !strings.Contains(v.Reason, "нет места") {
		t.Fatalf("%+v", v)
	}
	if v := r.s.Refresh(ctx); v.State != StateError || r.h.count(OpStart) != 2 {
		t.Fatalf("повтор без действия администратора: %+v, start=%d", v, r.h.count(OpStart))
	}
	delete(r.h.fail, OpStart)
	tk, err := r.s.Resume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded || r.h.count(OpRemount) != 0 {
		t.Fatalf("%+v", tk)
	}
}

func TestBeforeReadOnlyHook(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	if err := r.s.BeforeReadOnly(ctx, "p1"); err != nil || r.h.count(OpStop) != 0 {
		t.Fatal("не включено — ничего не делать")
	}
	tk, _ := r.s.Enable(ctx, "p1")
	r.wait(t, tk.ID)
	if r.s.PoolInUse(ctx, "p1") == "" || r.s.PoolInUse(ctx, "p2") != "" {
		t.Fatal("PoolInUse")
	}
	if err := r.s.BeforeReadOnly(ctx, "p1"); err != nil || r.h.count(OpStop) != 1 {
		t.Fatal(err)
	}
	if c, _ := r.s.load(ctx); c.Paused != ReasonProtection {
		t.Fatalf("%+v", c)
	}
	// Защита сняла RO (пул rw) — всё равно пауза до подтверждения.
	if v := r.s.Refresh(ctx); v.State != StatePaused || v.ReasonCode != ReasonResumeReady {
		t.Fatalf("%+v", v)
	}
}

func TestDockerEvents(t *testing.T) {
	r := newSvcRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.s.EventsWait = 1
	go r.s.watchEvents(ctx)
	now := time.Now()
	n := func(d time.Duration) int64 { return now.Add(d).UnixNano() }
	r.h.evs <- EventsResult{Running: true, UntilNano: n(time.Minute), Events: []DockerEvent{
		{TimeNano: n(1), Action: "die", Container: "a", ExitCode: "0"},               // штатно — нет
		{TimeNano: n(2), Action: "die", Container: "a", ExitCode: "3"},               // сбой
		{TimeNano: n(3 * time.Second), Action: "die", Container: "a", ExitCode: "3"}, // повтор в окне — подавлен
		{TimeNano: n(4), Action: "oom", Container: "b"},
		{TimeNano: n(5), Action: "health_status", Health: "healthy", Container: "c"}, // без прежнего unhealthy — нет
		{TimeNano: n(6), Action: "health_status", Health: "unhealthy", Container: "c"},
		{TimeNano: n(7), Action: "health_status", Health: "unhealthy", Container: "c"}, // повтор состояния — нет
		{TimeNano: n(8), Action: "health_status", Health: "healthy", Container: "c"},
		{TimeNano: n(6 * time.Minute), Action: "die", Container: "a", ExitCode: "3"}, // после окна — с числом подавленных
	}}
	// Следующий опрос — Docker остановлен (переподключение без ошибки).
	r.h.evs <- EventsResult{Running: false}
	want := []string{EvContainerDied, EvContainerOOM, EvContainerUnhealthy, EvContainerHealthy, EvContainerDied}
	var got []string
	for i := 0; i < 100; i++ {
		if got = r.types(t); len(got) >= len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("события %v, ожидалось %v", got, want)
	}
	evs, _ := r.log.Recent(context.Background(), 1)
	var d map[string]any
	json.Unmarshal(evs[0].Data, &d)
	if d["suppressed"] != float64(1) || d["exit_code"] != "3" || evs[0].Subject != "container:a" || evs[0].Severity != events.SevWarning {
		t.Fatalf("%+v %v", evs[0], d)
	}
}

func TestStopWindowSkipsSigterm(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	r.s.stopDocker(ctx, "тест")
	r.s.dockerEvent(ctx, DockerEvent{TimeNano: time.Now().UnixNano(), Action: "die", Container: "x", ExitCode: "143"})
	r.s.dockerEvent(ctx, DockerEvent{TimeNano: time.Now().Add(time.Hour).UnixNano(), Action: "die", Container: "y", ExitCode: "143"})
	if got := r.types(t); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestDieOOMKilled(t *testing.T) {
	r := newSvcRig(t)
	r.s.dockerEvent(context.Background(), DockerEvent{TimeNano: time.Now().UnixNano(), Action: "die", Container: "m", ExitCode: "137", OOM: true})
	if got := r.types(t); len(got) != 1 || got[0] != EvContainerOOM {
		t.Fatalf("%v", got)
	}
}

// Стенд 2026-11-20: ошибка запуска не должна держать Docker остановленным после
// возврата пула, перезапуска gamayund и по истечении RetryAfter.
func TestStartErrorRetried(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	tk, _ := r.s.Enable(ctx, "p1")
	r.wait(t, tk.ID)
	r.h.set(func(st *Status) { st.Docker, st.Containerd = "failed", "inactive" })
	r.h.fail[OpStart] = errors.New("start-limit-hit")
	if v := r.s.Refresh(ctx); v.State != StateError {
		t.Fatalf("%+v", v)
	}
	delete(r.h.fail, OpStart)
	r.h.set(func(st *Status) { st.PoolMounted = false })
	r.s.Refresh(ctx)
	r.h.set(func(st *Status) { st.PoolMounted = true })
	if v := r.s.Refresh(ctx); v.State != StateRunning {
		t.Fatalf("после возврата пула: %+v", v)
	}
	if c, _ := r.s.load(ctx); c.Error != "" {
		t.Fatalf("ошибка не снята: %+v", c)
	}
	// Повтор по времени.
	r.h.set(func(st *Status) { st.Docker = "failed" })
	r.h.fail[OpStart] = errors.New("x")
	r.s.Refresh(ctx)
	delete(r.h.fail, OpStart)
	r.s.now = func() time.Time { return time.Now().Add(RetryAfter + time.Second) }
	if v := r.s.Refresh(ctx); v.State != StateRunning {
		t.Fatalf("после RetryAfter: %+v", v)
	}
}

// Службы остановлены, но процессы контейнеров остались (dockerd упал
// раньше containerd) — при пропаже пула gamayund вызывает stop и для них.
func TestLeftoverStoppedOnPoolLoss(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	tk, err := r.s.Enable(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk)
	}
	r.h.set(func(st *Status) {
		st.PoolMounted, st.Docker, st.Containerd, st.Leftover = false, "inactive", "inactive", 2
	})
	if v := r.s.Refresh(ctx); v.State != StateUnavailable {
		t.Fatalf("%+v", v)
	}
	if r.h.count(OpStop) != 1 || r.h.st.Leftover != 0 {
		t.Fatalf("stop: %d, остатки %d", r.h.count(OpStop), r.h.st.Leftover)
	}
	r.s.Refresh(ctx)
	if r.h.count(OpStop) != 1 {
		t.Fatalf("без остатков stop не повторяется: %d", r.h.count(OpStop))
	}
}

// Отказ «пул занят» виден в состоянии, событие — один раз,
// сброс — когда пул освободился.
func TestPoolBusyState(t *testing.T) {
	r := newSvcRig(t)
	ctx := context.Background()
	tk, err := r.s.Enable(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Succeeded {
		t.Fatalf("%+v", tk)
	}
	// Остановка при RO: остатки не убиты — helper отказывает «пул занят».
	r.h.mu.Lock()
	r.h.fail[OpStop] = errors.New("Docker остановлен, но пул p1 занят: процесс 7 app: cwd /srv/p1/.apps/x")
	r.h.busy = PoolBusyResult{Apps: []string{"процесс 7 app: cwd /srv/p1/.apps/x"}}
	r.h.mu.Unlock()
	r.h.set(func(st *Status) { st.PoolReadOnly = true })
	for i := 0; i < 3; i++ {
		v := r.s.Refresh(ctx)
		if v.State != StateBusy || v.ReasonCode != ReasonPoolBusy || !strings.Contains(v.Reason, "процесс 7 app") {
			t.Fatalf("%d: %+v", i, v)
		}
	}
	n := 0
	for _, ty := range r.types(t) {
		if ty == EvPoolBusy {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("событий pool_busy: %d (%v)", n, r.types(t))
	}
	// Процесс закрыт — состояние сбрасывается в обычную паузу.
	r.h.mu.Lock()
	r.h.busy = PoolBusyResult{}
	r.h.mu.Unlock()
	if v := r.s.Refresh(ctx); v.State != StatePaused || v.ReasonCode != ReasonReadOnly {
		t.Fatalf("после освобождения: %+v", v)
	}
	// Возобновление: перемонтирование отказывает — посторонний процесс вне .apps.
	r.h.mu.Lock()
	delete(r.h.fail, OpStop)
	r.h.fail[OpRemount] = errors.New("refused: пул p1 занят, перемонтирование невозможно: процесс 9 sleep: cwd /srv/p1/data")
	r.h.busy = PoolBusyResult{Holders: []string{"процесс 9 sleep: cwd /srv/p1/data"}}
	r.h.mu.Unlock()
	tk, err = r.s.Resume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tk = r.wait(t, tk.ID); tk.State != tasks.Failed {
		t.Fatalf("%+v", tk)
	}
	if v := r.s.Refresh(ctx); v.State != StateBusy || !strings.Contains(v.Reason, "процесс 9 sleep") {
		t.Fatalf("remount: %+v", v)
	}
	r.h.mu.Lock()
	r.h.busy = PoolBusyResult{}
	r.h.mu.Unlock()
	if v := r.s.Refresh(ctx); v.State != StatePaused {
		t.Fatalf("после освобождения (remount): %+v", v)
	}
	// Успешное возобновление — пул rw, запуск.
	r.h.mu.Lock()
	delete(r.h.fail, OpRemount)
	r.h.mu.Unlock()
	if tk, _ = r.s.Resume(ctx); r.wait(t, tk.ID).State != tasks.Succeeded {
		t.Fatal("resume")
	}
	if v := r.s.View(); v.State != StateRunning {
		t.Fatalf("%+v", v)
	}
}
