package home

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/storage/planner"
)

// Добавление диска того же размера, что и срезы
// существующих массивов, во все поддерживаемые типы.

// seedPlanned — пул p1 типа typ из дисков размеров sizes (ГиБ) по плану
// planner; в снимке — эти диски (заняты) и свободный диск add (ГиБ).
func seedPlanned(t *testing.T, e *svcEnv, typ string, sizes []int64, add int64) DiskRef {
	t.Helper()
	pr := planner.Request{Type: planner.PoolType(typ), SystemReserve: PartitionReserve}
	sz := map[string]int64{}
	var refs []DiskRef
	e.h.snap = disks.Snapshot{Complete: true, Issues: []disks.Issue{}, Disks: []disks.Disk{snapDisk("sda", "", disks.SummaryRoot)}}
	mk := func(i int, g int64, role string) DiskRef {
		r := DiskRef{ID: fmt.Sprintf("wwn-0x50000000000002%02d", i), SizeBytes: g * gib}
		d := snapDisk(fmt.Sprintf("sd%c", 'b'+i), r.ID, role)
		d.SizeBytes = &r.SizeBytes
		e.h.snap.Disks = append(e.h.snap.Disks, d)
		return r
	}
	for i, g := range sizes {
		r := mk(i, g, disks.SummaryInUse)
		refs = append(refs, r)
		sz[r.ID] = r.SizeBytes
		pr.Disks = append(pr.Disks, planner.Disk{ID: r.ID, Size: r.SizeBytes})
	}
	nd := mk(len(sizes), add, disks.SummaryUnknown)
	pl, err := planner.Compute(pr)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := toSpecs(pl, sz)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.saveRecord(context.Background(), PoolRecord{Name: "p1", Type: typ, Disks: refs, Arrays: specs}, true); err != nil {
		t.Fatal(err)
	}
	st := PoolStatus{Pool: "p1", Health: HealthOK}
	for i, a := range specs {
		st.Arrays = append(st.Arrays, ArrayStatus{Index: i, Level: a.Level, RaidDevices: len(a.Members), ActiveDevices: len(a.Members)})
	}
	e.h.setStatus(st)
	return nd
}

func TestAddDiskSameSize(t *testing.T) {
	cases := []struct {
		name   string
		typ    string
		sizes  []int64
		add    int64
		target string
		want   string // ожидаемые ExpandArray: индекс уровень участники
		grows  bool   // ёмкость растёт
	}{
		{"raid5 3→4", "raid5", []int64{1, 1, 1}, 1, "", "0 raid5 4", true},
		{"raid5 3→4 диском больше (срез прежний)", "raid5", []int64{1, 1, 2}, 2, "", "0 raid5 4", true},
		{"raid6 4→5", "raid6", []int64{1, 1, 1, 1}, 1, "", "0 raid6 5", true},
		{"raid1 2→3 (третья копия)", "raid1", []int64{1, 1}, 1, "", "0 raid1 3", false},
		{"shr 3→4", "shr", []int64{1, 1, 1}, 1, "", "0 raid5 4", true},
		{"shr 1,2,2 + 1: растёт только нижний слой", "shr", []int64{1, 2, 2}, 1, "", "0 raid5 4", true},
		{"shr 1,2,2 + 2: растут оба слоя", "shr", []int64{1, 2, 2}, 2, "", "0 raid5 4;1 raid5 3", true},
		{"shr2 4→5", "shr2", []int64{1, 1, 1, 1}, 1, "", "0 raid6 5", true},
		{"shr2 1,2,2,2 + 1: растёт только нижний слой", "shr2", []int64{1, 2, 2, 2}, 1, "", "0 raid6 5", true},
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
			got := ""
			for i, x := range ap.Arrays {
				if i > 0 {
					got += ";"
				}
				got += fmt.Sprintf("%d %s %d", x.Index, x.Level, x.RaidDevices)
			}
			if got != c.want || ap.Disk.ID != nd.ID {
				t.Fatalf("расширение %q, ожидалось %q", got, c.want)
			}
			if (p.UsableBytes > p.PrevUsableBytes) != c.grows {
				t.Fatalf("ёмкость %d → %d", p.PrevUsableBytes, p.UsableBytes)
			}
			if !p.TransitionChecked {
				t.Fatalf("переход не отмечен проверенным: %+v", p.Warnings)
			}
			// Неизменные слои — в записи без нового диска.
			rec := e.s.plans[p.PlanID].task.Record
			for i, a := range rec.Arrays {
				in := false
				for _, x := range ap.Arrays {
					in = in || x.Index == i
				}
				if hasMember(a.Members, nd.ID) != in {
					t.Fatalf("слой %d: участники %v, расширяется %v", i, a.Members, in)
				}
			}
		})
	}
}

// Диск, который режет существующий слой (меньше верхней границы и не
// совпадает с границей слоёв), меняет срез — отказ: разделы прежних дисков
// не переразмечаются. Новый верхний слой — TestAddDiskNewLayerPlan.
func TestAddDiskLayerChangeUnsupported(t *testing.T) {
	cases := []struct {
		name  string
		typ   string
		sizes []int64
		add   int64
	}{
		{"shr 2,2 + 1 (меньший срез)", "shr", []int64{2, 2}, 1},
		{"shr 1,3,3 + 2 (режет слой 1)", "shr", []int64{1, 3, 3}, 2},
		{"shr2 2,2,2,2 + 1 (меньший срез)", "shr2", []int64{2, 2, 2, 2}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSvc(t)
			nd := seedPlanned(t, e, c.typ, c.sizes, c.add)
			_, err := e.s.Plan(context.Background(), PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: nd.ID})
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "меняет размер раздела") {
				t.Fatalf("ожидался отказ, получено %v", err)
			}
		})
	}
}

// Тот же уровень (raid1 → третья копия; так же raid5/raid6 +1): без
// --level, только --add и --grow --raid-devices.
func TestAddDiskSameLevelCommands(t *testing.T) {
	h := newHost(t)
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	p := AddDiskParams{Pool: "p1", Disk: ref103, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID1, RaidDevices: 3, SliceBytes: gib - PartitionReserve}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	got := strings.Join(writes(h.commands()), "\n")
	if strings.Contains(got, "--level") || !strings.HasSuffix(got, "mdadm "+dev+"/md/p1-0 --add "+dev+"/sdd1\nmdadm --grow "+dev+"/md/p1-0 --raid-devices=3") {
		t.Fatalf("команды:\n%s", got)
	}
}
