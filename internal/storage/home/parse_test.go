package home

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Образцы в testdata — реальный вывод с тестовой машины (Debian 13, mdadm 4.4,
// lvm2 2.03.31, btrfs-progs 6.14, cryptsetup 2.7.5), 2026-11-04.
// mdstat-recovery.txt собран из строк, наблюдавшихся во время замены
// диска (строка recovery скопирована из вывода, остальное — как в
// mdstat-faulty.txt).
func sample(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseMdstat(t *testing.T) {
	cases := []struct {
		file    string
		level   string
		members []string
		faulty  []string
		raid    int
		inSync  int
		action  string
		pct     float64
		blocks  int64
	}{
		{"mdstat-resync.txt", "raid1", []string{"dm-1", "dm-0"}, []string{}, 2, 2, "resync", 2.1, 1036416},
		{"mdstat-reshape.txt", "raid5", []string{"dm-3", "dm-1", "dm-0"}, []string{}, 3, 3, "reshape", 1.9, 1036416},
		{"mdstat-faulty.txt", "raid5", []string{"dm-1", "dm-3", "dm-0"}, []string{"dm-3"}, 3, 2, "", 0, 2072832},
		{"mdstat-recovery.txt", "raid5", []string{"dm-1", "dm-3", "dm-0"}, []string{}, 3, 2, "recovery", 24.2, 2072832},
	}
	for _, c := range cases {
		got := ParseMdstat(sample(t, c.file))
		if len(got) != 1 {
			t.Fatalf("%s: массивов %d", c.file, len(got))
		}
		a := got[0]
		if a.Device != "md127" || !a.Active || a.Level != c.level || a.RaidDevices != c.raid ||
			a.InSync != c.inSync || a.Action != c.action || a.Blocks != c.blocks {
			t.Errorf("%s: %+v", c.file, a)
		}
		if !reflect.DeepEqual(a.Members, c.members) || !reflect.DeepEqual(a.Faulty, c.faulty) {
			t.Errorf("%s: участники %v, отказавшие %v", c.file, a.Members, a.Faulty)
		}
		if c.action != "" && (a.Progress == nil || *a.Progress != c.pct) {
			t.Errorf("%s: прогресс %v", c.file, a.Progress)
		}
	}
	// Запасной, read-only, неактивный, отложенное действие.
	got := ParseMdstat(`Personalities : [raid1]
md0 : active (auto-read-only) raid1 sdb1[2](S) sdc1[1] sda1[0]
      1046528 blocks super 1.2 [2/2] [UU]
      	resync=DELAYED

md1 : inactive sdd1[0](S)
      1046528 blocks super 1.2

unused devices: <none>
`)
	if len(got) != 2 {
		t.Fatalf("массивов %d", len(got))
	}
	if !got[0].ReadOnly || !reflect.DeepEqual(got[0].Spares, []string{"sdb1"}) || got[0].Action != "resync" || !got[0].Delayed {
		t.Errorf("md0: %+v", got[0])
	}
	if got[1].Active || !got[1].Inactive || got[1].Level != "" {
		t.Errorf("md1: %+v", got[1])
	}
}

func TestParseMdadmDetail(t *testing.T) {
	d, err := ParseMdadmDetail(sample(t, "detail-raid5-faulty.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Level != "raid5" || d.RaidDevices != 3 || d.ActiveDevices != 2 || d.FailedDevices != 1 ||
		d.ArraySizeKiB != 2072832 || d.UsedDevKiB != 1036416 || d.Name != "nas-lab:int1-0" ||
		!reflect.DeepEqual(d.State, []string{"clean", "degraded"}) {
		t.Errorf("%+v", d)
	}
	want := []MdMember{
		{Number: 0, Major: 253, Minor: 0, RaidDevice: 0, State: "active sync", Device: "/dev/dm-0"},
		{Number: 3, Major: 253, Minor: 1, RaidDevice: 1, State: "active sync", Device: "/dev/dm-1"},
		{Number: -1, Major: 0, Minor: 0, RaidDevice: 2, State: "removed"},
		{Number: 2, Major: 253, Minor: 3, RaidDevice: -1, State: "faulty", Device: "/dev/dm-3"},
	}
	if !reflect.DeepEqual(d.Members, want) {
		t.Errorf("участники %+v", d.Members)
	}

	d, err = ParseMdadmDetail(sample(t, "detail-raid5-reshaping.txt"))
	if err != nil || d.ReshapePct == nil || *d.ReshapePct != 1 || !reflect.DeepEqual(d.State, []string{"clean", "reshaping"}) {
		t.Errorf("reshape: %+v %v", d, err)
	}
	d, err = ParseMdadmDetail(sample(t, "detail-raid1-resyncing.txt"))
	if err != nil || d.ResyncPct == nil || *d.ResyncPct != 2 || d.Level != "raid1" {
		t.Errorf("resync: %+v %v", d, err)
	}
	for _, f := range []string{"detail-raid5-clean.txt", "detail-raid1-integrity.txt", "detail-raid5-removed.txt"} {
		if d, err := ParseMdadmDetail(sample(t, f)); err != nil || len(d.Members) == 0 {
			t.Errorf("%s: %+v %v", f, d, err)
		}
	}
	if _, err := ParseMdadmDetail("mdadm: cannot open /dev/md/x: No such file or directory\n"); err == nil {
		t.Error("ожидалась ошибка на пустом выводе")
	}
}

func TestParseLVMReport(t *testing.T) {
	lv, err := ParseLVMReport([]byte(sample(t, "lvs.json")), "lv")
	if err != nil || len(lv) != 1 || lv[0]["lv_size"] != "1056964608" || lv[0]["lv_name"] != "data" {
		t.Fatalf("lvs: %v %v", lv, err)
	}
	vg, err := ParseLVMReport([]byte(sample(t, "vgs.json")), "vg")
	if err != nil || len(vg) != 1 || vg[0]["vg_free"] != "0" || vg[0]["vg_extent_size"] != "4194304" {
		t.Fatalf("vgs: %v %v", vg, err)
	}
	if _, err := ParseLVMReport([]byte("  Volume group not found"), "vg"); err == nil {
		t.Error("ожидалась ошибка разбора")
	}
}

func TestParseBtrfs(t *testing.T) {
	u, err := ParseBtrfsUsage(sample(t, "btrfs-usage.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := BtrfsUsage{DeviceSize: 1056964608, Allocated: 130809856, Unallocated: 926154752, Used: 294912,
		FreeEstimated: 934543360, FreeStatfs: 933494784, DataRatio: 1, MetadataRatio: 2,
		DataProfile: "single", MetaProfile: "DUP", GlobalReserve: 5767168}
	if u != want {
		t.Errorf("usage %+v", u)
	}
	if _, err := ParseBtrfsUsage("ERROR: not a btrfs filesystem"); err == nil {
		t.Error("ожидалась ошибка")
	}
	st := ParseBtrfsDevStats(sample(t, "btrfs-devstats.txt") + "[/dev/sdz].corruption_errs 3\n")
	if st["corruption_errs"] != 3 || st["read_io_errs"] != 0 || len(st) != 5 {
		t.Errorf("stats %v", st)
	}
}

func TestParseIntegrityStatus(t *testing.T) {
	got := ParseIntegrityStatus(sample(t, "dmsetup-integrity.txt"), "")
	want := []IntegrityStatus{
		{Name: "dni_int1_0_63abc9b6", Mismatches: 9, ProvidedBytes: 2074928 * 512},
		{Name: "dni_int1_0_e82eab07", Mismatches: 2, ProvidedBytes: 2074928 * 512},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%+v", got)
	}
	one := ParseIntegrityStatus("0 2074928 integrity 0 2074928 1024\n", "x")
	if len(one) != 1 || one[0].Name != "x" || !one[0].Recalculating || *one[0].RecalcBytes != 1024*512 {
		t.Errorf("%+v", one)
	}
	if p := one[0].RecalcProgress(); p == nil || *p < 0.04 || *p > 0.05 {
		t.Errorf("прогресс %v", p)
	}
	// После завершения пересчёта ядро 6.12 оставляет границу = размеру
	// Образец: пересчёт не идёт.
	done := ParseIntegrityStatus("dnx_0: 0 2074928 integrity 0 2074928 2074928\n", "")
	if len(done) != 1 || done[0].Recalculating || done[0].RecalcProgress() != nil || *done[0].RecalcBytes != 2074928*512 {
		t.Errorf("%+v", done)
	}
	if len(ParseIntegrityStatus("No devices found\n", "")) != 0 {
		t.Error("пустой список ожидался")
	}
}

func TestParseIntegrityTable(t *testing.T) {
	// Образцы dmsetup table с тестовой машины.
	got := ParseIntegrityTable(`dnx_0: 0 2074928 integrity 8:33 0 4 J 10 block_size:4096 recalculate journal_sectors:16072 interleave_sectors:32768 buffer_sectors:32 journal_watermark:50 commit_time:10000 fix_padding fix_hmac internal_hash:crc32c
e3big: 0 8581423336 integrity 7:0 0 4 B 10 block_size:4096 recalculate journal_sectors:130928 interleave_sectors:32768 buffer_sectors:32 sectors_per_bit:32768 bitmap_flush_interval:10000 fix_padding fix_hmac internal_hash:crc32c
No devices found
`)
	if len(got) != 2 || got["dnx_0"] != IntegrityJournal || got["e3big"] != IntegrityBitmap {
		t.Errorf("%v", got)
	}
}
