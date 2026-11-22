package home

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gamayun-nas/internal/tasks"
)

// Откат создания трогает только диски записи.
func TestCreateRollbackOnlyRecordDisks(t *testing.T) {
	h := newHost(t)
	for _, d := range []string{"sdb", "sdc"} {
		h.part(d, 1)
		h.write("sys/devices/"+d+"/"+d+"1/uevent", "PARTNAME=dn-p1-0\n")
	}
	_, err := h.e.CreateRollback(context.Background(), CreateRollbackParams{Pool: "p1", DiskIDs: []string{ref101.ID}})
	if !errors.Is(err, ErrRefused) || len(writes(h.commands())) != 0 {
		t.Fatalf("ожидался отказ без записей: %v %v", err, h.commands())
	}
	if _, err := h.e.CreateRollback(context.Background(), CreateRollbackParams{Pool: "p1", DiskIDs: []string{ref101.ID, ref102.ID}}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	if !strings.Contains(got, "sgdisk --zap-all "+h.p("dev/sdb")) || !strings.Contains(got, "sgdisk --zap-all "+h.p("dev/sdc")) {
		t.Fatalf("команды:\n%s", got)
	}
	if err := (CreateRollbackParams{Pool: "p1"}).Validate(); err == nil {
		t.Fatal("без дисков должно быть отклонено")
	}
}

// Запись «создаётся» — до helper; после успеха флаг снят.
func TestCreateRecordBeforeHelper(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	p, err := e.s.Plan(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	var during PoolRecord
	var derr error
	e.h.hook = func(op string) {
		if op == OpCreate {
			during, derr = e.s.getRecord(ctx, "p1")
		}
	}
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align))
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	if done := e.wait(t, tk.ID); done.State != tasks.Succeeded {
		t.Fatalf("задача %+v", done)
	}
	if derr != nil || !during.Creating {
		t.Fatalf("запись во время helper: %+v %v", during, derr)
	}
	if rec, err := e.s.getRecord(ctx, "p1"); err != nil || rec.Creating {
		t.Fatalf("запись после: %+v %v", rec, err)
	}
}

// Ошибка helper create — откат по дискам записи, записи нет, failed.
func TestCreateHelperErrorRollsBack(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	p, err := e.s.Plan(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	e.h.fail = map[string]error{OpCreate: errors.New("mdadm упал")}
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, tk.ID)
	if done.State != tasks.Failed || done.Error == nil || !strings.Contains(done.Error.Message, "выполнен откат") {
		t.Fatalf("задача %+v", done)
	}
	if e.h.called(OpCreateRollback) != 1 || !strings.Contains(string(e.h.params[OpCreateRollback]), ref102.ID) {
		t.Fatalf("вызовы %v %s", e.h.calls, e.h.params[OpCreateRollback])
	}
	if _, err := e.s.getRecord(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("запись после отката: %v", err)
	}
}

func seedCreating(t *testing.T, e *svcEnv) {
	t.Helper()
	rec := PoolRecord{Name: "p1", Type: "shr", Disks: []DiskRef{ref101, ref102}, InitialSync: true, Creating: true,
		Arrays: []ArraySpec{{Level: LevelRAID1, SliceBytes: gib - PartitionReserve, Members: []string{ref101.ID, ref102.ID}}}}
	if err := e.s.saveRecord(context.Background(), rec, true); err != nil {
		t.Fatal(err)
	}
}

// Старт gamayund с записью «создаётся»: три исхода.
func TestAssembleAllCreating(t *testing.T) {
	ctx := context.Background()
	// Полный стек — флаг снят, пул остаётся.
	e := newSvc(t)
	seedCreating(t, e)
	st := okStatus(LevelRAID1, 2, gib-3*Align)
	st.Mounted, st.LVSizeBytes = true, gib/2
	e.h.setStatus(st)
	if err := e.s.AssembleAll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if rec, err := e.s.getRecord(ctx, "p1"); err != nil || rec.Creating || e.h.called(OpCreateRollback) != 0 {
		t.Fatalf("%+v %v %v", rec, err, e.h.calls)
	}
	// Тома данных нет — откат, записи нет, причина в ошибке.
	e = newSvc(t)
	seedCreating(t, e)
	e.h.setStatus(PoolStatus{Pool: "p1", Health: HealthFailed})
	err := e.s.AssembleAll(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "выполнен откат") || e.h.called(OpCreateRollback) != 1 {
		t.Fatalf("%v %v", err, e.h.calls)
	}
	if _, err := e.s.getRecord(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("запись: %v", err)
	}
	// Том есть, не смонтирован — данные возможны: ничего не делать.
	e = newSvc(t)
	seedCreating(t, e)
	e.h.setStatus(PoolStatus{Pool: "p1", Health: HealthFailed, LVSizeBytes: gib / 2})
	err = e.s.AssembleAll(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "автоматический откат не выполняется") || e.h.called(OpCreateRollback) != 0 || e.h.called(OpDestroy) != 0 {
		t.Fatalf("%v %v", err, e.h.calls)
	}
	rec, err := e.s.getRecord(ctx, "p1")
	if err != nil || !rec.Creating {
		t.Fatalf("%+v %v", rec, err)
	}
	if st, _ := e.s.status(ctx, "p1"); st.Health != HealthFailed || !strings.Contains(strings.Join(st.Issues, ";"), "создание пула прервано") {
		t.Fatalf("состояние %+v", st)
	}
}

// Диски записи «создаётся» не свободны; удаление такого пула — откат.
func TestCreatingDisksClaimedAndDestroy(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedCreating(t, e) // снимок: диски ещё без разметки (unknown)
	req := createReq
	req.Pool = "p2"
	if _, err := e.s.Plan(ctx, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("ожидался конфликт: %v", err)
	}
	tk, err := e.s.Delete(ctx, "p1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if done := e.wait(t, tk.ID); done.State != tasks.Succeeded {
		t.Fatalf("задача %+v", done)
	}
	if e.h.called(OpCreateRollback) != 1 || e.h.called(OpDestroy) != 0 {
		t.Fatalf("вызовы %v", e.h.calls)
	}
}

// refusedErr — ответ helper refused/invalid_params (helper.RemoteError).
type refusedErr struct{}

func (refusedErr) Error() string {
	return "gamayun-helper: refused: на диске sdb есть сигнатуры"
}
func (refusedErr) RefusedBeforeWrite() bool { return true }

// Отказ helper до записи — запись пула удалена сразу, откат по
// дискам не вызывается (он бы тоже отказал), задача failed с причиной.
func TestCreateRefusedDropsRecordWithoutRollback(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	p, err := e.s.Plan(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	e.h.fail = map[string]error{OpCreate: refusedErr{}, OpCreateRollback: errors.New("не должен вызываться")}
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, tk.ID)
	if done.State != tasks.Failed || done.Error == nil ||
		!strings.Contains(done.Error.Message, "диски не тронуты") || !strings.Contains(done.Error.Message, "сигнатуры") {
		t.Fatalf("задача %+v %+v", done, done.Error)
	}
	if n := e.h.called(OpCreateRollback); n != 0 {
		t.Fatalf("откат вызван %d раз", n)
	}
	if _, err := e.s.getRecord(ctx, "p1"); err == nil {
		t.Fatal("запись пула осталась")
	}
}
