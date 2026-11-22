package home

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func mountPool(h *fakeHost, opts string) {
	h.write("dev/dn_p1/data", "")
	h.write("proc/mountinfo", "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"+
		"40 22 0:50 / "+h.p("mnt/p1")+" "+opts+",relatime - btrfs "+h.p("dev/dn_p1/data")+" rw\n")
}

func TestDestroyCommands(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	mountPool(h, "rw")
	res, err := h.e.Destroy(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	want := []string{
		"umount " + h.p("mnt/p1"),
		"lvm vgremove -q -f -y dn_p1",
		"lvm pvremove -q -y " + h.p("dev/md127"),
		"mdadm --stop " + h.p("dev/md/p1-0"),
		"mdadm --zero-superblock " + h.p("dev/sdb1"),
		"mdadm --zero-superblock " + h.p("dev/sdc1"),
		"wipefs -q -a " + h.p("dev/sdb1"),
		"sgdisk --zap-all " + h.p("dev/sdb"),
		"wipefs -q -a " + h.p("dev/sdb"),
		"partx --delete " + h.p("dev/sdb"), // устаревшие разделы — из ядра
		"sgdisk --zap-all " + h.p("dev/sdc"),
	}
	lines := strings.Split(got, "\n")
	k := 0
	for _, w := range want {
		for k < len(lines) && lines[k] != w {
			k++
		}
		if k == len(lines) {
			t.Fatalf("нет или не по порядку %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "/sda") {
		t.Fatalf("тронут системный диск:\n%s", got)
	}
	if len(res.Commands) == 0 {
		t.Fatal("нет журнала команд")
	}
}

func TestDestroyRefusesForeignDisk(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	h.e.AllowIDs = []string{ref101.ID}
	_, err := h.e.Destroy(context.Background(), PoolParams{Pool: "p1"})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("ожидался отказ: %v", err)
	}
	if w := writes(h.commands()); len(w) != 0 {
		t.Fatalf("запись до проверки: %v", w)
	}
}

func TestDestroyMissingPoolNoop(t *testing.T) {
	h := newHost(t)
	res, err := h.e.Destroy(context.Background(), PoolParams{Pool: "gone"})
	if err != nil || len(res.Commands) != 0 || len(writes(h.commands())) != 0 {
		t.Fatalf("%v %v %v", err, res.Commands, h.commands())
	}
}

func TestDestroyStopsOnFailure(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	h.failOn = "--stop"
	if _, err := h.e.Destroy(context.Background(), PoolParams{Pool: "p1"}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if strings.Contains(strings.Join(h.commands(), "\n"), "zap-all") {
		t.Fatal("продолжено после сбоя")
	}
}

func TestSetReadOnly(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	if _, err := h.e.SetReadOnly(context.Background(), ReadOnlyParams{Pool: "p1", ReadOnly: true}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не смонтирован: %v", err)
	}
	mountPool(h, "rw")
	if _, err := h.e.SetReadOnly(context.Background(), ReadOnlyParams{Pool: "p1", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if got := writes(h.commands()); len(got) != 1 || got[0] != "mount -o remount,ro "+h.p("mnt/p1") {
		t.Fatalf("%v", got)
	}
	mountPool(h, "ro")
	h.cmds = nil
	if _, err := h.e.SetReadOnly(context.Background(), ReadOnlyParams{Pool: "p1", ReadOnly: true}); err != nil || len(h.commands()) != 0 {
		t.Fatalf("повтор не идемпотентен: %v %v", err, h.commands())
	}
	st, _ := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
	if !st.ReadOnly || !st.Mounted {
		t.Fatalf("состояние ro не видно: %+v", st)
	}
	// Чужой источник в точке пула — отказ.
	h.write("proc/mountinfo", "40 22 0:50 / "+h.p("mnt/p1")+" ro - btrfs "+h.p("dev/sdd")+" rw\n")
	if _, err := h.e.SetReadOnly(context.Background(), ReadOnlyParams{Pool: "p1", ReadOnly: false}); !errors.Is(err, ErrRefused) {
		t.Fatalf("чужой том: %v", err)
	}
}

// Пул с общими папками не удаляется; после удаления папок — можно.
func TestDeleteRefusedWithShares(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	if _, err := e.s.db.Exec("INSERT INTO shares(name, pool, comment, created_at) VALUES ('docs', 'p1', '', 0)"); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.Delete(ctx, "p1", "p1")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "docs") {
		t.Fatalf("ожидался отказ ErrConflict с именем папки, получено %v", err)
	}
	if e.h.called(OpDestroy) != 0 {
		t.Fatal("helper не должен вызываться")
	}
	if _, err := e.s.db.Exec("DELETE FROM shares"); err != nil {
		t.Fatal(err)
	}
	tk, err := e.s.Delete(ctx, "p1", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.wait(t, tk.ID); got.State != "succeeded" {
		t.Fatalf("задача: %s %s", got.State, got.Error)
	}
}

// От пула остались только размеченные разделы — диски очищаются.
func TestDestroyPartitionsOnly(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.write("sys/devices/sdb/sdb1/uevent", "PARTNAME=dn-p1-0\n")
	h.part("sdc", 1)
	h.write("sys/devices/sdc/sdc1/uevent", "PARTNAME=dn-p10-0\n") // другой пул
	if _, err := h.e.Destroy(context.Background(), PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	if !strings.Contains(got, "sgdisk --zap-all "+h.p("dev/sdb")) || strings.Contains(got, "/sdc") {
		t.Fatalf("команды:\n%s", got)
	}
	// Метка на диске вне allowlist — отказ до записи.
	h2 := newHost(t)
	h2.part("sdb", 1)
	h2.write("sys/devices/sdb/sdb1/uevent", "PARTNAME=dn-p1-0\n")
	h2.e.AllowIDs = []string{ref102.ID}
	if _, err := h2.e.Destroy(context.Background(), PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) || len(writes(h2.commands())) != 0 {
		t.Fatalf("ожидался отказ без записей: %v", err)
	}
}

// Пустая точка монтирования несмонтированного пула удаляется, непустая — нет.
func TestDestroyRemovesEmptyMountDir(t *testing.T) {
	for _, full := range []bool{false, true} {
		h := newHost(t)
		h.part("sdb", 1)
		h.write("sys/devices/sdb/sdb1/uevent", "PARTNAME=dn-p1-0\n")
		os.MkdirAll(h.p("mnt/p1"), 0o755)
		if full {
			h.write("mnt/p1/keep", "x")
		}
		if _, err := h.e.Destroy(context.Background(), PoolParams{Pool: "p1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(h.p("mnt/p1")); (err == nil) != full {
			t.Fatalf("full=%v: каталог %v", full, err)
		}
	}
}

// Участники md исчезли, диски вернулись под другими именами —
// верхние слои разбираются (vgremove упал → vgchange -an), md останавливается,
// диски находятся по меткам и очищаются.
func TestDestroyArrayWithoutMembers(t *testing.T) {
	h := newHost(t)
	h.virt("md127") // без участников
	h.link("../md127", "dev/md/nas-lab:p1-0")
	h.write("proc/mdstat", "Personalities : [raid1]\nmd127 : inactive\n")
	h.vgs = []string{"dn_p1"}
	h.pvs[h.p("dev/md127")] = "dn_p1"
	h.failOn = "vgremove"
	h.part("sdd", 1)
	h.write("sys/devices/sdd/sdd1/uevent", "PARTNAME=dn-p1-0\n")
	res, err := h.e.Destroy(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	for _, w := range []string{"lvm vgchange -q -an dn_p1", "mdadm --stop " + h.p("dev/md127"), "sgdisk --zap-all " + h.p("dev/sdd")} {
		if !strings.Contains(got, w) {
			t.Fatalf("нет %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "pvremove") || len(res.Warnings) != 1 {
		t.Fatalf("pvremove или предупреждение: %v\n%s", res.Warnings, got)
	}
}

// «освободить диск» — только разделы неизвестного пула, без держателей.
func TestReleaseDisk(t *testing.T) {
	setup := func(label string) *fakeHost {
		h := newHost(t)
		h.part("sdd", 1)
		h.write("sys/devices/sdd/sdd1/uevent", "PARTNAME="+label+"\n")
		return h
	}
	p := ReleaseParams{Disk: ref103, KnownPools: []string{"p1"}}
	h := setup("dn-gone-0")
	if _, err := h.e.ReleaseDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(writes(h.commands()), "\n"); !strings.Contains(got, "sgdisk --zap-all "+h.p("dev/sdd")) {
		t.Fatalf("не очищен:\n%s", got)
	}
	for name, f := range map[string]func() *fakeHost{
		"известный пул": func() *fakeHost { return setup("dn-p1-0") },
		"старая метка":  func() *fakeHost { return setup("dn") },
		"чужая метка":   func() *fakeHost { return setup("data") },
		"занят чужим":   func() *fakeHost { h := setup("data"); h.virt("dm-5", "sdd1"); return h },
		"вне allowlist": func() *fakeHost { h := setup("dn-gone-0"); h.e.AllowIDs = []string{ref101.ID}; return h },
	} {
		h := f()
		if _, err := h.e.ReleaseDisk(context.Background(), p); !errors.Is(err, ErrRefused) || len(writes(h.commands())) != 0 {
			t.Errorf("%s: %v %v", name, err, writes(h.commands()))
		}
	}
}

// Разделы заняты стеком неизвестного пула — release разбирает этот пул.
func TestReleaseDiskOrphanStack(t *testing.T) {
	h := newHost(t)
	h.part("sdd", 1)
	h.write("sys/devices/sdd/sdd1/uevent", "PARTNAME=dn-gone-0\n")
	h.virt("md125", "sdd1")
	h.link("../md125", "dev/md/gone-0")
	h.write("proc/mdstat", "Personalities : [raid1]\nmd125 : active raid1 sdd1[0]\n      1045504 blocks super 1.2 [2/1] [U_]\n")
	r, err := h.e.ReleaseDisk(context.Background(), ReleaseParams{Disk: ref103, KnownPools: []string{"p1"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	if !strings.Contains(got, "mdadm --stop "+h.p("dev/md/gone-0")) || !strings.Contains(got, "sgdisk --zap-all "+h.p("dev/sdd")) || len(r.Warnings) == 0 {
		t.Fatalf("%v\n%s", r.Warnings, got)
	}
}

// Аварийный read-only Btrfs (Transaction aborted) — ro только в
// параметрах суперблока, у точки монтирования rw.
func TestStatusEmergencyReadOnly(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	h.write("dev/dn_p1/data", "")
	h.write("proc/mountinfo", "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"+
		"40 22 0:50 / "+h.p("mnt/p1")+" rw,relatime - btrfs "+h.p("dev/dn_p1/data")+" ro,space_cache=v2,subvolid=5,subvol=/\n")
	st, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mounted || !st.ReadOnly || !st.EmergencyReadOnly {
		t.Fatalf("аварийный ro не виден: %+v", st)
	}
	found := false
	for _, s := range st.Issues {
		found = found || strings.Contains(s, "аварийно")
	}
	if !found {
		t.Fatalf("нет замечания: %v", st.Issues)
	}
	// Обычный ro перемонтированием — не аварийный.
	mountPool(h, "ro")
	if st, _ = h.e.Status(context.Background(), PoolParams{Pool: "p1"}); !st.ReadOnly || st.EmergencyReadOnly {
		t.Fatalf("remount ro: %+v", st)
	}
	mountPool(h, "rw")
	if st, _ = h.e.Status(context.Background(), PoolParams{Pool: "p1"}); st.ReadOnly || st.EmergencyReadOnly {
		t.Fatalf("rw: %+v", st)
	}
}

// Аварийный RO не скрывает более тяжёлое состояние массивов.
func TestHealthEmergencyReadOnlyRank(t *testing.T) {
	for _, c := range []struct {
		arr  ArrayStatus
		want string
	}{
		{ArrayStatus{Level: "raid1", State: []string{"clean"}}, HealthWarning}, // массивы исправны, запись невозможна
		{ArrayStatus{Level: "raid1", State: []string{"clean"}, Action: "resync"}, HealthWarning},
		{ArrayStatus{Level: "raid1", State: []string{"clean", "degraded"}, Missing: 1}, HealthDegraded},
		{ArrayStatus{Level: "raid1", State: []string{"inactive"}}, HealthFailed},
	} {
		s := PoolStatus{Arrays: []ArrayStatus{c.arr}, EmergencyReadOnly: true}
		if got := health(s, true); got != c.want {
			t.Errorf("%+v: health %q, ожидался %q", c.arr, got, c.want)
		}
		s.EmergencyReadOnly = false
		if got := health(s, true); got == HealthWarning {
			t.Errorf("%+v: warning без аварийного ro", c.arr)
		}
	}
}
