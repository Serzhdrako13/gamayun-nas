package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
)

// recalcStatus — raid1 с идущим пересчётом тегов на одном отображении.
func recalcStatus(pos int64, lost bool) PoolStatus {
	st := okStatus(LevelRAID1, 2, gib-3*Align)
	st.Health = HealthResyncing
	p := pos
	st.Integrity = []IntegrityStatus{{Name: "dn-p1-0-a", ProvidedBytes: gib, Recalculating: true, RecalcBytes: &p, Lost: lost}}
	if lost {
		st.Integrity[0].LostWhy = "нет нижнего устройства"
	}
	return st
}

// Пересчёт тегов на пропавшем диске — задача не висит: окно, попытка
// сборки, второе окно, ошибка с причиной; рост счётчика на мёртвом
// устройстве не продлевает ожидание.
func TestWaitIdleRecalcOnLostDisk(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 40 * time.Millisecond
	seedPool(t, e, "shr", true)
	e.h.setStatus(recalcStatus(10, false), recalcStatus(20, true), recalcStatus(30, true), recalcStatus(40, true))
	start := time.Now()
	_, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, nil)
	if err == nil || !strings.Contains(err.Error(), "нет продвижения") || !strings.Contains(err.Error(), "нет нижнего устройства") {
		t.Fatalf("ошибка %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("ожидание %s", time.Since(start))
	}
	if e.h.called(OpAssemble) != 1 {
		t.Fatalf("вызовы %v", e.h.calls)
	}
}

// Пересчёт на живом устройстве идёт дольше окна — продвижение доказано,
// ошибки нет.
func TestWaitIdleRecalcProgressOutlivesWindow(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 30 * time.Millisecond
	seedPool(t, e, "shr", true)
	var seq []PoolStatus
	for i := int64(1); i <= 40; i++ { // 40 опросов × 5 мс > окна
		seq = append(seq, recalcStatus(i*1000, false))
	}
	seq = append(seq, okStatus(LevelRAID1, 2, gib-3*Align))
	e.h.setStatus(seq...)
	if _, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, nil); err != nil {
		t.Fatal(err)
	}
	if e.h.called(OpAssemble) != 0 {
		t.Fatalf("вызовы %v", e.h.calls)
	}
}

// Счётчик стоит на живом устройстве (зависший процесс) — тоже ошибка.
func TestWaitIdleRecalcStuck(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 30 * time.Millisecond
	seedPool(t, e, "shr", true)
	e.h.setStatus(recalcStatus(500, false))
	_, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, nil)
	if err == nil || !strings.Contains(err.Error(), "нет продвижения") {
		t.Fatalf("ошибка %v", err)
	}
}

// Ошибка опроса и непроверенная живость — «неизвестно», не «готово» и не
// «работает»: ошибка с причиной после окон.
func TestWaitIdleUnknown(t *testing.T) {
	t.Run("опрос", func(t *testing.T) {
		e := newSvc(t)
		e.s.DegradedWait = 30 * time.Millisecond
		seedPool(t, e, "shr", false)
		e.h.fail = map[string]error{OpStatus: errors.New("helper молчит")}
		_, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, nil)
		if err == nil || !strings.Contains(err.Error(), "состояние неизвестно") || !strings.Contains(err.Error(), "helper молчит") {
			t.Fatalf("ошибка %v", err)
		}
	})
	t.Run("живость", func(t *testing.T) {
		e := newSvc(t)
		e.s.DegradedWait = 30 * time.Millisecond
		seedPool(t, e, "shr", true)
		st := recalcStatus(1, false)
		st.Integrity[0].LiveErr = "нижние устройства: нет доступа"
		e.h.setStatus(st)
		_, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, nil)
		if err == nil || !strings.Contains(err.Error(), "состояние неизвестно") || !strings.Contains(err.Error(), "нет доступа") {
			t.Fatalf("ошибка %v", err)
		}
	})
	t.Run("массив не прочитан", func(t *testing.T) {
		st := okStatus(LevelRAID1, 2, gib)
		st.Arrays[0].Level = ""
		if o := poolObservation(st, nil); o.Target {
			t.Fatal("непрочитанный массив — готово")
		}
	})
}

// Пул failed — сразу ошибка, без окон.
func TestWaitIdleFailedImmediate(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = time.Hour
	seedPool(t, e, "shr", false)
	st := okStatus(LevelRAID1, 2, gib)
	st.Health, st.Issues = HealthFailed, []string{"массив неактивен"}
	e.h.setStatus(st)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.s.waitIdle(ctx, "p1", func(int) {}, 0, 100, nil)
	if err == nil || !strings.Contains(err.Error(), "массив неактивен") {
		t.Fatalf("ошибка %v", err)
	}
}

// Цель не достигнута при массивах в покое — ошибка после окон (раньше —
// счётчик 30 опросов).
func TestWaitIdleTargetNotReached(t *testing.T) {
	e := newSvc(t)
	e.s.DegradedWait = 30 * time.Millisecond
	seedPool(t, e, "shr", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	_, err := e.s.waitIdle(context.Background(), "p1", func(int) {}, 0, 100, func(PoolStatus) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "целевого состава") {
		t.Fatalf("ошибка %v", err)
	}
}

func TestParseMdstatSyncPos(t *testing.T) {
	a := ParseMdstat("md127 : active raid1 dm-1[1] dm-0[0]\n      1046528 blocks super 1.2 [2/2] [UU]\n      [=>...]  resync = 12.5% (130816/1046528) finish=1.0min speed=1K/sec\n\nunused devices: <none>\n")
	if len(a) != 1 || a[0].SyncPos == nil || *a[0].SyncPos != 130816 {
		t.Fatalf("%+v", a)
	}
}

// integrityLive: живой раздел, пропавший раздел, диск offline, faulty в md,
// нет узла отображения.
func TestIntegrityLive(t *testing.T) {
	root := t.TempDir()
	p := func(r string) string { return filepath.Join(root, r) }
	mk := func(r, s string) {
		os.MkdirAll(filepath.Dir(p(r)), 0o755)
		os.WriteFile(p(r), []byte(s), 0o644)
	}
	ln := func(target, r string) {
		os.MkdirAll(filepath.Dir(p(r)), 0o755)
		os.Symlink(target, p(r))
	}
	// sdb (running) / sdb1 — под dm-0; sdc (offline) / sdc1 — под dm-1;
	// dm-2 — раздел удалён.
	mk("sys/devices/sdb/device/state", "running\n")
	mk("sys/devices/sdb/sdb1/partition", "1")
	mk("sys/devices/sdc/device/state", "offline\n")
	mk("sys/devices/sdc/sdc1/partition", "1")
	ln(p("sys/devices/sdb/sdb1"), "sys/class/block/sdb1")
	ln(p("sys/devices/sdc/sdc1"), "sys/class/block/sdc1")
	for i, sl := range []string{"sdb1", "sdc1", "sdd1"} {
		dm := "dm-" + string(rune('0'+i))
		os.MkdirAll(p("sys/class/block/"+dm+"/slaves"), 0o755)
		ln("../../"+sl, "sys/class/block/"+dm+"/slaves/"+sl)
		mk("dev/"+dm, "")
		ln("../"+dm, "dev/mapper/m"+dm[3:])
	}
	e := &Executor{Paths: Paths{Dev: p("dev"), Disks: disks.Paths{SysClassBlock: p("sys/class/block")}}}
	arrays := []ArrayStatus{{Name: "p1-0", Members: []MemberStatus{{Device: "/dev/dm-0", State: "active sync"}}}}
	check := func(name string, arrays []ArrayStatus, lost bool, why, liveErr string) {
		t.Helper()
		m := IntegrityStatus{Name: name}
		e.integrityLive(&m, arrays)
		if m.Lost != lost || !strings.Contains(m.LostWhy, why) || (liveErr == "") != (m.LiveErr == "") {
			t.Fatalf("%s: %+v", name, m)
		}
	}
	check("m0", arrays, false, "", "")
	check("m1", arrays, true, "нет нижнего", "")
	check("m2", arrays, true, "нет нижнего", "")
	check("m9", arrays, false, "", "нет узла")
	arrays[0].Members[0].State = "faulty"
	check("m0", arrays, true, "отказал", "")
}
