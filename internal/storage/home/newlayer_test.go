package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Добавление диска, меняющее число слоёв — новый верхний
// слой (SHR/SHR-2 диском большего размера, JBOD +1).

func TestAddDiskNewLayerPlan(t *testing.T) {
	cases := []struct {
		name   string
		typ    string
		sizes  []int64
		add    int64
		expand string // индекс уровень участников
		layer  string // новый слой: индекс уровень участников срез(МиБ)
		tail   []int  // индексы прежних дисков (в sizes) — участники нового слоя
	}{
		{"shr 1,1,2 + 2", "shr", []int64{1, 1, 2}, 2, "0 raid5 4", "1 raid1 2 1024", []int{2}},
		{"shr 1,1,3 + 2 (слой 1 ГиБ на хвосте 3 ГиБ)", "shr", []int64{1, 1, 3}, 2, "0 raid5 4", "1 raid1 2 1024", []int{2}},
		{"shr 1,3 + 2 (промежуточный размер выше слоёв)", "shr", []int64{1, 3}, 2, "0 raid5 3", "1 raid1 2 1024", []int{1}},
		{"shr2 1,1,2,2 + 2", "shr2", []int64{1, 1, 2, 2}, 2, "0 raid6 5", "1 raid1 3 1024", []int{2, 3}},
		{"jbod 1,1 + 1", "jbod", []int64{1, 1}, 1, "", "2 single 1 1022", nil},
		{"jbod 1,2 + 2", "jbod", []int64{1, 2}, 2, "", "2 single 1 2046", nil},
		{"jbod 2,1 + 1 (индексы прежних слоёв не сдвигаются)", "jbod", []int64{2, 1}, 1, "", "2 single 1 1022", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSvc(t)
			nd := seedPlanned(t, e, c.typ, c.sizes, c.add)
			before, _ := e.s.getRecord(context.Background(), "p1")
			p, err := e.s.Plan(context.Background(), PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: nd.ID})
			if err != nil {
				t.Fatal(err)
			}
			ap := e.s.plans[p.PlanID].task.AddDisk
			if err := ap.Validate(); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, x := range ap.Arrays {
				got = append(got, fmt.Sprintf("%d %s %d", x.Index, x.Level, x.RaidDevices))
			}
			if strings.Join(got, ";") != c.expand {
				t.Fatalf("расширение %q, ожидалось %q", got, c.expand)
			}
			if len(ap.NewArrays) != 1 {
				t.Fatalf("новые слои: %+v", ap.NewArrays)
			}
			na := ap.NewArrays[0]
			if s := fmt.Sprintf("%d %s %d %d", na.Index, na.Level, len(na.Members), na.SliceBytes>>20); s != c.layer {
				t.Fatalf("новый слой %q, ожидался %q", s, c.layer)
			}
			ids := map[string]int64{nd.ID: nd.SizeBytes}
			for _, i := range c.tail {
				ids[before.Disks[i].ID] = before.Disks[i].SizeBytes
			}
			for _, m := range na.Members {
				if sz, ok := ids[m.ID]; !ok || sz != m.SizeBytes {
					t.Fatalf("участник %+v, ожидались %v", m, ids)
				}
			}
			if !p.TransitionChecked {
				t.Fatalf("переход не отмечен проверенным: %v", p.Warnings)
			}
			if p.UsableBytes <= p.PrevUsableBytes {
				t.Fatalf("ёмкость %d → %d", p.PrevUsableBytes, p.UsableBytes)
			}
			rec := e.s.plans[p.PlanID].task.Record
			if len(rec.Arrays) != len(before.Arrays)+1 || !rec.GrowPending || len(rec.Disks) != len(before.Disks)+1 {
				t.Fatalf("запись: %+v", rec)
			}
			// Прежние слои JBOD — на своих местах.
			for i, a := range before.Arrays {
				if rec.Arrays[i].SliceBytes != a.SliceBytes {
					t.Fatalf("слой %d сдвинут: %+v → %+v", i, a, rec.Arrays[i])
				}
			}
		})
	}
}

// Пул SHR 1+2 ГиБ (sdb, sdc; raid1 слоя 0 на sdb1/sdc1), добавляется sdd
// 2 ГиБ: слой 0 → raid5 из 3, новый слой 1 — raid1 на хвосте sdc и sdd.
const slice0 = gib - PartitionReserve

var (
	big102 = DiskRef{ID: ref102.ID, SizeBytes: 2 * gib}
	big103 = DiskRef{ID: ref103.ID, SizeBytes: 2 * gib}
)

func newLayerHost(t *testing.T) *fakeHost {
	h := newHost(t)
	for _, d := range []string{"sdc", "sdd"} {
		h.write("sys/devices/"+d+"/size", fmt.Sprint(2*gib/512))
	}
	existingPool(h, mdstatRAID1, "sdb", "sdc")
	for _, d := range []string{"sdb", "sdc"} {
		h.write("sys/devices/"+d+"/"+d+"1/start", "2048")
		h.write("sys/devices/"+d+"/"+d+"1/size", fmt.Sprint(slice0/512))
		h.write("sys/devices/"+d+"/"+d+"1/uevent", "PARTN=1\nPARTNAME=dn-p1-0\n")
	}
	return h
}

func newLayerParams() AddDiskParams {
	return AddDiskParams{Pool: "p1", Disk: big103,
		Arrays:    []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: slice0}},
		NewArrays: []NewArray{{Index: 1, Level: LevelRAID1, SliceBytes: gib, Members: []DiskRef{big102, big103}}}}
}

func zeroCmds(dev string, size int64) []string {
	return []string{
		fmt.Sprintf("blkdiscard --zeroout --offset 0 --length %d %s", staleHead, dev),
		fmt.Sprintf("blkdiscard --zeroout --offset %d --length %d %s", size-staleTail, staleTail, dev),
	}
}

func TestAddDiskNewLayerCommands(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("диск занят=%v", busy), func(t *testing.T) {
			h := newLayerHost(t)
			h.busy = map[string]bool{"sdc": busy}
			p := newLayerParams()
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			if _, err := h.e.AddDisk(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			dev := h.p("dev")
			want := []string{"sgdisk -n 1:2048:2095103 -t 1:FD00 -c 1:dn-p1-0 -n 2:2095104:4192255 -t 2:FD00 -c 2:dn-p1-1 " + dev + "/sdd"}
			want = append(want, zeroCmds(dev+"/sdd1", slice0)...)
			want = append(want, zeroCmds(dev+"/sdd2", gib)...)
			want = append(want,
				"mdadm "+dev+"/md/p1-0 --add "+dev+"/sdd1",
				"mdadm --grow "+dev+"/md/p1-0 --level=5",
				"mdadm --grow "+dev+"/md/p1-0 --raid-devices=3",
				"sgdisk -n 2:2095104:4192255 -t 2:FD00 -c 2:dn-p1-1 "+dev+"/sdc")
			if busy {
				want = append(want, "partx --add --nr 2 "+dev+"/sdc")
			}
			want = append(want, zeroCmds(dev+"/sdc2", gib)...)
			want = append(want,
				"mdadm --create "+dev+"/md/p1-1 --run --level=1 --raid-devices=2 --metadata=1.2 --bitmap=internal --name=p1-1 "+dev+"/sdc2 "+dev+"/sdd2",
				"lvm pvcreate -q -y "+dev+"/md/p1-1",
				"lvm vgextend -q dn_p1 "+dev+"/md/p1-1")
			if got := writes(h.commands()); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("команды:\n%s\nожидались:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// С dm-integrity: каждый участник нового слоя — format --no-wipe + open.
func TestAddDiskNewLayerIntegrity(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.write("sys/devices/sdb/sdb1/start", "2048")
	h.write("sys/devices/sdb/sdb1/size", fmt.Sprint(slice0/512))
	h.write("sys/devices/sdb/sdb1/uevent", "PARTNAME=dn-p1-0\n")
	h.write("sys/devices/sdb/size", fmt.Sprint(2*gib/512))
	h.write("sys/devices/sdd/size", fmt.Sprint(2*gib/512))
	h.pvs[h.p("dev/sdb1")] = "dn_p1"
	h.vgs = []string{"dn_p1"}
	// SHR-подобный пул из одного single нельзя расширить raid1 по плану,
	// но executor проверяет только геометрию: слой 1 на хвосте sdb и на sdd.
	big101 := DiskRef{ID: ref101.ID, SizeBytes: 2 * gib}
	p := AddDiskParams{Pool: "p1", Integrity: true, IntegrityMode: IntegrityBitmap, Disk: big103,
		NewArrays: []NewArray{{Index: 1, Level: LevelRAID1, SliceBytes: gib, Members: []DiskRef{big101, big103}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	in1, in3 := IntegrityName("p1", 1, ref101.ID), IntegrityName("p1", 1, ref103.ID)
	got := strings.Join(writes(h.commands()), "\n")
	for _, w := range []string{
		"sgdisk -n 2:2048:2099199 -t 2:FD00 -c 2:dn-p1-1 " + dev + "/sdd",
		"integritysetup open --integrity crc32c --integrity-recalculate --integrity-bitmap-mode " + dev + "/sdd2 " + in3,
		"sgdisk -n 2:2095104:4192255 -t 2:FD00 -c 2:dn-p1-1 " + dev + "/sdb",
		"integritysetup open --integrity crc32c --integrity-recalculate --integrity-bitmap-mode " + dev + "/sdb2 " + in1,
		"mdadm --create " + dev + "/md/p1-1 --run --level=1 --raid-devices=2 --metadata=1.2 --bitmap=internal --name=p1-1 " + dev + "/mapper/" + in1 + " " + dev + "/mapper/" + in3,
		"lvm vgextend -q dn_p1 " + dev + "/md/p1-1",
	} {
		if !strings.Contains(got, w) {
			t.Fatalf("нет %q в:\n%s", w, got)
		}
	}
}

// JBOD +1: слой single на новом диске, без md.
func TestAddDiskJBODCommands(t *testing.T) {
	h := newHost(t)
	h.part("sdb", 1)
	h.pvs[h.p("dev/sdb1")] = "dn_p1"
	h.part("sdc", 2)
	h.pvs[h.p("dev/sdc2")] = "dn_p1"
	h.vgs = []string{"dn_p1"}
	p := AddDiskParams{Pool: "p1", Disk: ref103,
		NewArrays: []NewArray{{Index: 2, Level: LevelSingle, SliceBytes: slice0, Members: []DiskRef{ref103}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.AddDisk(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dev := h.p("dev")
	want := []string{"sgdisk -n 3:2048:2095103 -t 3:FD00 -c 3:dn-p1-2 " + dev + "/sdd"}
	want = append(want, zeroCmds(dev+"/sdd3", slice0)...)
	want = append(want, "lvm pvcreate -q -y "+dev+"/sdd3", "lvm vgextend -q dn_p1 "+dev+"/sdd3")
	if got := writes(h.commands()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
	}
}

// Отказ до любой записи: место за нижними слоями занято, метка чужая,
// диск не из пула, массив слоя уже есть, раздел слоя занят.
func TestAddDiskNewLayerRefusals(t *testing.T) {
	for name, f := range map[string]func(h *fakeHost, p *AddDiskParams){
		"лишний раздел": func(h *fakeHost, p *AddDiskParams) {
			h.part("sdc", 3)
		},
		"чужая метка нижнего слоя": func(h *fakeHost, p *AddDiskParams) {
			h.write("sys/devices/sdc/sdc1/uevent", "PARTNAME=data\n")
		},
		"не помещается": func(h *fakeHost, p *AddDiskParams) {
			p.NewArrays[0].SliceBytes = gib + Align
			p.Disk.SizeBytes = 2*gib + 2*Align
			p.NewArrays[0].Members[1] = p.Disk
			h.write("sys/devices/sdd/size", fmt.Sprint(p.Disk.SizeBytes/512))
		},
		"диск не из пула": func(h *fakeHost, p *AddDiskParams) {
			h.disk("sde", "5000000000000104", 64)
			h.write("sys/devices/sde/size", fmt.Sprint(2*gib/512))
			h.e.AllowIDs = append(h.e.AllowIDs, "wwn-0x5000000000000104")
			p.NewArrays[0].Members[0] = DiskRef{ID: "wwn-0x5000000000000104", SizeBytes: 2 * gib}
		},
		"массив слоя есть": func(h *fakeHost, p *AddDiskParams) {
			h.virt("md120")
			h.link("../md120", "dev/md/p1-1")
		},
		"размер прежнего диска": func(h *fakeHost, p *AddDiskParams) {
			p.NewArrays[0].Members[0].SizeBytes = gib
		},
		"раздел слоя занят": func(h *fakeHost, p *AddDiskParams) {
			h.part("sdc", 2)
			h.write("sys/devices/sdc/sdc2/start", fmt.Sprint((Align+slice0)/512))
			h.write("sys/devices/sdc/sdc2/size", fmt.Sprint(gib/512))
			h.write("sys/devices/sdc/sdc2/uevent", "PARTNAME=dn-p1-1\n")
			h.virt("md9", "sdc2")
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLayerHost(t)
			p := newLayerParams()
			f(h, &p)
			if _, err := h.e.AddDisk(context.Background(), p); !errors.Is(err, ErrRefused) {
				t.Fatalf("ожидался отказ: %v", err)
			}
			if w := writes(h.commands()); len(w) != 0 {
				t.Fatalf("записи при отказе: %v", w)
			}
		})
	}
}

// Дозавершение нового слоя: раздел нового диска переиспользуется
// и обнуляется, хвостовой раздел прежнего диска создаётся или
// переиспользуется (если остался с той же меткой и геометрией).
func TestAddDiskNewLayerResume(t *testing.T) {
	for _, tailExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("хвост есть=%v", tailExists), func(t *testing.T) {
			h := newLayerHost(t)
			h.part("sdd", 1)
			h.part("sdd", 2)
			h.write("sys/devices/sdd/sdd2/size", fmt.Sprint(gib/512))
			if tailExists {
				h.part("sdc", 2)
				h.write("sys/devices/sdc/sdc2/start", fmt.Sprint((Align+slice0)/512))
				h.write("sys/devices/sdc/sdc2/size", fmt.Sprint(gib/512))
				h.write("sys/devices/sdc/sdc2/uevent", "PARTNAME=dn-p1-1\n")
			}
			p := newLayerParams()
			p.Arrays, p.Resume = nil, true
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			if _, err := h.e.AddDisk(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			dev := h.p("dev")
			var want []string
			if !tailExists {
				want = append(want, "sgdisk -n 2:2095104:4192255 -t 2:FD00 -c 2:dn-p1-1 "+dev+"/sdc")
			}
			want = append(want, zeroCmds(dev+"/sdc2", gib)...)
			want = append(want, zeroCmds(dev+"/sdd2", gib)...)
			want = append(want,
				"mdadm --create "+dev+"/md/p1-1 --run --level=1 --raid-devices=2 --metadata=1.2 --bitmap=internal --name=p1-1 "+dev+"/sdc2 "+dev+"/sdd2",
				"lvm pvcreate -q -y "+dev+"/md/p1-1",
				"lvm vgextend -q dn_p1 "+dev+"/md/p1-1")
			if got := writes(h.commands()); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("команды:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}

// Сборка после сброса: принят — если PV слоя
// в VG; не принят — откат слоя (метка LVM, md, суперблоки, хвостовой
// раздел прежнего диска); неизвестный PV в VG — ничего не трогать.
func TestAssembleNewLayer(t *testing.T) {
	mdstatGrown := "Personalities : [raid5]\nmd127 : active raid5 sdd1[2] sdc1[1] sdb1[0]\n      1045504 blocks super 1.2 level 5, 64k chunk, algorithm 2 [3/3] [UUU]\n      [>....................]  reshape =  1.0% (1/1) finish=1.0min speed=1K/sec\n\nunused devices: <none>\n"
	params := func() AssembleParams {
		return AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}},
			Pending: &PendingAdd{DiskID: ref103.ID,
				Arrays: []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: slice0}},
				New:    []NewArray{{Index: 1, Level: LevelRAID1, SliceBytes: gib, Members: []DiskRef{big102, big103}}}}}
	}
	for _, tc := range []struct {
		name     string
		layer    bool // разделы слоя созданы (md соберётся)
		inVG     bool
		unknown  bool
		accepted string
		rollback bool
	}{
		{name: "до создания слоя", accepted: "0"},
		{name: "после pvcreate до vgextend", layer: true, accepted: "0", rollback: true},
		{name: "после vgextend", layer: true, inVG: true, accepted: "0,1"},
		{name: "в VG неизвестный PV", layer: true, unknown: true, accepted: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			h.part("sdb", 1)
			h.part("sdc", 1)
			h.part("sdd", 1)
			h.part("sdd", 2)
			h.write("sys/devices/sdd/sdd1/uevent", "PARTNAME=dn-p1-0\n")
			h.examine = map[string]string{h.p("dev/sdb1"): uuidA, h.p("dev/sdc1"): uuidA, h.p("dev/sdd1"): uuidA}
			h.write("proc/mdstat", mdstatGrown)
			h.vgs = []string{"dn_p1"}
			if tc.layer {
				h.part("sdc", 2)
				h.write("sys/devices/sdc/sdc2/uevent", "PARTNAME=dn-p1-1\n")
				h.examine[h.p("dev/sdc2")] = uuidB
				h.examine[h.p("dev/sdd2")] = uuidB
			}
			if tc.inVG {
				// mdadm --assemble создаст md128 → /dev/md/p1-1.
				h.write("dev/md128", "")
				h.pvs[h.p("dev/md128")] = "dn_p1"
			}
			if tc.unknown {
				h.pvs["[unknown]"] = "dn_p1"
			}
			res, err := h.e.Assemble(context.Background(), params())
			if err != nil {
				t.Fatal(err)
			}
			acc := strings.Trim(strings.Join(strings.Fields(fmt.Sprint(res.PendingAccepted)), ","), "[]")
			if acc != tc.accepted {
				t.Fatalf("принято %v, ожидалось %s: %+v", res.PendingAccepted, tc.accepted, res)
			}
			got := strings.Join(writes(h.commands()), "\n")
			dev := h.p("dev")
			rb := strings.Contains(got, "sgdisk -d 2 "+dev+"/sdc")
			if rb != tc.rollback || rb != strings.Contains(got, "mdadm --stop "+dev+"/md/p1-1") ||
				rb != strings.Contains(got, "wipefs -q -a "+dev+"/md/p1-1") {
				t.Fatalf("откат %v, ожидался %v:\n%s", rb, tc.rollback, got)
			}
			if strings.Contains(got, "zap-all") {
				t.Fatalf("диск принят в слой 0, но очищен:\n%s", got)
			}
			if tc.unknown && len(res.PendingErrors) == 0 {
				t.Fatalf("нет причины: %+v", res)
			}
		})
	}
}

// JBOD: новый слой — единственное, куда входит диск; не принят → откат и
// очистка диска, принят по PV раздела в VG.
func TestAssembleJBODNewLayer(t *testing.T) {
	for _, inVG := range []bool{false, true} {
		t.Run(fmt.Sprintf("в VG=%v", inVG), func(t *testing.T) {
			h := newHost(t)
			h.part("sdb", 1)
			h.part("sdd", 2)
			h.write("sys/devices/sdd/sdd2/uevent", "PARTNAME=dn-p1-1\n")
			h.vgs = []string{"dn_p1"}
			h.pvs[h.p("dev/sdb1")] = "dn_p1"
			if inVG {
				h.pvs[h.p("dev/sdd2")] = "dn_p1"
			}
			p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelSingle, Members: []string{ref101.ID}}},
				Pending: &PendingAdd{DiskID: ref103.ID,
					New: []NewArray{{Index: 1, Level: LevelSingle, SliceBytes: slice0, Members: []DiskRef{ref103}}}}}
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			res, err := h.e.Assemble(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(writes(h.commands()), "\n")
			if (len(res.PendingAccepted) == 1) != inVG || res.PendingReleased == inVG ||
				strings.Contains(got, "wipefs -q -a "+h.p("dev/sdd2")) == inVG {
				t.Fatalf("%+v\n%s", res, got)
			}
		})
	}
}

func TestPendingNewValidate(t *testing.T) {
	p := AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelSingle, Members: []string{ref101.ID}}},
		Pending: &PendingAdd{DiskID: ref103.ID, New: []NewArray{{Index: 2, Level: LevelSingle, SliceBytes: slice0, Members: []DiskRef{ref103}}}}}
	if p.Validate() == nil {
		t.Fatal("разрыв индексов принят")
	}
	p.Pending.New[0].Index = 1
	p.Pending.New[0].Members = []DiskRef{ref101}
	if p.Validate() == nil {
		t.Fatal("слой без ожидаемого диска принят")
	}
}

const uuidB = "ARRAY /dev/md/p1-1 metadata=1.2 UUID=1111:2222:3333:4444 name=h:p1-1\n"

// Запись: ожидание с расширением слоя 0 и новым слоем 1.
func newLayerPending(r PoolRecord) PoolRecord {
	r.Arrays = []ArraySpec{{Level: LevelRAID1, SliceBytes: slice0, Members: []string{ref101.ID, ref102.ID}}}
	r.Disks = []DiskRef{ref101, big102}
	r.Pending = &PoolPending{Disk: big103, Type: "shr",
		Arrays: []ArraySpec{
			{Level: LevelRAID5, SliceBytes: slice0, Members: []string{ref101.ID, ref102.ID, ref103.ID}},
			{Level: LevelRAID1, SliceBytes: gib, Members: []string{ref102.ID, ref103.ID}},
		},
		Expand:    []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: slice0}},
		NewArrays: []NewArray{{Index: 1, Level: LevelRAID1, SliceBytes: gib, Members: []DiskRef{big102, big103}}}}
	return r
}

func TestResolveNewLayer(t *testing.T) {
	r := newLayerPending(PoolRecord{Name: "p1", Type: "shr"})
	// Слой 0 принят, новый — нет: дозавершение только нового слоя.
	part := r.resolve([]int{0})
	if len(part.Arrays) != 1 || part.Pending == nil || len(part.Pending.Expand) != 0 || len(part.Pending.NewArrays) != 1 || !part.GrowPending {
		t.Fatalf("частично: %+v %+v", part, part.Pending)
	}
	tp, ok := part.resumeTask()
	if !ok || !tp.AddDisk.Resume || len(tp.AddDisk.Arrays) != 0 || len(tp.AddDisk.NewArrays) != 1 || len(tp.Record.Arrays) != 2 ||
		tp.Record.Pending != nil || !tp.Record.GrowPending {
		t.Fatalf("дозавершение: %+v", tp)
	}
	if err := tp.AddDisk.Validate(); err != nil {
		t.Fatal(err)
	}
	// Оба приняты.
	all := r.resolve([]int{0, 1})
	if len(all.Arrays) != 2 || all.Pending != nil || len(all.Disks) != 3 || !hasMember(all.Arrays[1].Members, ref103.ID) {
		t.Fatalf("всё: %+v", all)
	}
	// Только новый слой (слой 0 не принят) — новый слой в записи, слой 0 ждёт.
	only := r.resolve([]int{1})
	if len(only.Arrays) != 2 || only.Pending == nil || len(only.Pending.Expand) != 1 || len(only.Pending.NewArrays) != 0 {
		t.Fatalf("только новый: %+v %+v", only, only.Pending)
	}
	// Ничего — откат.
	if rb := r.resolve(nil); rb.Pending != nil || len(rb.Arrays) != 1 || len(rb.Disks) != 2 {
		t.Fatalf("откат: %+v", rb)
	}
}

// Сброс после расширения слоя 0 до vgextend нового слоя: при старте —
// задача дозавершения только нового слоя.
func TestAssembleAllSubmitsNewLayerResume(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	rec, _ := e.s.getRecord(ctx, "p1")
	if err := e.s.saveRecord(ctx, newLayerPending(rec), false); err != nil {
		t.Fatal(err)
	}
	e.h.assemble = &AssembleResult{PendingAccepted: []int{0}}
	st := okStatus(LevelRAID5, 3, 2*gib)
	st.Arrays = append(st.Arrays, ArrayStatus{Index: 1, Name: "p1-1", Level: LevelRAID1, RaidDevices: 2, ActiveDevices: 2})
	e.h.setStatus(st)
	if err := e.s.AssembleAll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var sent AssembleParams
	json.Unmarshal(e.h.params[OpAssemble], &sent)
	if sent.Pending == nil || len(sent.Pending.New) != 1 {
		t.Fatalf("новый слой не передан в сборку: %+v", sent.Pending)
	}
	deadline := time.Now().Add(3 * time.Second)
	for e.h.called(OpGrow) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	var p AddDiskParams
	e.h.mu.Lock()
	json.Unmarshal(e.h.params[OpAddDisk], &p)
	e.h.mu.Unlock()
	if !p.Resume || len(p.Arrays) != 0 || len(p.NewArrays) != 1 || p.NewArrays[0].Index != 1 {
		t.Fatalf("дозавершение: %+v", p)
	}
	for time.Now().Before(deadline) {
		if r, _ := e.s.getRecord(ctx, "p1"); !r.GrowPending && r.Pending == nil {
			if len(r.Arrays) != 2 || len(r.Disks) != 3 {
				t.Fatalf("запись: %+v", r)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	r, _ := e.s.getRecord(ctx, "p1")
	t.Fatalf("рост не снят: %+v", r)
}

// Сброс во время resync нового слоя (запись уже новая, ожидания нет,
// reshape нет): GrowPending → задача роста при старте, после неё снят.
func TestAssembleAllGrowPending(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	rec, _ := e.s.getRecord(ctx, "p1")
	rec.GrowPending = true
	if err := e.s.saveRecord(ctx, rec, false); err != nil {
		t.Fatal(err)
	}
	e.h.setStatus(okStatus(LevelRAID1, 2, gib))
	if err := e.s.AssembleAll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := e.s.getRecord(ctx, "p1"); !r.GrowPending {
			if e.h.called(OpGrow) != 1 {
				t.Fatalf("grow вызван %d раз", e.h.called(OpGrow))
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("задача роста не выполнена")
}
