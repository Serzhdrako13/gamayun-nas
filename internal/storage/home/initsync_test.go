package home

import (
	"strings"
	"testing"
	"time"
)

// Прогноз начальной синхронизации — пересчёт + остаток md на
// скорости пересчёта; после пересчёта — оценка mdstat.
func TestInitialSyncProgress(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	s := &Service{now: func() time.Time { return now }}
	const size = int64(1000) << 20 // 1000 МиБ
	mk := func(recalc int64, mdPct float64, finish *float64) PoolStatus {
		a := ArrayStatus{Level: "raid1", RaidDevices: 2, ActiveDevices: 2, DevSizeBytes: size, Action: "resync", Progress: &mdPct, FinishMin: finish}
		var integ []IntegrityStatus
		for _, n := range []string{"a", "b"} {
			r := recalc
			integ = append(integ, IntegrityStatus{Name: n, ProvidedBytes: size, RecalcBytes: &r, Recalculating: recalc < size})
		}
		return PoolStatus{Pool: "p", InitialSync: true, Arrays: []ArrayStatus{a}, Integrity: integ}
	}
	st := mk(0, 0, nil)
	s.initialSyncProgress(&st)
	if st.Sync.Stage != SyncStageIntegrity || st.Sync.EtaMin != nil {
		t.Fatalf("первый замер %+v", st.Sync)
	}
	// 60 с: пересчитано 600 МиБ на каждом → 10 МиБ/с; остаток 400 + 1000 (md 0 %) = 1400 МиБ → 140 с.
	now = now.Add(time.Minute)
	st = mk(600<<20, 0, nil)
	s.initialSyncProgress(&st)
	if st.Sync.EtaMin == nil || *st.Sync.EtaMin < 2.33 || *st.Sync.EtaMin > 2.34 || st.Sync.Percent != 30 {
		t.Fatalf("прогноз %+v %v", st.Sync, *st.Sync.EtaMin)
	}
	// Пересчёт окончен — оценка mdstat.
	f := 7.5
	st = mk(size, 50, &f)
	s.initialSyncProgress(&st)
	if st.Sync.Stage != SyncStageMirror || st.Sync.EtaMin == nil || *st.Sync.EtaMin != 7.5 || st.Sync.Percent != 75 {
		t.Fatalf("после пересчёта %+v", st.Sync)
	}
	if m := syncBusy("p", st, "рост"); !strings.Contains(m, "75 %") || !strings.Contains(m, "~8 мин") || !strings.Contains(m, "рост — после") {
		t.Fatalf("отказ %q", m)
	}
	// Синхронизация закончена — замеры сброшены, Sync нет.
	st = PoolStatus{Pool: "p"}
	s.initialSyncProgress(&st)
	if st.Sync != nil || len(s.meter.m) != 0 {
		t.Fatalf("после конца %+v %v", st.Sync, s.meter.m)
	}
}

// raid5 при создании — recovery достраиваемого диска: избыточности нет.
func TestInitialSyncNoRedundancy(t *testing.T) {
	s := &Service{now: time.Now}
	pct := 10.0
	st := PoolStatus{Pool: "p", InitialSync: true, Arrays: []ArrayStatus{{Level: "raid5", RaidDevices: 3, ActiveDevices: 2, Missing: 1, Action: "recovery", Progress: &pct}}}
	s.initialSyncProgress(&st)
	if st.Sync == nil || !st.Sync.NoRedundancy {
		t.Fatalf("%+v", st.Sync)
	}
	st.Arrays[0] = ArrayStatus{Level: "raid1", RaidDevices: 2, ActiveDevices: 2, Action: "resync", Progress: &pct}
	s.initialSyncProgress(&st)
	if st.Sync.NoRedundancy {
		t.Fatal("raid1 resync — избыточность есть")
	}
}
