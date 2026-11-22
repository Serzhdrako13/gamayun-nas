package home

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/tasks"
)

// health unknown, срок опроса, сторож цикла
// защиты, остановка без новых изменяющих действий.

// statusErrRunner — Status хоста, в котором запрос tool/arg0 падает.
func statusErrRunner(t *testing.T, h *fakeHost, tool, arg0 string) {
	h.e.Runner = runnerFunc(func(ctx context.Context, to time.Duration, path string, args ...string) (CmdResult, error) {
		if filepath.Base(path) == tool && len(args) > 0 && args[0] == arg0 {
			return CmdResult{}, errors.New("таймаут запроса")
		}
		if filepath.Base(path) == "mdadm" && args[0] == "--detail" {
			return CmdResult{Stdout: []byte(sample(t, "detail-raid1-bitmap.txt"))}, nil
		}
		return h.Run(ctx, to, path, args...)
	})
}

// Ошибка опроса значимой части стека — unknown, а не ok/missing.
func TestStatusUnknownOnQueryError(t *testing.T) {
	for _, c := range []struct{ tool, arg, want string }{
		{"lvm", "vgs", "vgs:"},
		{"mdadm", "--detail", "mdadm --detail"},
		{"dmsetup", "status", "dmsetup"},
	} {
		h := newHost(t)
		existingPool(h, mdstatRAID1, "sdb", "sdc")
		statusErrRunner(t, h, c.tool, c.arg)
		st, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
		if err != nil {
			t.Fatal(err)
		}
		if st.Health != HealthUnknown || !strings.Contains(strings.Join(st.Unknown, ";"), c.want) {
			t.Errorf("%s %s: %s %v", c.tool, c.arg, st.Health, st.Unknown)
		}
	}
}

func unknownStatus() PoolStatus {
	st := okStatus(LevelRAID1, 2, gib-3*Align)
	st.Health, st.Unknown = HealthUnknown, []string{"lvs: таймаут запроса"}
	return st
}

// Ожидание перестройки не принимает неполные данные за «готово».
func TestWaitIdleUnknownHealth(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 30 * time.Millisecond
	seedPool(t, e, "shr", true)
	e.h.setStatus(unknownStatus())
	_, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, nil)
	if err == nil || !strings.Contains(err.Error(), "lvs: таймаут запроса") {
		t.Fatalf("ошибка %v", err)
	}
}

// Прерванное создание при неизвестном состоянии не откатывается
// (непрочитанный том данных — не «тома нет»).
func TestResolveCreatingUnknownNoRollback(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "shr", true)
	rec, _ := e.s.getRecord(context.Background(), "p1")
	rec.Creating = true
	if err := e.s.saveRecord(context.Background(), rec, false); err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(unknownStatus())
	err := e.s.AssembleAll(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "не определено") {
		t.Fatalf("AssembleAll: %v", err)
	}
	if e.h.called(OpCreateRollback) != 0 {
		t.Fatal("откат по неполным данным")
	}
}

// Остановка во время create — откат не начинается, запись остаётся
// «создаётся» для следующего запуска; изменяющие вызовы отклоняются,
// опрос состояния — нет; новые задачи не ставятся.
func TestStopDefersCreateRollback(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	p, err := e.s.Plan(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	e.h.fail = map[string]error{OpCreate: errors.New("обрыв соединения")}
	e.h.hook = func(op string) {
		if op == OpCreate {
			e.s.Stop()
		}
	}
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, tk.ID)
	if done.State != tasks.Failed || !strings.Contains(done.Error.Message, "откат выполнит следующий запуск") {
		t.Fatalf("задача %+v %+v", done, done.Error)
	}
	if e.h.called(OpCreateRollback) != 0 {
		t.Fatal("откат начат при остановке")
	}
	if rec, err := e.s.getRecord(ctx, "p1"); err != nil || !rec.Creating {
		t.Fatalf("запись %+v %v", rec, err)
	}
	if err := e.s.helper.Call(ctx, OpAssemble, AssembleParams{Pool: "p1"}, &AssembleResult{}); !errors.Is(err, ErrStopping) {
		t.Fatalf("изменяющий вызов при остановке: %v", err)
	}
	if _, err := e.s.status(ctx, "p1"); err != nil {
		t.Fatalf("опрос при остановке: %v", err)
	}
	raw, _ := json.Marshal(destroyTask{Pool: "p1"})
	if _, err := e.s.submit(ctx, "p1", TaskDestroy, raw); !errors.Is(err, ErrConflict) {
		t.Fatalf("постановка при остановке: %v", err)
	}
}

// hangHelper — опрос состояния висит, пока hang (до отмены ctx).
type hangHelper struct {
	*fakeHelper
	mu   sync.Mutex
	hang bool
}

func (h *hangHelper) set(v bool) { h.mu.Lock(); h.hang = v; h.mu.Unlock() }

func (h *hangHelper) Call(ctx context.Context, op string, params, result any) error {
	h.mu.Lock()
	hang := h.hang
	h.mu.Unlock()
	if op == OpStatus && hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return h.fakeHelper.Call(ctx, op, params, result)
}

// Зависший опрос состояния — ошибка за StatusTimeout, а не за срок клиента.
func TestStatusTimeout(t *testing.T) {
	e := newSvc(t)
	hh := &hangHelper{fakeHelper: e.h, hang: true}
	e.s.helper = guardedHelper{h: hh, s: e.s}
	e.s.StatusTimeout = 30 * time.Millisecond
	start := time.Now()
	if _, err := e.s.status(context.Background(), "p1"); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("опрос ждал %s", d)
	}
}

// Просрочка прохода защиты — unknown в состоянии пула, отказ в
// постановке операций; после прохода — снято.
func TestProtectionStaleMarksUnknown(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align))
	e.s.protStart(30 * time.Second)
	if why := e.s.ProtectionStale(); why != "" {
		t.Fatalf("сразу просрочено: %s", why)
	}
	e.now = e.now.Add(3 * time.Minute)
	if e.s.ProtectionStale() == "" {
		t.Fatal("просрочка не обнаружена")
	}
	v, err := e.s.Get(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status.Health != HealthUnknown || v.Protection.RedundancyExhausted != nil {
		t.Fatalf("состояние %s, запас %v", v.Status.Health, v.Protection.RedundancyExhausted)
	}
	raw, _ := json.Marshal(destroyTask{Pool: "p1"})
	if _, err := e.s.submit(ctx, "p1", TaskDestroy, raw); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "проход защиты") {
		t.Fatalf("постановка: %v", err)
	}
	e.s.protPassed()
	if v, _ := e.s.Get(ctx, "p1"); v.Status.Health != HealthOK {
		t.Fatalf("после прохода %s", v.Status.Health)
	}
}

// Зависший проход RunProtection — сторож сообщает (событие) один раз.
func TestRunProtectionStaleEvent(t *testing.T) {
	e := newSvc(t)
	e.s.SetClock(time.Now)
	seedPool(t, e, "shr", false)
	hh := &hangHelper{fakeHelper: e.h, hang: true}
	e.s.helper = guardedHelper{h: hh, s: e.s}
	e.s.StatusTimeout = time.Hour
	e.s.ProtectionStaleAfter = 50 * time.Millisecond
	e.s.MdstatPoll = 10 * time.Millisecond
	e.s.MdstatPath = filepath.Join(t.TempDir(), "mdstat")
	os.WriteFile(e.s.MdstatPath, nil, 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []string
	go e.s.RunProtection(ctx, time.Hour, func(err error) {
		mu.Lock()
		got = append(got, err.Error())
		mu.Unlock()
	})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := 0
	for _, g := range got {
		if strings.Contains(g, "проход защиты пулов не завершён") {
			n++
		}
	}
	mu.Unlock()
	if n != 1 {
		t.Fatalf("событий о просрочке %d: %v", n, got)
	}
	if e.s.ProtectionStale() == "" {
		t.Fatal("просрочка не отмечена")
	}
	cancel()
}

// Ошибка опроса и неизвестное состояние — решение защиты не принято,
// это ошибка с причиной.
func TestEnforceUnknown(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.fail = map[string]error{OpStatus: errors.New("helper молчит")}
	if err := e.s.EnforceAll(ctx); err == nil || !strings.Contains(err.Error(), "решение защиты не принято") {
		t.Fatalf("ошибка опроса: %v", err)
	}
	e.h.fail = nil
	e.h.setStatus(unknownStatus())
	if err := e.s.EnforceAll(ctx); err == nil || !strings.Contains(err.Error(), "только по прочитанному") {
		t.Fatalf("unknown: %v", err)
	}
	v, _ := e.s.Get(ctx, "p1")
	if v.Protection.RedundancyExhausted != nil {
		t.Fatalf("запас при unknown: %v", *v.Protection.RedundancyExhausted)
	}
}

// Непрочитанные массивы записи — не «массив не собран» (failed).
func TestUnknownNotOverriddenByRecord(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "shr", false)
	st := unknownStatus()
	st.Arrays = nil
	e.h.setStatus(st)
	got, err := e.s.status(context.Background(), "p1")
	if err != nil || got.Health != HealthUnknown {
		t.Fatalf("%s %v", got.Health, err)
	}
}
