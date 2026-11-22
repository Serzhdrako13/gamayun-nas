package home

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Смена уровня при добавлении диска — RAID5 → RAID6 и
// SHR → SHR-2 (слой raid5 → raid6, слой raid1 → raid1 из 3 копий).
// raid5 → raid6 — только с --layout=preserve (без reshape раскладки): reshape
// раскладки при прерывании портит данные (mdadm 4.4, ядро 6.12).

var ref104 = DiskRef{ID: "wwn-0x5000000000000104", SizeBytes: gib}

const mdstatRAID5x3 = `Personalities : [raid6] [raid5] [raid4]
md127 : active raid5 sdd1[3] sdc1[1] sdb1[0]
      2091008 blocks super 1.2 level 5, 512k chunk, algorithm 2 [3/3] [UUU]

unused devices: <none>
`

// raid6 после перехода с --layout=preserve: раскладка left-symmetric-6 (18).
const mdstatRAID6L6x4 = `Personalities : [raid6] [raid5] [raid4]
md127 : active raid6 sde1[4] sdd1[3] sdc1[1] sdb1[0]
      2091008 blocks super 1.2 level 6, 512k chunk, algorithm 18 [4/4] [UUUU]

unused devices: <none>
`

func TestParseMdstatAlgorithm(t *testing.T) {
	for in, want := range map[string]int{mdstatRAID5x3: 2, mdstatRAID6L6x4: 18, mdstatRAID1: -1} {
		a := ParseMdstat(in)
		if len(a) != 1 || a[0].Algorithm != want {
			t.Fatalf("algorithm %+v, ожидалось %d", a, want)
		}
	}
}

func TestGrowCmds(t *testing.T) {
	ms := func(level string, n, alg int) MdstatArray {
		return MdstatArray{Level: level, RaidDevices: n, Algorithm: alg}
	}
	want := func(level string, n int) ExpandArray { return ExpandArray{Level: level, RaidDevices: n} }
	cases := []struct {
		name string
		ms   MdstatArray
		want ExpandArray
		cmds string // ";" между командами; пусто — отказ
	}{
		{"raid1 2→3", ms(LevelRAID1, 2, -1), want(LevelRAID1, 3), "--grow md --raid-devices=3"},
		{"raid5 3→4", ms(LevelRAID5, 3, 2), want(LevelRAID5, 4), "--grow md --raid-devices=4"},
		{"raid6 4→5", ms(LevelRAID6, 4, 2), want(LevelRAID6, 5), "--grow md --raid-devices=5"},
		{"raid6 (-6) 4→5", ms(LevelRAID6, 4, 18), want(LevelRAID6, 5), "--grow md --raid-devices=5 --layout=preserve"},
		{"raid6 (right-asymmetric-6) 4→5", ms(LevelRAID6, 4, 17), want(LevelRAID6, 5), "--grow md --raid-devices=5 --layout=preserve"},
		{"raid1 2 → raid5 3", ms(LevelRAID1, 2, -1), want(LevelRAID5, 3), "--grow md --level=5;--grow md --raid-devices=3"},
		{"raid5 3 → raid6 4", ms(LevelRAID5, 3, 2), want(LevelRAID6, 4), "--grow md --level=6 --raid-devices=4 --layout=preserve"},
		{"raid5 5 → raid6 6", ms(LevelRAID5, 5, 2), want(LevelRAID6, 6), "--grow md --level=6 --raid-devices=6 --layout=preserve"},
		{"raid5 3 → raid6 5", ms(LevelRAID5, 3, 2), want(LevelRAID6, 5), ""},
		{"raid5 3 → raid6 3", ms(LevelRAID5, 3, 2), want(LevelRAID6, 3), ""},
		{"raid6 → raid5", ms(LevelRAID6, 4, 2), want(LevelRAID5, 5), ""},
		{"raid1 3 → raid5 4", ms(LevelRAID1, 3, -1), want(LevelRAID5, 4), ""},
		{"raid1 → raid6", ms(LevelRAID1, 2, -1), want(LevelRAID6, 3), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := growCmds("md", c.ms, c.want)
			if c.cmds == "" {
				if err == nil || !strings.Contains(err.Error(), "не поддерживается") {
					t.Fatalf("ожидался отказ, получено %v %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var s []string
			for _, a := range got {
				s = append(s, strings.Join(a, " "))
				// Переход на raid6 — никогда без preserve (reshape раскладки).
				if strings.Contains(strings.Join(a, " "), "--level=6") && !strings.Contains(strings.Join(a, " "), "--layout=preserve") {
					t.Fatalf("--level=6 без --layout=preserve: %v", a)
				}
			}
			if strings.Join(s, ";") != c.cmds {
				t.Fatalf("команды %q, ожидалось %q", strings.Join(s, ";"), c.cmds)
			}
		})
	}
}

// addHostDisk — ещё тестовый диск в allowlist.
func addHostDisk(h *fakeHost, name, wwn string, minor int) {
	h.disk(name, wwn, minor)
	h.e.AllowIDs = append(h.e.AllowIDs, "wwn-0x"+wwn)
}

func TestAddDiskRAID5ToRAID6Commands(t *testing.T) {
	h := newHost(t)
	addHostDisk(h, "sde", "5000000000000104", 64)
	existingPool(h, mdstatRAID5x3, "sdb", "sdc", "sdd")
	p := AddDiskParams{Pool: "p1", Disk: ref104, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID6, RaidDevices: 4, SliceBytes: gib - PartitionReserve}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	got := strings.Join(writes(h.commands()), "\n")
	want := "mdadm " + dev + "/md/p1-0 --add " + dev + "/sde1\nmdadm --grow " + dev + "/md/p1-0 --level=6 --raid-devices=4 --layout=preserve"
	if !strings.HasSuffix(got, want) || strings.Count(got, "--grow") != 1 {
		t.Fatalf("команды:\n%s", got)
	}
}

// Рост raid6 с раскладкой *-6 (после перехода из raid5) на +1 — с preserve.
func TestAddDiskRAID6Layout6Commands(t *testing.T) {
	h := newHost(t)
	addHostDisk(h, "sde", "5000000000000104", 64)
	addHostDisk(h, "sdf", "5000000000000105", 80)
	existingPool(h, mdstatRAID6L6x4, "sdb", "sdc", "sdd", "sde")
	ref105 := DiskRef{ID: "wwn-0x5000000000000105", SizeBytes: gib}
	p := AddDiskParams{Pool: "p1", Disk: ref105, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID6, RaidDevices: 5, SliceBytes: gib - PartitionReserve}}}
	if _, err := h.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	got := strings.Join(writes(h.commands()), "\n")
	if !strings.HasSuffix(got, "mdadm --grow "+dev+"/md/p1-0 --raid-devices=5 --layout=preserve") || strings.Contains(got, "--level") {
		t.Fatalf("команды:\n%s", got)
	}
	// Обычная раскладка raid6 — без preserve (как в 1.2.2).
	h2 := newHost(t)
	addHostDisk(h2, "sde", "5000000000000104", 64)
	addHostDisk(h2, "sdf", "5000000000000105", 80)
	existingPool(h2, strings.Replace(mdstatRAID6L6x4, "algorithm 18", "algorithm 2", 1), "sdb", "sdc", "sdd", "sde")
	if _, err := h2.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(writes(h2.commands()), "\n"); strings.Contains(got, "preserve") || !strings.HasSuffix(got, "--raid-devices=5") {
		t.Fatalf("команды:\n%s", got)
	}
}

// raid5 → raid6 с числом участников не N+1 — отказ до записи.
func TestAddDiskRAID5ToRAID6Refused(t *testing.T) {
	h := newHost(t)
	addHostDisk(h, "sde", "5000000000000104", 64)
	existingPool(h, strings.Replace(mdstatRAID1, "raid1", "raid5", 2), "sdb", "sdc")
	p := AddDiskParams{Pool: "p1", Disk: ref104, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID6, RaidDevices: 3, SliceBytes: gib - PartitionReserve}}}
	_, err := h.e.AddDisk(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "не поддерживается") {
		t.Fatalf("ожидался отказ: %v", err)
	}
	if w := writes(h.commands()); len(w) != 0 {
		t.Fatalf("запись при отказе: %v", w)
	}
}

// Сброс внутри перехода raid5 → raid6: после --add —
// дозавершение той же командой с preserve; после --grow (raid6 -6, recovery
// нового диска) — принят без команд.
func TestAssemblePendingRAID6(t *testing.T) {
	params := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID5, Members: []string{ref101.ID, ref102.ID, ref103.ID}}},
		Pending: &PendingAdd{DiskID: ref104.ID, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID6, RaidDevices: 4, SliceBytes: gib - PartitionReserve}}}}
	if err := params.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		sb       bool
		mdstat   string
		accepted bool
		grow     string
	}{
		{name: "до --add", mdstat: mdstatRAID5x3},
		{name: "после --add (запасной)", sb: true, accepted: true,
			mdstat: strings.Replace(mdstatRAID5x3, "sdd1[3]", "sde1[4](S) sdd1[3]", 1),
			grow:   "--level=6 --raid-devices=4 --layout=preserve"},
		{name: "после --grow: recovery нового диска", sb: true, accepted: true,
			mdstat: "Personalities : [raid6]\nmd127 : active raid6 sde1[4] sdd1[3] sdc1[1] sdb1[0]\n      2091008 blocks super 1.2 level 6, 512k chunk, algorithm 18 [4/3] [UUU_]\n      [>....................]  recovery =  3.0% (1/2) finish=1.0min speed=1K/sec\n\nunused devices: <none>\n"},
		{name: "после recovery", sb: true, accepted: true, mdstat: mdstatRAID6L6x4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			addHostDisk(h, "sde", "5000000000000104", 64)
			for _, d := range []string{"sdb", "sdc", "sdd"} {
				h.part(d, 1)
			}
			h.part("sde", 1)
			h.write("sys/devices/sde/sde1/uevent", "PARTN=1\nPARTNAME=dn-p1-0\n")
			h.examine = map[string]string{h.p("dev/sdb1"): uuidA, h.p("dev/sdc1"): uuidA, h.p("dev/sdd1"): uuidA}
			if tc.sb {
				h.examine[h.p("dev/sde1")] = uuidA
			}
			h.write("proc/mdstat", tc.mdstat)
			res, err := h.e.Assemble(context.Background(), params)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(writes(h.commands()), "\n")
			if (len(res.PendingAccepted) == 1) != tc.accepted || len(res.PendingErrors) != 0 {
				t.Fatalf("%+v\n%s", res, got)
			}
			var grows []string
			for _, l := range strings.Split(got, "\n") {
				if strings.HasPrefix(l, "mdadm --grow") {
					grows = append(grows, strings.Join(strings.Fields(l)[3:], " "))
				}
			}
			if strings.Join(grows, ";") != tc.grow {
				t.Fatalf("grow %q, ожидалось %q:\n%s", grows, tc.grow, got)
			}
		})
	}
}

// План: RAID5 1,1,2 + 2 → RAID6 и SHR 1,2,2 + 2 → SHR-2 — слои растут на
// новый диск со сменой уровня, ёмкость не растёт, отказоустойчивость 2.
func TestAddDiskLevelChangePlan(t *testing.T) {
	cases := []struct {
		name, typ, target string
		sizes             []int64
		add               int64
		want              string
	}{
		{"raid5 1,1,2 + 2 → raid6", "raid5", "raid6", []int64{1, 1, 2}, 2, "0 raid6 4"},
		{"raid5 1,1,1,1 + 1 → raid6", "raid5", "raid6", []int64{1, 1, 1, 1}, 1, "0 raid6 5"},
		{"shr 1,2,2 + 2 → shr2", "shr", "shr2", []int64{1, 2, 2}, 2, "0 raid6 4;1 raid1 3"},
		{"shr 1,1,1 + 1 → shr2", "shr", "shr2", []int64{1, 1, 1}, 1, "0 raid6 4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSvc(t)
			nd := seedPlanned(t, e, c.typ, c.sizes, c.add)
			p, err := e.s.Plan(context.Background(), PlanRequest{Op: PlanAddDisk, Pool: "p1", Type: c.target, DiskID: nd.ID})
			if err != nil {
				t.Fatal(err)
			}
			ap := e.s.plans[p.PlanID].task.AddDisk
			var got []string
			for _, x := range ap.Arrays {
				got = append(got, fmt.Sprintf("%d %s %d", x.Index, x.Level, x.RaidDevices))
			}
			if strings.Join(got, ";") != c.want || len(ap.NewArrays) != 0 {
				t.Fatalf("расширение %q, ожидалось %q", strings.Join(got, ";"), c.want)
			}
			if p.Type != c.target || p.FromType != c.typ || p.Tolerance != 2 || p.UsableBytes != p.PrevUsableBytes {
				t.Fatalf("план %s→%s, отказов %d, ёмкость %d → %d", p.FromType, p.Type, p.Tolerance, p.PrevUsableBytes, p.UsableBytes)
			}
			if !p.TransitionChecked {
				t.Fatalf("переход не отмечен проверенным: %v", p.Warnings)
			}
			if rec := e.s.plans[p.PlanID].task.Record; rec.Type != c.target || !rec.GrowPending {
				t.Fatalf("запись: %+v", rec)
			}
		})
	}
}
