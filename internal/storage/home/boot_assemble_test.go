package home

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mdstatRAID1Degraded = `Personalities : [raid1]
md127 : active raid1 sdb1[0]
      1045504 blocks super 1.2 [2/1] [U_]
      bitmap: 0/1 pages [0KB], 65536KB chunk

unused devices: <none>
`

// Диск вернулся (раздел есть, в md его нет) — mdadm --re-add; отказ
// re-add — полный --add; отказ обоих не прерывает сборку и
// попадает в ReaddFailed.
func TestAssembleReAddsReturnedDisk(t *testing.T) {
	for _, tc := range []struct {
		failOn string
		cmds   []string // после md
		ok     bool
		full   bool
	}{
		{"", []string{"--re-add {md} {dev}"}, true, false},
		{"--re-add", []string{"--re-add {md} {dev}", "{md} --add {dev}"}, true, true},
		{"p1-0", []string{"--re-add {md} {dev}", "{md} --add {dev}"}, false, false},
	} {
		h := newHost(t)
		existingPool(h, mdstatRAID1Degraded, "sdb", "sdc")
		mountPool(h, "ro")
		h.failOn = tc.failOn
		p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
		res, err := h.e.Assemble(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, c := range tc.cmds {
			c = strings.ReplaceAll(c, "{md}", h.p("dev/md/p1-0"))
			want = append(want, "mdadm "+strings.ReplaceAll(c, "{dev}", h.p("dev/sdc1")))
		}
		if w := writes(h.commands()); strings.Join(w, "|") != strings.Join(want, "|") {
			t.Fatalf("%q: %v", tc.failOn, w)
		}
		if tc.ok != (len(res.Readded) == 1) || tc.ok == (len(res.ReaddFailed) == 1) || tc.full != (len(res.FullRecovery) == 1) {
			t.Fatalf("%q: %+v", tc.failOn, res)
		}
	}
}

// Участник остался в деградированном массиве запасным (S), recovery
// не идёт (прервался после re-add) — --remove + полный --add. Пока идёт
// recovery — не трогаем.
func TestAssembleReturnsStuckSpare(t *testing.T) {
	stuck := strings.Replace(mdstatRAID1Degraded, "sdb1[0]", "sdc1[2](S) sdb1[0]", 1)
	recovering := strings.Replace(strings.Replace(mdstatRAID1Degraded, "sdb1[0]", "sdc1[2] sdb1[0]", 1),
		"      bitmap:", "      [=>...................]  recovery =  5.0% (52000/1045504) finish=1.0min speed=16000K/sec\n      bitmap:", 1)
	for _, tc := range []struct {
		name, mdstat, failOn string
		want                 []string
		ok                   bool
	}{
		{"stuck", stuck, "", []string{"{md} --remove {dev}", "{md} --add {dev}"}, true},
		{"add-fails", stuck, "--add", []string{"{md} --remove {dev}", "{md} --add {dev}"}, false},
		{"recovering", recovering, "", nil, false},
	} {
		h := newHost(t)
		existingPool(h, tc.mdstat, "sdb", "sdc")
		mountPool(h, "rw")
		h.failOn = tc.failOn
		p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
		res, err := h.e.Assemble(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, c := range tc.want {
			c = strings.ReplaceAll(c, "{md}", h.p("dev/md/p1-0"))
			want = append(want, "mdadm "+strings.ReplaceAll(c, "{dev}", h.p("dev/sdc1")))
		}
		if w := writes(h.commands()); strings.Join(w, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: %v", tc.name, w)
		}
		if tc.ok != (len(res.FullRecovery) == 1) || (tc.name == "add-fails") != (len(res.ReaddFailed) == 1) {
			t.Fatalf("%s: %+v", tc.name, res)
		}
	}
}

// Полный массив — re-add не вызывается (TestAssembleIdempotent), участник
// уже в массиве — тоже.
func TestAssembleNoReAddWhenComplete(t *testing.T) {
	h := newHost(t)
	existingPool(h, strings.Replace(mdstatRAID1Degraded, "sdb1[0]", "sdc1[1] sdb1[0]", 1), "sdb", "sdc")
	mountPool(h, "rw")
	p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
	if _, err := h.e.Assemble(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if w := writes(h.commands()); len(w) != 0 {
		t.Fatalf("%v", w)
	}
}

// Ранний юнит открывает только dm-integrity (без md/LVM/mount);
// отсутствующий файл — не ошибка.
func TestOpenIntegrityFile(t *testing.T) {
	h := newHost(t)
	for _, d := range []string{"sdb", "sdc", "sdd"} {
		h.part(d, 1)
	}
	f := filepath.Join(t.TempDir(), BootFileName)
	if err := OpenIntegrityFile(context.Background(), h.e, f, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]AssembleParams{assembleParams(true, IntegrityBitmap)})
	os.WriteFile(f, raw, 0o644)
	var got []string
	err := OpenIntegrityFile(context.Background(), h.e, f, func(pool string, r AssembleResult, err error) {
		if err != nil {
			t.Error(err)
		}
		got = append(got, pool)
	})
	if err != nil || len(got) != 1 {
		t.Fatal(err, got)
	}
	cmds := strings.Join(writes(h.commands()), "\n")
	if strings.Count(cmds, "integritysetup open --integrity crc32c --integrity-bitmap-mode") != 3 ||
		strings.Contains(cmds, "mdadm") || strings.Contains(cmds, "lvm") || strings.Contains(cmds, "mount") {
		t.Fatalf("%s", cmds)
	}
}

// gamayund пишет состав пулов с integrity в BootFile при записи пула.
func TestBootFileWritten(t *testing.T) {
	e := newSvc(t)
	e.s.BootFile = filepath.Join(t.TempDir(), BootFileName)
	seedPool(t, e, "raid1", true)
	raw, err := os.ReadFile(e.s.BootFile)
	if err != nil {
		t.Fatal(err)
	}
	var ps []AssembleParams
	if err := json.Unmarshal(raw, &ps); err != nil || len(ps) != 1 || ps[0].Pool != "p1" || ps[0].IntegrityMode != LegacyIntegrityMode || ps[0].Validate() != nil {
		t.Fatalf("%v %+v", err, ps)
	}
}

// Деградированный пул — gamayund повторяет сборку; диск вернулся →
// read-only не включается (статус после сборки исправен).
func TestEnforceReassemblesDegraded(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "shr", false)
	ok := okStatus(LevelRAID1, 2, gib)
	ok.Mounted = true
	e.h.setStatus(degraded(false), ok)
	e.h.assemble = &AssembleResult{Result: Result{Pool: "p1"}, Readded: []string{"/dev/mapper/x"}}
	var logged int
	e.s.OnReassemble = func(string, AssembleResult, error) { logged++ }
	if err := e.s.EnforceAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.h.called(OpAssemble) != 1 || e.h.called(OpSetReadOnly) != 0 || logged != 1 {
		t.Fatalf("assemble %d ro %d log %d", e.h.called(OpAssemble), e.h.called(OpSetReadOnly), logged)
	}
	// Диск не вернулся — read-only как раньше.
	e.h.assemble = &AssembleResult{Result: Result{Pool: "p1"}}
	e.h.setStatus(degraded(false))
	if err := e.s.EnforceAll(context.Background()); err != nil || e.h.called(OpSetReadOnly) != 1 {
		t.Fatalf("%v %d", err, e.h.called(OpSetReadOnly))
	}
}

// Д7: начальная синхронизация нового raid5 (деградированный + recovery) —
// не отказ; признак снимается, когда пул исправен. Отказ диска во время
// синхронизации — read-only.
func TestInitialSyncNotDegraded(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	rec := PoolRecord{Name: "p1", Type: "raid5", InitialSync: true, Disks: []DiskRef{ref101, ref102, ref103},
		Arrays: []ArraySpec{{Level: LevelRAID5, SliceBytes: gib - PartitionReserve, Members: []string{ref101.ID, ref102.ID, ref103.ID}}}}
	if err := e.s.saveRecord(ctx, rec, true); err != nil {
		t.Fatal(err)
	}
	sync := okStatus(LevelRAID5, 3, 2*gib)
	sync.Mounted, sync.Health = true, HealthRebuilding
	sync.Arrays[0].Missing, sync.Arrays[0].Action = 1, "recovery"
	sync.Arrays[0].State = []string{"clean", "degraded", "recovering"}
	e.h.setStatus(sync)
	if err := e.s.EnforceAll(ctx); err != nil || e.h.called(OpSetReadOnly) != 0 || e.h.called(OpAssemble) != 0 {
		t.Fatalf("%v ro %d", err, e.h.called(OpSetReadOnly))
	}
	if v, err := e.s.Get(ctx, "p1"); err != nil || v.Status.Health != HealthResyncing || !v.Status.InitialSync {
		t.Fatalf("%v %+v", err, v.Status)
	}
	failed := sync
	failed.Arrays = append([]ArrayStatus{}, sync.Arrays...)
	failed.Arrays[0].FailedDevices, failed.Health = 1, HealthDegraded
	if ex, _ := Exhausted(func() PoolStatus { s := failed; markInitialSync(&s); return s }()); !ex {
		t.Fatal("отказ во время синхронизации не замечен")
	}
	ok := okStatus(LevelRAID5, 3, 2*gib)
	ok.Mounted = true
	e.h.setStatus(ok)
	if err := e.s.EnforceAll(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.s.getRecord(ctx, "p1"); r.InitialSync {
		t.Fatal("признак не снят")
	}
	e.h.setStatus(sync)
	if p, err := e.s.enforcePool(ctx, "p1"); err != nil || !p.WriteBlocked {
		t.Fatalf("после синхронизации тот же статус — отказ: %v %+v", err, p)
	}
}
