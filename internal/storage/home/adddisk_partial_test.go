package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Пул из двух массивов (SHR), диск ref103 ожидается в обоих.
func twoArrayPending(r PoolRecord) PoolRecord {
	s := gib - PartitionReserve
	r.Arrays = []ArraySpec{
		{Level: LevelRAID1, SliceBytes: s / 2, Members: []string{ref101.ID, ref102.ID}},
		{Level: LevelRAID1, SliceBytes: s / 2, Members: []string{ref101.ID, ref102.ID}},
	}
	r.Pending = &PoolPending{Disk: ref103, Type: "shr",
		Arrays: []ArraySpec{
			{Level: LevelRAID5, SliceBytes: s / 2, Members: []string{ref101.ID, ref102.ID, ref103.ID}},
			{Level: LevelRAID5, SliceBytes: s / 2, Members: []string{ref101.ID, ref102.ID, ref103.ID}},
		},
		Expand: []ExpandArray{
			{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: s / 2},
			{Index: 1, Level: LevelRAID5, RaidDevices: 3, SliceBytes: s / 2},
		}}
	return r
}

// Диск принят в массив 0, но не в 1 — ожидание остаётся для 1,
// задача дозавершения добавляет диск только в массив 1.
func TestResolvePartialKeepsPending(t *testing.T) {
	r := twoArrayPending(PoolRecord{Name: "p1", Type: "shr", Disks: []DiskRef{ref101, ref102}})
	got := r.resolve([]int{0})
	if !hasMember(got.Arrays[0].Members, ref103.ID) || hasMember(got.Arrays[1].Members, ref103.ID) {
		t.Fatalf("массивы: %+v", got.Arrays)
	}
	if got.Pending == nil || len(got.Pending.Expand) != 1 || got.Pending.Expand[0].Index != 1 || len(got.Disks) != 3 {
		t.Fatalf("ожидание/диски: %+v %+v", got.Pending, got.Disks)
	}
	tp, ok := got.resumeTask()
	if !ok || !tp.AddDisk.Resume || len(tp.AddDisk.Arrays) != 1 || tp.AddDisk.Arrays[0].Index != 1 ||
		tp.Record.Pending != nil || !hasMember(tp.Record.Arrays[1].Members, ref103.ID) || len(tp.Record.Disks) != 3 {
		t.Fatalf("задача дозавершения: %+v", tp)
	}
	// Повторная сверка без принятия: диск уже в пуле — ожидание не снимается.
	again := got.resolve(nil)
	if again.Pending == nil || len(again.Disks) != 3 {
		t.Fatalf("повторная сверка: %+v", again)
	}
	// Дозавершено: ожидание снято, диск не задвоен, тип целевой.
	done := got.resolve([]int{1})
	if done.Pending != nil || len(done.Disks) != 3 || !hasMember(done.Arrays[1].Members, ref103.ID) {
		t.Fatalf("дозавершение: %+v", done)
	}
	if _, ok := done.resumeTask(); ok {
		t.Fatal("лишняя задача дозавершения")
	}
	// Полный откат (диск не в пуле).
	if rb := r.resolve(nil); rb.Pending != nil || len(rb.Disks) != 2 {
		t.Fatalf("откат: %+v", rb)
	}
}

// Ошибка helper add_disk без сброса: сверка сразу, диск не принят — откат.
func TestAddDiskHelperErrorResolves(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align))
	pl, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.s.mu.Lock()
	tp := e.s.plans[pl.PlanID].task
	e.s.mu.Unlock()
	e.h.fail = map[string]error{OpAddDisk: errors.New("сбой helper")}
	e.h.assemble = &AssembleResult{}
	raw, _ := json.Marshal(tp)
	if _, err := e.s.runAddDisk(ctx, raw, func(int) {}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if e.h.called(OpAssemble) != 1 {
		t.Fatalf("сверка не вызвана: %d", e.h.called(OpAssemble))
	}
	if r, _ := e.s.getRecord(ctx, "p1"); r.Pending != nil || len(r.Disks) != 2 {
		t.Fatalf("после отката: %+v", r)
	}
}

// Сбой самого дозавершения не ставит новую задачу (нет цикла).
func TestResumeFailureNoLoop(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	rec, _ := e.s.getRecord(ctx, "p1")
	rec = twoArrayPending(rec).resolve([]int{0})
	if err := e.s.saveRecord(ctx, rec, false); err != nil {
		t.Fatal(err)
	}
	tp, ok := rec.resumeTask()
	if !ok {
		t.Fatal("нет задачи")
	}
	e.h.fail = map[string]error{OpAddDisk: errors.New("раздел занят")}
	e.h.assemble = &AssembleResult{}
	raw, _ := json.Marshal(tp)
	if _, err := e.s.runAddDisk(ctx, raw, func(int) {}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	time.Sleep(200 * time.Millisecond)
	if n := e.h.called(OpAddDisk); n != 1 {
		t.Fatalf("add_disk вызван %d раз", n)
	}
	if r, _ := e.s.getRecord(ctx, "p1"); r.Pending == nil || len(r.Pending.Expand) != 1 {
		t.Fatalf("ожидание потеряно: %+v", r.Pending)
	}
}

// Helper: дозавершение переиспользует раздел, разметки нет.
func TestAddDiskResumeCommands(t *testing.T) {
	slice := gib - PartitionReserve
	p := AddDiskParams{Pool: "p1", Disk: ref103, Resume: true,
		Arrays: []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: slice}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	setup := func() *fakeHost {
		h := newHost(t)
		existingPool(h, mdstatRAID1, "sdb", "sdc")
		h.part("sdd", 1)
		h.write("sys/devices/sdd/sdd1/size", fmt.Sprint(slice/512))
		return h
	}
	h := setup()
	if _, err := h.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	got := strings.Join(writes(h.commands()), "\n")
	want := strings.Join([]string{
		"mdadm " + dev + "/md/p1-0 --add " + dev + "/sdd1",
		"mdadm --grow " + dev + "/md/p1-0 --level=5",
		"mdadm --grow " + dev + "/md/p1-0 --raid-devices=3",
	}, "\n")
	if got != want {
		t.Fatalf("команды:\n%s", got)
	}

	refusals := map[string]func(h *fakeHost){
		"раздел занят":       func(h *fakeHost) { h.virt("md9", "sdd1") },
		"нет раздела":        func(h *fakeHost) { h.write("sys/devices/sdd/sdd1/partition", "2") },
		"размер раздела":     func(h *fakeHost) { h.write("sys/devices/sdd/sdd1/size", "100") },
		"диск вне allowlist": func(h *fakeHost) { h.e.AllowIDs = []string{ref101.ID, ref102.ID} },
	}
	for name, f := range refusals {
		t.Run(name, func(t *testing.T) {
			h := setup()
			f(h)
			if _, err := h.e.AddDisk(context.Background(), p); !errors.Is(err, ErrRefused) {
				t.Fatalf("ожидался отказ: %v", err)
			}
			if w := writes(h.commands()); len(w) != 0 {
				t.Fatalf("записи при отказе: %v", w)
			}
		})
	}
}

// Сборка после сброса между массивами ставит задачу дозавершения.
func TestAssembleAllSubmitsResume(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	rec, _ := e.s.getRecord(ctx, "p1")
	if err := e.s.saveRecord(ctx, twoArrayPending(rec), false); err != nil {
		t.Fatal(err)
	}
	e.h.assemble = &AssembleResult{PendingAccepted: []int{0}}
	e.h.setStatus(okStatus(LevelRAID5, 3, 2*gib))
	if err := e.s.AssembleAll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for e.h.called(OpAddDisk) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	var p AddDiskParams
	e.h.mu.Lock()
	json.Unmarshal(e.h.params[OpAddDisk], &p)
	e.h.mu.Unlock()
	if !p.Resume || len(p.Arrays) != 1 || p.Arrays[0].Index != 1 || p.Disk.ID != ref103.ID {
		t.Fatalf("дозавершение: %+v", p)
	}
}

// Чужая метка или занятый раздел — диск не трогается, причина в PendingErrors.
func TestReleasePendingRefusals(t *testing.T) {
	for name, f := range map[string]func(h *fakeHost){
		"чужая метка":   func(h *fakeHost) { h.write("sys/devices/sdd/sdd1/uevent", "PARTNAME=data\n") },
		"занят":         func(h *fakeHost) { h.virt("dm-9", "sdd1") },
		"вне allowlist": func(h *fakeHost) { h.e.AllowIDs = []string{ref101.ID, ref102.ID} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.part("sdb", 1)
			h.part("sdc", 1)
			h.part("sdd", 1)
			h.write("sys/devices/sdd/sdd1/uevent", "PARTNAME=dn-p1-0\n")
			h.examine = map[string]string{h.p("dev/sdb1"): uuidA, h.p("dev/sdc1"): uuidA}
			h.write("proc/mdstat", mdstatRAID1)
			f(h)
			res, _ := h.e.Assemble(context.Background(), pendingParams())
			got := strings.Join(writes(h.commands()), "\n")
			if res.PendingReleased || strings.Contains(got, "/sdd") && (strings.Contains(got, "wipefs") || strings.Contains(got, "sgdisk")) {
				t.Fatalf("диск тронут:\n%s", got)
			}
			if name != "вне allowlist" && len(res.PendingErrors) == 0 {
				t.Fatalf("нет причины: %+v", res)
			}
		})
	}
}

// Массив из записи не найден — health failed, а не ok.
func TestCheckExpectedArrays(t *testing.T) {
	rec := PoolRecord{Arrays: []ArraySpec{{Level: LevelRAID1}, {Level: LevelSingle}}}
	st := PoolStatus{Health: HealthOK}
	checkExpectedArrays(&st, rec)
	if st.Health != HealthFailed || len(st.Issues) != 1 {
		t.Fatalf("%+v", st)
	}
	st = PoolStatus{Health: HealthOK, Arrays: []ArrayStatus{{Index: 0}}}
	checkExpectedArrays(&st, rec)
	if st.Health != HealthOK {
		t.Fatalf("%+v", st)
	}
}

// Массив, собранный как чужой (<хост>:<пул>-<i>), находится.
func TestArraysHomehostName(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.virt("md127", "sdb1")
	h.link("../md127", "dev/md/otherhost:p1-0")
	h.virt("md126")
	h.link("../md126", "dev/md/p10-0")
	arrs, err := h.e.arrays(context.Background(), "p1")
	if err != nil || len(arrs) != 1 || arrs[0].index != 0 || arrs[0].kname != "md127" || arrs[0].node != h.p("dev/md127") {
		t.Fatalf("%+v %v", arrs, err)
	}
}

// Над пулом одна задача за раз; после окончания пул свободен.
func TestPoolBusyLock(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align))
	release := make(chan struct{})
	e.h.hook = func(op string) {
		if op == OpAddDisk {
			<-release
		}
	}
	pl, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID})
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.s.Confirm(ctx, pl.PlanID, pl.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	if b := e.s.Busy("p1"); b != "добавление диска" {
		t.Fatalf("Busy: %q", b)
	}
	if _, err := e.s.Delete(ctx, "p1", "p1"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "занят") {
		t.Fatalf("удаление во время add_disk: %v", err)
	}
	e.h.setStatus(okStatus(LevelRAID5, 3, 2*gib))
	e.h.fail = map[string]error{OpGrow: errors.New("стоп")}
	close(release)
	e.wait(t, task.ID)
	if b := e.s.Busy("p1"); b != "" {
		t.Fatalf("пул не освобождён: %q", b)
	}
	if _, err := e.s.Delete(ctx, "p1", "p1"); err != nil {
		t.Fatalf("удаление после окончания: %v", err)
	}
}
