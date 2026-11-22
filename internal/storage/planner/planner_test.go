package planner

import (
	"errors"
	"fmt"
	"testing"
)

const TB = int64(1_000_000_000_000)

// Размеры в тестах кратны Align, чтобы выравнивание не искажало ожидания.
func tb(n int64) int64 { return alignDown(n * TB) }

func disks(sizes ...int64) []Disk {
	out := make([]Disk, len(sizes))
	for i, s := range sizes {
		out[i] = Disk{ID: fmt.Sprintf("d%d", i+1), Size: tb(s)}
	}
	return out
}

func TestCapacity(t *testing.T) {
	cases := []struct {
		name      string
		typ       PoolType
		sizes     []int64
		usable    int64 // в TB (до выравнивания)
		unused    int64
		tolerance int
	}{
		{"basic", Basic, []int64{4}, 4, 0, 0},
		{"jbod разные", JBOD, []int64{1, 2, 3}, 6, 0, 0},
		{"raid0", RAID0, []int64{2, 2}, 4, 0, 0},
		{"raid1 два", RAID1, []int64{4, 4}, 4, 0, 1},
		{"raid1 три", RAID1, []int64{4, 4, 4}, 4, 0, 2},
		{"raid1 разные", RAID1, []int64{2, 4}, 2, 2, 1},
		{"raid5", RAID5, []int64{4, 4, 4}, 8, 0, 1},
		{"raid6", RAID6, []int64{4, 4, 4, 4}, 8, 0, 2},
		{"raid10", RAID10, []int64{4, 4, 4, 4}, 8, 0, 1},
		// SHR: примеры, совпадающие с публичной логикой калькулятора Synology.
		{"shr один диск", SHR, []int64{4}, 4, 0, 0},
		{"shr 2+4", SHR, []int64{2, 4}, 2, 2, 1},
		{"shr 4+4", SHR, []int64{4, 4}, 4, 0, 1},
		{"shr 2+4+4", SHR, []int64{2, 4, 4}, 6, 0, 1},
		{"shr 1+2+3+4", SHR, []int64{1, 2, 3, 4}, 6, 1, 1},
		{"shr 4+4+8", SHR, []int64{4, 4, 8}, 8, 4, 1},
		{"shr 4+8+8", SHR, []int64{4, 8, 8}, 12, 0, 1},
		{"shr2 4x4", SHR2, []int64{4, 4, 4, 4}, 8, 0, 2},
		{"shr2 4,4,4,4,8", SHR2, []int64{4, 4, 4, 4, 8}, 12, 4, 2},
		{"shr2 2,4,4,4,4", SHR2, []int64{2, 4, 4, 4, 4}, 10, 0, 2},
		{"shr2 тройной слой", SHR2, []int64{2, 2, 4, 4, 4}, 8, 0, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Compute(Request{Type: c.typ, Disks: disks(c.sizes...)})
			if err != nil {
				t.Fatal(err)
			}
			// Сравнение с точностью до выравнивания на каждом слое.
			near := func(got, wantTB int64) bool {
				d := got - wantTB*TB
				if d < 0 {
					d = -d
				}
				return d < int64(len(c.sizes)*4)*Align
			}
			if !near(p.Usable, c.usable) {
				t.Errorf("usable = %d, want ~%d TB", p.Usable, c.usable)
			}
			if !near(p.Unused, c.unused) {
				t.Errorf("unused = %d, want ~%d TB", p.Unused, c.unused)
			}
			if p.Tolerance != c.tolerance {
				t.Errorf("tolerance = %d, want %d", p.Tolerance, c.tolerance)
			}
			if p.Usable+p.Unused > p.Raw {
				t.Errorf("usable+unused > raw")
			}
		})
	}
}

func TestSHRLayers(t *testing.T) {
	p, err := Compute(Request{Type: SHR, Disks: disks(1, 2, 3, 4)})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		level   string
		members int
	}{{"raid5", 4}, {"raid5", 3}, {"raid1", 2}}
	if len(p.Arrays) != len(want) {
		t.Fatalf("arrays = %+v", p.Arrays)
	}
	for i, w := range want {
		if p.Arrays[i].Level != w.level || len(p.Arrays[i].Members) != w.members {
			t.Errorf("layer %d = %s/%d, want %s/%d", i, p.Arrays[i].Level, len(p.Arrays[i].Members), w.level, w.members)
		}
	}
}

func TestSystemReserve(t *testing.T) {
	reserve := int64(8 << 30)
	p, err := Compute(Request{Type: RAID1, Disks: disks(4, 4), SystemReserve: reserve})
	if err != nil {
		t.Fatal(err)
	}
	if want := alignDown(tb(4) - reserve); p.Usable != want {
		t.Errorf("usable = %d, want %d", p.Usable, want)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		req  Request
		want error
	}{
		{Request{Type: RAID5}, ErrNoDisks},
		{Request{Type: "raidX", Disks: disks(1)}, ErrUnknownType},
		{Request{Type: RAID5, Disks: disks(1, 1)}, ErrTooFewDisks},
		{Request{Type: RAID6, Disks: disks(1, 1, 1)}, ErrTooFewDisks},
		{Request{Type: SHR2, Disks: disks(1, 1, 1)}, ErrTooFewDisks},
		{Request{Type: RAID10, Disks: disks(1, 1, 1, 1, 1)}, ErrTooFewDisks},
		{Request{Type: Basic, Disks: disks(1, 1)}, ErrTooFewDisks},
		{Request{Type: RAID1, Disks: []Disk{{"a", TB}, {"a", TB}}}, ErrDuplicateDisk},
		{Request{Type: Basic, Disks: []Disk{{"a", 1 << 20}}, SystemReserve: 8 << 30}, ErrDiskTooSmall},
	}
	for _, c := range cases {
		if _, err := Compute(c.req); !errors.Is(err, c.want) {
			t.Errorf("%v: err = %v, want %v", c.req.Type, err, c.want)
		}
	}
}

func TestWarnings(t *testing.T) {
	p, _ := Compute(Request{Type: RAID0, Disks: disks(1, 1)})
	if len(p.Warnings) == 0 {
		t.Error("RAID0 без предупреждения о защите")
	}
	p, _ = Compute(Request{Type: SHR, Disks: disks(4, 4, 4)})
	if len(p.Warnings) != 0 {
		t.Errorf("лишние предупреждения: %v", p.Warnings)
	}
}

func TestDeterministicOrder(t *testing.T) {
	a, _ := Compute(Request{Type: SHR, Disks: []Disk{{"b", tb(4)}, {"a", tb(4)}, {"c", tb(2)}}})
	b, _ := Compute(Request{Type: SHR, Disks: []Disk{{"c", tb(2)}, {"a", tb(4)}, {"b", tb(4)}}})
	if fmt.Sprint(a.Arrays) != fmt.Sprint(b.Arrays) {
		t.Errorf("порядок дисков меняет план:\n%v\n%v", a.Arrays, b.Arrays)
	}
}

// verified — переходы, проверенные на стенде
// (2026-11-04…12): тип, число дисков, операция, цель. Все
// остальные должны быть Verified=false.
var verified = func() map[string]bool {
	m := map[string]bool{
		"raid1/2/convert/raid5": true, // RAID 1 → RAID 5 добавлением диска (с dm-integrity)
		// Замена на больший с ростом.
		"raid1/2/replace_larger/raid1": true,
		"shr/3/replace_larger/shr":     true,
	}
	// +1 диск того же среза; число дисков — от минимума типа.
	for n := 2; n <= 8; n++ {
		m[fmt.Sprintf("shr/%d/add_disk/shr", n)] = true
		m[fmt.Sprintf("raid1/%d/add_mirror/raid1", n)] = true
		m[fmt.Sprintf("jbod/%d/add_disk/jbod", n)] = true // новый слой single
		if n >= 3 {
			m[fmt.Sprintf("raid5/%d/add_disk/raid5", n)] = true
			m[fmt.Sprintf("raid5/%d/convert/raid6", n)] = true // 1.2.4
			m[fmt.Sprintf("shr/%d/convert/shr2", n)] = true
		}
		if n >= 4 {
			m[fmt.Sprintf("raid6/%d/add_disk/raid6", n)] = true
			m[fmt.Sprintf("shr2/%d/add_disk/shr2", n)] = true
		}
	}
	return m
}()

func TestTransitionsVerifiedOnlyOnStand(t *testing.T) {
	for _, typ := range []PoolType{Basic, JBOD, RAID0, RAID1, RAID5, RAID6, RAID10, SHR, SHR2} {
		for n := 1; n <= 8; n++ {
			for _, x := range Transitions(typ, n) {
				key := fmt.Sprintf("%s/%d/%s/%s", typ, n, x.Op, x.Target)
				if x.Verified != verified[key] {
					t.Errorf("%s: Verified=%v", key, x.Verified)
				}
			}
		}
	}
}

func TestTransitions(t *testing.T) {
	has := func(tr []Transition, op Op, target PoolType) bool {
		for _, x := range tr {
			if x.Op == op && x.Target == target {
				return true
			}
		}
		return false
	}
	if !has(Transitions(Basic, 1), OpConvert, RAID1) {
		t.Error("basic → raid1")
	}
	if !has(Transitions(RAID1, 2), OpConvert, RAID5) {
		t.Error("raid1 → raid5")
	}
	if !has(Transitions(SHR, 2), OpAddDisk, SHR) {
		t.Error("shr + диск")
	}
	if has(Transitions(SHR, 2), OpConvert, SHR2) {
		t.Error("shr(2) → shr2 не должен предлагаться")
	}
	if !has(Transitions(SHR, 3), OpConvert, SHR2) {
		t.Error("shr(3) → shr2")
	}
	if len(Transitions(RAID0, 2)) != 0 {
		t.Error("raid0 не расширяется")
	}
}
