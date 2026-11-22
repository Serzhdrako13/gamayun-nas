package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Массив ядра без ссылки udev /dev/md/<имя> опознаётся по имени в
// суперблоке; имя не прочитано — «неизвестно», а не «массив не собран».

// unlinkedPool — как existingPool, но без /dev/md/p1-0 (udev не создал ссылку).
func unlinkedPool(h *fakeHost, mdstat string, members ...string) {
	var slaves []string
	for _, m := range members {
		slaves = append(slaves, h.part(m, 1))
	}
	h.virt("md127", slaves...)
	h.write("proc/mdstat", mdstat)
	h.vgs = []string{"dn_p1"}
	h.pvs[h.p("dev/md127")] = "dn_p1"
}

const exportP1 = "MD_LEVEL=raid1\nMD_DEVICES=2\nMD_METADATA=1.2\nMD_UUID=a:b:c:d\nMD_NAME=nas:p1-0\n"

// mdadmF2 — подмена: --detail --export / --examine --export по устройству
// (нет ключа — код 1), --detail — образец raid1, остальное — fakeHost.
func mdadmF2(h *fakeHost, detailExp, examineExp map[string]string) {
	detail := sample(h.t, "detail-raid1-bitmap.txt")
	h.e.Runner = runnerFunc(func(ctx context.Context, to time.Duration, path string, args ...string) (CmdResult, error) {
		if filepath.Base(path) == "mdadm" && len(args) == 3 && args[1] == "--export" {
			m := detailExp
			if args[0] == "--examine" {
				m = examineExp
			}
			h.mu.Lock()
			h.cmds = append(h.cmds, "mdadm "+strings.Join(args, " "))
			h.mu.Unlock()
			if out, ok := m[filepath.Base(args[2])]; ok {
				return CmdResult{Stdout: []byte(out)}, nil
			}
			return CmdResult{ExitCode: 1, Stderr: []byte("нет")}, nil
		}
		if filepath.Base(path) == "mdadm" && args[0] == "--detail" {
			return CmdResult{Stdout: []byte(detail)}, nil
		}
		return h.Run(ctx, to, path, args...)
	})
}

func TestArraysUnlinkedByName(t *testing.T) {
	h := newHost(t)
	unlinkedPool(h, mdstatRAID1, "sdb", "sdc")
	mdadmF2(h, map[string]string{"md127": exportP1}, nil)
	arrs, err := h.e.arrays(context.Background(), "p1")
	if err != nil || len(arrs) != 1 {
		t.Fatalf("%v %+v", err, arrs)
	}
	a := arrs[0]
	if a.index != 0 || a.kname != "md127" || a.node != h.p("dev/md127") || a.inactive {
		t.Fatalf("%+v", a)
	}
	// Чужой массив без ссылки — не наш, без ошибки.
	if arrs, err := h.e.arrays(context.Background(), "p2"); err != nil || len(arrs) != 0 {
		t.Fatalf("p2: %v %+v", err, arrs)
	}
	// Status: массив найден, не «не собран».
	st, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Arrays) != 1 || st.Arrays[0].Device != "md127" || len(st.Unknown) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestArraysUnlinkedForeignAndOldMeta(t *testing.T) {
	h := newHost(t)
	unlinkedPool(h, mdstatRAID1, "sdb", "sdc")
	for name, out := range map[string]string{
		"чужое имя": strings.Replace(exportP1, "p1-0", "p10-0", 1),
		"0.90":      "MD_LEVEL=raid1\nMD_METADATA=0.90\nMD_UUID=x\n",
		"без хоста": strings.Replace(exportP1, "nas:p1-0", "p1-0", 1),
	} {
		mdadmF2(h, map[string]string{"md127": out}, nil)
		arrs, err := h.e.arrays(context.Background(), "p1")
		want := 0
		if name == "без хоста" {
			want = 1
		}
		if err != nil || len(arrs) != want {
			t.Fatalf("%s: %v %+v", name, err, arrs)
		}
	}
}

// Имя не прочитано (мусор, сбой mdadm) — ошибка: Status unknown, create и
// разборка отказывают, а не считают массив отсутствующим.
func TestArraysUnlinkedUnreadable(t *testing.T) {
	for name, exp := range map[string]map[string]string{
		"сбой mdadm": {},
		"мусор":      {"md127": "garbage\n"},
	} {
		h := newHost(t)
		unlinkedPool(h, mdstatRAID1, "sdb", "sdc")
		mdadmF2(h, exp, nil)
		if _, err := h.e.arrays(context.Background(), "p1"); err == nil || !strings.Contains(err.Error(), "md127") {
			t.Fatalf("%s: ошибка %v", name, err)
		}
		st, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
		if err != nil {
			t.Fatal(err)
		}
		if st.Health != HealthUnknown || len(st.Unknown) == 0 {
			t.Fatalf("%s: health %s, unknown %v", name, st.Health, st.Unknown)
		}
		if _, err := h.e.Destroy(context.Background(), PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: destroy %v", name, err)
		}
		if w := writes(h.commands()); len(w) != 0 {
			t.Fatalf("%s: записи %v", name, w)
		}
	}
}

// Неактивный массив без ссылки: имя — из суперблока участника; сборка его не
// принимает за собранный.
func TestArraysUnlinkedInactive(t *testing.T) {
	h := newHost(t)
	mdstat := "Personalities : [raid1]\nmd127 : inactive sdb1[0](S)\n      1045504 blocks super 1.2\n\nunused devices: <none>\n"
	unlinkedPool(h, mdstat, "sdb")
	mdadmF2(h, map[string]string{"md127": "MD_LEVEL=raid1\nMD_DEVICES=2\n"}, map[string]string{"sdb1": exportP1})
	arrs, err := h.e.arrays(context.Background(), "p1")
	if err != nil || len(arrs) != 1 || !arrs[0].inactive {
		t.Fatalf("%v %+v", err, arrs)
	}
	// Ни --detail, ни участники не прочитаны — неизвестно.
	mdadmF2(h, map[string]string{"md127": "MD_LEVEL=raid1\n"}, nil)
	if _, err := h.e.arrays(context.Background(), "p1"); err == nil {
		t.Fatal("ожидалась ошибка")
	}
}

// Откат create находит массив без ссылки и останавливает его по узлу ядра
// (раньше падал «не закрыто отображение»: md не найден — не остановлен).
func TestCreateRollbackStopsUnlinked(t *testing.T) {
	h := newHost(t)
	unlinkedPool(h, mdstatRAID1, "sdb", "sdc")
	mdadmF2(h, map[string]string{"md127": exportP1}, nil)
	if _, err := h.e.CreateRollback(context.Background(), CreateRollbackParams{Pool: "p1", DiskIDs: []string{ref101.ID, ref102.ID}}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	if !strings.Contains(got, "mdadm --stop "+h.p("dev/md127")) {
		t.Fatalf("md не остановлен:\n%s", got)
	}
}

// create: массив пула без ссылки уже есть / не читается — отказ до записи.
func TestCreateRefusesUnlinked(t *testing.T) {
	for name, exp := range map[string]map[string]string{"есть": {"md127": exportP1}, "не читается": {}} {
		h := newHost(t)
		h.write("proc/mdstat", mdstatRAID1)
		h.virt("md127")
		mdadmF2(h, exp, nil)
		if _, err := h.e.Create(context.Background(), createParams(false)); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: %v", name, err)
		}
		if w := writes(h.commands()); len(w) != 0 {
			t.Fatalf("%s: записи %v", name, w)
		}
	}
}

// create: udev не создал ссылку после mdadm --create («мусор, задевший
// udev») — create падает, внутренний откат останавливает md по /dev/mdN
// (раньше --stop по ссылке молча падал, откат «неполный»: busy).
func TestCreateUndoStopsUnlinked(t *testing.T) {
	h := newHost(t)
	mdadmF2(h, map[string]string{"md0": exportP1}, nil)
	inner := h.e.Runner
	h.e.Runner = runnerFunc(func(ctx context.Context, to time.Duration, path string, args ...string) (CmdResult, error) {
		r, err := inner.Run(ctx, to, path, args...)
		if filepath.Base(path) == "mdadm" && len(args) > 0 && args[0] == "--create" {
			h.mu.Lock()
			_ = os.Remove(h.p("dev/md/" + filepath.Base(args[1])))
			h.mu.Unlock()
			h.virt("md0")
			h.write("proc/mdstat", strings.Replace(mdstatRAID1, "md127", "md0", 1))
		}
		return r, err
	})
	_, err := h.e.Create(context.Background(), createParams(false))
	if err == nil || !strings.Contains(err.Error(), "не появился") || strings.Contains(err.Error(), "неполный") {
		t.Fatalf("ожидался сбой create с полным откатом: %v", err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	if !strings.Contains(got, "mdadm --stop "+h.p("dev/md0")) {
		t.Fatalf("md не остановлен по узлу ядра:\n%s", got)
	}
}
