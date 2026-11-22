package planner

// Op — операция изменения пула.
type Op string

const (
	OpAddDisk       Op = "add_disk"       // добавить диск(и) с ростом ёмкости
	OpAddMirror     Op = "add_mirror"     // добавить копию в RAID1 без роста ёмкости
	OpConvert       Op = "convert"        // смена типа
	OpReplaceLarger Op = "replace_larger" // замена дисков на большие
)

// Transition — допустимый переход из текущего пула.
type Transition struct {
	Op       Op
	Target   PoolType
	AddDisks int    // сколько дисков нужно добавить (минимум)
	Note     string // пояснение для интерфейса
	// Verified — переход проверен на стенде собственной реализацией
	// (internal/storage/home). Проверено 2026-11-04: SHR 2→3 и
	// RAID 1 → RAID 5 с двух дисков. 2026-11-11: +1 диск того
	// же среза в RAID 5, RAID 6, RAID 1 (третья копия), SHR и SHR-2 —
	// со сбросом ВМ до --add, после --add и во время reshape.
	// 2026-11-11: новый верхний слой — SHR/SHR-2 диском большего размера,
	// JBOD +1 — со сбросом до слоя, после pvcreate, после vgextend и во
	// время resync нового md. 2026-11-12: RAID 5 → RAID 6 и
	// SHR → SHR-2 добавлением диска (raid5 → raid6 с --layout=preserve, raid1
	// → третья копия) — со сбросом до --add, после --add, после --grow
	// --level и во время recovery; рост raid6 с раскладкой -6.
	// 2026-11-12: замена на больший (replace_larger) — см. replaceVerified.
	Verified bool
}

// Transitions перечисляет переходы, которые поддерживает md/LVM для пула
// данного типа и числа дисков. Не проверяет свободные слоты и размеры
// новых дисков — это делает Compute для целевого набора.
func Transitions(t PoolType, disks int) []Transition {
	var out []Transition
	add := func(tr Transition) { out = append(out, tr) }
	switch t {
	case Basic:
		add(Transition{Op: OpConvert, Target: RAID1, AddDisks: 1, Note: "Добавить второй диск и получить зеркало"})
	case JBOD:
		// Проверено: JBOD 1+2 ГиБ + 2 ГиБ — новый слой single.
		add(Transition{Op: OpAddDisk, Target: JBOD, AddDisks: 1, Note: "Ёмкость растёт, защиты нет", Verified: disks >= 2})
	case RAID0:
		// md умеет расти через raid4, но риск высок; не предлагаем.
	case RAID1:
		add(Transition{Op: OpAddMirror, Target: RAID1, AddDisks: 1, Note: "Ещё одна копия, ёмкость не растёт", Verified: disks >= 2})
		// Проверено: 2 диска → RAID 5 из 3 (с dm-integrity).
		add(Transition{Op: OpConvert, Target: RAID5, AddDisks: max(0, 3-disks), Note: "Переход на RAID 5 с ростом ёмкости", Verified: disks == 2})
	case RAID5:
		add(Transition{Op: OpAddDisk, Target: RAID5, AddDisks: 1, Note: "Перестройка массива; защита сохраняется", Verified: disks >= 3})
		// Проверено: RAID 5 1,1,2 + 2 → RAID 6. Ёмкость не растёт.
		add(Transition{Op: OpConvert, Target: RAID6, AddDisks: 1, Note: "Переход на RAID 6: защита от двух отказов, ёмкость не растёт", Verified: disks >= 3})
	case RAID6:
		add(Transition{Op: OpAddDisk, Target: RAID6, AddDisks: 1, Note: "Перестройка массива; защита сохраняется", Verified: disks >= 4})
	case RAID10:
		add(Transition{Op: OpAddDisk, Target: RAID10, AddDisks: 2, Note: "Добавить пару дисков"})
	case SHR:
		// Проверено: SHR 2→3, +1 диск того же среза в любом слое,
		// в т.ч. только в нижний слой из нескольких, и диском
		// большего размера — новый слой raid1 (1.2.3).
		add(Transition{Op: OpAddDisk, Target: SHR, AddDisks: 1, Note: sizeNote, Verified: disks >= 2})
		if disks >= 3 {
			// Проверено: SHR 1,2,2 + 2 → SHR-2.
			add(Transition{Op: OpConvert, Target: SHR2, AddDisks: 1, Note: "Переход на SHR-2: защита от двух отказов, ёмкость не растёт", Verified: true})
		}
	case SHR2:
		add(Transition{Op: OpAddDisk, Target: SHR2, AddDisks: 1, Note: sizeNote, Verified: disks >= 4})
	}
	switch t {
	case RAID1, RAID5, RAID6, RAID10, SHR, SHR2:
		// Проверено: горячая замена (mdadm --replace) и рост
		// после замены — RAID 1 из 2, SHR из 3 (новый слой и рост слоя raid1
		// → raid5) — со сбросами ВМ. RAID 5: замена и рост прошли, но сборка
		// нового raid5-слоя включала read-only; исправление на стенде не
		// перепроверено — не отмечен.
		add(Transition{Op: OpReplaceLarger, Target: t, Note: replaceNote(t), Verified: replaceVerified(t, disks)})
	}
	return out
}

// sizeNote — какой диск можно добавить в SHR/SHR-2: разделы
// прежних дисков не переразмечаются, поэтому диск, размер которого режет
// существующий слой, не подходит.
const sizeNote = "Добавить диск размером как один из дисков пула или не меньше верхней границы его слоёв (больший диск — новый слой)"

// replaceVerified — замена на больший проверена на стенде собственной
// реализацией для типа и числа дисков.
func replaceVerified(t PoolType, disks int) bool {
	switch t {
	case RAID1:
		return disks == 2
	case SHR:
		return disks == 3
	}
	return false
}

func replaceNote(t PoolType) string {
	if t == SHR || t == SHR2 {
		return "Ёмкость растёт после замены дисков, создающих новый защищённый слой"
	}
	return "Ёмкость растёт только после замены всех дисков массива"
}
