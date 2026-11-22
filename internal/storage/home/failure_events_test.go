package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/tasks"
)

// Участник sdc пропал посреди синхронизации и вернулся;
// отображение dm-integrity dm-1 осталось на исчезнувшем устройстве (среди
// slaves нет sdc1), в md оно faulty. Сборка убирает его из md, закрывает,
// открывает заново на sdc1 и возвращает mdadm --re-add.
func TestAssembleReplacesStaleIntegrity(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.part("sdc", 1)
	n0, n1 := IntegrityName("p1", 0, ref101.ID), IntegrityName("p1", 0, ref102.ID)
	h.virt("dm-0", "sdb1")
	h.virt("dm-1") // slaves пуст: прежнее устройство удалено ядром
	os.MkdirAll(h.p("dev/mapper"), 0o755)
	h.link("../dm-0", "dev/mapper/"+n0)
	h.link("../dm-1", "dev/mapper/"+n1)
	h.virt("md127", "dm-0")
	h.link("../md127", "dev/md/p1-0")
	h.write("proc/mdstat", `Personalities : [raid1]
md127 : active raid1 dm-1[1](F) dm-0[0]
      1045504 blocks super 1.2 [2/1] [U_]
      bitmap: 0/1 pages [0KB], 65536KB chunk

unused devices: <none>
`)
	h.vgs = []string{"dn_p1"}
	mountPool(h, "rw")
	p := AssembleParams{Pool: "p1", Integrity: true, IntegrityMode: IntegrityBitmap,
		Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
	res, err := h.e.Assemble(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	got := writes(h.commands())
	md, dm1 := h.p("dev/md127"), h.p("dev/dm-1")
	want := []string{
		"mdadm " + md + " --fail " + dm1,
		"mdadm " + md + " --remove " + dm1,
		"dmsetup remove " + n1,
		"integritysetup open --integrity crc32c --integrity-bitmap-mode " + h.p("dev/sdc1") + " " + n1,
		"mdadm --re-add " + h.p("dev/md/p1-0") + " " + h.p("dev/mapper/"+n1),
	}
	k := 0
	all := strings.Join(h.commands(), "\n")
	for _, w := range want {
		for k < len(h.commands()) && h.commands()[k] != w {
			k++
		}
		if k == len(h.commands()) {
			t.Fatalf("нет или не по порядку %q:\n%s", w, all)
		}
	}
	if len(res.Readded) != 1 || len(res.Failed) != 0 {
		t.Fatalf("%+v", res)
	}
	// Исправное отображение sdb (dm-0 на sdb1) не тронуто.
	for _, c := range got {
		if strings.Contains(c, n0) && !strings.HasPrefix(c, "mdadm --assemble") {
			t.Fatalf("тронуто исправное отображение: %s", c)
		}
	}
}

// Отображение на текущем разделе — ничего не убирается.
func TestAssembleKeepsLiveIntegrity(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.part("sdc", 1)
	n0, n1 := IntegrityName("p1", 0, ref101.ID), IntegrityName("p1", 0, ref102.ID)
	h.virt("dm-0", "sdb1")
	h.virt("dm-1", "sdc1")
	os.MkdirAll(h.p("dev/mapper"), 0o755)
	h.link("../dm-0", "dev/mapper/"+n0)
	h.link("../dm-1", "dev/mapper/"+n1)
	h.virt("md127", "dm-0", "dm-1")
	h.link("../md127", "dev/md/p1-0")
	h.write("proc/mdstat", "Personalities : [raid1]\nmd127 : active raid1 dm-1[1] dm-0[0]\n      1045504 blocks super 1.2 [2/2] [UU]\n\nunused devices: <none>\n")
	h.vgs = []string{"dn_p1"}
	mountPool(h, "rw")
	p := AssembleParams{Pool: "p1", Integrity: true, IntegrityMode: IntegrityBitmap,
		Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
	if _, err := h.e.Assemble(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if w := writes(h.commands()); len(w) != 0 {
		t.Fatalf("%v", w)
	}
}

// degradedIdle — массив без участника, синхронизация не идёт.
func degradedIdle(level string, n int, size int64) PoolStatus {
	st := okStatus(level, n, size)
	st.Health = HealthDegraded
	st.Arrays[0].Missing, st.Arrays[0].ActiveDevices = 1, n-1
	return st
}

// Create: участник выпал при начальной синхронизации и не вернулся —
// одна попытка сборки, ожидание DegradedWait, откат (destroy, запись
// удалена) и failed с причиной; задача не висит.
func TestCreateRollbackOnStalledDegraded(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 30 * time.Millisecond
	ctx := context.Background()
	p, err := e.s.Plan(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(degradedIdle(LevelRAID1, 2, gib-3*Align))
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	done := e.wait(t, tk.ID)
	if done.State != tasks.Failed || !(done.Error != nil && strings.Contains(done.Error.Message, "откат")) || !(done.Error != nil && strings.Contains(done.Error.Message, "не вернулся")) {
		t.Fatalf("задача %+v", done)
	}
	if e.h.called(OpAssemble) != 1 || e.h.called(OpCreateRollback) != 1 || e.h.called(OpDestroy) != 0 {
		t.Fatalf("вызовы %v", e.h.calls)
	}
	if _, err := e.s.getRecord(ctx, "p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("запись после отката: %v", err)
	}
}

// Участник выпал, синхронизации нет; после окна без продвижения —
// попытка сборки, участник вернулся, recovery, задача завершается успешно.
func TestWaitIdleDegradedRecovers(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 50 * time.Millisecond
	seedPool(t, e, "shr", false)
	rec := degradedIdle(LevelRAID1, 2, gib-3*Align)
	rec.Arrays[0].Action, rec.Health = "recovery", HealthRebuilding
	e.h.setStatus(degradedIdle(LevelRAID1, 2, gib-3*Align))
	e.h.hook = func(op string) {
		if op == OpAssemble {
			e.h.statuses = []PoolStatus{rec, okStatus(LevelRAID1, 2, gib-3*Align)}
		}
	}
	if _, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, nil); err != nil {
		t.Fatal(err)
	}
	if e.h.called(OpAssemble) != 1 {
		t.Fatalf("вызовы %v", e.h.calls)
	}
}

// Add_disk: участник выпал при перестройке — failed с причиной, пул
// заблокирован (GrowPending): новые add_disk/expand отклоняются, Resume
// доступен; AssembleAll не ставит задачу роста, а сообщает причину.
func TestAddDiskStalledBlocksPool(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 30 * time.Millisecond
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align))
	p, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(degradedIdle(LevelRAID5, 3, gib-3*Align))
	done := e.wait(t, tk.ID)
	if done.State != tasks.Failed || !(done.Error != nil && strings.Contains(done.Error.Message, "заблокирован")) {
		t.Fatalf("задача %+v", done)
	}
	if e.h.called(OpDestroy) != 0 || e.h.called(OpGrow) != 0 {
		t.Fatalf("вызовы %v", e.h.calls)
	}
	rec, _ := e.s.getRecord(ctx, "p1")
	if !rec.GrowPending {
		t.Fatalf("запись %+v", rec)
	}
	e.h.setStatus(okStatus(LevelRAID5, 3, 2*(gib-3*Align)))
	if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref104.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("add_disk в заблокированный пул: %v", err)
	}
	e.h.setStatus(degradedIdle(LevelRAID5, 3, gib-3*Align))
	grows := e.h.called(OpGrow)
	err = e.s.AssembleAll(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "рост не поставлен") {
		t.Fatalf("AssembleAll: %v", err)
	}
	if b := e.s.Busy("p1"); b != "" {
		t.Fatalf("поставлена задача %s", b)
	}
	if e.h.called(OpGrow) != grows {
		t.Fatal("рост выполнен")
	}
}

// Массив есть, группы томов нет (сброс посреди удаления) — failed.
func TestStatusArraysWithoutVG(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	h.vgs = nil
	h.e.Runner = runnerFunc(func(ctx context.Context, to time.Duration, path string, args ...string) (CmdResult, error) {
		if filepath.Base(path) == "mdadm" && args[0] == "--detail" {
			return CmdResult{Stdout: []byte(sample(t, "detail-raid1-bitmap.txt"))}, nil
		}
		return h.Run(ctx, to, path, args...)
	})
	st, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Health != HealthFailed || !strings.Contains(strings.Join(st.Issues, ";"), "группа томов dn_p1 не найдена") {
		t.Fatalf("%s %v", st.Health, st.Issues)
	}
}
