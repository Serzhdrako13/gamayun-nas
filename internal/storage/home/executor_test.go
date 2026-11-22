package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gamayun-nas/internal/disks"
)

const gib = int64(1 << 30)

// fakeHost — искусственные sysfs, /dev, /proc и подменённый Runner,
// который запоминает команды и изображает их действие на дереве.
type fakeHost struct {
	t    *testing.T
	root string
	e    *Executor

	mu      sync.Mutex
	cmds    []string
	sigs    map[string]string // /dev/X → вывод wipefs (сигнатуры)
	blkidOK map[string]bool   // /dev/X → blkid нашёл сигнатуру
	devnums map[string]string // /dev/X → maj:min для stat
	vgs     []string
	pvs     map[string]string // pv → vg
	failOn  string            // подстрока команды, на которой Runner вернёт код 1
	dmStat  string            // вывод dmsetup status --target integrity ("" — нет устройств)
	dmTable string            // вывод dmsetup table --target integrity
	nextMD  int
	examine map[string]string // /dev/X → вывод mdadm --examine --brief (нет — код 1)
	busy    map[string]bool   // диск занят: sgdisk не обновляет разделы в ядре (нужен partx)
}

func (h *fakeHost) p(rel string) string { return filepath.Join(h.root, rel) }

func (h *fakeHost) write(rel, s string) {
	h.t.Helper()
	p := h.p(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) link(target, rel string) {
	h.t.Helper()
	os.MkdirAll(filepath.Dir(h.p(rel)), 0o755)
	if err := os.Symlink(target, h.p(rel)); err != nil {
		h.t.Fatal(err)
	}
}

// disk — физический диск 1 ГиБ с WWN naa.<wwn>.
func (h *fakeHost) disk(name, wwn string, minor int) {
	h.write("sys/devices/"+name+"/device/wwid", "naa."+wwn+"\n")
	h.write("sys/devices/"+name+"/size", fmt.Sprint(gib/512))
	h.write("sys/devices/"+name+"/dev", fmt.Sprintf("8:%d", minor))
	h.write("sys/devices/"+name+"/queue/logical_block_size", "512")
	h.write("sys/devices/"+name+"/queue/rotational", "1")
	os.MkdirAll(h.p("sys/devices/"+name+"/holders"), 0o755)
	h.link(h.p("sys/devices/"+name), "sys/class/block/"+name)
	h.write("dev/"+name, "")
	h.devnums[h.p("dev/"+name)] = fmt.Sprintf("8:%d", minor)
}

// part — раздел num диска (как делает sgdisk + udev).
func (h *fakeHost) part(disk string, num int) string {
	name := fmt.Sprintf("%s%d", disk, num)
	dir := "sys/devices/" + disk + "/" + name
	h.write(dir+"/partition", fmt.Sprint(num))
	os.MkdirAll(h.p(dir+"/holders"), 0o755)
	if _, err := os.Lstat(h.p("sys/class/block/" + name)); err != nil {
		h.link(h.p(dir), "sys/class/block/"+name)
	}
	h.write("dev/"+name, "")
	return name
}

// virt — md/dm-устройство с участниками (имена в sys/class/block).
func (h *fakeHost) virt(name string, slaves ...string) {
	dir := "sys/devices/virtual/" + name
	os.MkdirAll(h.p(dir+"/slaves"), 0o755)
	os.MkdirAll(h.p(dir+"/holders"), 0o755)
	h.link(h.p(dir), "sys/class/block/"+name)
	for _, s := range slaves {
		real, _ := filepath.EvalSymlinks(h.p("sys/class/block/" + s))
		h.link(real, dir+"/slaves/"+s)
		h.link(h.p(dir), strings.TrimPrefix(real, h.root+"/")+"/holders/"+name)
	}
	h.write("dev/"+name, "")
}

func newHost(t *testing.T) *fakeHost {
	root := t.TempDir()
	h := &fakeHost{t: t, root: root, sigs: map[string]string{}, blkidOK: map[string]bool{},
		devnums: map[string]string{}, pvs: map[string]string{}, nextMD: 127}
	h.write("proc/mounts", "")
	h.write("proc/swaps", "Filename\tType\tSize\tUsed\tPriority\n")
	h.write("proc/mdstat", "Personalities :\nunused devices: <none>\n")
	os.MkdirAll(h.p("sys/dev/block"), 0o755)
	os.MkdirAll(h.p("sys/fs/btrfs"), 0o755)
	// Системный диск: sda1 смонтирован в /.
	h.disk("sda", "5000000000000000", 0)
	h.part("sda", 1)
	h.link(h.p("sys/class/block/sda1"), "sys/dev/block/8:1")
	h.write("proc/mountinfo", "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n")
	h.disk("sdb", "5000000000000101", 16)
	h.disk("sdc", "5000000000000102", 32)
	h.disk("sdd", "5000000000000103", 48)
	h.e = &Executor{
		Runner: h,
		Tools:  DefaultTools(),
		Paths: Paths{
			Disks: disks.Paths{
				SysClassBlock: h.p("sys/class/block"),
				SysDevBlock:   h.p("sys/dev/block"),
				SysFsBtrfs:    h.p("sys/fs/btrfs"),
				Mountinfo:     h.p("proc/mountinfo"),
				Swaps:         h.p("proc/swaps"),
				Dev:           h.p("dev"),
				Stat:          h.stat,
			},
			Dev:    h.p("dev"),
			Mdstat: h.p("proc/mdstat"),
		},
		AllowIDs:  []string{"wwn-0x5000000000000101", "wwn-0x5000000000000102", "wwn-0x5000000000000103"},
		MountRoot: h.p("mnt"),
		Wait:      time.Second,
		Poll:      10 * time.Millisecond,
	}
	return h
}

func (h *fakeHost) stat(path string) (disks.FileStat, error) {
	n, ok := h.devnums[path]
	if !ok {
		return disks.FileStat{}, os.ErrNotExist
	}
	var maj, min uint32
	fmt.Sscanf(n, "%d:%d", &maj, &min)
	return disks.FileStat{Block: true, Rdev: disks.DevNum{Major: maj, Minor: min}}, nil
}

var (
	ref101 = DiskRef{ID: "wwn-0x5000000000000101", SizeBytes: gib}
	ref102 = DiskRef{ID: "wwn-0x5000000000000102", SizeBytes: gib}
	ref103 = DiskRef{ID: "wwn-0x5000000000000103", SizeBytes: gib}
)

// Run — подменённый Runner.
func (h *fakeHost) Run(_ context.Context, timeout time.Duration, path string, args ...string) (CmdResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if timeout <= 0 || !filepath.IsAbs(path) {
		h.t.Errorf("недопустимый вызов %s %v", path, args)
	}
	line := filepath.Base(path) + " " + strings.Join(args, " ")
	h.cmds = append(h.cmds, line)
	if h.failOn != "" && strings.Contains(line, h.failOn) {
		return CmdResult{ExitCode: 1, Stderr: []byte("искусственный сбой")}, nil
	}
	ok := CmdResult{}
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	switch filepath.Base(path) {
	case "wipefs":
		return CmdResult{Stdout: []byte(h.sigs[last])}, nil
	case "blkid":
		if h.blkidOK[last] {
			return CmdResult{Stdout: []byte(last + `: PTTYPE="gpt"`)}, nil
		}
		return CmdResult{ExitCode: 2}, nil
	case "partx":
		if args[0] == "--add" {
			n, _ := strconv.Atoi(args[2])
			h.part(filepath.Base(last), n)
		}
	case "sgdisk":
		disk := filepath.Base(last)
		for i, a := range args {
			if a == "-n" && !h.busy[disk] {
				var num int
				fmt.Sscanf(args[i+1], "%d:", &num)
				h.part(disk, num)
			}
		}
	case "integritysetup":
		if args[0] == "open" {
			h.write("dev/mapper/"+last, "")
		}
	case "mdadm":
		if args[0] == "--examine" {
			if out, ok := h.examine[last]; ok {
				return CmdResult{Stdout: []byte(out)}, nil
			}
			return CmdResult{ExitCode: 1}, nil
		}
		if args[0] == "--assemble" {
			md := fmt.Sprintf("md%d", h.nextMD)
			h.nextMD++
			h.write("dev/"+md, "")
			h.link("../"+md, "dev/md/"+filepath.Base(args[2]))
		}
		if args[0] == "--create" {
			md := fmt.Sprintf("md%d", h.nextMD)
			h.nextMD++
			h.write("dev/"+md, "")
			h.link("../"+md, "dev/md/"+filepath.Base(args[1]))
		}
	case "lvm":
		switch args[0] {
		case "vgs":
			var rows []string
			for _, v := range h.vgs {
				rows = append(rows, fmt.Sprintf(`{"vg_name":%q, "vg_size":"2139095040", "vg_free":"0", "pv_count":"1", "vg_extent_size":"4194304"}`, v))
			}
			return CmdResult{Stdout: []byte(`{"report":[{"vg":[` + strings.Join(rows, ",") + `]}]}`)}, nil
		case "pvs":
			var rows []string
			for pv, vg := range h.pvs {
				rows = append(rows, fmt.Sprintf(`{"pv_name":%q, "vg_name":%q, "pv_size":"1", "pv_free":"0", "dev_size":"1"}`, pv, vg))
			}
			return CmdResult{Stdout: []byte(`{"report":[{"pv":[` + strings.Join(rows, ",") + `]}]}`)}, nil
		case "lvs":
			return CmdResult{Stdout: []byte(`{"report":[{"lv":[{"lv_name":"data","vg_name":"x","lv_size":"2139095040","lv_attr":"-wi-ao----"}]}]}`)}, nil
		case "vgcreate":
			h.vgs = append(h.vgs, args[2])
		case "vgchange":
			h.write("dev/"+last+"/data", "")
		case "lvcreate":
			h.write("dev/"+last+"/data", "")
		}
	case "dmsetup":
		if args[0] == "remove" {
			os.Remove(h.p("dev/mapper/" + last))
			return ok, nil
		}
		out := h.dmStat
		if args[0] == "table" {
			out = h.dmTable
		}
		if out == "" {
			out = "No devices found\n"
		}
		return CmdResult{Stdout: []byte(out)}, nil
	}
	return ok, nil
}

func (h *fakeHost) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.cmds...)
}

// writes — команды, меняющие диски (всё, кроме чтения состояния).
func writes(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		f := strings.Fields(c)
		switch {
		case f[0] == "wipefs" && len(f) > 1 && f[1] == "--no-act",
			f[0] == "blkid", f[0] == "dmsetup", f[0] == "udevadm",
			f[0] == "lvm" && (f[1] == "vgs" || f[1] == "pvs" || f[1] == "lvs"),
			f[0] == "mdadm" && (f[1] == "--detail" || f[1] == "--examine"),
			f[0] == "sgdisk" && f[1] == "-i",
			f[0] == "btrfs" && (f[1] == "device" || f[2] == "usage"):
			continue
		}
		out = append(out, c)
	}
	return out
}

func TestCheckFreeDiskRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *fakeHost)
		ref   DiskRef
		want  string
	}{
		{"системный диск без allowlist", func(h *fakeHost) { h.e.AllowIDs = nil },
			DiskRef{ID: "wwn-0x5000000000000000", SizeBytes: gib}, "занят"},
		{"не в allowlist", func(h *fakeHost) { h.e.AllowIDs = []string{"wwn-0x5000000000000102"} }, ref101, "не входит"},
		{"системный диск в allowlist", func(h *fakeHost) { h.e.AllowIDs = []string{"wwn-0x5000000000000000"} },
			DiskRef{ID: "wwn-0x5000000000000000", SizeBytes: gib}, "занят"},
		{"неполный снимок", func(h *fakeHost) {
			h.write("proc/mountinfo", "22 1 8:1 / / rw - ext4 /dev/sda1 rw\nмусор\n")
		}, ref101, "неполный"},
		{"нет такого диска", nil, DiskRef{ID: "wwn-0x5000000000000999", SizeBytes: gib}, "не входит"},
		{"дубликат идентичности", func(h *fakeHost) {
			h.disk("sde", "5000000000000101", 64)
		}, ref101, "найден 2 раз"},
		{"размер не совпадает", nil, DiskRef{ID: ref101.ID, SizeBytes: 2 * gib}, "размер"},
		{"смонтирован", func(h *fakeHost) {
			h.link(h.p("sys/class/block/sdb"), "sys/dev/block/8:16")
			h.write("proc/mountinfo", "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n30 22 8:16 / /data rw - ext4 /dev/sdb rw\n")
		}, ref101, "занят"},
		{"есть раздел", func(h *fakeHost) { h.part("sdb", 1) }, ref101, "раздел"},
		{"есть holders (dm поверх диска)", func(h *fakeHost) { h.virt("dm-5", "sdb") }, ref101, "занят"},
		{"узел /dev не того устройства", func(h *fakeHost) { h.devnums[h.p("dev/sdb")] = "8:32" }, ref101, "не совпадает"},
		{"сигнатура wipefs", func(h *fakeHost) {
			h.sigs[h.p("dev/sdb")] = "DEVICE OFFSET TYPE\nsdb 0x200 gpt"
		}, ref101, "сигнатуры"},
		{"сигнатура blkid", func(h *fakeHost) { h.blkidOK[h.p("dev/sdb")] = true }, ref101, "blkid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHost(t)
			if c.setup != nil {
				c.setup(h)
			}
			_, err := h.e.CheckFreeDisk(context.Background(), c.ref)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ожидался отказ «%s», получено %v", c.want, err)
			}
			if w := writes(h.commands()); len(w) != 0 {
				t.Fatalf("при отказе выполнены записи: %v", w)
			}
		})
	}
	h := newHost(t)
	k, err := h.e.CheckFreeDisk(context.Background(), ref101)
	if err != nil || k != "sdb" {
		t.Fatalf("исправный диск: %q %v", k, err)
	}
}

func createParams(integrity bool) CreateParams {
	mode := ""
	if integrity {
		mode = IntegrityBitmap
	}
	return CreateParams{Pool: "p1", Integrity: integrity, IntegrityMode: mode, Disks: []DiskRef{ref101, ref102},
		Arrays: []ArraySpec{{Level: LevelRAID1, SliceBytes: gib - PartitionReserve, Members: []string{ref101.ID, ref102.ID}}}}
}

func TestCreateCommands(t *testing.T) {
	h := newHost(t)
	res, err := h.e.Create(context.Background(), createParams(true))
	if err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	in1, in2 := IntegrityName("p1", 0, ref101.ID), IntegrityName("p1", 0, ref102.ID)
	// Раздел 1022 МиБ: обнуляются первые 256 МиБ и последние 16 МиБ.
	zero := func(part string) []string {
		return []string{
			"blkdiscard --zeroout --offset 0 --length 268435456 " + dev + "/" + part,
			"blkdiscard --zeroout --offset 1054867456 --length 16777216 " + dev + "/" + part,
		}
	}
	want := []string{"sgdisk -n 1:2048:2095103 -t 1:FD00 -c 1:dn-p1-0 " + dev + "/sdb"}
	want = append(want, zero("sdb1")...)
	want = append(want, "sgdisk -n 1:2048:2095103 -t 1:FD00 -c 1:dn-p1-0 "+dev+"/sdc")
	want = append(want, zero("sdc1")...)
	want = append(want,
		// Без полной записи: --no-wipe + фоновый пересчёт тегов, режим bitmap.
		"integritysetup format --batch-mode --no-wipe --sector-size 4096 --integrity crc32c "+dev+"/sdb1",
		"integritysetup open --integrity crc32c --integrity-recalculate --integrity-bitmap-mode "+dev+"/sdb1 "+in1,
		"integritysetup format --batch-mode --no-wipe --sector-size 4096 --integrity crc32c "+dev+"/sdc1",
		"integritysetup open --integrity crc32c --integrity-recalculate --integrity-bitmap-mode "+dev+"/sdc1 "+in2,
		"mdadm --create "+dev+"/md/p1-0 --run --level=1 --raid-devices=2 --metadata=1.2 --bitmap=internal --name=p1-0 "+
			dev+"/mapper/"+in1+" "+dev+"/mapper/"+in2,
		"lvm pvcreate -q -y "+dev+"/md/p1-0",
		"lvm vgcreate -q dn_p1 "+dev+"/md/p1-0",
		"lvm lvcreate -q -y -l 100%FREE -n data dn_p1",
		"mkfs.btrfs -q -L p1 -d single -m single "+dev+"/dn_p1/data",
		"mount -t btrfs "+dev+"/dn_p1/data "+h.p("mnt/p1"),
	)
	got := writes(h.commands())
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("команды:\n%s\nожидались:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if res.Mount != h.p("mnt/p1") || len(res.Commands) == 0 {
		t.Fatalf("результат %+v", res)
	}
}

func TestCreateWithoutIntegrityUsesDupMetadata(t *testing.T) {
	h := newHost(t)
	if _, err := h.e.Create(context.Background(), createParams(false)); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(h.commands(), "\n")
	if strings.Contains(all, "integritysetup") || !strings.Contains(all, "-m dup") ||
		!strings.Contains(all, h.p("dev")+"/sdb1 "+h.p("dev")+"/sdc1") {
		t.Fatalf("команды:\n%s", all)
	}
}

// Отказ проверки любого диска — до первой записи на любой диск.
func TestCreateRefusesBeforeAnyWrite(t *testing.T) {
	for name, setup := range map[string]func(h *fakeHost){
		"второй диск с сигнатурой":  func(h *fakeHost) { h.sigs[h.p("dev/sdc")] = "sdc 0x0 xfs" },
		"второй диск вне allowlist": func(h *fakeHost) { h.e.AllowIDs = []string{ref101.ID} },
		"VG уже есть":               func(h *fakeHost) { h.vgs = []string{"dn_p1"} },
		"массив уже есть":           func(h *fakeHost) { h.write("dev/md127", ""); h.link("../md127", "dev/md/p1-0") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			setup(h)
			_, err := h.e.Create(context.Background(), createParams(true))
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("ожидался отказ, %v", err)
			}
			if w := writes(h.commands()); len(w) != 0 {
				t.Fatalf("записи при отказе: %v", w)
			}
		})
	}
}

func TestCreateFailureStopsAndReports(t *testing.T) {
	h := newHost(t)
	h.failOn = "mdadm --create"
	res, err := h.e.Create(context.Background(), createParams(false))
	if err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "искусственный сбой") {
		t.Fatalf("ошибка %v", err)
	}
	if strings.Contains(strings.Join(h.commands(), "\n"), "pvcreate") || len(res.Commands) == 0 {
		t.Fatalf("после сбоя продолжено: %v", h.commands())
	}
}

// existingPool — пул p1: md127 (raid1/raid5) из разделов sdb1, sdc1[, sdd1].
func existingPool(h *fakeHost, mdstat string, members ...string) {
	var slaves []string
	for _, m := range members {
		slaves = append(slaves, h.part(m, 1))
	}
	h.virt("md127", slaves...)
	h.link("../md127", "dev/md/p1-0")
	h.write("proc/mdstat", mdstat)
	h.vgs = []string{"dn_p1"}
	h.pvs[h.p("dev/md127")] = "dn_p1"
}

const mdstatRAID1 = `Personalities : [raid1]
md127 : active raid1 sdc1[1] sdb1[0]
      1045504 blocks super 1.2 [2/2] [UU]

unused devices: <none>
`

func TestAddDiskCommands(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	p := AddDiskParams{Pool: "p1", Disk: ref103, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: gib - PartitionReserve}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	want := []string{
		"sgdisk -n 1:2048:2095103 -t 1:FD00 -c 1:dn-p1-0 " + dev + "/sdd",
		"blkdiscard --zeroout --offset 0 --length 268435456 " + dev + "/sdd1",
		"blkdiscard --zeroout --offset 1054867456 --length 16777216 " + dev + "/sdd1",
		"mdadm " + dev + "/md/p1-0 --add " + dev + "/sdd1",
		"mdadm --grow " + dev + "/md/p1-0 --level=5",
		"mdadm --grow " + dev + "/md/p1-0 --raid-devices=3",
	}
	if got := writes(h.commands()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
	}
}

func TestAddDiskRefusals(t *testing.T) {
	p := AddDiskParams{Pool: "p1", Disk: ref103, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: gib - PartitionReserve}}}
	cases := map[string]struct {
		mdstat string
		setup  func(h *fakeHost)
		p      AddDiskParams
		want   string
	}{
		"идёт resync":             {strings.Replace(mdstatRAID1, "[UU]\n", "[UU]\n      [>....]  resync =  2.1% (1/2) finish=0.7min speed=1K/sec\n", 1), nil, p, "покое"},
		"деградирован":            {strings.Replace(mdstatRAID1, "[2/2] [UU]", "[2/1] [U_]", 1), nil, p, "покое"},
		"диск пула вне allowlist": {mdstatRAID1, func(h *fakeHost) { h.e.AllowIDs = []string{ref103.ID} }, p, "не входит"},
		"integrity не совпадает":  {mdstatRAID1, nil, AddDiskParams{Pool: "p1", Integrity: true, Disk: ref103, Arrays: p.Arrays}, "integrity"},
		"неверное число участников": {mdstatRAID1, nil, AddDiskParams{Pool: "p1", Disk: ref103,
			Arrays: []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 4, SliceBytes: gib - PartitionReserve}}}, "raid_devices"},
		"неподдерживаемый переход": {mdstatRAID1, nil, AddDiskParams{Pool: "p1", Disk: ref103,
			Arrays: []ExpandArray{{Index: 0, Level: LevelRAID6, RaidDevices: 3, SliceBytes: gib - PartitionReserve}}}, "не поддерживается"},
		"новый диск занят": {mdstatRAID1, func(h *fakeHost) { h.sigs[h.p("dev/sdd")] = "sdd 0x0 ext4" }, p, "сигнатуры"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			existingPool(h, c.mdstat, "sdb", "sdc")
			if c.setup != nil {
				c.setup(h)
			}
			_, err := h.e.AddDisk(context.Background(), c.p)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ожидался отказ «%s»: %v", c.want, err)
			}
			if w := writes(h.commands()); len(w) != 0 {
				t.Fatalf("записи при отказе: %v", w)
			}
		})
	}
}

func TestReplaceCommands(t *testing.T) {
	h := newHost(t)
	// raid5 из трёх, sdd отказал и удалён: в mdstat два участника.
	existingPool(h, `Personalities : [raid5]
md127 : active raid5 sdc1[1] sdb1[0]
      2091008 blocks super 1.2 level 5, 64k chunk, algorithm 2 [3/2] [UU_]

unused devices: <none>
`, "sdb", "sdc")
	p := ReplaceParams{Pool: "p1", Disk: ref103, Arrays: []ReplaceArray{{Index: 0, SliceBytes: gib - PartitionReserve}}}
	if _, err := h.e.Replace(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	want := []string{
		"mdadm " + dev + "/md/p1-0 --remove failed",
		"mdadm " + dev + "/md/p1-0 --remove detached",
		"sgdisk -n 1:2048:2095103 -t 1:FD00 -c 1:dn-p1-0 " + dev + "/sdd",
		"blkdiscard --zeroout --offset 0 --length 268435456 " + dev + "/sdd1",
		"blkdiscard --zeroout --offset 1054867456 --length 16777216 " + dev + "/sdd1",
		"mdadm " + dev + "/md/p1-0 --add " + dev + "/sdd1",
	}
	if got := writes(h.commands()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
	}

	// Исправный массив не заменяется.
	h2 := newHost(t)
	existingPool(h2, mdstatRAID1, "sdb", "sdc")
	if _, err := h2.e.Replace(context.Background(), p); !errors.Is(err, ErrRefused) || len(writes(h2.commands())) != 0 {
		t.Fatalf("замена в исправном массиве: %v %v", err, h2.commands())
	}
}

func TestGrowRefusesDuringReshape(t *testing.T) {
	h := newHost(t)
	existingPool(h, `md127 : active raid5 sdd1[2] sdc1[1] sdb1[0]
      1045504 blocks super 1.2 level 5, 64k chunk, algorithm 2 [3/3] [UUU]
      [>....]  reshape =  1.9% (1/2) finish=1.6min speed=10050K/sec

`, "sdb", "sdc", "sdd")
	if _, err := h.e.Grow(context.Background(), PoolParams{Pool: "p1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("ожидался отказ: %v", err)
	}
	h.write("proc/mdstat", "md127 : active raid5 sdd1[2] sdc1[1] sdb1[0]\n      2091008 blocks super 1.2 level 5, 64k chunk, algorithm 2 [3/3] [UUU]\n\n")
	if _, err := h.e.Grow(context.Background(), PoolParams{Pool: "p1"}); err != nil {
		t.Fatal(err)
	}
	// vg_free = 0 в подмене: lvextend не вызывается; не смонтирован — без btrfs resize.
	if got := writes(h.commands()); len(got) != 1 || !strings.HasPrefix(got[0], "lvm pvresize") {
		t.Fatalf("команды %v", got)
	}
}

func TestStatusDegraded(t *testing.T) {
	h := newHost(t)
	existingPool(h, sample(t, "mdstat-faulty.txt"), "sdb", "sdc")
	detail := sample(t, "detail-raid5-faulty.txt")
	h.e.Runner = runnerFunc(func(ctx context.Context, to time.Duration, path string, args ...string) (CmdResult, error) {
		if filepath.Base(path) == "mdadm" && args[0] == "--detail" {
			return CmdResult{Stdout: []byte(detail)}, nil
		}
		return h.Run(ctx, to, path, args...)
	})
	st, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Health != HealthDegraded || len(st.Arrays) != 1 || st.Arrays[0].Missing != 1 || st.Arrays[0].FailedDevices != 1 {
		t.Fatalf("%+v", st)
	}
	if strings.Join(st.DiskIDs, ",") != ref101.ID+","+ref102.ID {
		t.Fatalf("диски %v", st.DiskIDs)
	}
	if st.Mounted || len(st.Issues) == 0 {
		t.Fatalf("не смонтированный пул должен давать issue: %+v", st.Issues)
	}
	if w := writes(h.commands()); len(w) != 0 {
		t.Fatalf("Status выполнил записи: %v", w)
	}
}

func TestHealth(t *testing.T) {
	p := func(v float64) *float64 { return &v }
	cases := []struct {
		arrays []ArrayStatus
		want   string
	}{
		{[]ArrayStatus{{Level: "raid1", State: []string{"clean"}}}, HealthOK},
		{[]ArrayStatus{{Level: "raid1", Action: "resync", Progress: p(3)}}, HealthResyncing},
		{[]ArrayStatus{{Level: "raid5", Action: "reshape"}}, HealthReshaping},
		{[]ArrayStatus{{Level: "raid5", Missing: 1}}, HealthDegraded},
		{[]ArrayStatus{{Level: "raid5", Missing: 1, Action: "recovery"}}, HealthRebuilding},
		{[]ArrayStatus{{Level: "raid5", State: []string{"inactive"}}}, HealthFailed},
		{[]ArrayStatus{{Level: "raid1"}, {Level: "raid5", Missing: 1}}, HealthDegraded},
	}
	for i, c := range cases {
		if got := health(PoolStatus{Arrays: c.arrays}, true); got != c.want {
			t.Errorf("%d: %s, ожидалось %s", i, got, c.want)
		}
	}
	// Пересчёт тегов dm-integrity при исправном массиве — защита неполная.
	rs := int64(512)
	recalc := PoolStatus{Arrays: []ArrayStatus{{Level: "raid1", State: []string{"clean"}}},
		Integrity: []IntegrityStatus{{Name: "a", ProvidedBytes: 1024, RecalcBytes: &rs, Recalculating: true}}}
	if got := health(recalc, true); got != HealthResyncing {
		t.Errorf("пересчёт тегов: %s", got)
	}
	recalc.Integrity[0].Recalculating = false
	if got := health(recalc, true); got != HealthOK {
		t.Errorf("пересчёт завершён: %s", got)
	}
	if health(PoolStatus{}, false) != HealthMissing {
		t.Error("отсутствующий пул")
	}
}

type runnerFunc func(ctx context.Context, timeout time.Duration, path string, args ...string) (CmdResult, error)

func (f runnerFunc) Run(ctx context.Context, timeout time.Duration, path string, args ...string) (CmdResult, error) {
	return f(ctx, timeout, path, args...)
}

func TestParamsValidate(t *testing.T) {
	good := createParams(true)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(p *CreateParams){
		"имя":              func(p *CreateParams) { p.Pool = "P1;rm" },
		"идентичность":     func(p *CreateParams) { p.Disks[0].ID = "../sda" },
		"уровень":          func(p *CreateParams) { p.Arrays[0].Level = "raid7" },
		"мало участников":  func(p *CreateParams) { p.Arrays[0].Level = LevelRAID5 },
		"не кратно 1 МиБ":  func(p *CreateParams) { p.Arrays[0].SliceBytes-- },
		"не помещается":    func(p *CreateParams) { p.Arrays[0].SliceBytes = gib },
		"чужой участник":   func(p *CreateParams) { p.Arrays[0].Members[1] = ref103.ID },
		"повтор участника": func(p *CreateParams) { p.Arrays[0].Members[1] = ref101.ID },
		"лишний диск":      func(p *CreateParams) { p.Disks = append(p.Disks, ref103) },
		"размер диска":     func(p *CreateParams) { p.Disks[0].SizeBytes = 1000 },
		"режим integrity":  func(p *CreateParams) { p.IntegrityMode = "direct" },
		"нет режима":       func(p *CreateParams) { p.IntegrityMode = "" },
		"режим без integrity": func(p *CreateParams) {
			p.Integrity = false
		},
	}
	for name, mod := range bad {
		p := createParams(true)
		p.Disks = append([]DiskRef{}, p.Disks...)
		p.Arrays = []ArraySpec{{Level: p.Arrays[0].Level, SliceBytes: p.Arrays[0].SliceBytes, Members: append([]string{}, p.Arrays[0].Members...)}}
		mod(&p)
		if p.Validate() == nil {
			t.Errorf("%s: ожидалась ошибка", name)
		}
	}
	if (ReplaceParams{Pool: "p1", Disk: ref103, Arrays: []ReplaceArray{{Index: 0, SliceBytes: 2 * gib}}}).Validate() == nil {
		t.Error("замена: раздел больше диска")
	}
	if (AddDiskParams{Pool: "p1", Disk: ref103, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID0, RaidDevices: 3, SliceBytes: 64 << 20}}}).Validate() == nil {
		t.Error("расширение raid0 должно отклоняться")
	}
}

// Без allowlist изменяющие операции не обращаются к системе вовсе.
// Пустой allowlist — без ограничения списком; защиту дают
// проверки перед записью (системный диск, принадлежность пулу).
func TestEmptyAllowlistNoRestriction(t *testing.T) {
	ctx := context.Background()
	h := newHost(t)
	h.e.AllowIDs = nil
	if _, err := h.e.CheckFreeDisk(ctx, ref101); err != nil {
		t.Fatalf("свободный диск без allowlist: %v", err)
	}
	if _, err := h.e.Create(ctx, createParams(false)); err != nil {
		t.Fatalf("создание без allowlist: %v", err)
	}
	h2 := newHost(t)
	h2.e.AllowIDs = nil
	sys := createParams(false)
	sys.Disks[0] = DiskRef{ID: "wwn-0x5000000000000000", SizeBytes: sys.Disks[0].SizeBytes}
	if _, err := h2.e.Create(ctx, sys); !errors.Is(err, ErrRefused) {
		t.Fatalf("системный диск без allowlist: %v", err)
	}
	if _, err := h2.e.Grow(ctx, PoolParams{Pool: "nope"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("рост несуществующего пула: %v", err)
	}
	if w := writes(h2.commands()); len(w) != 0 {
		t.Fatalf("запись при отказе: %v", w)
	}
}

// Пул сразу после создания без полной записи: массив исправен, у одного
// участника идёт пересчёт тегов — защита неполная (resyncing), режим и
// граница пересчёта видны в состоянии. Образцы dmsetup — с тестовой машины.
func TestStatusIntegrityRecalculating(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	a, b := IntegrityName("p1", 0, ref101.ID), IntegrityName("p1", 0, ref102.ID)
	h.dmStat = a + ": 0 2074928 integrity 0 2074928 884736\n" + b + ": 0 2074928 integrity 0 2074928 2074928\nother: 0 8 integrity 0 8 -\n"
	h.dmTable = a + ": 0 2074928 integrity 8:33 0 4 B 10 block_size:4096 recalculate journal_sectors:16072 interleave_sectors:32768 buffer_sectors:32 sectors_per_bit:32768 bitmap_flush_interval:10000 fix_padding fix_hmac internal_hash:crc32c\n" +
		b + ": 0 2074928 integrity 8:65 0 4 B 10 block_size:4096 recalculate journal_sectors:16072 interleave_sectors:32768 buffer_sectors:32 sectors_per_bit:32768 bitmap_flush_interval:10000 fix_padding fix_hmac internal_hash:crc32c\n"
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
	if st.Health != HealthResyncing || len(st.Integrity) != 2 {
		t.Fatalf("%s %+v", st.Health, st.Integrity)
	}
	if !st.Integrity[0].Recalculating || st.Integrity[0].Mode != IntegrityBitmap || st.Integrity[1].Recalculating || st.Integrity[1].Mode != IntegrityBitmap {
		t.Fatalf("%+v", st.Integrity)
	}
	if len(st.Arrays) != 1 || st.Arrays[0].Bitmap != "internal" {
		t.Fatalf("bitmap md: %+v", st.Arrays)
	}
}

// inOrder проверяет, что want встречаются в got по порядку.
func inOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	k := 0
	for _, w := range want {
		for k < len(got) && got[k] != w {
			k++
		}
		if k == len(got) {
			t.Fatalf("нет или не по порядку %q:\n%s", w, strings.Join(got, "\n"))
		}
	}
}

// Сбой на последнем шаге — весь стек разбирается в обратном порядке.
func TestCreateRollbackFullStack(t *testing.T) {
	h := newHost(t)
	h.failOn = "mount -t btrfs"
	_, err := h.e.Create(context.Background(), createParams(true))
	if err == nil || !strings.Contains(err.Error(), "частично созданные объекты удалены") {
		t.Fatalf("ошибка %v", err)
	}
	dev := h.p("dev")
	in1, in2 := IntegrityName("p1", 0, ref101.ID), IntegrityName("p1", 0, ref102.ID)
	got := writes(h.commands())
	inOrder(t, got,
		"mount -t btrfs "+dev+"/dn_p1/data "+h.p("mnt/p1"),
		"lvm vgremove -q -f -y dn_p1",
		"lvm pvremove -q -y -ff "+dev+"/md/p1-0",
		"mdadm --stop "+dev+"/md/p1-0",
		"mdadm --zero-superblock "+dev+"/mapper/"+in2,
		"integritysetup close "+in2,
		"integritysetup close "+in1,
		"sgdisk --zap-all "+dev+"/sdc",
		"wipefs -q -a "+dev+"/sdc",
		"sgdisk --zap-all "+dev+"/sdb",
		"wipefs -q -a "+dev+"/sdb",
	)
	if _, err := os.Stat(h.p("mnt/p1")); err == nil {
		t.Fatal("точка монтирования не удалена")
	}
	for _, c := range got {
		if strings.Contains(c, "/sda") {
			t.Fatalf("тронут системный диск: %s", c)
		}
	}
}

// Сбой на mdadm --create — откат разделов, LVM не трогается.
func TestCreateRollbackEarly(t *testing.T) {
	h := newHost(t)
	h.failOn = "mdadm --create"
	_, err := h.e.Create(context.Background(), createParams(false))
	if err == nil || !strings.Contains(err.Error(), "искусственный сбой") {
		t.Fatalf("ошибка %v", err)
	}
	dev := h.p("dev")
	got := writes(h.commands())
	inOrder(t, got, "mdadm --zero-superblock "+dev+"/sdb1", "sgdisk --zap-all "+dev+"/sdc", "sgdisk --zap-all "+dev+"/sdb")
	if j := strings.Join(got, "\n"); strings.Contains(j, "vgremove") || strings.Contains(j, "integritysetup") {
		t.Fatalf("лишние шаги отката:\n%s", j)
	}
}

// Ошибки целостности видны в состоянии пула,
// health не меняют (ошибка могла быть исправлена копией md).
func TestStatusIntegrityErrors(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	a, b := IntegrityName("p1", 0, ref101.ID), IntegrityName("p1", 0, ref102.ID)
	h.dmStat = a + ": 0 2074928 integrity 2 2074928 -\n" + b + ": 0 2074928 integrity 0 2074928 -\n"
	base, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if base.IntegrityErrors != 2 || len(base.Issues) == 0 ||
		!strings.Contains(base.Issues[len(base.Issues)-1], "dm-integrity "+a+"=2") {
		t.Fatalf("ошибки %d, issues %v", base.IntegrityErrors, base.Issues)
	}
	h.dmStat = a + ": 0 2074928 integrity 0 2074928 -\n" + b + ": 0 2074928 integrity 0 2074928 -\n"
	clean, err := h.e.Status(context.Background(), PoolParams{Pool: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if clean.IntegrityErrors != 0 || clean.Health != base.Health {
		t.Fatalf("без ошибок: %d %s, с ошибками health %s", clean.IntegrityErrors, clean.Health, base.Health)
	}
	for _, i := range clean.Issues {
		if strings.Contains(i, "целостности") {
			t.Fatalf("лишнее предупреждение: %v", clean.Issues)
		}
	}
}

func TestNoteIntegrityErrorsBtrfs(t *testing.T) {
	s := PoolStatus{BtrfsErrors: map[string]int64{"read_io_errs": 1, "corruption_errs": 3, "write_io_errs": 0}}
	noteIntegrityErrors(&s)
	if s.IntegrityErrors != 4 || len(s.Issues) != 1 ||
		!strings.Contains(s.Issues[0], "btrfs corruption_errs=3, btrfs read_io_errs=1") {
		t.Fatalf("%d %v", s.IntegrityErrors, s.Issues)
	}
}
