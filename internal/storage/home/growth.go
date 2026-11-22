package home

// Рост пула после замены диска на больший.
//
// Разделы прежних дисков не переразмечаются: рост — только за счёт
// свободного места за последним разделом каждого диска.
//   - Заменённый диск Y входит в вышележащие слои, верхнюю границу которых
//     он покрывает (SHR: raid1 из 2 → raid5 из 3; raid5/raid6 — +1); слой,
//     который Y режет, — нет (остаток Y не используется, замена не
//     отказывает из-за «лишнего» места).
//   - Если Y вошёл во все слои, над верхней границей пула строятся новые
//     слои: SHR/SHR-2 — по правилам hybrid на хвостах дисков, достающих
//     выше верха; RAID1/5/6/10 — один слой того же уровня на хвостах всех
//     дисков, только когда хвост есть у каждого (после замены последнего
//     малого диска). Ёмкость RAID растёт новым слоем, а не mdadm --grow
//     --size: это механизм нового слоя (проверен сбросами) без роста разделов
//     занятых дисков и без --grow --size с bitmap и integrity.

import (
	"fmt"
	"sort"
)

// growth — итог расчёта роста.
type growth struct {
	Expand []ExpandArray
	New    []NewArray
	Final  []ArraySpec // состав всех слоёв после роста
	Notes  []string    // почему часть места не используется
}

func availBytes(size int64) int64 { return (size - PartitionReserve) / Align * Align }

// growthPlan считает рост пула rec (уже после замены: Y — участник) за
// счёт свободного места диска y. Пустой итог — расти нечему.
func growthPlan(rec PoolRecord, y DiskRef) (growth, error) {
	g := growth{Final: append([]ArraySpec{}, rec.Arrays...)}
	switch rec.Type {
	case "raid1", "raid5", "raid6", "raid10", "shr", "shr2":
	default:
		return g, fmt.Errorf("тип %s: рост после замены не поддерживается", rec.Type)
	}
	sizes := map[string]int64{}
	for _, d := range rec.Disks {
		sizes[d.ID] = d.SizeBytes
	}
	if _, ok := sizes[y.ID]; !ok {
		return g, fmt.Errorf("диск %s не в пуле", y.ID)
	}
	tops := make([]int64, len(rec.Arrays)) // верхняя граница слоя i на диске
	t := int64(0)
	for i, a := range rec.Arrays {
		t += a.SliceBytes
		tops[i] = t
	}
	// Слои Y — 0..m-1 подряд.
	m := 0
	for m < len(rec.Arrays) && hasMember(rec.Arrays[m].Members, y.ID) {
		m++
	}
	for i := m; i < len(rec.Arrays); i++ {
		if hasMember(rec.Arrays[i].Members, y.ID) {
			return g, fmt.Errorf("диск %s: слои не подряд (%d)", y.ID, i)
		}
	}
	shr := rec.Type == "shr" || rec.Type == "shr2"
	avail := availBytes(y.SizeBytes)
	for i := m; i < len(rec.Arrays); i++ {
		if avail < tops[i] {
			g.Notes = append(g.Notes, fmt.Sprintf("диск %s режет слой %d: остаток диска не используется", y.ID, i))
			return g, nil
		}
		if !shr {
			return g, fmt.Errorf("слой %d пула %s без диска %s", i, rec.Type, y.ID)
		}
		a := rec.Arrays[i]
		n := len(a.Members) + 1
		level := a.Level
		switch {
		case a.Level == LevelRAID1 && rec.Type == "shr" && n == 3:
			level = LevelRAID5
		case a.Level == LevelRAID5 || a.Level == LevelRAID6:
		default:
			// raid1 из 3 → raid6 из 4 (SHR-2) md не умеет одной командой.
			g.Notes = append(g.Notes, fmt.Sprintf("слой %d (%s из %d): рост на диск %s не поддерживается", i, a.Level, len(a.Members), y.ID))
			return g, nil
		}
		g.Expand = append(g.Expand, ExpandArray{Index: i, Level: level, RaidDevices: n, SliceBytes: a.SliceBytes})
		g.Final[i] = ArraySpec{Level: level, SliceBytes: a.SliceBytes, Members: append(append([]string{}, a.Members...), y.ID)}
	}
	// Новые слои над верхом пула: участники — диски всех слоёв.
	top := int64(0)
	if len(tops) > 0 {
		top = tops[len(tops)-1]
	}
	var tails []diskTail
	for _, d := range rec.Disks {
		inAll := true
		for _, a := range g.Final {
			inAll = inAll && hasMember(a.Members, d.ID)
		}
		if h := availBytes(d.SizeBytes) - top; inAll && h >= 64*Align {
			tails = append(tails, diskTail{d.ID, h})
		} else if !shr {
			return g, nil // RAID: новый слой — только когда хвост есть у всех
		}
	}
	sort.Slice(tails, func(i, j int) bool {
		if tails[i].size != tails[j].size {
			return tails[i].size < tails[j].size
		}
		return tails[i].id < tails[j].id
	})
	var newSpecs []ArraySpec
	if !shr {
		if len(tails) > 0 {
			newSpecs = append(newSpecs, ArraySpec{Level: rec.Arrays[0].Level, SliceBytes: tails[0].size, Members: tailIDs(tails)})
		}
	} else {
		parity := 1
		if rec.Type == "shr2" {
			parity = 2
		}
		prev := int64(0)
		for i := range tails {
			h := tails[i].size - prev
			if h < 64*Align {
				continue
			}
			k := len(tails) - i
			level := ""
			switch {
			case parity == 1 && k >= 3:
				level = LevelRAID5
			case parity == 1 && k == 2:
				level = LevelRAID1
			case parity == 2 && k >= 4:
				level = LevelRAID6
			case parity == 2 && k == 3:
				level = LevelRAID1
			default:
				prev = tails[i].size
				continue
			}
			newSpecs = append(newSpecs, ArraySpec{Level: level, SliceBytes: h, Members: tailIDs(tails[i:])})
			prev = tails[i].size
		}
	}
	for _, sp := range newSpecs {
		if !hasMember(sp.Members, y.ID) {
			g.Notes = append(g.Notes, fmt.Sprintf("свободное место без диска %s (%d Б) не используется", y.ID, sp.SliceBytes))
			break
		}
		idx := len(g.Final)
		if idx >= 16 {
			g.Notes = append(g.Notes, "достигнут предел 16 слоёв")
			break
		}
		na := NewArray{Index: idx, Level: sp.Level, SliceBytes: sp.SliceBytes}
		for _, id := range sp.Members {
			na.Members = append(na.Members, DiskRef{ID: id, SizeBytes: sizes[id]})
		}
		g.New = append(g.New, na)
		g.Final = append(g.Final, sp)
	}
	return g, nil
}

// diskTail — свободное место диска над верхом пула.
type diskTail struct {
	id   string
	size int64
}

func tailIDs(ts []diskTail) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.id
	}
	return out
}
