package home

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gamayun-nas/internal/tasks"
)

// waitTasks ждёт, пока задач станет не меньше n и все закончатся.
func (e *svcEnv) waitTasks(t *testing.T, n int) []tasks.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ts, err := e.m.List(context.Background(), 50)
		if err != nil {
			t.Fatal(err)
		}
		done := len(ts) >= n
		for _, tk := range ts {
			if tk.State == tasks.Queued || tk.State == tasks.Running {
				done = false
			}
		}
		if done {
			return ts
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("задачи не завершились")
	return nil
}

// Сбой задачи дозавершения замены оставлял ожидание до перезапуска
// gamayund, новая замена — 409. «Продолжить замену» (Resume) подхватывает
// ожидание без перезапуска; повтор и занятый пул — 409, ничего не ломают.
func TestResumeAfterFailedReplaceResume(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", true)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	if _, err := e.s.Resume(ctx, "p1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("нечего продолжать: %v", err)
	}
	if _, err := e.s.Resume(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет пула: %v", err)
	}
	p, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID, OldDiskID: ref101.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Новый диск принят в массив (--add прошёл), --replace падает и в
	// задаче, и в её дозавершении.
	e.h.fail = map[string]error{OpReplace: errors.New("mdadm --replace: сбой")}
	e.h.assemble = &AssembleResult{PendingAccepted: []int{0}}
	if _, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500 && e.h.called(OpReplace) < 2; i++ {
		waitShort()
	}
	for _, tk := range e.waitTasks(t, 2) {
		if tk.State != tasks.Failed {
			t.Fatalf("задача %s: %s", tk.Kind, tk.State)
		}
	}
	rec, _ := e.s.getRecord(ctx, "p1")
	if rec.Pending == nil || !rec.Pending.Replace || rec.Pending.Disk.ID != ref103.ID {
		t.Fatalf("ожидание замены не осталось: %+v", rec.Pending)
	}
	if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID, OldDiskID: ref101.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("новая замена при ожидании: %v", err)
	}

	// Пул занят задачей — 409, ожидание не трогается.
	e.s.mu.Lock()
	e.s.busy["p1"] = TaskReplace
	e.s.mu.Unlock()
	if _, err := e.s.Resume(ctx, "p1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("занятый пул: %v", err)
	}
	e.s.release("p1")

	// Причина сбоя устранена — «продолжить замену».
	e.h.mu.Lock()
	e.h.fail = nil
	e.h.mu.Unlock()
	e.h.setStatus(replacedStatus(ref102.ID, ref103.ID, ref101.ID))
	tk, err := e.s.Resume(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Kind != TaskResume {
		t.Fatalf("вид задачи %s", tk.Kind)
	}
	if _, err := e.s.Resume(ctx, "p1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("повтор во время задачи: %v", err)
	}
	if r := e.wait(t, tk.ID); r.State != tasks.Succeeded {
		t.Fatalf("продолжение: %+v", r)
	}
	var rp ReplaceParams
	json.Unmarshal(e.h.params[OpReplace], &rp)
	if !rp.Resume || rp.Old == nil || rp.Old.ID != ref101.ID || rp.Disk.ID != ref103.ID || len(rp.Arrays) != 1 {
		t.Fatalf("дозавершение %+v", rp)
	}
	if e.h.called(OpReplaceFinish) != 1 {
		t.Fatal("старый диск не выведен")
	}
	rec, _ = e.s.getRecord(ctx, "p1")
	if rec.Pending != nil || rec.hasDisk(ref101.ID) || !rec.hasDisk(ref103.ID) || !hasMember(rec.Arrays[0].Members, ref103.ID) {
		t.Fatalf("запись %+v", rec)
	}
	if e.s.Busy("p1") != "" {
		t.Fatal("пул не освобождён")
	}
	// Идемпотентность: продолжать больше нечего.
	if _, err := e.s.Resume(ctx, "p1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("повтор после успеха: %v", err)
	}
}

// Новый диск не принят ни в один массив — продолжение = откат ожидания.
func TestResumeReplaceNotAccepted(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", true)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	rec, _ := e.s.getRecord(ctx, "p1")
	rec.Pending = &PoolPending{Replace: true, Disk: ref103, Old: &rec.Disks[0], Type: rec.Type, Arrays: rec.Arrays, Disks: rec.Disks,
		Expand: []ExpandArray{{Index: 0, Level: LevelRAID1, RaidDevices: 2, SliceBytes: rec.Arrays[0].SliceBytes}}}
	if err := e.s.saveRecord(ctx, rec, false); err != nil {
		t.Fatal(err)
	}
	e.h.assemble = &AssembleResult{}
	tk, err := e.s.Resume(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.wait(t, tk.ID); r.State != tasks.Succeeded {
		t.Fatalf("%+v", r)
	}
	if e.h.called(OpReplace) != 0 {
		t.Fatal("замена вызвана для непринятого диска")
	}
	rec, _ = e.s.getRecord(ctx, "p1")
	if rec.Pending != nil || !rec.hasDisk(ref101.ID) || rec.hasDisk(ref103.ID) {
		t.Fatalf("откат: %+v", rec)
	}
}

// Прерванное добавление/рост (не замена): принятый диск — запись по
// суперблокам и рост LVM/Btrfs; только grow_pending — рост.
func TestResumeAddDiskAndGrow(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", true)
	rec, _ := e.s.getRecord(ctx, "p1")
	after := []ArraySpec{{Level: LevelRAID1, SliceBytes: rec.Arrays[0].SliceBytes, Members: []string{ref101.ID, ref102.ID, ref103.ID}}}
	rec.Pending = &PoolPending{Disk: ref103, Type: rec.Type, Arrays: after,
		Expand: []ExpandArray{{Index: 0, Level: LevelRAID1, RaidDevices: 3, SliceBytes: rec.Arrays[0].SliceBytes}}}
	if err := e.s.saveRecord(ctx, rec, false); err != nil {
		t.Fatal(err)
	}
	e.h.assemble = &AssembleResult{PendingAccepted: []int{0}}
	e.h.setStatus(okStatus(LevelRAID1, 3, gib))
	tk, err := e.s.Resume(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.wait(t, tk.ID); r.State != tasks.Succeeded {
		t.Fatalf("%+v", r)
	}
	rec, _ = e.s.getRecord(ctx, "p1")
	if rec.Pending != nil || rec.GrowPending || !rec.hasDisk(ref103.ID) || len(rec.Arrays[0].Members) != 3 || e.h.called(OpGrow) != 1 {
		t.Fatalf("запись %+v, рост %d", rec, e.h.called(OpGrow))
	}

	rec.GrowPending = true
	if err := e.s.saveRecord(ctx, rec, false); err != nil {
		t.Fatal(err)
	}
	tk, err = e.s.Resume(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.wait(t, tk.ID); r.State != tasks.Succeeded {
		t.Fatalf("%+v", r)
	}
	rec, _ = e.s.getRecord(ctx, "p1")
	if rec.GrowPending || e.h.called(OpGrow) != 2 {
		t.Fatalf("grow_pending %v, рост %d", rec.GrowPending, e.h.called(OpGrow))
	}
}

// Задача, упавшая с постановкой дозавершения своего пула, не должна
// освобождать пул из-под этого дозавершения: иначе «продолжить замену»
// запустило бы вторую замену параллельно.
func TestFollowUpTaskKeepsPoolBusy(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", true)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	p, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID, OldDiskID: ref101.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.h.assemble = &AssembleResult{PendingAccepted: []int{0}}
	gate := make(chan struct{})
	inResume := make(chan struct{})
	n := 0
	e.h.hook = func(op string) {
		if op != OpReplace {
			return
		}
		if n++; n == 1 {
			e.h.fail = map[string]error{OpReplace: errors.New("сбой")}
			return
		}
		e.h.fail = nil
		close(inResume)
		e.h.mu.Unlock()
		<-gate
		e.h.mu.Lock()
	}
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	<-inResume
	if r := e.wait(t, tk.ID); r.State != tasks.Failed {
		t.Fatalf("первая задача %+v", r)
	}
	busy := e.s.Busy("p1")
	_, rerr := e.s.Resume(ctx, "p1")
	e.h.setStatus(replacedStatus(ref102.ID, ref103.ID, ref101.ID))
	close(gate)
	e.waitTasks(t, 2)
	if busy == "" {
		t.Fatal("пул свободен во время дозавершения")
	}
	if !errors.Is(rerr, ErrConflict) {
		t.Fatalf("продолжение во время дозавершения: %v", rerr)
	}
	if e.s.Busy("p1") != "" {
		t.Fatal("пул не освобождён после дозавершения")
	}
}
