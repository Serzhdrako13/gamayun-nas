package disks

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeSys — искусственное дерево sysfs/proc/dev в t.TempDir().
// Диски — каталоги в class/block, разделы — подкаталоги диска (как в реальном
// sysfs) плюс ссылка в class/block; md/dm — каталоги без device/.
type fakeSys struct {
	t    *testing.T
	root string
	p    Paths
}

func newFake(t *testing.T) *fakeSys {
	t.Helper()
	root := t.TempDir()
	f := &fakeSys{t: t, root: root, p: Paths{
		SysClassBlock: filepath.Join(root, "sys/class/block"),
		SysDevBlock:   filepath.Join(root, "sys/dev/block"),
		SysFsBtrfs:    filepath.Join(root, "sys/fs/btrfs"),
		SysModule:     filepath.Join(root, "sys/module"),
		Mountinfo:     filepath.Join(root, "proc/mountinfo"),
		Swaps:         filepath.Join(root, "proc/swaps"),
		Dev:           filepath.Join(root, "dev"),
		Stat: func(string) (FileStat, error) {
			return FileStat{}, errors.New("stat не задан в тесте")
		},
	}}
	for _, d := range []string{f.p.SysClassBlock, f.p.SysDevBlock, f.p.SysModule, filepath.Dir(f.p.Mountinfo), f.p.Dev} {
		f.mkdir(d)
	}
	return f
}

func (f *fakeSys) mkdir(p string) {
	f.t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSys) write(p, s string) {
	f.t.Helper()
	f.mkdir(filepath.Dir(p))
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSys) link(target, at string) {
	f.t.Helper()
	f.mkdir(filepath.Dir(at))
	if err := os.Symlink(target, at); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSys) block(name string) string { return filepath.Join(f.p.SysClassBlock, name) }

// disk создаёт физический диск (rotational, size в секторах).
func (f *fakeSys) disk(name, rotational, size string) string {
	d := f.block(name)
	f.mkdir(filepath.Join(d, "device"))
	f.write(filepath.Join(d, "queue", "rotational"), rotational)
	f.write(filepath.Join(d, "size"), size)
	f.write(filepath.Join(d, "device", "model"), "Test disk\n")
	return d
}

// part создаёт раздел внутри диска и ссылку на него в class/block.
func (f *fakeSys) part(disk, name string) string {
	p := filepath.Join(disk, name)
	f.write(filepath.Join(p, "partition"), "1")
	f.link(p, f.block(name))
	return p
}

// virt создаёт md/dm-устройство с slaves и выставляет holders у нижних узлов.
func (f *fakeSys) virt(name string, slaves ...string) string {
	v := f.block(name)
	f.mkdir(filepath.Join(v, "slaves"))
	f.mkdir(filepath.Join(v, "holders"))
	for _, s := range slaves {
		f.link(s, filepath.Join(v, "slaves", filepath.Base(s)))
		f.mkdir(filepath.Join(s, "holders"))
		f.link(v, filepath.Join(s, "holders", name))
	}
	return v
}

func (f *fakeSys) devnum(num, node string) { f.link(node, filepath.Join(f.p.SysDevBlock, num)) }

// devnode создаёт /dev/<kernel> и необязательные ссылки на него (/dev/mapper/…).
func (f *fakeSys) devnode(kernel string, aliases ...string) {
	f.write(filepath.Join(f.p.Dev, kernel), "")
	for _, a := range aliases {
		rel, _ := filepath.Rel(filepath.Dir(filepath.Join(f.p.Dev, a)), filepath.Join(f.p.Dev, kernel))
		f.link(rel, filepath.Join(f.p.Dev, a))
	}
}

func (f *fakeSys) btrfs(fsid string, nodes ...string) {
	f.mkdir(filepath.Join(f.p.SysFsBtrfs, fsid, "devices"))
	f.mkdir(filepath.Join(f.p.SysFsBtrfs, "features"))
	for _, n := range nodes {
		f.link(n, filepath.Join(f.p.SysFsBtrfs, fsid, "devices", filepath.Base(n)))
	}
}

func (f *fakeSys) mounts(lines ...string) {
	f.write(f.p.Mountinfo, strings.Join(lines, "\n")+"\n")
}

func (f *fakeSys) observe() Snapshot {
	f.t.Helper()
	s, err := Observe(f.p)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func roles(s Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, d := range s.Disks {
		out[d.KernelName] = d.SystemRoles
	}
	return out
}

func summaries(s Snapshot) map[string]string {
	out := map[string]string{}
	for _, d := range s.Disks {
		out[d.KernelName] = d.SystemRole
	}
	return out
}

const btrfsFSID = "0b6a5f1e-1111-4c2a-9a0e-2f4f3c1d7e01"

// TestScenarios — целевой стек: разделы → [dm-integrity] → md → LVM → Btrfs.
func TestScenarios(t *testing.T) {
	cases := []struct {
		name     string
		build    func(f *fakeSys)
		roles    map[string][]string
		summary  map[string]string
		complete bool
	}{
		{
			name: "ext4 на разделе",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				f.disk("sdb", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				f.mounts("22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw")
			},
			roles:    map[string][]string{"sda": {"root"}, "sdb": {}},
			summary:  map[string]string{"sda": "root", "sdb": "unknown"},
			complete: true,
		},
		{
			name: "root btrfs на md raid1 из разделов двух дисков",
			build: func(f *fakeSys) {
				sda, sdb := f.disk("sda", "0", "100"), f.disk("sdb", "0", "100")
				f.disk("sdc", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				f.part(sdb, "sdb1")
				md0 := f.virt("md0", f.part(sda, "sda2"), f.part(sdb, "sdb2"))
				f.devnode("md0", "md/system")
				f.btrfs(btrfsFSID, md0)
				f.mounts(
					"26 1 0:25 /@ / rw,noatime - btrfs /dev/md/system rw,subvolid=256,subvol=/@",
					"30 26 8:1 / /boot/efi rw - vfat /dev/sda1 rw",
				)
			},
			roles: map[string][]string{
				"sda": {"efi", "root", "stack_member"},
				"sdb": {"root", "stack_member"},
				"sdc": {},
			},
			summary:  map[string]string{"sda": "root", "sdb": "root", "sdc": "unknown"},
			complete: true,
		},
		{
			name: "btrfs на LVM поверх md",
			build: func(f *fakeSys) {
				sda, sdb := f.disk("sda", "1", "100"), f.disk("sdb", "1", "100")
				f.disk("sdc", "1", "100")
				md1 := f.virt("md1", f.part(sda, "sda3"), f.part(sdb, "sdb3"))
				dm0 := f.virt("dm-0", md1)
				f.devnode("dm-0", "mapper/vg1-volume_1")
				f.btrfs(btrfsFSID, dm0)
				f.mounts(
					"1 0 254:1 / / rw - ext4 /dev/sdc1 rw",
					"40 1 0:40 / /volume1 rw - btrfs /dev/mapper/vg1-volume_1 rw",
				)
				f.devnum("254:1", f.part(f.block("sdc"), "sdc1"))
			},
			roles: map[string][]string{
				"sda": {"mounted", "stack_member"},
				"sdb": {"mounted", "stack_member"},
				"sdc": {"root"},
			},
			summary:  map[string]string{"sda": "in_use", "sdb": "in_use", "sdc": "root"},
			complete: true,
		},
		{
			name: "dm-name без /dev: поиск по dm/name",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				dm0 := f.virt("dm-0", f.part(sda, "sda1"))
				f.write(filepath.Join(dm0, "dm", "name"), "vg0-root\n")
				f.btrfs(btrfsFSID, dm0)
				f.p.Dev = ""
				f.mounts("1 0 0:30 / / rw - btrfs /dev/mapper/vg0-root rw")
			},
			roles:    map[string][]string{"sda": {"root", "stack_member"}},
			summary:  map[string]string{"sda": "root"},
			complete: true,
		},
		{
			name: "многодисковый btrfs без md",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				sdb, sdc := f.disk("sdb", "1", "100"), f.disk("sdc", "1", "100")
				f.disk("sdd", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				f.devnode("sdb")
				f.btrfs(btrfsFSID, sdb, sdc)
				f.btrfs("aaaaaaaa-0000-0000-0000-000000000000", f.block("nonexistent"))
				f.mounts(
					"1 0 8:1 / / rw - ext4 /dev/sda1 rw",
					"50 1 0:50 / /mnt/data rw - btrfs /dev/sdb rw",
				)
			},
			roles:    map[string][]string{"sda": {"root"}, "sdb": {"mounted"}, "sdc": {"mounted"}, "sdd": {}},
			summary:  map[string]string{"sdb": "in_use", "sdc": "in_use", "sdd": "unknown"},
			complete: true,
		},
		{
			name: "dm-integrity под md",
			build: func(f *fakeSys) {
				sda, sdb := f.disk("sda", "1", "100"), f.disk("sdb", "1", "100")
				i1 := f.virt("dm-1", f.part(sda, "sda2"))
				i2 := f.virt("dm-2", f.part(sdb, "sdb2"))
				md0 := f.virt("md0", i1, i2)
				f.btrfs(btrfsFSID, md0)
				f.mounts("1 0 0:28 /@ / rw - btrfs /dev/md0 rw")
			},
			roles:    map[string][]string{"sda": {"root", "stack_member"}, "sdb": {"root", "stack_member"}},
			complete: true,
		},
		{
			name: "swap на md",
			build: func(f *fakeSys) {
				sda, sdb := f.disk("sda", "1", "100"), f.disk("sdb", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				md1 := f.virt("md1", f.part(sda, "sda2"), f.part(sdb, "sdb2"))
				f.devnum("9:1", md1)
				f.mounts("1 0 8:1 / / rw - ext4 /dev/sda1 rw")
				f.write(f.p.Swaps, "Filename Type Size Used Priority\n/dev/md1 partition 1 0 -2\n")
				f.p.Stat = func(p string) (FileStat, error) {
					if p == "/dev/md1" {
						return FileStat{Block: true, Rdev: DevNum{9, 1}}, nil
					}
					return FileStat{}, os.ErrNotExist
				}
			},
			roles:    map[string][]string{"sda": {"root", "stack_member", "swap"}, "sdb": {"stack_member", "swap"}},
			summary:  map[string]string{"sda": "root", "sdb": "system"},
			complete: true,
		},
		{
			name: "swap-файл на btrfs (анонимный st_dev)",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				f.disk("sdb", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				sdb2 := f.part(f.block("sdb"), "sdb2")
				f.btrfs(btrfsFSID, sdb2)
				f.mounts(
					"1 0 8:1 / / rw - ext4 /dev/sda1 rw",
					"60 1 0:60 / /swap rw - btrfs /dev/sdb2 rw",
				)
				f.write(f.p.Swaps, "Filename Type Size Used Priority\n/swap/swapfile file 1 0 -2\n")
				f.p.Stat = func(string) (FileStat, error) { return FileStat{Dev: DevNum{0, 60}}, nil }
			},
			roles:    map[string][]string{"sda": {"root"}, "sdb": {"mounted", "swap"}},
			summary:  map[string]string{"sdb": "system"},
			complete: true,
		},
		{
			name: "zram swap не считается диском и не неясность",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				f.mounts("1 0 8:1 / / rw - ext4 /dev/sda1 rw")
				f.write(f.p.Swaps, "Filename Type Size Used Priority\n/dev/zram0 partition 1 0 100\n")
			},
			roles:    map[string][]string{"sda": {"root"}},
			complete: true,
		},
		{
			name: "zfs: диски не свободны, снимок неполный",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				f.disk("sdb", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				f.mounts(
					"1 0 8:1 / / rw - ext4 /dev/sda1 rw",
					"70 1 0:70 / /tank rw - zfs tank rw,xattr",
				)
			},
			roles:    map[string][]string{"sda": {"root"}, "sdb": {}},
			summary:  map[string]string{"sdb": "unknown"},
			complete: false,
		},
		{
			name: "root на zfs",
			build: func(f *fakeSys) {
				f.disk("sda", "1", "100")
				f.mounts("1 0 0:23 / / rw - zfs rpool/ROOT/debian rw")
			},
			roles:    map[string][]string{"sda": {}},
			summary:  map[string]string{"sda": "unknown"},
			complete: false,
		},
		{
			name: "модуль zfs загружен без монтирований",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				f.mounts("1 0 8:1 / / rw - ext4 /dev/sda1 rw")
				f.mkdir(filepath.Join(f.p.SysModule, "zfs"))
			},
			roles:    map[string][]string{"sda": {"root"}},
			complete: false,
		},
		{
			name: "пустой mountinfo",
			build: func(f *fakeSys) {
				f.disk("sda", "1", "100")
				f.mounts("")
			},
			roles:    map[string][]string{"sda": {}},
			summary:  map[string]string{"sda": "unknown"},
			complete: false,
		},
		{
			name: "отсутствующий mountinfo",
			build: func(f *fakeSys) {
				f.disk("sda", "1", "100")
			},
			roles:    map[string][]string{"sda": {}},
			complete: false,
		},
		{
			name: "битые строки mountinfo пропускаются с неясностью",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				f.mounts(
					"garbage",
					"2 1 8:1 / /x rw ext4 /dev/sda1 rw",
					"3 1 x:y / /y rw - ext4 /dev/sda1 rw",
					"4 1 8:1 / /z rw -",
					"1 0 8:1 / / rw - ext4 /dev/sda1 rw",
				)
			},
			roles:    map[string][]string{"sda": {"root"}},
			complete: false,
		},
		{
			name: "дубли: подтома btrfs и bind-монтирование",
			build: func(f *fakeSys) {
				sda, sdb := f.disk("sda", "1", "100"), f.disk("sdb", "1", "100")
				md0 := f.virt("md0", f.part(sda, "sda2"), f.part(sdb, "sdb2"))
				f.btrfs(btrfsFSID, md0)
				f.mounts(
					"1 0 0:30 /@ / rw - btrfs /dev/md0 rw,subvol=/@",
					"2 1 0:30 /@home /home rw - btrfs /dev/md0 rw,subvol=/@home",
					"3 1 0:30 /@ / rw - btrfs /dev/md0 rw,subvol=/@",
					"4 1 0:30 /@home/x /srv/x rw - btrfs /dev/md0 rw",
					"5 1 0:99 / /broken rw - btrfs /dev/root rw",
					"6 1 0:99 / /broken rw - btrfs /dev/root rw",
				)
			},
			roles:    map[string][]string{"sda": {"mounted", "root", "stack_member"}, "sdb": {"mounted", "root", "stack_member"}},
			complete: false, // /dev/root не сопоставлен (одна неясность, без дублей)
		},
		{
			name: "экранирование пробелов в mountinfo и swaps",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				sdb := f.disk("sdb", "1", "100")
				f.devnum("8:1", f.part(sda, "sda1"))
				sdb1 := f.part(sdb, "sdb1")
				f.devnode("sdb1", "disk/by-label/My Disk")
				f.btrfs(btrfsFSID, sdb1)
				f.mounts(
					"1 0 8:1 / / rw - ext4 /dev/sda1 rw",
					`9 1 0:41 / /mnt/my\040data rw - btrfs /dev/disk/by-label/My\040Disk rw`,
				)
				f.write(f.p.Swaps, "Filename Type Size Used Priority\n/my\\040swap file 1 0 -2\n")
				f.p.Stat = func(p string) (FileStat, error) {
					if p == "/my swap" {
						return FileStat{Dev: DevNum{8, 1}}, nil
					}
					return FileStat{}, os.ErrNotExist
				}
			},
			roles:    map[string][]string{"sda": {"root", "swap"}, "sdb": {"mounted"}},
			complete: true,
		},
		{
			name: "btrfs: источник не найден в /sys/fs/btrfs — учитывается сам, снимок неполный",
			build: func(f *fakeSys) {
				sda := f.disk("sda", "1", "100")
				f.disk("sdb", "1", "100")
				f.devnode("sda1")
				f.part(sda, "sda1")
				f.mkdir(f.p.SysFsBtrfs)
				f.mounts("1 0 0:25 / / rw - btrfs /dev/sda1 rw")
			},
			roles:    map[string][]string{"sda": {"root"}, "sdb": {}},
			complete: false,
		},
		{
			name: "root на /dev/root не сопоставлен",
			build: func(f *fakeSys) {
				f.disk("sda", "1", "100")
				f.btrfs(btrfsFSID)
				f.mounts("1 0 0:25 / / rw - btrfs /dev/root rw")
			},
			roles:    map[string][]string{"sda": {}},
			summary:  map[string]string{"sda": "unknown"},
			complete: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			c.build(f)
			s := f.observe()
			if got := roles(s); !reflect.DeepEqual(got, c.roles) {
				t.Errorf("роли: получено %v, ожидалось %v", got, c.roles)
			}
			got := summaries(s)
			for disk, want := range c.summary {
				if got[disk] != want {
					t.Errorf("%s: сводная роль %q, ожидалась %q", disk, got[disk], want)
				}
			}
			if s.Complete != c.complete {
				t.Errorf("complete=%v, ожидалось %v; issues=%+v", s.Complete, c.complete, s.Issues)
			}
			if s.Complete != (len(s.Issues) == 0) {
				t.Errorf("complete=%v не согласован с issues=%+v", s.Complete, s.Issues)
			}
			for _, d := range s.Disks {
				if d.SystemRole == SummaryUnknown && d.UnknownReason == "" {
					t.Errorf("%s: unknown без причины", d.KernelName)
				}
				if d.SystemRole != SummaryUnknown && d.UnknownReason != "" {
					t.Errorf("%s: причина при роли %s", d.KernelName, d.SystemRole)
				}
			}
			seen := map[Issue]bool{}
			for _, i := range s.Issues {
				if seen[i] {
					t.Errorf("повтор неясности %+v", i)
				}
				seen[i] = true
			}
		})
	}
}

func TestZFSIssueReported(t *testing.T) {
	f := newFake(t)
	f.disk("sda", "1", "100")
	f.mounts("1 0 0:23 / / rw - zfs rpool/ROOT/debian rw", "2 1 0:24 / /home rw - zfs rpool/home rw")
	s := f.observe()
	kinds := map[string]int{}
	for _, i := range s.Issues {
		kinds[i.Kind]++
	}
	if kinds["zfs"] != 1 {
		t.Fatalf("ожидалась одна неясность zfs: %+v", s.Issues)
	}
	if !strings.Contains(s.Disks[0].UnknownReason, "неполный") {
		t.Fatalf("причина unknown: %q", s.Disks[0].UnknownReason)
	}
}

func TestUnescape(t *testing.T) {
	cases := map[string]string{
		`/mnt/a\040b`:    "/mnt/a b",
		`a\011b`:         "a\tb",
		`a\012b`:         "a\nb",
		`a\134b`:         `a\b`,
		`a\134040`:       `a\040`,
		`tail\04`:        `tail\04`,
		`a\999`:          `a\999`,
		`no-escape`:      "no-escape",
		`\040lead\040`:   " lead ",
		`multi\040\040x`: "multi  x",
	}
	for in, want := range cases {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q)=%q, ожидалось %q", in, got, want)
		}
	}
}

func TestDecodeDev(t *testing.T) {
	// makedev(259, 65536) из glibc.
	d := uint64(259&0xfff)<<8 | uint64(259&^0xfff)<<32 | uint64(65536&0xff) | uint64(65536&^0xff)<<12
	if got := decodeDev(d); got != (DevNum{259, 65536}) {
		t.Fatalf("decodeDev: %v", got)
	}
	if got := decodeDev(8<<8 | 1); got != (DevNum{8, 1}) {
		t.Fatalf("decodeDev: %v", got)
	}
}

// ---------- порт tests/test_disks.py ----------

func TestPhysicalAndVirtualDisksWithoutPartitions(t *testing.T) {
	f := newFake(t)
	f.disk("sda", "1", "100")
	f.write(filepath.Join(f.block("sda1"), "partition"), "1")
	f.disk("vda", "0", "200")
	f.disk("loop0", "1", "100")
	f.disk("sr0", "1", "100")
	optical := f.disk("sdb", "1", "100")
	f.write(filepath.Join(optical, "device", "type"), "5")
	s := f.observe()
	var names []string
	var sizes []int64
	for _, d := range s.Disks {
		names = append(names, d.Name)
		sizes = append(sizes, *d.SizeBytes)
	}
	if !reflect.DeepEqual(names, []string{"Диск 1", "SSD 1"}) || !reflect.DeepEqual(sizes, []int64{51200, 102400}) {
		t.Fatalf("names=%v sizes=%v", names, sizes)
	}
}

func TestUnknownDataIsNotReportedAsHealthyOrZero(t *testing.T) {
	f := newFake(t)
	f.disk("sda", "?", "?")
	d := f.observe().Disks[0]
	if d.Kind != "unknown" || d.SizeBytes != nil {
		t.Fatalf("%+v", d)
	}
}

func TestMissingSourceIsAnError(t *testing.T) {
	f := newFake(t)
	f.p.SysClassBlock = filepath.Join(f.root, "missing")
	if _, err := Observe(f.p); !errors.Is(err, ErrNoSource) {
		t.Fatalf("ожидалась ErrNoSource, получено %v", err)
	}
}

func TestRootPartitionMapsToParentDisk(t *testing.T) {
	f := newFake(t)
	sda := f.disk("sda", "1", "100")
	part := filepath.Join(sda, "sda1")
	f.write(filepath.Join(part, "partition"), "1")
	f.devnum("8:1", part)
	f.mounts("1 0 8:1 / / rw - ext4 /dev/sda1 rw")
	if r := f.observe().Disks[0].SystemRole; r != "root" {
		t.Fatal(r)
	}
}

func TestDMSlavesAndUnknownDisk(t *testing.T) {
	f := newFake(t)
	sda := f.disk("sda", "1", "100")
	f.disk("sdb", "1", "100")
	dm := filepath.Join(f.p.SysClassBlock, "device-mapper")
	f.link(sda, filepath.Join(dm, "slaves", "sda"))
	f.devnum("253:0", dm)
	f.mounts("1 0 253:0 / / rw - ext4 /dev/mapper/root rw")
	s := f.observe()
	if got := []string{s.Disks[0].SystemRole, s.Disks[1].SystemRole}; !reflect.DeepEqual(got, []string{"root", "unknown"}) {
		t.Fatal(got)
	}
}

func TestUnmappableRootDoesNotClaimOtherDisksAreSafe(t *testing.T) {
	f := newFake(t)
	f.disk("sda", "1", "100")
	f.p.SysDevBlock = filepath.Join(f.root, "missing")
	f.mounts("1 0 0:1 / / rw - overlay overlay rw")
	s := f.observe()
	if s.Disks[0].SystemRole != "unknown" || s.Complete {
		t.Fatalf("%+v", s)
	}
}

func TestSeparateBootDiskIsIdentified(t *testing.T) {
	f := newFake(t)
	sda, sdb := f.disk("sda", "1", "100"), f.disk("sdb", "1", "100")
	for _, d := range []string{sda, sdb} {
		f.write(filepath.Join(d, "part1", "partition"), "1")
	}
	f.devnum("8:1", filepath.Join(sda, "part1"))
	f.devnum("8:17", filepath.Join(sdb, "part1"))
	f.mounts("1 0 8:1 / / rw - ext4 /dev/sda1 rw", "2 1 8:17 / /boot/efi rw - vfat /dev/sdb1 rw")
	s := f.observe()
	if !reflect.DeepEqual(s.Disks[0].SystemRoles, []string{"root"}) ||
		!reflect.DeepEqual(s.Disks[1].SystemRoles, []string{"efi"}) || s.Disks[1].SystemRole != "system" {
		t.Fatalf("%+v", s.Disks)
	}
}

func TestSwapPartitionAndFileAreMapped(t *testing.T) {
	f := newFake(t)
	sda := f.disk("sda", "1", "100")
	part := filepath.Join(sda, "part1")
	f.write(filepath.Join(part, "partition"), "1")
	f.devnum("8:1", part)
	f.mounts("1 0 0:1 / / rw - overlay overlay rw")
	f.write(f.p.Swaps, "Filename Type Size Used Priority\n/dev/sda1 partition 1 0 -2\n/swapfile file 1 0 -3\n")
	f.p.Stat = func(p string) (FileStat, error) {
		if p == "/dev/sda1" {
			return FileStat{Block: true, Rdev: DevNum{8, 1}}, nil
		}
		return FileStat{Dev: DevNum{8, 1}}, nil
	}
	got, _, err := SystemBackingDisks(f.p)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(sda)
	if !reflect.DeepEqual(got[real], map[string]bool{"swap": true}) {
		t.Fatalf("%v", got)
	}
}

func TestMountedDataDiskIsNotUnknown(t *testing.T) {
	f := newFake(t)
	f.disk("sda", "1", "100")
	sdb := f.disk("sdb", "1", "100")
	part := filepath.Join(sdb, "part1")
	f.write(filepath.Join(part, "partition"), "1")
	f.devnum("8:17", part)
	f.mounts("1 0 8:17 / /mnt/data rw - ext4 /dev/sdb1 rw")
	d := f.observe().Disks[1]
	if !reflect.DeepEqual(d.SystemRoles, []string{"mounted"}) || d.SystemRole != "in_use" {
		t.Fatalf("%+v", d)
	}
}

func TestDiskWithPartitionUsedByActiveStackIsInUse(t *testing.T) {
	f := newFake(t)
	sda := f.disk("sda", "1", "100")
	part := filepath.Join(sda, "sda1")
	f.write(filepath.Join(part, "partition"), "1")
	dm := filepath.Join(f.p.SysClassBlock, "dm-0")
	f.mkdir(dm)
	f.link(dm, filepath.Join(part, "holders", "dm-0"))
	f.link(part, f.block("sda1"))
	d := f.observe().Disks[0]
	if d.SystemRole != "in_use" || !reflect.DeepEqual(d.SystemRoles, []string{"stack_member"}) {
		t.Fatalf("%+v", d)
	}
	members, _, err := ActiveStackMembers(f.p)
	if err != nil || len(members) != 1 {
		t.Fatalf("members=%v err=%v", members, err)
	}
}

func TestUnreadableOrEmptyHoldersDoNotMarkDiskFree(t *testing.T) {
	f := newFake(t)
	sda := f.disk("sda", "1", "100")
	f.mkdir(filepath.Join(sda, "holders"))
	d := f.observe().Disks[0]
	if d.SystemRole != "unknown" || d.UnknownReason == "" {
		t.Fatalf("%+v", d)
	}
}

func TestIdentity(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		name, file, value, id string
		wwn                   string
	}{
		{"sda", "device/wwid", "naa.5000000000000101\n", "wwn-0x5000000000000101", "0x5000000000000101"},
		{"sdb", "device/wwid", "naa.5000C500ABCDEF01", "wwn-0x5000c500abcdef01", "0x5000c500abcdef01"},
		{"nvme0n1", "wwid", "eui.0025385B71B07E2F", "eui-0025385b71b07e2f", ""},
		{"sdc", "device/wwid", "t10.ATA     QEMU HARDDISK   QM00005", "wwid-t10.ATA_QEMU_HARDDISK_QM00005", ""},
		{"vda", "serial", "abc 123", "serial-abc_123", ""},
		{"sdd", "device/wwid", "naa.zz", "", ""},
		{"sde", "device/wwid", "t10.bad/slash", "", ""},
		{"sdf", "", "", "", ""},
	}
	for _, c := range cases {
		d := f.disk(c.name, "1", "2048")
		if c.file != "" {
			f.write(filepath.Join(d, c.file), c.value)
		}
		id, wwn := Identity(d)
		if id != c.id {
			t.Errorf("%s: id %q, ожидалось %q", c.name, id, c.id)
		}
		got := ""
		if wwn != nil {
			got = *wwn
		}
		if got != c.wwn {
			t.Errorf("%s: wwn %q, ожидалось %q", c.name, got, c.wwn)
		}
	}
	snap := f.observe()
	for _, d := range snap.Disks {
		if d.KernelName == "sda" && d.ID != "wwn-0x5000000000000101" {
			t.Errorf("Observe не заполнил ID: %+v", d)
		}
	}
}

// TestVanishedSlaveIsNotAnIssue — dm-integrity над удалённым диском
// (echo 1 > device/delete) остаётся участником md с висячей ссылкой в
// slaves. Исчезнувшее устройство не может быть текущим диском: снимок
// остаётся полным, остальные участники — stack_member.
func TestVanishedSlaveIsNotAnIssue(t *testing.T) {
	f := newFake(t)
	sdb := f.disk("sdb", "1", "2097152")
	sdb1 := f.part(sdb, "sdb1")
	gone := filepath.Join(f.root, "sys/devices/gone/sdd/sdd1") // цели нет
	dm0 := f.virt("dm-0", sdb1)
	dm1 := f.block("dm-1")
	f.mkdir(filepath.Join(dm1, "holders"))
	f.link(gone, filepath.Join(dm1, "slaves", "sdd1"))
	f.virt("md127", dm0, dm1)
	f.disk("sdc", "1", "2097152")
	sda := f.disk("sda", "0", "2097152")
	f.part(sda, "sda1")
	f.devnum("8:1", f.block("sda1"))
	f.mounts("22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw")
	s := f.observe()
	if !s.Complete || len(s.Issues) != 0 {
		t.Fatalf("снимок должен быть полным: %+v", s.Issues)
	}
	roles := map[string]string{}
	for _, d := range s.Disks {
		roles[d.KernelName] = d.SystemRole
	}
	if roles["sdb"] != SummaryInUse || roles["sdc"] != SummaryUnknown {
		t.Fatalf("роли: %v", roles)
	}

	// Обычная неразрешимая ссылка (не slaves) по-прежнему неясность.
	if vanished(filepath.Join(dm1, "nonexistent")) {
		t.Fatal("несуществующий путь — не висячая ссылка")
	}
}
