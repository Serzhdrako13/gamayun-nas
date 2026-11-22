package home

import (
	"bufio"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MdstatArray — массив из /proc/mdstat.
type MdstatArray struct {
	Device  string   `json:"device"` // md127
	Active  bool     `json:"active"`
	Level   string   `json:"level"`   // raid1, raid5…
	Members []string `json:"members"` // dm-0[0], sdb1[1](F) → ["dm-0", …]
	Faulty  []string `json:"faulty"`  // участники с (F)
	Spares  []string `json:"spares"`  // участники с (S)
	// Replacing — замены с (R): mdadm --replace, идёт копирование на них;
	// входят и в Members.
	Replacing   []string `json:"replacing,omitempty"`
	RaidDevices int      `json:"raid_devices"`
	InSync      int      `json:"in_sync"`
	Blocks      int64    `json:"blocks"`    // КиБ
	Algorithm   int      `json:"algorithm"` // раскладка raid5/6 («algorithm N»; -1 — нет)
	Action      string   `json:"action"`    // resync, recovery, reshape, check, repair; пусто — нет
	Progress    *float64 `json:"progress"`  // %, если идёт действие
	// SyncPos — позиция действия («(done/total)», КиБ); nil — нет в выводе.
	// Счётчик продвижения для наблюдателя: процент слишком груб.
	SyncPos   *int64   `json:"sync_pos,omitempty"`
	Delayed   bool     `json:"delayed"`     // действие ожидает (DELAYED/PENDING)
	FinishMin *float64 `json:"finish_min"`  // оценка в минутах
	SpeedKBs  *int64   `json:"speed_kb_s"`  // скорость, КиБ/с
	Status    string   `json:"status_mask"` // [UU_]
	ReadOnly  bool     `json:"read_only"`   // (read-only) / (auto-read-only)
	Inactive  bool     `json:"inactive"`
}

var (
	mdHeadRe   = regexp.MustCompile(`^(md\S+) : (.*)$`)
	mdMemberRe = regexp.MustCompile(`^(\S+)\[(\d+)\](\([A-Z]\))*$`)
	mdStatusRe = regexp.MustCompile(`\[(\d+)/(\d+)\] \[([U_]+)\]`)
	mdBlocksRe = regexp.MustCompile(`^\s*(\d+) blocks`)
	mdAlgRe    = regexp.MustCompile(`algorithm (\d+)`)
	mdActionRe = regexp.MustCompile(`(resync|recovery|reshape|check|repair)\s*=\s*([\d.]+)%`)
	mdPosRe    = regexp.MustCompile(`%\s*\((\d+)/(\d+)\)`)
	mdFinishRe = regexp.MustCompile(`finish=([\d.]+)min`)
	mdSpeedRe  = regexp.MustCompile(`speed=(\d+)K/sec`)
	mdDelayRe  = regexp.MustCompile(`(resync|recovery|reshape|check|repair)\s*=\s*(DELAYED|PENDING)`)
)

// ParseMdstat разбирает /proc/mdstat. Неизвестные строки пропускаются.
func ParseMdstat(text string) []MdstatArray {
	var out []MdstatArray
	var cur *MdstatArray
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if m := mdHeadRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &MdstatArray{Device: m[1], Members: []string{}, Faulty: []string{}, Spares: []string{}, Algorithm: -1}
			f := strings.Fields(m[2])
			i := 0
			if i < len(f) && (f[i] == "active" || f[i] == "inactive") {
				cur.Active = f[i] == "active"
				cur.Inactive = f[i] == "inactive"
				i++
			}
			for i < len(f) && strings.HasPrefix(f[i], "(") {
				if strings.Contains(f[i], "read-only") {
					cur.ReadOnly = true
				}
				i++
			}
			if i < len(f) && !strings.Contains(f[i], "[") {
				cur.Level = f[i]
				i++
			}
			for ; i < len(f); i++ {
				mm := mdMemberRe.FindStringSubmatch(f[i])
				if mm == nil {
					continue
				}
				cur.Members = append(cur.Members, mm[1])
				switch {
				case strings.Contains(f[i], "(F)"):
					cur.Faulty = append(cur.Faulty, mm[1])
				case strings.Contains(f[i], "(S)"):
					cur.Spares = append(cur.Spares, mm[1])
				case strings.Contains(f[i], "(R)"):
					cur.Replacing = append(cur.Replacing, mm[1])
				}
			}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if m := mdBlocksRe.FindStringSubmatch(line); m != nil {
			cur.Blocks, _ = strconv.ParseInt(m[1], 10, 64)
			if a := mdAlgRe.FindStringSubmatch(line); a != nil {
				cur.Algorithm, _ = strconv.Atoi(a[1])
			}
		}
		if m := mdStatusRe.FindStringSubmatch(line); m != nil {
			cur.RaidDevices, _ = strconv.Atoi(m[1])
			cur.InSync, _ = strconv.Atoi(m[2])
			cur.Status = "[" + m[3] + "]"
		}
		if m := mdActionRe.FindStringSubmatch(line); m != nil {
			cur.Action = m[1]
			if v, err := strconv.ParseFloat(m[2], 64); err == nil {
				cur.Progress = &v
			}
			if p := mdPosRe.FindStringSubmatch(line); p != nil {
				if v, err := strconv.ParseInt(p[1], 10, 64); err == nil {
					cur.SyncPos = &v
				}
			}
			if f := mdFinishRe.FindStringSubmatch(line); f != nil {
				if v, err := strconv.ParseFloat(f[1], 64); err == nil {
					cur.FinishMin = &v
				}
			}
			if s := mdSpeedRe.FindStringSubmatch(line); s != nil {
				if v, err := strconv.ParseInt(s[1], 10, 64); err == nil {
					cur.SpeedKBs = &v
				}
			}
		} else if m := mdDelayRe.FindStringSubmatch(line); m != nil {
			cur.Action = m[1]
			cur.Delayed = true
		}
	}
	flush()
	return out
}

// MdDetail — разбор mdadm --detail.
type MdDetail struct {
	Level          string     `json:"level"`
	ArraySizeKiB   int64      `json:"array_size_kib"`
	UsedDevKiB     int64      `json:"used_dev_size_kib"`
	RaidDevices    int        `json:"raid_devices"`
	TotalDevices   int        `json:"total_devices"`
	State          []string   `json:"state"` // clean, degraded, recovering, resyncing, reshaping…
	ActiveDevices  int        `json:"active_devices"`
	WorkingDevices int        `json:"working_devices"`
	FailedDevices  int        `json:"failed_devices"`
	SpareDevices   int        `json:"spare_devices"`
	Name           string     `json:"name"`
	UUID           string     `json:"uuid"`
	Events         int64      `json:"events"`
	RebuildPct     *int       `json:"rebuild_percent,omitempty"`
	ReshapePct     *int       `json:"reshape_percent,omitempty"`
	ResyncPct      *int       `json:"resync_percent,omitempty"`
	Bitmap         string     `json:"bitmap,omitempty"` // Intent Bitmap: Internal; пусто — нет
	Members        []MdMember `json:"members"`
}

// MdMember — строка таблицы участников mdadm --detail.
type MdMember struct {
	Number     int    `json:"number"`
	Major      int    `json:"major"`
	Minor      int    `json:"minor"`
	RaidDevice int    `json:"raid_device"` // -1 — нет слота (запасной, отказавший)
	State      string `json:"state"`       // active sync, faulty, spare, removed, spare rebuilding…
	Device     string `json:"device"`      // /dev/dm-0; пусто у removed
}

var pctRe = regexp.MustCompile(`^(\d+)%`)

// ParseMdadmDetail разбирает вывод mdadm --detail (LC_ALL=C).
func ParseMdadmDetail(text string) (MdDetail, error) {
	d := MdDetail{Members: []MdMember{}, State: []string{}}
	inTable := false
	seen := false
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "Number") && strings.Contains(trim, "RaidDevice") {
			inTable = true
			continue
		}
		if inTable {
			if trim == "" {
				continue
			}
			if m, ok := parseMemberLine(trim); ok {
				d.Members = append(d.Members, m)
			}
			continue
		}
		k, v, ok := strings.Cut(trim, " : ")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		seen = true
		first := strings.Fields(v)
		num := func() int64 {
			if len(first) == 0 {
				return 0
			}
			n, _ := strconv.ParseInt(first[0], 10, 64)
			return n
		}
		pct := func() *int {
			if m := pctRe.FindStringSubmatch(v); m != nil {
				n, _ := strconv.Atoi(m[1])
				return &n
			}
			return nil
		}
		switch k {
		case "Raid Level":
			d.Level = v
		case "Array Size":
			d.ArraySizeKiB = num()
		case "Used Dev Size":
			d.UsedDevKiB = num()
		case "Raid Devices":
			d.RaidDevices = int(num())
		case "Total Devices":
			d.TotalDevices = int(num())
		case "State":
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					d.State = append(d.State, s)
				}
			}
		case "Active Devices":
			d.ActiveDevices = int(num())
		case "Working Devices":
			d.WorkingDevices = int(num())
		case "Failed Devices":
			d.FailedDevices = int(num())
		case "Spare Devices":
			d.SpareDevices = int(num())
		case "Name":
			if len(first) > 0 {
				d.Name = first[0]
			}
		case "UUID":
			d.UUID = v
		case "Events":
			d.Events = num()
		case "Rebuild Status":
			d.RebuildPct = pct()
		case "Reshape Status":
			d.ReshapePct = pct()
		case "Resync Status":
			d.ResyncPct = pct()
		case "Intent Bitmap":
			d.Bitmap = strings.ToLower(v)
		}
	}
	if !seen || d.Level == "" {
		return d, fmt.Errorf("mdadm --detail: не найдены поля массива")
	}
	return d, nil
}

// parseMemberLine: "0  253  0  0  active sync  /dev/dm-0",
// "-  0  0  1  removed", "2  253  3  -  faulty  /dev/dm-3".
func parseMemberLine(s string) (MdMember, bool) {
	f := strings.Fields(s)
	if len(f) < 5 {
		return MdMember{}, false
	}
	var m MdMember
	var err error
	if f[0] == "-" {
		m.Number = -1
	} else if m.Number, err = strconv.Atoi(f[0]); err != nil {
		return MdMember{}, false
	}
	if m.Major, err = strconv.Atoi(f[1]); err != nil {
		return MdMember{}, false
	}
	if m.Minor, err = strconv.Atoi(f[2]); err != nil {
		return MdMember{}, false
	}
	if f[3] == "-" {
		m.RaidDevice = -1
	} else if m.RaidDevice, err = strconv.Atoi(f[3]); err != nil {
		return MdMember{}, false
	}
	rest := f[4:]
	if last := rest[len(rest)-1]; strings.HasPrefix(last, "/dev/") {
		m.Device = last
		rest = rest[:len(rest)-1]
	}
	m.State = strings.Join(rest, " ")
	return m, true
}

// LVMReport — разбор JSON-отчёта lvs/vgs/pvs (--reportformat json
// --units b --nosuffix): значения — строки.
type LVMReport struct {
	Report []map[string][]map[string]string `json:"report"`
}

// ParseLVMReport возвращает строки раздела kind ("lv", "vg", "pv").
func ParseLVMReport(data []byte, kind string) ([]map[string]string, error) {
	var r LVMReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("отчёт LVM: %w", err)
	}
	var out []map[string]string
	for _, part := range r.Report {
		out = append(out, part[kind]...)
	}
	return out, nil
}

// BtrfsUsage — сводка btrfs filesystem usage -b.
type BtrfsUsage struct {
	DeviceSize     int64   `json:"device_size"`
	Allocated      int64   `json:"allocated"`
	Unallocated    int64   `json:"unallocated"`
	Used           int64   `json:"used"`
	FreeEstimated  int64   `json:"free_estimated"`
	FreeStatfs     int64   `json:"free_statfs"`
	DataRatio      float64 `json:"data_ratio"`
	MetadataRatio  float64 `json:"metadata_ratio"`
	DeviceMissing  int64   `json:"device_missing"`
	DataProfile    string  `json:"data_profile"`
	MetaProfile    string  `json:"metadata_profile"`
	GlobalReserve  int64   `json:"global_reserve"`
	MultipleProfil bool    `json:"multiple_profiles"`
}

// ParseBtrfsUsage разбирает btrfs filesystem usage -b.
func ParseBtrfsUsage(text string) (BtrfsUsage, error) {
	var u BtrfsUsage
	found := false
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "Data,") || strings.HasPrefix(trim, "Metadata,") {
			kind, rest, _ := strings.Cut(trim, ",")
			prof, _, _ := strings.Cut(rest, ":")
			if kind == "Data" {
				u.DataProfile = prof
			} else {
				u.MetaProfile = prof
			}
			continue
		}
		k, v, ok := strings.Cut(trim, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, nerr := strconv.ParseInt(f[0], 10, 64)
		fl, ferr := strconv.ParseFloat(f[0], 64)
		switch strings.TrimSpace(k) {
		case "Device size":
			u.DeviceSize, found = n, nerr == nil
		case "Device allocated":
			u.Allocated = n
		case "Device unallocated":
			u.Unallocated = n
		case "Device missing":
			u.DeviceMissing = n
		case "Used":
			u.Used = n
		case "Free (estimated)":
			u.FreeEstimated = n
		case "Free (statfs, df)":
			u.FreeStatfs = n
		case "Global reserve":
			u.GlobalReserve = n
		case "Data ratio":
			if ferr == nil {
				u.DataRatio = fl
			}
		case "Metadata ratio":
			if ferr == nil {
				u.MetadataRatio = fl
			}
		case "Multiple profiles":
			u.MultipleProfil = f[0] == "yes"
		}
	}
	if !found {
		return u, fmt.Errorf("btrfs filesystem usage: нет поля Device size")
	}
	return u, nil
}

// ParseBtrfsDevStats разбирает btrfs device stats: суммы счётчиков по всем
// устройствам ФС (write_io_errs, read_io_errs, flush_io_errs,
// corruption_errs, generation_errs).
func ParseBtrfsDevStats(text string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		dot := strings.LastIndex(f[0], "].")
		if dot < 0 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		out[f[0][dot+2:]] += n
	}
	return out
}

// IntegrityStatus — строка dmsetup status цели integrity.
type IntegrityStatus struct {
	Name          string `json:"name"`
	Mismatches    int64  `json:"mismatches"`     // обнаруженных несовпадений контрольных сумм
	ProvidedBytes int64  `json:"provided_bytes"` // размер устройства данных
	// Recalculating — идёт фоновый пересчёт тегов (инициализация без
	// полной записи): выше RecalcBytes чтения не проверяются.
	Recalculating bool `json:"recalculating"`
	// RecalcBytes — граница пересчёта; nil — пересчёт не запускался.
	RecalcBytes *int64 `json:"recalc_bytes,omitempty"`
	// Mode — режим из dmsetup table: journal, bitmap, direct, recovery,
	// inline; пусто — не определён.
	Mode string `json:"mode,omitempty"`
	// Lost — под отображением нет исправного устройства (физический диск
	// пропал или участник md faulty): пересчёт на нём не продвинется.
	// LiveErr — живость проверить не удалось (неизвестно).
	Lost    bool   `json:"lost,omitempty"`
	LostWhy string `json:"lost_why,omitempty"`
	LiveErr string `json:"live_err,omitempty"`
}

// RecalcProgress — процент пересчёта тегов (nil — пересчёт не идёт).
func (s IntegrityStatus) RecalcProgress() *float64 {
	if !s.Recalculating || s.RecalcBytes == nil || s.ProvidedBytes <= 0 {
		return nil
	}
	v := 100 * float64(*s.RecalcBytes) / float64(s.ProvidedBytes)
	return &v
}

// ParseIntegrityStatus разбирает вывод `dmsetup status --target integrity`:
// "name: 0 2074928 integrity 0 2074928 -" (по строке на устройство).
// Последнее поле — граница пересчёта тегов в секторах: "-" — пересчёт не
// запускался; после завершения ядро оставляет значение, равное размеру
// устройства (проверено на ядре 6.12) — это не «идёт пересчёт».
// Одно устройство без имени ("0 2074928 integrity …") — имя name.
func ParseIntegrityStatus(text, name string) []IntegrityStatus {
	var out []IntegrityStatus
	for _, line := range strings.Split(text, "\n") {
		n, f := splitDMLine(line, name)
		if len(f) < 6 || f[2] != "integrity" {
			continue
		}
		mis, err1 := strconv.ParseInt(f[3], 10, 64)
		sect, err2 := strconv.ParseInt(f[4], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		st := IntegrityStatus{Name: n, Mismatches: mis, ProvidedBytes: sect * 512}
		if f[5] != "-" {
			if rs, err := strconv.ParseInt(f[5], 10, 64); err == nil {
				b := rs * 512
				st.RecalcBytes = &b
				st.Recalculating = rs < sect
			} else {
				st.Recalculating = true // неизвестный формат — не считать завершённым
			}
		}
		out = append(out, st)
	}
	return out
}

// splitDMLine — "name: start len target …" → имя и поля после него.
func splitDMLine(line, name string) (string, []string) {
	line = strings.TrimSpace(line)
	n := name
	if k, rest, ok := strings.Cut(line, ": "); ok && !strings.Contains(k, " ") {
		n, line = k, rest
	}
	return n, strings.Fields(line)
}

// ParseIntegrityTable разбирает `dmsetup table --target integrity`:
// "name: 0 2074928 integrity 8:33 0 4 J 10 block_size:4096 recalculate …"
// → имя → режим (journal, bitmap, direct, recovery, inline).
func ParseIntegrityTable(text string) map[string]string {
	modes := map[string]string{"J": IntegrityJournal, "B": IntegrityBitmap, "D": "direct", "R": "recovery", "I": "inline"}
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		n, f := splitDMLine(line, "")
		if n == "" || len(f) < 7 || f[2] != "integrity" {
			continue
		}
		if m, ok := modes[f[6]]; ok {
			out[n] = m
		}
	}
	return out
}
