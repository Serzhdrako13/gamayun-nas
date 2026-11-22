package home

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Сброс внутри add_disk. Пул raid1 sdb+sdc, ожидается
// добавление sdd с переходом в raid5 из 3 участников.
func pendingParams() AssembleParams {
	return AssembleParams{Pool: "p1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{ref101.ID, ref102.ID}}},
		Pending: &PendingAdd{DiskID: ref103.ID, Arrays: []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: gib - PartitionReserve}}}}
}

const uuidA = "ARRAY /dev/md/p1-0 metadata=1.2 UUID=aaaa:bbbb:cccc:dddd name=h:p1-0\n"

func TestAssemblePendingAdd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		part     bool   // раздел sdd1 создан
		sb       string // суперблок md на sdd1
		mdstat   string
		accepted bool
		grow     []string // ожидаемые --grow
	}{
		{name: "до разметки", mdstat: mdstatRAID1},
		{name: "до --add", part: true, mdstat: mdstatRAID1},
		{name: "чужой суперблок", part: true, sb: strings.Replace(uuidA, "aaaa", "ffff", 1), mdstat: mdstatRAID1},
		{name: "после --add до --grow", part: true, sb: uuidA, accepted: true,
			mdstat: "Personalities : [raid1]\nmd127 : active raid1 sdd1[2](S) sdc1[1] sdb1[0]\n      1045504 blocks super 1.2 [2/2] [UU]\n\nunused devices: <none>\n",
			grow:   []string{"--level=5", "--raid-devices=3"}},
		{name: "после --level", part: true, sb: uuidA, accepted: true,
			mdstat: "Personalities : [raid5]\nmd127 : active raid5 sdd1[2](S) sdc1[1] sdb1[0]\n      1045504 blocks super 1.2 level 5, 64k chunk, algorithm 2 [2/2] [UU]\n\nunused devices: <none>\n",
			grow:   []string{"--raid-devices=3"}},
		{name: "reshape идёт", part: true, sb: uuidA, accepted: true,
			mdstat: "Personalities : [raid5]\nmd127 : active raid5 sdd1[2] sdc1[1] sdb1[0]\n      1045504 blocks super 1.2 level 5, 64k chunk, algorithm 2 [3/3] [UUU]\n      [>....................]  reshape =  1.0% (1/1) finish=1.0min speed=1K/sec\n\nunused devices: <none>\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			h.part("sdb", 1)
			h.part("sdc", 1)
			if tc.part {
				h.part("sdd", 1)
				h.write("sys/devices/sdd/sdd1/uevent", "PARTN=1\nPARTNAME=dn-p1-0\n")
			}
			h.examine = map[string]string{h.p("dev/sdb1"): uuidA, h.p("dev/sdc1"): uuidA}
			if tc.sb != "" {
				h.examine[h.p("dev/sdd1")] = tc.sb
			}
			h.write("proc/mdstat", tc.mdstat)
			res, err := h.e.Assemble(context.Background(), pendingParams())
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(writes(h.commands()), "\n")
			asm := "mdadm --assemble --run " + h.p("dev/md/p1-0") + " " + h.p("dev/sdb1") + " " + h.p("dev/sdc1")
			if !strings.Contains(got, asm) || strings.Contains(got, asm+" "+h.p("dev/sdd1")) != tc.accepted {
				t.Fatalf("сборка:\n%s", got)
			}
			if (len(res.PendingAccepted) == 1) != tc.accepted || len(res.MissingDisks) != 0 || len(res.PendingErrors) != 0 {
				t.Fatalf("%+v", res)
			}
			// Не принятый размеченный диск очищается.
			wantRel := tc.part && !tc.accepted
			if res.PendingReleased != wantRel || strings.Contains(got, "sgdisk --zap-all "+h.p("dev/sdd")) != wantRel {
				t.Fatalf("очистка %v, ожидалась %v:\n%s", res.PendingReleased, wantRel, got)
			}
			var grows []string
			for _, l := range strings.Split(got, "\n") {
				if strings.HasPrefix(l, "mdadm --grow") {
					f := strings.Fields(l)
					grows = append(grows, f[len(f)-1])
				}
			}
			if strings.Join(grows, ",") != strings.Join(tc.grow, ",") {
				t.Fatalf("grow %v, ожидалось %v:\n%s", grows, tc.grow, got)
			}
		})
	}
}

func TestPendingParamsValidate(t *testing.T) {
	p := pendingParams()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Pending.Arrays[0].Index = 1
	if p.Validate() == nil {
		t.Fatal("индекс вне массивов принят")
	}
	p = pendingParams()
	p.Pending.DiskID = ref101.ID
	if p.Validate() == nil {
		t.Fatal("ожидаемый диск уже участник")
	}
}

// gamayund: ожидаемый состав пишется до вызова add_disk; при старте —
// принятие (запись с новым диском, задача роста) или откат.
func TestAssembleAllResolvesPending(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		e := newSvc(t)
		ctx := context.Background()
		seedPool(t, e, "shr", false)
		rec, _ := e.s.getRecord(ctx, "p1")
		rec.Pending = &PoolPending{Disk: ref103, Type: "shr",
			Arrays: []ArraySpec{{Level: LevelRAID5, SliceBytes: gib - PartitionReserve, Members: []string{ref101.ID, ref102.ID, ref103.ID}}},
			Expand: []ExpandArray{{Index: 0, Level: LevelRAID5, RaidDevices: 3, SliceBytes: gib - PartitionReserve}}}
		if err := e.s.saveRecord(ctx, rec, false); err != nil {
			t.Fatal(err)
		}
		ar := AssembleResult{}
		if accepted {
			ar.PendingAccepted = []int{0}
		}
		e.h.assemble = &ar
		e.h.setStatus(okStatus(LevelRAID5, 3, 2*gib))
		if err := e.s.AssembleAll(ctx, nil); err != nil {
			t.Fatal(err)
		}
		var p AssembleParams
		json.Unmarshal(e.h.params[OpAssemble], &p)
		if p.Pending == nil || p.Pending.DiskID != ref103.ID {
			t.Fatalf("ожидание не передано в сборку: %+v", p)
		}
		got, _ := e.s.getRecord(ctx, "p1")
		if got.Pending != nil || hasMember(got.Arrays[0].Members, ref103.ID) != accepted || (len(got.Disks) == 3) != accepted {
			t.Fatalf("accepted=%v: %+v", accepted, got)
		}
	}
}

// Ожидание пишется до вызова helper add_disk.
func TestAddDiskWritesPendingFirst(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	seedPool(t, e, "shr", false)
	e.h.setStatus(okStatus(LevelRAID1, 2, gib-3*Align))
	pl, err := e.s.Plan(ctx, PlanRequest{Op: PlanAddDisk, Pool: "p1", DiskID: ref103.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.s.mu.Lock()
	tp := e.s.plans[pl.PlanID].task
	e.s.mu.Unlock()
	var seen *PoolPending
	e.h.hook = func(op string) {
		if op == OpAddDisk {
			r, _ := e.s.getRecord(ctx, "p1")
			seen = r.Pending
		}
	}
	e.h.setStatus(okStatus(LevelRAID5, 3, 2*gib))
	e.h.fail = map[string]error{OpGrow: context.Canceled}
	raw, _ := json.Marshal(tp)
	e.s.runAddDisk(ctx, raw, func(int) {})
	if seen == nil || seen.Disk.ID != ref103.ID || !hasMember(seen.Arrays[0].Members, ref103.ID) {
		t.Fatalf("ожидание до add_disk: %+v", seen)
	}
	if r, _ := e.s.getRecord(ctx, "p1"); r.Pending != nil || !hasMember(r.Arrays[0].Members, ref103.ID) {
		t.Fatalf("после add_disk: %+v", r)
	}
}
