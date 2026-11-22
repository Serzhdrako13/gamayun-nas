package home

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/tasks"
)

// Замена диска на больший (горячая — mdadm --replace),
// ожидание замены в записи до вызова helper, рост после замены.

func drefs(ids map[string]int64) []DiskRef {
	var out []DiskRef
	for _, id := range []string{"a", "b", "c", "d", "e", "y"} {
		if sz, ok := ids[id]; ok {
			out = append(out, DiskRef{ID: id, SizeBytes: sz})
		}
	}
	return out
}

func TestGrowthPlan(t *testing.T) {
	sl := gib - PartitionReserve // срез диска 1 ГиБ
	cases := []struct {
		name  string
		typ   string
		sizes map[string]int64
		arrs  []ArraySpec
		want  string // "expand:i/level/n;new:i/level/n/slice-ГиБ" или "" — расти нечему
		err   bool
	}{
		{"raid1 1,1 → y=2, второй ещё 1", "raid1", map[string]int64{"a": gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID1, sl, []string{"a", "y"}}}, "", false},
		{"raid1 2,2 после второй замены → новый raid1", "raid1", map[string]int64{"a": 2 * gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID1, sl, []string{"a", "y"}}}, "new:1/raid1/2/1", false},
		{"raid5 2,2,1 — не все большие", "raid5", map[string]int64{"a": 2 * gib, "b": gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID5, sl, []string{"a", "b", "y"}}}, "", false},
		{"raid5 2,2,2 → новый raid5", "raid5", map[string]int64{"a": 2 * gib, "b": 2 * gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID5, sl, []string{"a", "b", "y"}}}, "new:1/raid5/3/1", false},
		{"raid6 2,2,2,4 → новый raid6 по наименьшему хвосту", "raid6", map[string]int64{"a": 2 * gib, "b": 2 * gib, "c": 2 * gib, "y": 4 * gib},
			[]ArraySpec{{LevelRAID6, sl, []string{"a", "b", "c", "y"}}}, "new:1/raid6/4/1", false},
		{"shr 1,1,2: 1→2 → новый raid1 на хвостах", "shr", map[string]int64{"a": gib, "b": 2 * gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID5, sl, []string{"a", "b", "y"}}}, "new:1/raid1/2/1", false},
		{"shr 1,2,2: 1→2 → слой 1 raid1 → raid5", "shr", map[string]int64{"a": 2 * gib, "b": 2 * gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID5, sl, []string{"a", "b", "y"}}, {LevelRAID1, gib, []string{"a", "b"}}}, "expand:1/raid5/3", false},
		{"shr 1,2,2: 1→4 → слой 1 + остаток не используется", "shr", map[string]int64{"a": 2 * gib, "b": 2 * gib, "y": 4 * gib},
			[]ArraySpec{{LevelRAID5, sl, []string{"a", "b", "y"}}, {LevelRAID1, gib, []string{"a", "b"}}}, "expand:1/raid5/3", false},
		{"shr 1,3,3: 1→2 режет слой 1", "shr", map[string]int64{"a": 3 * gib, "b": 3 * gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID5, sl, []string{"a", "b", "y"}}, {LevelRAID1, 2 * gib, []string{"a", "b"}}}, "", false},
		{"shr 1,1,3: 1→3 → новый raid1 из двух 3", "shr", map[string]int64{"a": gib, "b": 3 * gib, "y": 3 * gib},
			[]ArraySpec{{LevelRAID5, sl, []string{"a", "b", "y"}}}, "new:1/raid1/2/2", false},
		{"shr2 1,2,2,2: 1→2 → слой 1 raid1 из 3 → raid6 не поддерживается", "shr2", map[string]int64{"a": 2 * gib, "b": 2 * gib, "c": 2 * gib, "y": 2 * gib},
			[]ArraySpec{{LevelRAID6, sl, []string{"a", "b", "c", "y"}}, {LevelRAID1, gib, []string{"a", "b", "c"}}}, "", false},
		{"jbod — нет", "jbod", map[string]int64{"y": 2 * gib}, []ArraySpec{{LevelSingle, sl, []string{"y"}}}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := PoolRecord{Name: "p1", Type: c.typ, Disks: drefs(c.sizes), Arrays: c.arrs}
			g, err := growthPlan(rec, DiskRef{ID: "y", SizeBytes: c.sizes["y"]})
			if c.err {
				if err == nil {
					t.Fatalf("ожидалась ошибка: %+v", g)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, x := range g.Expand {
				got = append(got, "expand:"+itoa(x.Index)+"/"+x.Level+"/"+itoa(x.RaidDevices))
			}
			for _, n := range g.New {
				got = append(got, "new:"+itoa(n.Index)+"/"+n.Level+"/"+itoa(len(n.Members))+"/"+itoa(int(n.SliceBytes/gib)))
				if !hasMember(g.Final[n.Index].Members, "y") {
					t.Fatalf("новый слой без y: %+v", g.Final)
				}
			}
			if strings.Join(got, ";") != c.want {
				t.Fatalf("рост %q, ожидалось %q (%v)", strings.Join(got, ";"), c.want, g.Notes)
			}
			if len(g.Expand)+len(g.New) > 0 {
				ap := AddDiskParams{Pool: "p1", Disk: DiskRef{ID: "y", SizeBytes: c.sizes["y"]}, Arrays: g.Expand, NewArrays: g.New, Grow: true}
				if err := ap.Validate(); err != nil {
					t.Fatalf("параметры роста: %v", err)
				}
			}
		})
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// mdstat во время горячей замены sdb1 → sdd1 и после неё.
const mdstatRAID1Replacing = `Personalities : [raid1]
md127 : active raid1 sdd1[2](R) sdc1[1] sdb1[0]
      1045504 blocks super 1.2 [2/2] [UU]
      [=>...................]  recovery =  5.0% (52000/1045504) finish=1.0min speed=10000K/sec

unused devices: <none>
`
const mdstatRAID1Replaced = `Personalities : [raid1]
md127 : active raid1 sdd1[2] sdc1[1] sdb1[0](F)
      1045504 blocks super 1.2 [2/2] [UU]

unused devices: <none>
`

func TestParseMdstatReplacement(t *testing.T) {
	a := ParseMdstat(mdstatRAID1Replacing)
	if len(a) != 1 || len(a[0].Replacing) != 1 || a[0].Replacing[0] != "sdd1" || a[0].Action != "recovery" || len(a[0].Members) != 3 {
		t.Fatalf("%+v", a)
	}
	if mdRole(a[0], "sdd1") != "replacing" || mdRole(a[0], "sdb1") != "member" {
		t.Fatal("роли")
	}
	b := ParseMdstat(mdstatRAID1Replaced)
	if mdRole(b[0], "sdb1") != "faulty" || mdRole(b[0], "sdd1") != "member" {
		t.Fatalf("%+v", b)
	}
}

// Во время горячей замены и после неё (заменённый — отказавший вне слотов)
// пул не деградирован: read-only не включается.
func TestStatusDuringReplace(t *testing.T) {
	copying := PoolStatus{Arrays: []ArrayStatus{{Name: "p1-0", Level: LevelRAID5, RaidDevices: 3, ActiveDevices: 3, SpareDevices: 1,
		State: []string{"clean", "recovering"}, Action: "recovery"}}}
	if h := health(copying, true); h != HealthResyncing {
		t.Fatalf("копирование: %s", h)
	}
	if ex, why := Exhausted(copying); ex {
		t.Fatalf("копирование исчерпало запас: %s", why)
	}
	done := PoolStatus{Arrays: []ArrayStatus{{Name: "p1-0", Level: LevelRAID5, RaidDevices: 3, ActiveDevices: 3, FailedDevices: 1, State: []string{"clean"}}}}
	if h := health(done, true); h != HealthOK {
		t.Fatalf("после замены: %s", h)
	}
	if ex, _ := Exhausted(done); ex {
		t.Fatal("после замены запас исчерпан")
	}
	// Настоящий отказ: слот пуст.
	failed := PoolStatus{Arrays: []ArrayStatus{{Name: "p1-0", Level: LevelRAID5, RaidDevices: 3, ActiveDevices: 2, FailedDevices: 1, Missing: 1, State: []string{"clean", "degraded"}}}}
	if h := health(failed, true); h != HealthDegraded {
		t.Fatalf("отказ: %s", h)
	}
	if ex, _ := Exhausted(failed); !ex {
		t.Fatal("отказ raid5 не исчерпал запас")
	}
}

func TestReplaceHotCommands(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	p := ReplaceParams{Pool: "p1", Disk: ref103, Old: &ref101, Arrays: []ReplaceArray{{Index: 0, SliceBytes: gib - PartitionReserve}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Replace(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	want := []string{
		"sgdisk -n 1:2048:2095103 -t 1:FD00 -c 1:dn-p1-0 " + dev + "/sdd",
		"blkdiscard --zeroout --offset 0 --length 268435456 " + dev + "/sdd1",
		"blkdiscard --zeroout --offset 1054867456 --length 16777216 " + dev + "/sdd1",
		"mdadm " + dev + "/md/p1-0 --add " + dev + "/sdd1",
		"mdadm " + dev + "/md/p1-0 --replace " + dev + "/sdb1 --with " + dev + "/sdd1",
	}
	if got := writes(h.commands()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
	}
}

func TestReplaceHotRefusals(t *testing.T) {
	sl := gib - PartitionReserve
	cases := map[string]struct {
		mdstat string
		p      ReplaceParams
	}{
		"старый не в массиве":   {mdstatRAID1, ReplaceParams{Pool: "p1", Disk: ref103, Old: &DiskRef{ID: "wwn-0x5000000000000101", SizeBytes: gib}, Arrays: []ReplaceArray{{0, sl}}}},
		"массив деградирован":   {strings.Replace(mdstatRAID1, "[2/2] [UU]", "[2/1] [U_]", 1), ReplaceParams{Pool: "p1", Disk: ref103, Old: &ref101, Arrays: []ReplaceArray{{0, sl}}}},
		"идёт копирование":      {strings.Replace(mdstatRAID1Replacing, "sdd1[2](R) ", "", 1), ReplaceParams{Pool: "p1", Disk: ref103, Old: &ref101, Arrays: []ReplaceArray{{0, sl}}}},
		"старый не в allowlist": {mdstatRAID1, ReplaceParams{Pool: "p1", Disk: ref103, Old: &DiskRef{ID: "wwn-0x50000000000001ff", SizeBytes: gib}, Arrays: []ReplaceArray{{0, sl}}}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			members := []string{"sdb", "sdc"}
			if name == "старый не в массиве" {
				members = []string{"sdc"}
				c.mdstat = strings.Replace(c.mdstat, " sdb1[0]", "", 1)
			}
			existingPool(h, c.mdstat, members...)
			if _, err := h.e.Replace(context.Background(), c.p); !errors.Is(err, ErrRefused) {
				t.Fatalf("ожидался отказ: %v", err)
			}
			if w := writes(h.commands()); len(w) != 0 {
				t.Fatalf("записи при отказе: %v", w)
			}
		})
	}
	// Новый диск меньше срезов старого — отказ ещё в параметрах.
	if (ReplaceParams{Pool: "p1", Disk: ref103, Old: &ref101, Arrays: []ReplaceArray{{0, 2 * gib}}}).Validate() == nil {
		t.Fatal("меньший диск принят")
	}
}

// Дозавершение после сброса: диск собрался запасным (сброс между --add и
// --replace) — только --replace; уже замена — ничего.
func TestReplaceResume(t *testing.T) {
	sl := gib - PartitionReserve
	p := ReplaceParams{Pool: "p1", Disk: ref103, Old: &ref101, Arrays: []ReplaceArray{{0, sl}}, Resume: true}
	h := newHost(t)
	existingPool(h, strings.Replace(mdstatRAID1, "sdc1[1]", "sdd1[2](S) sdc1[1]", 1), "sdb", "sdc")
	h.part("sdd", 1)
	if _, err := h.e.Replace(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	if got := writes(h.commands()); strings.Join(got, "\n") != "mdadm "+dev+"/md/p1-0 --replace "+dev+"/sdb1 --with "+dev+"/sdd1" {
		t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
	}
	h2 := newHost(t)
	existingPool(h2, mdstatRAID1Replacing, "sdb", "sdc")
	h2.part("sdd", 1)
	if _, err := h2.e.Replace(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if w := writes(h2.commands()); len(w) != 0 {
		t.Fatalf("команды при идущей замене: %v", w)
	}
	// Без Resume диск в массиве — отказ.
	p.Resume = false
	if _, err := h2.e.Replace(context.Background(), p); !errors.Is(err, ErrRefused) {
		t.Fatalf("%v", err)
	}
}

func TestReplaceFinish(t *testing.T) {
	sl := gib - PartitionReserve
	p := ReplaceParams{Pool: "p1", Disk: ref103, Old: &ref101, Arrays: []ReplaceArray{{0, sl}}}
	// Копирование не закончено — отказ без записи.
	h := newHost(t)
	existingPool(h, mdstatRAID1Replacing, "sdb", "sdc")
	h.part("sdd", 1)
	if _, err := h.e.ReplaceFinish(context.Background(), p); !errors.Is(err, ErrRefused) || len(writes(h.commands())) != 0 {
		t.Fatalf("незаконченная замена: %v %v", err, h.commands())
	}
	h2 := newHost(t)
	existingPool(h2, mdstatRAID1Replaced, "sdc", "sdd")
	h2.part("sdb", 1)
	h2.write("sys/devices/sdb/sdb1/uevent", "PARTNAME=dn-p1-0\n")
	if _, err := h2.e.ReplaceFinish(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h2.p("dev")
	want := []string{
		"mdadm " + dev + "/md/p1-0 --remove " + dev + "/sdb1",
		"wipefs -q -a " + dev + "/sdb1",
		"sgdisk --zap-all " + dev + "/sdb",
		"wipefs -q -a " + dev + "/sdb",
	}
	if got := writes(h2.commands()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
	}
}

// Рост после замены: новые разделы на диске пула — хвостами (partx), диск
// целиком не размечается и не стирается.
func TestAddDiskGrowCommands(t *testing.T) {
	h := newHost(t)
	h.write("sys/devices/sdd/size", "4194304") // 2 ГиБ
	existingPool(h, mdstatRAID1, "sdb", "sdd")
	for _, d := range []string{"sdb", "sdd"} {
		h.write("sys/devices/"+d+"/"+d+"1/uevent", "PARTNAME=dn-p1-0\n")
		h.write("sys/devices/"+d+"/"+d+"1/start", "2048")
		h.write("sys/devices/"+d+"/"+d+"1/size", "2093056")
	}
	h.write("sys/devices/sdb/size", "4194304")
	h.busy = map[string]bool{"sdb": true, "sdd": true}
	b := DiskRef{ID: ref101.ID, SizeBytes: 2 * gib}
	y := DiskRef{ID: ref103.ID, SizeBytes: 2 * gib}
	rec := PoolRecord{Name: "p1", Type: "raid1", Disks: []DiskRef{b, y}, Arrays: []ArraySpec{{LevelRAID1, gib - PartitionReserve, []string{b.ID, y.ID}}}}
	g, err := growthPlan(rec, y)
	if err != nil || len(g.New) != 1 {
		t.Fatalf("%+v %v", g, err)
	}
	ap := AddDiskParams{Pool: "p1", Disk: y, NewArrays: g.New, Grow: true}
	if err := ap.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.AddDisk(context.Background(), ap); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(writes(h.commands()), "\n")
	for _, w := range []string{"sgdisk -n 2:", "partx --add --nr 2 " + h.p("dev") + "/sdd", "partx --add --nr 2 " + h.p("dev") + "/sdb", "mdadm --create", "--level=1", "pvcreate", "vgextend"} {
		if !strings.Contains(got, w) {
			t.Fatalf("нет %q в:\n%s", w, got)
		}
	}
	if strings.Contains(got, "--zap-all") || strings.Contains(got, "sgdisk -n 1:") || strings.Contains(got, "wipefs") {
		t.Fatalf("диск пула переразмечен:\n%s", got)
	}
}

// Ожидание замены пишется до вызова helper; сбой helper без принятия —
// откат ожидания; после копирования горячей замены — вывод старого диска.
func TestReplaceHotPlanAndTask(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", true)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	// Деградированный путь без old_disk_id в исправном пуле — отказ.
	if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	if _, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID, OldDiskID: "wwn-0x50000000000001ff"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("чужой старый диск: %v", err)
	}
	p, err := e.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID, OldDiskID: ref101.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(replacedStatus(ref102.ID, ref103.ID, ref101.ID))
	var sawPending bool
	e.h.hook = func(op string) {
		if op == OpReplace {
			rec, _ := e.s.getRecord(ctx, "p1")
			sawPending = rec.Pending != nil && rec.Pending.Replace && rec.Pending.Old != nil && rec.Pending.Old.ID == ref101.ID
		}
	}
	tk, err := e.s.Confirm(ctx, p.PlanID, p.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.wait(t, tk.ID); r.State != tasks.Succeeded {
		t.Fatalf("задача %+v", r)
	}
	if !sawPending {
		t.Fatal("ожидание замены не записано до helper")
	}
	if e.h.called(OpReplaceFinish) != 1 {
		t.Fatal("старый диск не выведен")
	}
	rec, _ := e.s.getRecord(ctx, "p1")
	if rec.Pending != nil || len(rec.Disks) != 2 || rec.hasDisk(ref101.ID) || !rec.hasDisk(ref103.ID) || !hasMember(rec.Arrays[0].Members, ref103.ID) {
		t.Fatalf("запись %+v", rec)
	}

	// Сбой helper, диск не принят ни в один массив — ожидание снято.
	e2 := newSvc(t)
	seedPool(t, e2, "raid1", true)
	e2.h.setStatus(okStatus(LevelRAID1, 2, gib))
	p2, err := e2.s.Plan(ctx, PlanRequest{Op: PlanReplace, Pool: "p1", DiskID: ref103.ID, OldDiskID: ref101.ID})
	if err != nil {
		t.Fatal(err)
	}
	e2.h.fail = map[string]error{OpReplace: errors.New("mdadm: сбой")}
	e2.h.assemble = &AssembleResult{}
	tk2, _ := e2.s.Confirm(ctx, p2.PlanID, p2.ConfirmToken)
	if r := e2.wait(t, tk2.ID); r.State != tasks.Failed {
		t.Fatalf("задача %+v", r)
	}
	rec2, _ := e2.s.getRecord(ctx, "p1")
	if rec2.Pending != nil || !rec2.hasDisk(ref101.ID) || rec2.hasDisk(ref103.ID) {
		t.Fatalf("откат: %+v", rec2)
	}
}

// Сброс внутри замены: принят — задача дозавершения replace с Resume.
func TestAssembleAllSubmitsReplaceResume(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", true)
	rec, _ := e.s.getRecord(ctx, "p1")
	final := rec
	final.Disks = []DiskRef{rec.Disks[1], ref103}
	final.Arrays = []ArraySpec{{rec.Arrays[0].Level, rec.Arrays[0].SliceBytes, []string{rec.Disks[1].ID, ref103.ID}}}
	rec.Pending = &PoolPending{Replace: true, Disk: ref103, Old: &rec.Disks[0], Type: rec.Type, Arrays: final.Arrays, Disks: final.Disks,
		Expand: []ExpandArray{{Index: 0, Level: LevelRAID1, RaidDevices: 2, SliceBytes: rec.Arrays[0].SliceBytes}}}
	if err := e.s.saveRecord(ctx, rec, false); err != nil {
		t.Fatal(err)
	}
	e.h.assemble = &AssembleResult{PendingAccepted: []int{0}}
	e.h.setStatus(replacedStatus(ref102.ID, ref103.ID, ref101.ID))
	if err := e.s.AssembleAll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var ap AssembleParams
	json.Unmarshal(e.h.params[OpAssemble], &ap)
	if ap.Pending == nil || ap.Pending.DiskID != ref103.ID || len(ap.Pending.Arrays) != 1 {
		t.Fatalf("сборка без ожидаемого диска: %+v", ap)
	}
	for i := 0; i < 200 && e.h.called(OpReplaceFinish) == 0; i++ {
		waitShort()
	}
	var rp ReplaceParams
	json.Unmarshal(e.h.params[OpReplace], &rp)
	if !rp.Resume || rp.Old == nil || rp.Disk.ID != ref103.ID {
		t.Fatalf("дозавершение %+v", rp)
	}
}

func waitShort() { time.Sleep(10 * time.Millisecond) }

// replacedStatus — raid1 после замены: участники в синхроне, старый — faulty.
func replacedStatus(a, b, old string) PoolStatus {
	st := okStatus(LevelRAID1, 2, gib)
	st.Arrays[0].Members = []MemberStatus{{DiskID: a, State: "active sync"}, {DiskID: b, State: "active sync"}}
	if old != "" {
		st.Arrays[0].Members = append(st.Arrays[0].Members, MemberStatus{DiskID: old, State: "faulty"})
		st.Arrays[0].FailedDevices = 1
	}
	return st
}

// Сразу после --replace recovery ещё не началась: статус «в покое», но
// старый диск — исправный участник; задача ждёт, а не выводит его.
func TestReplacedWaitsForCopy(t *testing.T) {
	rp := &ReplaceParams{Disk: ref103, Old: &ref101, Arrays: []ReplaceArray{{Index: 0}}}
	before := okStatus(LevelRAID1, 2, gib)
	before.Arrays[0].Members = []MemberStatus{{DiskID: ref101.ID, State: "active sync"}, {DiskID: ref102.ID, State: "active sync"}, {DiskID: ref103.ID, State: "spare"}}
	if replaced(rp)(before) {
		t.Fatal("замена считается завершённой до копирования")
	}
	if !replaced(rp)(replacedStatus(ref102.ID, ref103.ID, ref101.ID)) {
		t.Fatal("завершённая замена не распознана")
	}
}

// Рост RAID 5 новым слоем: признак начальной синхронизации ставится в
// запись до helper (иначе read-only на время сборки слоя — найдено на стенде).
func TestGrowNewRAID5LayerInitialSync(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", false)
	rec, _ := e.s.getRecord(ctx, "p1")
	ap := &AddDiskParams{Pool: "p1", Disk: ref101, Grow: true,
		NewArrays: []NewArray{{Index: 1, Level: LevelRAID5, SliceBytes: 64 * Align, Members: []DiskRef{ref101, ref102, ref103}}}}
	var during bool
	e.h.hook = func(op string) {
		if op == OpAddDisk {
			r, _ := e.s.getRecord(ctx, "p1")
			during = r.InitialSync
		}
	}
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	if _, err := e.s.addDisk(ctx, ap, rec, 0, func(int) {}); err != nil {
		t.Fatal(err)
	}
	if !during {
		t.Fatal("признак начальной синхронизации не записан до helper")
	}
	if !newParityLayer(ap) || newParityLayer(&AddDiskParams{NewArrays: []NewArray{{Level: LevelRAID1}}}) {
		t.Fatal("newParityLayer")
	}
}

// Проход защиты во время helper add_disk (пул ещё ok — новый
// слой не собран) не должен снимать признак начальной синхронизации,
// иначе после возврата helper новый raid5-слой в recovery считается
// деградацией и включается read-only (стенд 2026-11-12).
func TestGrowInitialSyncSurvivesProtectionPass(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "raid1", false)
	rec, _ := e.s.getRecord(ctx, "p1")
	ap := &AddDiskParams{Pool: "p1", Disk: ref101, Grow: true,
		NewArrays: []NewArray{{Index: 1, Level: LevelRAID5, SliceBytes: 64 * Align, Members: []DiskRef{ref101, ref102, ref103}}}}
	var during bool
	e.h.hook = func(op string) {
		if op == OpAddDisk {
			// enforcePool при health ok вызывает endInitialSync; сам
			// enforcePool здесь не вызвать — фейковый helper держит mu.
			if err := e.s.endInitialSync(ctx, "p1"); err != nil {
				t.Error(err)
			}
			r, _ := e.s.getRecord(ctx, "p1")
			during = r.InitialSync
		}
	}
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	if _, err := e.s.addDisk(ctx, ap, rec, 0, func(int) {}); err != nil {
		t.Fatal(err)
	}
	if !during {
		t.Fatal("проход защиты во время add_disk снял признак начальной синхронизации")
	}
	// Рост завершён (pending/grow_pending сняты), пул ok — признак снимается.
	if _, err := e.s.enforcePool(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.s.getRecord(ctx, "p1"); r.InitialSync || r.Pending != nil || r.GrowPending {
		t.Fatalf("после роста: initial_sync=%v pending=%v grow_pending=%v", r.InitialSync, r.Pending != nil, r.GrowPending)
	}
}
