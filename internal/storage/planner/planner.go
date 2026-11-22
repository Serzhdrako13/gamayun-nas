// Package planner рассчитывает раскладку пула профиля Home
// (разделы → [dm-integrity] → md → LVM → Btrfs) без обращения к дискам.
//
// Чистая функция: на входе размеры дисков и тип пула, на выходе слои,
// md-массивы, полезная ёмкость, допустимые отказы, неиспользуемый остаток
// и возможные переходы. Ничего не проверяет на реальном оборудовании;
// Verified=true — только у переходов, проверенных на стенде.
package planner

import (
	"errors"
	"fmt"
	"sort"
)

// PoolType — тип пула, совпадающий с вариантами Synology DSM.
type PoolType string

const (
	Basic  PoolType = "basic"
	JBOD   PoolType = "jbod"
	RAID0  PoolType = "raid0"
	RAID1  PoolType = "raid1"
	RAID5  PoolType = "raid5"
	RAID6  PoolType = "raid6"
	RAID10 PoolType = "raid10"
	SHR    PoolType = "shr"
	SHR2   PoolType = "shr2"
)

// Align — выравнивание разделов и слоёв (1 МиБ).
const Align int64 = 1 << 20

// Disk — диск-кандидат. ID — стабильный внутренний идентификатор (алиас/WWN).
type Disk struct {
	ID   string
	Size int64 // байты
}

// Request — запрос на расчёт.
type Request struct {
	Type  PoolType
	Disks []Disk
	// SystemReserve — место на каждом диске под системные разделы
	// (ESP + системный RAID1), вычитается до расчёта данных.
	SystemReserve int64
}

// Array — один md-массив (или LVM linear для JBOD, или один раздел для Basic).
type Array struct {
	Level     string   // "raid1", "raid5", "raid6", "raid10", "raid0", "linear", "single"
	Members   []string // ID дисков
	SliceSize int64    // размер раздела на каждом участнике
	Usable    int64
	Tolerance int // гарантированно переживаемых отказов дисков
}

// Plan — результат расчёта.
type Plan struct {
	Type      PoolType
	Arrays    []Array
	Usable    int64 // полезная ёмкость под Btrfs (до накладных расходов ФС)
	Raw       int64 // сумма размеров дисков после SystemReserve
	Unused    int64 // место, не вошедшее ни в один массив
	Tolerance int   // гарантированно переживаемых отказов любого диска
	Warnings  []string
}

var (
	ErrNoDisks       = errors.New("не выбраны диски")
	ErrUnknownType   = errors.New("неизвестный тип пула")
	ErrTooFewDisks   = errors.New("недостаточно дисков для выбранного типа")
	ErrDuplicateDisk = errors.New("диск указан дважды")
	ErrDiskTooSmall  = errors.New("диск меньше системного резерва")
)

func alignDown(v int64) int64 { return v / Align * Align }

// MinDisks возвращает минимальное число дисков для типа.
func MinDisks(t PoolType) (int, error) {
	switch t {
	case Basic, JBOD, SHR:
		return 1, nil
	case RAID0, RAID1:
		return 2, nil
	case RAID5:
		return 3, nil
	case RAID6, RAID10, SHR2:
		return 4, nil
	}
	return 0, ErrUnknownType
}

// Compute строит план пула.
func Compute(req Request) (Plan, error) {
	min, err := MinDisks(req.Type)
	if err != nil {
		return Plan{}, err
	}
	if len(req.Disks) == 0 {
		return Plan{}, ErrNoDisks
	}
	if len(req.Disks) < min {
		return Plan{}, fmt.Errorf("%w: %s требует не менее %d", ErrTooFewDisks, req.Type, min)
	}
	if req.Type == Basic && len(req.Disks) != 1 {
		return Plan{}, fmt.Errorf("%w: basic — ровно один диск", ErrTooFewDisks)
	}
	if req.Type == RAID10 && len(req.Disks)%2 != 0 {
		return Plan{}, fmt.Errorf("%w: raid10 требует чётного числа дисков", ErrTooFewDisks)
	}

	disks := make([]Disk, 0, len(req.Disks))
	seen := map[string]bool{}
	for _, d := range req.Disks {
		if seen[d.ID] {
			return Plan{}, fmt.Errorf("%w: %s", ErrDuplicateDisk, d.ID)
		}
		seen[d.ID] = true
		size := alignDown(d.Size - req.SystemReserve)
		if size <= 0 {
			return Plan{}, fmt.Errorf("%w: %s", ErrDiskTooSmall, d.ID)
		}
		disks = append(disks, Disk{ID: d.ID, Size: size})
	}
	// По возрастанию размера; при равенстве — по ID для детерминизма.
	sort.Slice(disks, func(i, j int) bool {
		if disks[i].Size != disks[j].Size {
			return disks[i].Size < disks[j].Size
		}
		return disks[i].ID < disks[j].ID
	})

	p := Plan{Type: req.Type}
	for _, d := range disks {
		p.Raw += d.Size
	}

	switch req.Type {
	case Basic:
		p.Arrays = []Array{{Level: "single", Members: ids(disks), SliceSize: disks[0].Size, Usable: disks[0].Size}}
	case JBOD:
		p.Arrays = []Array{{Level: "linear", Members: ids(disks), Usable: p.Raw}}
	case RAID0, RAID1, RAID5, RAID6, RAID10:
		p.Arrays = []Array{uniform(req.Type, disks)}
	case SHR:
		p.Arrays = hybrid(disks, 1)
	case SHR2:
		p.Arrays = hybrid(disks, 2)
	}

	p.Tolerance = -1
	used := int64(0)
	for _, a := range p.Arrays {
		p.Usable += a.Usable
		if a.Level == "linear" {
			used += p.Raw
		} else {
			used += a.SliceSize * int64(len(a.Members))
		}
		if p.Tolerance < 0 || a.Tolerance < p.Tolerance {
			p.Tolerance = a.Tolerance
		}
	}
	if p.Tolerance < 0 {
		p.Tolerance = 0
	}
	p.Unused = p.Raw - used
	p.Warnings = warnings(p, disks)
	return p, nil
}

func ids(ds []Disk) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func uniform(t PoolType, disks []Disk) Array {
	n := int64(len(disks))
	slice := disks[0].Size // наименьший
	a := Array{Level: string(t), Members: ids(disks), SliceSize: slice}
	switch t {
	case RAID0:
		a.Usable, a.Tolerance = n*slice, 0
	case RAID1:
		a.Usable, a.Tolerance = slice, int(n)-1
	case RAID5:
		a.Usable, a.Tolerance = (n-1)*slice, 1
	case RAID6:
		a.Usable, a.Tolerance = (n-2)*slice, 2
	case RAID10:
		a.Usable, a.Tolerance = n/2*slice, 1
	}
	return a
}

// hybrid реализует SHR (parity=1) и SHR-2 (parity=2): диски режутся на слои
// по границам различных размеров, каждый слой — отдельный md-массив на всех
// дисках, достающих до этого слоя. Слой, который не может обеспечить
// требуемую защиту, не используется (кроме SHR на одном диске).
func hybrid(disks []Disk, parity int) []Array {
	var arrays []Array
	if parity == 1 && len(disks) == 1 {
		return []Array{{Level: "single", Members: ids(disks), SliceSize: disks[0].Size, Usable: disks[0].Size}}
	}
	prev := int64(0)
	for i := range disks {
		h := disks[i].Size - prev
		if h <= 0 {
			continue
		}
		members := disks[i:]
		k := len(members)
		a := Array{Members: ids(members), SliceSize: h}
		switch {
		case parity == 1 && k >= 3:
			a.Level, a.Usable, a.Tolerance = "raid5", int64(k-1)*h, 1
		case parity == 1 && k == 2:
			a.Level, a.Usable, a.Tolerance = "raid1", h, 1
		case parity == 2 && k >= 4:
			a.Level, a.Usable, a.Tolerance = "raid6", int64(k-2)*h, 2
		case parity == 2 && k == 3:
			a.Level, a.Usable, a.Tolerance = "raid1", h, 2 // тройное зеркало
		default:
			prev = disks[i].Size
			continue // недостаточно дисков для защиты — остаток
		}
		arrays = append(arrays, a)
		prev = disks[i].Size
	}
	return arrays
}

func warnings(p Plan, disks []Disk) []string {
	var w []string
	if p.Tolerance == 0 {
		w = append(w, "Нет защиты от отказа диска: отказ любого диска может привести к потере данных пула")
	}
	if p.Unused > 0 {
		w = append(w, fmt.Sprintf("Не используется %d байт: диски разного размера", p.Unused))
	}
	if p.Type == RAID10 {
		w = append(w, "RAID 10 гарантированно переживает отказ одного диска; два — только если они в разных зеркальных парах")
	}
	if len(disks) > 12 && (p.Type == RAID5 || p.Type == SHR) {
		w = append(w, "Много дисков при одной чётности: долгое восстановление; рекомендуется RAID 6 / SHR-2")
	}
	return w
}
