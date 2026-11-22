// Package home — адаптер хранения профиля Home:
//
//	разделы → [dm-integrity] → md → LVM → Btrfs (single)
//
// Две стороны:
//   - Executor (выполняется внутри gamayun-helper, root): строит команды
//     sgdisk, integritysetup, mdadm, lvm, mkfs.btrfs, btrfs, mount и запускает
//     их только через Runner (массив аргументов, без shell). Перед любой
//     разрушающей записью диск заново проверяется (Guard): allowlist
//     идентичностей, полный снимок, роль unknown, размер, нет разделов,
//     holders и сигнатур.
//   - Service (gamayund, без root): план по planner.Compute, одноразовый
//     токен подтверждения, задачи internal/tasks, вызовы helper.
//
// Имена объектов пула p (p — [a-z][a-z0-9]{0,15}):
//
//	раздел массива i      — номер i+1, имя GPT "dn-p-i", тип FD00 (Linux RAID)
//	dm-integrity          — dni_p_i_<8 hex от sha256(ID диска)>
//	md                    — /dev/md/p-i (имя массива p-i)
//	VG / LV               — dn_p / data
//	Btrfs                 — метка p, точка монтирования <mount_root>/p
package home

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Align — выравнивание разделов (совпадает с planner.Align).
const Align int64 = 1 << 20

// PartitionReserve — место на диске под таблицу GPT: 1 МиБ в начале
// (первый раздел с сектора 2048) и 1 МиБ в конце (резервная GPT).
// Передаётся в planner как SystemReserve.
const PartitionReserve = 2 * Align

// MaxDisks — предел дисков в одном пуле.
const MaxDisks = 32

var (
	poolNameRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	diskIDRe   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// ValidPoolName — имя пула: латиница в нижнем регистре и цифры, до 16 символов.
func ValidPoolName(s string) bool { return poolNameRe.MatchString(s) }

// ValidDiskID — формат стабильной идентичности диска (disks.Identity).
func ValidDiskID(s string) bool { return diskIDRe.MatchString(s) }

// VGName — группа томов пула.
func VGName(pool string) string { return "dn_" + pool }

// LVName — логический том данных.
const LVName = "data"

// MDName — имя md-массива i пула.
func MDName(pool string, i int) string { return pool + "-" + strconv.Itoa(i) }

// IntegrityName — имя отображения dm-integrity участника массива i.
func IntegrityName(pool string, i int, diskID string) string {
	h := sha256.Sum256([]byte(diskID))
	return fmt.Sprintf("dni_%s_%d_%s", pool, i, hex.EncodeToString(h[:4]))
}

// IntegrityPrefix — префикс имён dm-integrity пула.
func IntegrityPrefix(pool string) string { return "dni_" + pool + "_" }

// PartLabel — имя раздела GPT «dn-<пул>-<i>». Без двоеточий: sgdisk -c
// «N:имя» обрезает имя на следующем двоеточии — до 2026-11-06 метка
// «dn:p:i» записывалась как «dn». Имя пула —
// [a-z0-9], дефис однозначно разделяет части.
func PartLabel(pool string, i int) string { return fmt.Sprintf("%s%d", PartLabelPrefix(pool), i) }

// PartLabelPrefix — общий префикс меток разделов пула.
func PartLabelPrefix(pool string) string { return "dn-" + pool + "-" }

// Уровни массивов.
const (
	LevelSingle = "single" // один раздел без md (Basic, SHR на одном диске, JBOD)
	LevelRAID0  = "raid0"
	LevelRAID1  = "raid1"
	LevelRAID5  = "raid5"
	LevelRAID6  = "raid6"
	LevelRAID10 = "raid10"
)

func minMembers(level string) (int, error) {
	switch level {
	case LevelSingle:
		return 1, nil
	case LevelRAID0, LevelRAID1:
		return 2, nil
	case LevelRAID5:
		return 3, nil
	case LevelRAID6, LevelRAID10:
		return 4, nil
	}
	return 0, fmt.Errorf("неизвестный уровень %q", level)
}

// Режимы dm-integrity (как поддерживать согласованность данных и тегов
// при сбое питания). Режим — параметр открытия (не хранится в суперблоке
// dm-integrity), поэтому записывается в пул и передаётся в каждую операцию.
const (
	// IntegrityJournal — данные и теги сначала пишутся в журнал: после
	// сбоя каждый блок либо старый, либо новый с верным тегом. Цена —
	// двойная запись.
	IntegrityJournal = "journal"
	// IntegrityBitmap — без журнала; «грязные» области отмечаются в
	// битовой карте и после сбоя их теги пересчитываются по тому, что
	// оказалось на диске: порча, случившаяся в этих областях в момент
	// сбоя, dm-integrity не обнаруживается (Btrfs выше её обнаружит).
	IntegrityBitmap = "bitmap"
)

// DefaultIntegrityMode — режим для новых пулов.
const DefaultIntegrityMode = IntegrityBitmap

// LegacyIntegrityMode — режим пулов, созданных в ранних версиях (запись без поля
// integrity_mode): они открывались в режиме журнала.
const LegacyIntegrityMode = IntegrityJournal

func validIntegrityMode(integrity bool, mode string) error {
	switch {
	case !integrity && mode == "":
		return nil
	case !integrity:
		return fmt.Errorf("integrity_mode %q без integrity", mode)
	case mode == IntegrityJournal || mode == IntegrityBitmap:
		return nil
	}
	return fmt.Errorf("недопустимый режим dm-integrity %q", mode)
}

// DiskRef — диск в параметрах операции: стабильная идентичность и точный
// размер (байты), с которым считался план. Несовпадение размера при
// проверке перед записью — отказ.
type DiskRef struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"size_bytes"`
}

func (d DiskRef) validate() error {
	if !ValidDiskID(d.ID) {
		return fmt.Errorf("недопустимая идентичность диска %q", d.ID)
	}
	if d.SizeBytes <= PartitionReserve || d.SizeBytes%512 != 0 {
		return fmt.Errorf("диск %s: недопустимый размер %d", d.ID, d.SizeBytes)
	}
	return nil
}

// ArraySpec — массив пула в порядке слоёв (индекс = номер слоя).
// Для single — ровно один участник; JBOD — по массиву single на диск.
type ArraySpec struct {
	Level      string   `json:"level"`
	SliceBytes int64    `json:"slice_bytes"` // размер раздела на каждом участнике
	Members    []string `json:"members"`     // ID дисков
}

// CreateParams — home.pool.create.
type CreateParams struct {
	Pool          string      `json:"pool"`
	Integrity     bool        `json:"integrity"`
	IntegrityMode string      `json:"integrity_mode,omitempty"` // при integrity обязателен
	Disks         []DiskRef   `json:"disks"`
	Arrays        []ArraySpec `json:"arrays"`
}

// Validate — структура плана: имена, уровни, число участников, выравнивание,
// разделы помещаются на диски.
func (p CreateParams) Validate() error {
	if !ValidPoolName(p.Pool) {
		return fmt.Errorf("недопустимое имя пула %q", p.Pool)
	}
	if err := validIntegrityMode(p.Integrity, p.IntegrityMode); err != nil {
		return err
	}
	if len(p.Disks) == 0 || len(p.Disks) > MaxDisks {
		return fmt.Errorf("число дисков должно быть 1..%d", MaxDisks)
	}
	if len(p.Arrays) == 0 || len(p.Arrays) > 16 {
		return errors.New("число массивов должно быть 1..16")
	}
	sizes := map[string]int64{}
	for _, d := range p.Disks {
		if err := d.validate(); err != nil {
			return err
		}
		if _, dup := sizes[d.ID]; dup {
			return fmt.Errorf("диск %s указан дважды", d.ID)
		}
		sizes[d.ID] = d.SizeBytes
	}
	used := map[string]int64{}
	inUse := map[string]bool{}
	for i, a := range p.Arrays {
		min, err := minMembers(a.Level)
		if err != nil {
			return fmt.Errorf("массив %d: %w", i, err)
		}
		if len(a.Members) < min || (a.Level == LevelSingle && len(a.Members) != 1) {
			return fmt.Errorf("массив %d (%s): недопустимое число дисков %d", i, a.Level, len(a.Members))
		}
		if a.Level == LevelRAID10 && len(a.Members)%2 != 0 {
			return fmt.Errorf("массив %d: raid10 требует чётного числа дисков", i)
		}
		if a.SliceBytes < 64*Align || a.SliceBytes%Align != 0 {
			return fmt.Errorf("массив %d: размер раздела %d не кратен 1 МиБ или меньше 64 МиБ", i, a.SliceBytes)
		}
		seen := map[string]bool{}
		for _, m := range a.Members {
			if _, ok := sizes[m]; !ok {
				return fmt.Errorf("массив %d: диск %s не входит в disks", i, m)
			}
			if seen[m] {
				return fmt.Errorf("массив %d: диск %s указан дважды", i, m)
			}
			seen[m] = true
			used[m] += a.SliceBytes
			inUse[m] = true
		}
	}
	for id, size := range sizes {
		if !inUse[id] {
			return fmt.Errorf("диск %s не входит ни в один массив", id)
		}
		if used[id]+PartitionReserve > size {
			return fmt.Errorf("диск %s: разделы (%d) не помещаются в %d", id, used[id], size)
		}
	}
	return nil
}

// ExpandArray — изменение одного массива при добавлении диска.
type ExpandArray struct {
	Index       int    `json:"index"`
	Level       string `json:"level"`        // целевой уровень
	RaidDevices int    `json:"raid_devices"` // целевое число участников
	SliceBytes  int64  `json:"slice_bytes"`  // раздел на новом диске
}

// AddDiskParams — home.pool.add_disk: новый диск входит в каждый указанный
// массив; уровень и число участников меняются до целевых (reshape идёт в
// фоне, рост LVM/Btrfs — отдельной операцией home.pool.grow после него).
type AddDiskParams struct {
	Pool          string        `json:"pool"`
	Integrity     bool          `json:"integrity"`
	IntegrityMode string        `json:"integrity_mode,omitempty"`
	Disk          DiskRef       `json:"disk"`
	Arrays        []ExpandArray `json:"arrays"`
	// NewArrays — новые верхние слои: SHR/SHR-2 диском
	// большего размера, JBOD +1. Индексы — сразу над существующими.
	NewArrays []NewArray `json:"new_arrays,omitempty"`
	// Resume — дозавершение прерванного добавления: диск уже
	// размечен под пул, разделы указанных массивов не используются;
	// разметка не выполняется, раздел каждого массива переиспользуется.
	Resume bool `json:"resume,omitempty"`
	// Grow — рост после замены диска на больший: Disk уже
	// участник пула (разделы его прежних слоёв на месте); разделы
	// расширяемых массивов и новых слоёв создаются на нём хвостами
	// (sgdisk -n + partx), диск целиком не размечается и не стирается.
	Grow bool `json:"grow,omitempty"`
}

// NewArray — новый слой пула при добавлении диска: раздел на
// новом диске и на свободном хвосте прежних дисков пула → [dm-integrity] →
// md (single — без md) → pvcreate → vgextend; рост LV/Btrfs — home.pool.grow.
type NewArray struct {
	Index      int       `json:"index"`
	Level      string    `json:"level"` // single (JBOD), raid1 (SHR — 2 копии, SHR-2 — 3); рост после замены (1.2.5) — и raid5/raid6/raid10
	SliceBytes int64     `json:"slice_bytes"`
	Members    []DiskRef `json:"members"` // прежние диски пула и новый диск
}

// validateNew — новые слои: уровень, участники (новый диск — ровно один раз),
// индексы подряд сразу над расширяемыми массивами.
func validateNew(disk DiskRef, prev int, arrs []NewArray) error {
	for _, a := range arrs {
		if a.Index <= prev {
			return fmt.Errorf("новый слой %d: индекс не выше расширяемых массивов", a.Index)
		}
		prev = a.Index
		switch a.Level {
		case LevelSingle:
			if len(a.Members) != 1 {
				return fmt.Errorf("новый слой %d (single): дисков %d", a.Index, len(a.Members))
			}
		case LevelRAID1, LevelRAID5, LevelRAID6, LevelRAID10:
			min, _ := minMembers(a.Level)
			if len(a.Members) < min || len(a.Members) > MaxDisks || (a.Level == LevelRAID10 && len(a.Members)%2 != 0) {
				return fmt.Errorf("новый слой %d (%s): дисков %d", a.Index, a.Level, len(a.Members))
			}
		default:
			return fmt.Errorf("новый слой %d: уровень %q не поддерживается", a.Index, a.Level)
		}
		seen := map[string]bool{}
		for _, m := range a.Members {
			if err := m.validate(); err != nil {
				return fmt.Errorf("новый слой %d: %w", a.Index, err)
			}
			if seen[m.ID] {
				return fmt.Errorf("новый слой %d: диск %s указан дважды", a.Index, m.ID)
			}
			seen[m.ID] = true
		}
		if !seen[disk.ID] {
			return fmt.Errorf("новый слой %d: нет нового диска %s", a.Index, disk.ID)
		}
	}
	for i := 1; i < len(arrs); i++ {
		if arrs[i].Index != arrs[i-1].Index+1 {
			return errors.New("новые слои должны идти подряд")
		}
	}
	return nil
}

// Validate проверяет параметры добавления диска.
func (p AddDiskParams) Validate() error {
	if !ValidPoolName(p.Pool) {
		return fmt.Errorf("недопустимое имя пула %q", p.Pool)
	}
	if err := validIntegrityMode(p.Integrity, p.IntegrityMode); err != nil {
		return err
	}
	if err := p.Disk.validate(); err != nil {
		return err
	}
	prev := -1
	if n := len(p.Arrays); n > 0 {
		prev = p.Arrays[n-1].Index
	}
	if err := validateNew(p.Disk, prev, p.NewArrays); err != nil {
		return err
	}
	// Разделы нового диска: расширяемые массивы, затем новые слои.
	return validateSlices(p.Disk, len(p.Arrays)+len(p.NewArrays), func(i int) (int, int64, error) {
		if i >= len(p.Arrays) {
			a := p.NewArrays[i-len(p.Arrays)]
			return a.Index, a.SliceBytes, nil
		}
		a := p.Arrays[i]
		switch a.Level {
		case LevelRAID1, LevelRAID5, LevelRAID6:
		default:
			return 0, 0, fmt.Errorf("массив %d: уровень %q для расширения не поддерживается", a.Index, a.Level)
		}
		if a.RaidDevices < 2 || a.RaidDevices > MaxDisks {
			return 0, 0, fmt.Errorf("массив %d: raid_devices %d", a.Index, a.RaidDevices)
		}
		return a.Index, a.SliceBytes, nil
	})
}

// ReplaceArray — массив, в который встаёт новый диск взамен отказавшего.
type ReplaceArray struct {
	Index      int   `json:"index"`
	SliceBytes int64 `json:"slice_bytes"`
}

// ReplaceParams — home.pool.replace: отказавшие участники удаляются из
// деградированных массивов, новый диск размечается и добавляется
// (восстановление идёт в фоне).
//
// Old != nil — горячая замена исправного диска: новый диск
// добавляется и становится заменой (mdadm --replace Old --with Disk) в
// каждом массиве Old; избыточность во время копирования не падает. Старый
// диск выводится и очищается отдельной операцией home.pool.replace_finish
// после конца копирования.
//
// Resume — дозавершение после сброса: разделы нового диска уже есть;
// массивы, где он уже участник или замена, пропускаются, где запасной —
// выдаётся --replace, где его нет — раздел переинициализируется и --add.
type ReplaceParams struct {
	Pool          string         `json:"pool"`
	Integrity     bool           `json:"integrity"`
	IntegrityMode string         `json:"integrity_mode,omitempty"`
	Disk          DiskRef        `json:"disk"`
	Old           *DiskRef       `json:"old,omitempty"`
	Arrays        []ReplaceArray `json:"arrays"`
	Resume        bool           `json:"resume,omitempty"`
}

// Validate проверяет параметры замены.
func (p ReplaceParams) Validate() error {
	if !ValidPoolName(p.Pool) {
		return fmt.Errorf("недопустимое имя пула %q", p.Pool)
	}
	if err := validIntegrityMode(p.Integrity, p.IntegrityMode); err != nil {
		return err
	}
	if err := p.Disk.validate(); err != nil {
		return err
	}
	if p.Old != nil {
		if err := p.Old.validate(); err != nil {
			return err
		}
		if p.Old.ID == p.Disk.ID {
			return errors.New("старый и новый диск совпадают")
		}
	}
	return validateSlices(p.Disk, len(p.Arrays), func(i int) (int, int64, error) {
		return p.Arrays[i].Index, p.Arrays[i].SliceBytes, nil
	})
}

// validateSlices — индексы массивов возрастают без повторов, разделы
// выровнены и помещаются на новый диск.
func validateSlices(d DiskRef, n int, get func(i int) (int, int64, error)) error {
	if n == 0 || n > 16 {
		return errors.New("число массивов должно быть 1..16")
	}
	prev := -1
	total := int64(0)
	for i := 0; i < n; i++ {
		idx, slice, err := get(i)
		if err != nil {
			return err
		}
		if idx <= prev || idx > 15 {
			return fmt.Errorf("индексы массивов должны возрастать в 0..15: %d", idx)
		}
		prev = idx
		if slice < 64*Align || slice%Align != 0 {
			return fmt.Errorf("массив %d: размер раздела %d не кратен 1 МиБ или меньше 64 МиБ", idx, slice)
		}
		total += slice
	}
	if total+PartitionReserve > d.SizeBytes {
		return fmt.Errorf("диск %s: разделы (%d) не помещаются в %d", d.ID, total, d.SizeBytes)
	}
	return nil
}

// PoolParams — операции над существующим пулом без дисков (status, grow).
type PoolParams struct {
	Pool string `json:"pool"`
}

// Validate проверяет имя пула.
func (p PoolParams) Validate() error {
	if !ValidPoolName(p.Pool) {
		return fmt.Errorf("недопустимое имя пула %q", p.Pool)
	}
	return nil
}

// Result — итог изменяющей операции helper.
type Result struct {
	Pool     string   `json:"pool"`
	Mount    string   `json:"mount,omitempty"`
	Commands []string `json:"commands"` // выполненные команды (для журнала задачи)
	Warnings []string `json:"warnings,omitempty"`
}
