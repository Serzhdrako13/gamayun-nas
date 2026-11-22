package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/smart"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
)

type fakeHelper struct {
	mu    sync.Mutex
	ids   []string
	info  map[string]smart.Info
	tests []string
	// Вызовы disk.smart.set; failSet — диск не включает SMART.
	sets    []string
	failSet bool
}

func (f *fakeHelper) Call(_ context.Context, op string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var v any
	switch op {
	case "disks.observe":
		s := disks.Snapshot{}
		for _, id := range f.ids {
			s.Disks = append(s.Disks, disks.Disk{ID: id, KernelName: "sdx"})
		}
		s.Disks = append(s.Disks, disks.Disk{KernelName: "sr0"}) // без ID — пропускается
		v = s
	case smart.OpRead:
		p := params.(smart.DiskParams)
		in, ok := f.info[p.DiskID]
		if !ok {
			return errors.New("нет диска")
		}
		in.DiskID = p.DiskID
		v = in
	case smart.OpTest:
		p := params.(smart.TestParams)
		f.tests = append(f.tests, p.DiskID+":"+p.Type)
		v = smart.TestResult{DiskID: p.DiskID, Type: p.Type, Started: true}
	case smart.OpSet:
		p := params.(smart.SetParams)
		f.sets = append(f.sets, fmt.Sprintf("%s:%v", p.DiskID, p.On))
		if f.failSet {
			return errors.New("smartctl -s on: код 4")
		}
		in := f.info[p.DiskID]
		in.DiskID = p.DiskID
		in.Disabled, in.Supported = !p.On, p.On
		f.info[p.DiskID] = in
		v = in
	default:
		return errors.New("неизвестная операция " + op)
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, result)
}

type fakeStorage struct {
	names   []string
	scrubs  []string
	running bool
	fail    map[string]error
}

func (f *fakeStorage) Names(context.Context) ([]string, error) { return f.names, nil }
func (f *fakeStorage) ScrubRunning() bool                      { return f.running }
func (f *fakeStorage) Scrub(_ context.Context, pool, trigger string) (tasks.Task, error) {
	if err := f.fail[pool]; err != nil {
		return tasks.Task{}, err
	}
	f.scrubs = append(f.scrubs, pool+":"+trigger)
	return tasks.Task{ID: "t-" + pool}, nil
}

type env struct {
	s   *Service
	h   *fakeHelper
	st  *fakeStorage
	log *events.Log
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{h: &fakeHelper{info: map[string]smart.Info{}}, st: &fakeStorage{}, log: events.NewLog(db),
		now: time.Date(2026, 11, 14, 3, 5, 0, 0, time.UTC)}
	e.s = New(db, e.h, e.st, e.log, Options{Location: time.UTC})
	e.s.SetClock(func() time.Time { return e.now })
	return e
}

func (e *env) events(t *testing.T) []events.Event {
	t.Helper()
	evs, err := e.log.Recent(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func types(evs []events.Event) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		m[e.Type+"/"+e.Severity]++
	}
	return m
}

func p64(v int64) *int64 { return &v }
func pb(v bool) *bool    { return &v }

func TestScheduleValidateAndStore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sc, err := e.s.Schedule(ctx)
	if err != nil || sc != DefaultSchedule() || !sc.Scrub.Enabled || sc.Scrub.IntervalDays != 30 || sc.SmartShort.IntervalDays != 7 {
		t.Fatalf("по умолчанию: %+v %v", sc, err)
	}
	bad := sc
	bad.Hour = 24
	if _, err := e.s.SetSchedule(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Error("hour 24")
	}
	bad = sc
	bad.SmartLong.IntervalDays = 0
	if _, err := e.s.SetSchedule(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Error("interval 0")
	}
	sc.Scrub.Enabled, sc.Hour = false, 4
	if _, err := e.s.SetSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.Schedule(ctx); got != sc {
		t.Fatalf("сохранено %+v", got)
	}
}

func TestScrubScheduling(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.names = []string{"a", "b"}
	// Первый такт — только отметки, без запуска.
	if err := e.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.st.scrubs) != 0 {
		t.Fatalf("первый такт запустил %v", e.st.scrubs)
	}
	// Через 30 дней в 03:xx — один пул за такт.
	e.now = e.now.Add(30 * 24 * time.Hour)
	e.s.Tick(ctx)
	if len(e.st.scrubs) != 1 || e.st.scrubs[0] != "a:schedule" {
		t.Fatalf("запуски %v", e.st.scrubs)
	}
	// Пока идёт проверка — второй не начинается.
	e.st.running = true
	e.now = e.now.Add(time.Minute)
	e.s.Tick(ctx)
	if len(e.st.scrubs) != 1 {
		t.Fatalf("параллельная проверка: %v", e.st.scrubs)
	}
	e.st.running = false
	e.now = e.now.Add(time.Minute)
	e.s.Tick(ctx)
	if len(e.st.scrubs) != 2 || e.st.scrubs[1] != "b:schedule" {
		t.Fatalf("второй пул: %v", e.st.scrubs)
	}
	// Повторно в тот же день — нет.
	e.now = e.now.Add(time.Minute)
	e.s.Tick(ctx)
	if len(e.st.scrubs) != 2 {
		t.Fatalf("повтор: %v", e.st.scrubs)
	}
}

func TestScrubMissedAndRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.names = []string{"a"}
	e.s.Tick(ctx)
	// Простой 90 дней, включение в 12:00 — не в час расписания.
	e.now = e.now.Add(90*24*time.Hour + 9*time.Hour)
	e.s.Tick(ctx)
	if len(e.st.scrubs) != 0 {
		t.Fatal("запуск вне часа расписания")
	}
	// Пул деградирован — отложено, отметка не ставится.
	e.now = e.now.Add(15 * time.Hour) // 03:05 следующего дня
	e.st.fail = map[string]error{"a": errors.New("degraded")}
	e.s.Tick(ctx)
	last, _ := e.s.Last(ctx)
	if !last[scrubKey("a")].Before(e.now.Add(-24 * time.Hour)) {
		t.Fatal("отказанный запуск не должен ставить отметку")
	}
	// Пул восстановлен в тот же час — один запуск, не серия за 90 дней.
	e.st.fail = nil
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(time.Minute)
		e.s.Tick(ctx)
	}
	if len(e.st.scrubs) != 1 {
		t.Fatalf("пропущенные запуски: %v", e.st.scrubs)
	}
}

func TestDisabled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.names = []string{"a"}
	e.h.ids = []string{"d1"}
	e.h.info["d1"] = smart.Info{Supported: true, Passed: pb(true)}
	sc := DefaultSchedule()
	sc.Scrub.Enabled, sc.SmartShort.Enabled, sc.SmartLong.Enabled = false, false, false
	e.s.SetSchedule(ctx, sc)
	for i := 0; i < 3; i++ {
		e.s.Tick(ctx)
		e.now = e.now.Add(40 * 24 * time.Hour)
	}
	if len(e.st.scrubs) != 0 || len(e.h.tests) != 0 {
		t.Fatalf("выключено, но запущено: %v %v", e.st.scrubs, e.h.tests)
	}
}

func TestSmartTestsSchedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.h.ids = []string{"d1", "q1", "z1"}
	e.h.info["d1"] = smart.Info{Supported: true, Passed: pb(true)}
	e.h.info["q1"] = smart.Info{Supported: false} // QEMU
	e.h.info["z1"] = smart.Info{Supported: true, Standby: true}
	e.s.Tick(ctx)
	if len(e.h.tests) != 0 {
		t.Fatal("первый такт")
	}
	e.now = e.now.Add(7 * 24 * time.Hour)
	e.s.Tick(ctx)
	if len(e.h.tests) != 1 || e.h.tests[0] != "d1:short" {
		t.Fatalf("короткий: %v", e.h.tests)
	}
	// Через 30 дней от начала — длинный вместо короткого.
	e.now = e.now.Add(23 * 24 * time.Hour)
	e.s.Tick(ctx)
	if len(e.h.tests) != 2 || e.h.tests[1] != "d1:long" {
		t.Fatalf("длинный: %v", e.h.tests)
	}
}

func TestSmartEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.h.ids = []string{"d1", "q1"}
	e.h.info["q1"] = smart.Info{Supported: false}
	ok := smart.Info{Supported: true, Passed: pb(true), Reallocated: p64(0), Pending: p64(0),
		LastTest: &smart.SelfTest{Type: "short", Passed: true, Status: "ok", LifetimeHours: p64(100)}}
	e.h.info["d1"] = ok
	if _, err := e.s.PollSmart(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events(t)); n != 0 {
		t.Fatalf("исправный диск и QEMU — без событий: %v", types(e.events(t)))
	}
	// Повтор без изменений — без событий.
	e.s.PollSmart(ctx)
	if n := len(e.events(t)); n != 0 {
		t.Fatal("повтор дал события")
	}
	// Рост ожидающих секторов, неудачный длинный тест, итог FAILED.
	bad := ok
	bad.Pending = p64(8)
	bad.Reallocated = p64(3) // два счётчика — одно событие
	bad.Passed = pb(false)
	bad.LastTest = &smart.SelfTest{Type: "long", Passed: false, Status: "read failure", LifetimeHours: p64(120)}
	e.h.info["d1"] = bad
	e.s.PollSmart(ctx)
	got := types(e.events(t))
	want := map[string]int{EvSmartFailed + "/critical": 1, EvSmartSectors + "/warning": 1, EvSmartTestFail + "/critical": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d (все: %v)", k, got[k], got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("лишние события: %v", got)
	}
	// Повтор того же состояния — без новых событий (не спамим).
	before := len(e.events(t))
	e.s.PollSmart(ctx)
	if len(e.events(t)) != before {
		t.Fatal("повтор плохого состояния дал события")
	}
	// Спящий диск — прошлое состояние сохраняется, событий нет.
	e.h.info["d1"] = smart.Info{Supported: true, Standby: true}
	e.s.PollSmart(ctx)
	st, _ := e.s.Smart(ctx)
	if st["d1"].Standby || val(st["d1"].Pending) != 8 {
		t.Fatalf("standby затёр состояние: %+v", st["d1"])
	}
	// Состояние переживает перезапуск (новый сервис на той же базе).
	s2 := New(e.s.db, e.h, e.st, e.log, Options{})
	e.h.info["d1"] = bad
	s2.PollSmart(ctx)
	if len(e.events(t)) != before {
		t.Fatal("после перезапуска старое состояние сообщено повторно")
	}
	// Успешный тест — info.
	good := bad
	good.LastTest = &smart.SelfTest{Type: "short", Passed: true, Status: "ok", LifetimeHours: p64(130)}
	e.h.info["d1"] = good
	in, err := s2.ReadDisk(ctx, "d1")
	if err != nil || !in.Supported {
		t.Fatal(err)
	}
	if types(e.events(t))[EvSmartTestPass+"/info"] != 1 {
		t.Fatalf("успешный тест: %v", types(e.events(t)))
	}
}

func TestOnScrubEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.s.OnScrub(home.ScrubResult{Pool: "a", Trigger: "manual", Btrfs: &home.BtrfsScrub{Status: "finished"}}, nil)
	e.s.OnScrub(home.ScrubResult{Pool: "a", IntegrityErrorsDelta: 2, Btrfs: &home.BtrfsScrub{}}, nil)
	e.s.OnScrub(home.ScrubResult{Pool: "a", Btrfs: &home.BtrfsScrub{CsumErrors: 1, UncorrectableErrors: 1}}, nil)
	e.s.OnScrub(home.ScrubResult{Pool: "a", Cancelled: true}, nil)
	e.s.OnScrub(home.ScrubResult{Pool: "a"}, errors.New("прервана"))
	got := types(e.events(t))
	want := map[string]int{EvScrubFinished + "/info": 2, EvScrubFinished + "/warning": 1, EvScrubFinished + "/critical": 1, EvScrubFailed + "/warning": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d (все: %v)", k, got[k], got)
		}
	}
	last, _ := e.s.Last(ctx)
	if _, ok := last[scrubKey("a")]; !ok {
		t.Error("завершённая проверка должна сдвигать плановую")
	}
	for _, ev := range e.events(t) {
		if ev.Subject != "pool:a" {
			t.Errorf("subject %q", ev.Subject)
		}
	}
}

// Автовключение, предупреждение без повторов, ручной выбор.
func TestSmartDisabledAuto(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.h.ids = []string{"d1", "d2"}
	e.h.info["d1"] = smart.Info{Disabled: true, Model: "TOSHIBA"}
	e.h.info["d2"] = smart.Info{Disabled: true}
	e.h.failSet = true
	// Не включается: одна попытка, одно предупреждение.
	got, _ := e.s.PollSmart(ctx)
	e.s.PollSmart(ctx)
	if len(e.h.sets) != 2 || !got["d1"].Disabled {
		t.Fatalf("попытки: %v %+v", e.h.sets, got["d1"])
	}
	if c := types(e.events(t)); c["disk.smart_disabled/warning"] != 2 || len(c) != 1 {
		t.Fatalf("события: %v", c)
	}
	// Включается: после перезапуска gamayund (новый сервис) — попытка, info.
	e.h.failSet = false
	e.s = New(e.s.db, e.h, e.st, e.log, Options{Location: time.UTC})
	got, _ = e.s.PollSmart(ctx)
	if !got["d1"].Supported || got["d1"].Disabled {
		t.Fatalf("не включён: %+v", got["d1"])
	}
	if c := types(e.events(t)); c["disk.smart_enabled/info"] != 2 {
		t.Fatalf("события: %v", c)
	}
	// Выключен снаружи в том же запуске (smartctl -s off) — снова включается.
	e.h.sets = nil
	e.h.info["d2"] = smart.Info{Disabled: true}
	got, _ = e.s.PollSmart(ctx)
	if len(e.h.sets) != 1 || got["d2"].Disabled {
		t.Fatalf("повторное автовключение: %v %+v", e.h.sets, got["d2"])
	}
	if c := types(e.events(t)); c["disk.smart_enabled/info"] != 3 {
		t.Fatalf("события: %v", c)
	}
	// Оператор выключил d1 — автоматика не включает, без предупреждения.
	in, err := e.s.SetSmart(ctx, "d1", false, "admin")
	if err != nil || !in.Disabled || !in.ManualOff {
		t.Fatalf("выключение: %+v %v", in, err)
	}
	e.h.sets = nil
	got, _ = e.s.PollSmart(ctx)
	if len(e.h.sets) != 0 || !got["d1"].ManualOff || !got["d1"].Disabled {
		t.Fatalf("автовключение после ручного: %v %+v", e.h.sets, got["d1"])
	}
	c := types(e.events(t))
	if c["disk.smart_disabled/info"] != 1 || c["disk.smart_disabled/warning"] != 2 {
		t.Fatalf("события: %v", c)
	}
	sm, _ := e.s.Summaries(ctx, map[string]string{"d1": "hdd"})
	if !sm["d1"].Disabled || !sm["d1"].ManualOff {
		t.Fatalf("итог: %+v", sm["d1"])
	}
	// Оператор включил — снова под автоматикой.
	if in, err := e.s.SetSmart(ctx, "d1", true, "admin"); err != nil || in.Disabled {
		t.Fatalf("включение: %+v %v", in, err)
	}
	if m, _ := e.s.manualOff(ctx); m["d1"] {
		t.Fatal("ручной выбор не снят")
	}
}
