package home

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func assembleParams(integrity bool, mode string) AssembleParams {
	return AssembleParams{Pool: "p1", Integrity: integrity, IntegrityMode: mode,
		Arrays: []ArraySpec{{Level: LevelRAID5, SliceBytes: gib - PartitionReserve, Members: []string{ref101.ID, ref102.ID, ref103.ID}}}}
}

// После загрузки: разделы есть, слоёв нет — открыть integrity в режиме
// пула без пересчёта, собрать md, активировать VG, смонтировать.
func TestAssembleFromBoot(t *testing.T) {
	for _, mode := range []string{IntegrityBitmap, IntegrityJournal} {
		h := newHost(t)
		for _, d := range []string{"sdb", "sdc", "sdd"} {
			h.part(d, 1)
		}
		res, err := h.e.Assemble(context.Background(), assembleParams(true, mode))
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(writes(h.commands()), "\n")
		if strings.Contains(got, "--integrity-recalculate") || strings.Contains(got, "format") {
			t.Fatalf("%s: повторное открытие с пересчётом/форматом:\n%s", mode, got)
		}
		if (mode == IntegrityBitmap) != strings.Contains(got, "--integrity-bitmap-mode") {
			t.Fatalf("%s: режим не из записи:\n%s", mode, got)
		}
		want := []string{
			"integritysetup open --integrity crc32c",
			"mdadm --assemble --run " + h.p("dev/md/p1-0") + " " + h.p("dev/mapper/"+IntegrityName("p1", 0, ref101.ID)),
			"lvm vgchange -q -ay dn_p1",
			"mount -t btrfs " + h.p("dev/dn_p1/data") + " " + h.p("mnt/p1"),
		}
		k := 0
		lines := strings.Split(got, "\n")
		for _, w := range want {
			for k < len(lines) && !strings.HasPrefix(lines[k], w) {
				k++
			}
			if k == len(lines) {
				t.Fatalf("%s: нет или не по порядку %q:\n%s", mode, w, got)
			}
		}
		if strings.Count(got, "integritysetup open") != 3 || res.Mount != h.p("mnt/p1") || len(res.MissingDisks) != 0 {
			t.Fatalf("%s: %+v\n%s", mode, res, got)
		}
	}
}

// Всё уже поднято (udev собрал, пул смонтирован) — ни одной записи.
func TestAssembleIdempotent(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	mountPool(h, "rw")
	p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
	if _, err := h.e.Assemble(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if w := writes(h.commands()); len(w) != 0 {
		t.Fatalf("лишние команды: %v", w)
	}
}

// Собранный массив в auto-read-only (resync/reshape PENDING) — --readwrite;
// в точке пула чужой том — отказ.
func TestAssembleReadwriteAndForeignMount(t *testing.T) {
	h := newHost(t)
	existingPool(h, strings.Replace(mdstatRAID1, "active raid1", "active (auto-read-only) raid1", 1), "sdb", "sdc")
	p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
	h.write("dev/dn_p1/data", "")
	h.write("proc/mountinfo", "40 22 0:50 / "+h.p("mnt/p1")+" rw - btrfs "+h.p("dev/sdd")+" rw\n")
	_, err := h.e.Assemble(context.Background(), p)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("чужой том: %v", err)
	}
	if w := writes(h.commands()); len(w) != 1 || w[0] != "mdadm --readwrite "+h.p("dev/md/p1-0") {
		t.Fatalf("%v", w)
	}
}

// Неактивный массив (неполная инкрементальная сборка) — mdadm --run.
func TestAssembleRunsInactive(t *testing.T) {
	h := newHost(t)
	existingPool(h, "Personalities : [raid1]\nmd127 : inactive sdb1[0](S)\n      1045504 blocks super 1.2\n\nunused devices: <none>\n", "sdb", "sdc")
	mountPool(h, "rw")
	p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}}}
	h.e.Assemble(context.Background(), p)
	if !strings.Contains(strings.Join(h.commands(), "\n"), "mdadm --run "+h.p("dev/md/p1-0")) {
		t.Fatalf("%v", h.commands())
	}
}

// Отсутствующий диск — сборка деградированного массива, диск в отчёте.
func TestAssembleDegraded(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.part("sdc", 1) // у sdd раздела нет (диск заменён/пуст)
	p := assembleParams(false, "")
	res, err := h.e.Assemble(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.MissingDisks) != 1 || res.MissingDisks[0] != ref103.ID {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(strings.Join(h.commands(), "\n"), "mdadm --assemble --run "+h.p("dev/md/p1-0")+" "+h.p("dev/sdb1")+" "+h.p("dev/sdc1")) {
		t.Fatalf("%v", h.commands())
	}
}

func TestAssembleStopsOnFailure(t *testing.T) {
	h := newHost(t)
	for _, d := range []string{"sdb", "sdc", "sdd"} {
		h.part(d, 1)
	}
	h.failOn = "--assemble"
	if _, err := h.e.Assemble(context.Background(), assembleParams(true, IntegrityBitmap)); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if strings.Contains(strings.Join(h.commands(), "\n"), "vgchange") {
		t.Fatal("продолжено после сбоя")
	}
}

func TestAssembleParamsValidate(t *testing.T) {
	bad := []AssembleParams{
		{Pool: "p1"},
		{Pool: "../x", Arrays: assembleParams(false, "").Arrays},
		{Pool: "p1", Integrity: true, Arrays: assembleParams(false, "").Arrays},
		{Pool: "p1", IntegrityMode: IntegrityBitmap, Arrays: assembleParams(false, "").Arrays},
		{Pool: "p1", Arrays: []ArraySpec{{Level: "raid9", Members: []string{ref101.ID}}}},
		{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{"/dev/sda"}}}},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("%d: принято %+v", i, p)
		}
	}
	if err := assembleParams(true, IntegrityBitmap).Validate(); err != nil {
		t.Fatal(err)
	}
}

// helper, который поднимается не сразу.
type lateHelper struct {
	fakeHelper
	down int
}

var errDown = errors.New("недоступен")

func (l *lateHelper) Call(ctx context.Context, op string, params, result any) error {
	if op == OpAssemble && l.down > 0 {
		l.down--
		return errDown
	}
	return l.fakeHelper.Call(ctx, op, params, result)
}

// gamayund при старте: режим из записи (старый пул без режима — journal),
// повтор, пока helper недоступен.
func TestAssembleAllUsesRecordMode(t *testing.T) {
	e := newSvc(t)
	seedPool(t, e, "raid1", true) // запись без integrity_mode
	lh := &lateHelper{down: 2}
	e.s.helper = lh
	e.s.Unavailable = func(err error) bool { return errors.Is(err, errDown) }
	var logged []string
	if err := e.s.AssembleAll(context.Background(), func(pool string, _ AssembleResult, err error) {
		logged = append(logged, pool)
		if err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	var p AssembleParams
	json.Unmarshal(lh.params[OpAssemble], &p)
	if len(logged) != 1 || p.IntegrityMode != LegacyIntegrityMode || !p.Integrity || len(p.Arrays) != 1 {
		t.Fatalf("%v %+v", logged, p)
	}
}

// Сброс во время reshape после add_disk — задача add_disk стала
// interrupted; при старте ставится задача роста, она ждёт конца reshape и
// вызывает home.pool.grow. Без reshape задача не ставится.
func TestAssembleAllResumesGrowAfterReshape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reshape bool
	}{{"reshape", true}, {"idle", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSvc(t)
			seedPool(t, e, "raid5", true)
			busy := okStatus(LevelRAID5, 4, 3*gib)
			if tc.reshape {
				busy.Arrays[0].Action = "reshape"
			}
			e.h.setStatus(busy, busy, okStatus(LevelRAID5, 4, 3*gib))
			if err := e.s.AssembleAll(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for e.h.called(OpGrow) == 0 && tc.reshape && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			want := 0
			if tc.reshape {
				want = 1
			}
			if got := e.h.called(OpGrow); got != want {
				t.Fatalf("home.pool.grow вызван %d раз, ожидалось %d: %v", got, want, e.h.calls)
			}
		})
	}
}

// udev начал инкрементальную сборку и не дождался всех
// участников — неактивный массив держит устройства. Его останавливают до
// --assemble; чужие неактивные массивы не трогают.
func TestAssembleStopsPartialIncremental(t *testing.T) {
	h := newHost(t)
	for _, d := range []string{"sdb", "sdc", "sdd"} {
		h.part(d, 1)
	}
	n0 := IntegrityName("p1", 0, ref101.ID)
	h.write("proc/mdstat", "Personalities : [raid5]\nmd127 : inactive "+n0+"[0](S)\n      1000 blocks super 1.2\n\n"+
		"md126 : inactive sdz1[0](S)\n      1000 blocks super 1.2\n\nunused devices: <none>\n")
	if _, err := h.e.Assemble(context.Background(), assembleParams(true, IntegrityBitmap)); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	stop := strings.Index(got, "mdadm --stop "+h.p("dev/md127"))
	asm := strings.Index(got, "mdadm --assemble")
	if stop < 0 || asm < 0 || stop > asm || strings.Contains(got, "md126") {
		t.Fatalf("ожидалась остановка md127 до сборки:\n%s", got)
	}
}

// Суперблок dm-integrity одного участника испорчен —
// integritysetup open падает, пул собирается деградированным без него.
func TestAssembleSkipsBrokenIntegrityMember(t *testing.T) {
	h := newHost(t)
	for _, d := range []string{"sdb", "sdc", "sdd"} {
		h.part(d, 1)
	}
	h.failOn = "integritysetup open --integrity crc32c --integrity-bitmap-mode " + h.p("dev/sdc1")
	res, err := h.e.Assemble(context.Background(), assembleParams(true, IntegrityBitmap))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	if len(res.MissingDisks) != 1 || len(res.Failed) != 1 || strings.Contains(got, "mdadm --assemble --run "+h.p("dev/md/p1-0")+" "+h.p("dev/mapper/"+IntegrityName("p1", 0, ref101.ID))+" "+h.p("dev/mapper/"+IntegrityName("p1", 0, ref102.ID))+" ") || !strings.Contains(got, "mount -t btrfs") {
		t.Fatalf("%+v\n%s", res, got)
	}
}
