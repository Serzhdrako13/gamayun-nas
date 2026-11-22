package home

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Состояние пула (PoolStatus.Health).
const (
	HealthOK = "ok"
	// HealthWarning — массивы исправны, но Btrfs аварийно перешёл в
	// read-only: данные доступны на чтение, запись невозможна.
	HealthWarning    = "warning"
	HealthResyncing  = "resyncing"  // начальная синхронизация md или пересчёт тегов dm-integrity: защита ещё не полная
	HealthReshaping  = "reshaping"  // смена уровня/числа дисков
	HealthDegraded   = "degraded"   // запас избыточности уменьшен
	HealthRebuilding = "rebuilding" // деградирован, идёт восстановление
	HealthFailed     = "failed"     // массив неактивен или потерян
	HealthMissing    = "missing"    // пул не найден
	// HealthUnknown — опрос значимой части стека (массивы, mdadm, dmsetup,
	// LVM, Btrfs) не удался: состояние не определено и не считается ни
	// исправным, ни отказом. Причины — в Unknown.
	HealthUnknown = "unknown"
)

// MemberStatus — участник массива.
type MemberStatus struct {
	Device     string `json:"device"` // /dev/dm-0
	State      string `json:"state"`  // active sync, faulty, spare rebuilding…
	RaidDevice int    `json:"raid_device"`
	DiskID     string `json:"disk_id,omitempty"`
}

// ArrayStatus — md-массив пула.
type ArrayStatus struct {
	Index          int            `json:"index"`
	Name           string         `json:"name"`
	Device         string         `json:"device"`
	Level          string         `json:"level"`
	State          []string       `json:"state"`
	RaidDevices    int            `json:"raid_devices"`
	ActiveDevices  int            `json:"active_devices"`
	FailedDevices  int            `json:"failed_devices"`
	SpareDevices   int            `json:"spare_devices"`
	Missing        int            `json:"missing"` // слотов без исправного участника
	Integrity      bool           `json:"integrity"`
	Action         string         `json:"action,omitempty"`
	Progress       *float64       `json:"progress,omitempty"`
	SyncPos        *int64         `json:"sync_pos,omitempty"` // позиция действия, КиБ (mdstat)
	Delayed        bool           `json:"delayed,omitempty"`  // действие ждёт очереди (DELAYED/PENDING)
	FinishMin      *float64       `json:"finish_min,omitempty"`
	ArraySizeBytes int64          `json:"array_size_bytes"`
	DevSizeBytes   int64          `json:"dev_size_bytes"`
	MismatchCnt    *int64         `json:"mismatch_cnt,omitempty"` // md/mismatch_cnt после check
	Bitmap         string         `json:"bitmap,omitempty"`       // write-intent bitmap md: internal; пусто — нет
	Members        []MemberStatus `json:"members"`
}

// PoolStatus — состояние пула.
type PoolStatus struct {
	Pool        string            `json:"pool"`
	Health      string            `json:"health"`
	Arrays      []ArrayStatus     `json:"arrays"`
	Integrity   []IntegrityStatus `json:"integrity"`
	VGSizeBytes int64             `json:"vg_size_bytes"`
	VGFreeBytes int64             `json:"vg_free_bytes"`
	LVSizeBytes int64             `json:"lv_size_bytes"`
	Mount       string            `json:"mount"`
	Mounted     bool              `json:"mounted"`
	ReadOnly    bool              `json:"read_only"` // смонтирован только для чтения (точка или суперблок)
	// EmergencyReadOnly — read-only выставило ядро (Btrfs после ошибки
	// записи), а не перемонтирование: снимается только umount/mount.
	EmergencyReadOnly bool `json:"emergency_read_only,omitempty"`
	// InitialSync — идёт начальная синхронизация нового пула
	// (заполняет gamayund по записи пула).
	InitialSync bool `json:"initial_sync,omitempty"`
	// Sync — ход начальной синхронизации.
	Sync        *SyncProgress    `json:"sync,omitempty"`
	Btrfs       *BtrfsUsage      `json:"btrfs,omitempty"`
	BtrfsErrors map[string]int64 `json:"btrfs_errors,omitempty"`
	// IntegrityErrors — сумма счётчиков ошибок: btrfs device stats (все
	// поля, хранятся в ФС до сброса) и несовпадений dm-integrity (с
	// загрузки). Ненулевое — предупреждение в Issues, health не меняет:
	// ошибка могла быть исправлена копией md.
	IntegrityErrors int64    `json:"integrity_errors"`
	Issues          []string `json:"issues"`
	// Unknown — что не удалось прочитать; непусто — Health
	// не лучше unknown.
	Unknown []string `json:"unknown,omitempty"`
	DiskIDs []string `json:"disk_ids"` // доступные физические диски пула
}

// Status собирает состояние пула: /proc/mdstat, mdadm --detail,
// dmsetup status, lvs/vgs, btrfs filesystem usage, btrfs device stats.
// Только чтение; не берёт блокировку изменяющих операций.
func (e *Executor) Status(ctx context.Context, p PoolParams) (PoolStatus, error) {
	s := PoolStatus{Pool: p.Pool, Arrays: []ArrayStatus{}, Integrity: []IntegrityStatus{}, Issues: []string{}, DiskIDs: []string{}}
	issue := func(msg string) { s.Issues = append(s.Issues, msg) }
	// unread — значимая часть не прочитана: состояние неизвестно.
	unread := func(msg string) { issue(msg); s.Unknown = append(s.Unknown, msg) }

	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		unread("массивы: " + err.Error())
	}
	st, err := e.mdstat()
	if err != nil {
		unread("/proc/mdstat: " + err.Error())
	}
	ids := map[string]bool{}
	for _, a := range arrs {
		as := ArrayStatus{Index: a.index, Name: a.name, Device: a.kname, Members: []MemberStatus{}, State: []string{}}
		ms, ok := st[a.kname]
		if ok {
			as.Level = ms.Level
			as.RaidDevices = ms.RaidDevices
			as.Action = ms.Action
			as.Progress = ms.Progress
			as.SyncPos = ms.SyncPos
			as.Delayed = ms.Delayed
			as.FinishMin = ms.FinishMin
			if ms.RaidDevices > 0 {
				as.Missing = ms.RaidDevices - ms.InSync
			}
			if !ms.Active {
				as.State = append(as.State, "inactive")
			}
		} else {
			issue("массив " + a.name + " отсутствует в /proc/mdstat")
		}
		if out, err := e.query(ctx, e.Tools.Mdadm, "--detail", a.node); err != nil {
			unread("mdadm --detail " + a.name + ": " + err.Error())
		} else if d, err := ParseMdadmDetail(string(out)); err != nil {
			unread(err.Error())
		} else {
			as.Level = d.Level
			as.State = d.State
			as.RaidDevices = d.RaidDevices
			as.ActiveDevices = d.ActiveDevices
			as.FailedDevices = d.FailedDevices
			as.SpareDevices = d.SpareDevices
			as.ArraySizeBytes = d.ArraySizeKiB * 1024
			as.DevSizeBytes = d.UsedDevKiB * 1024
			as.Bitmap = d.Bitmap
			if m := d.RaidDevices - d.ActiveDevices; m > as.Missing {
				as.Missing = m
			}
			for _, m := range d.Members {
				mem := MemberStatus{Device: m.Device, State: m.State, RaidDevice: m.RaidDevice}
				if m.Device != "" {
					if got, err := e.diskIDsUnder(m.Device); err == nil && len(got) == 1 {
						mem.DiskID = got[0]
					}
				}
				as.Members = append(as.Members, mem)
			}
		}
		if as.FailedDevices > 0 && as.Missing == 0 {
			// Все слоты заняты исправными участниками, отказавший — лишний
			// (заменённый mdadm --replace или отказавший запасной): запас
			// избыточности не уменьшен, участник ждёт mdadm --remove.
			issue(fmt.Sprintf("массив %s: отказавший диск вне состава (%d) — ждёт удаления", a.name, as.FailedDevices))
		}
		if integ, err := e.isIntegrityArray(a.kname); err == nil {
			as.Integrity = integ
		}
		if v, err := e.readInt(filepath.Join(e.Paths.Disks.SysClassBlock, a.kname, "md", "mismatch_cnt")); err == nil {
			as.MismatchCnt = &v
		}
		if got, err := e.diskIDsUnder(a.node); err == nil {
			for _, id := range got {
				ids[id] = true
			}
		}
		s.Arrays = append(s.Arrays, as)
	}

	if maps, err := e.integrityMaps(ctx); err != nil {
		unread("dmsetup: " + err.Error())
	} else {
		var modes map[string]string
		if len(maps) > 0 {
			if out, err := e.query(ctx, e.Tools.Dmsetup, "table", "--target", "integrity"); err != nil {
				unread("dmsetup table: " + err.Error())
			} else {
				modes = ParseIntegrityTable(string(out))
			}
		}
		for _, m := range maps {
			if strings.HasPrefix(m.Name, IntegrityPrefix(p.Pool)) {
				m.Mode = modes[m.Name]
				e.integrityLive(&m, s.Arrays)
				s.Integrity = append(s.Integrity, m)
			}
		}
	}

	vg := VGName(p.Pool)
	vgFound, lvFound, broken := false, false, false
	if vgs, err := e.vgList(ctx); err != nil {
		unread("vgs: " + err.Error())
	} else {
		for _, v := range vgs {
			if v["vg_name"] == vg {
				vgFound = true
				s.VGSizeBytes, _ = strconv.ParseInt(v["vg_size"], 10, 64)
				s.VGFreeBytes, _ = strconv.ParseInt(v["vg_free"], 10, 64)
			}
		}
		if len(arrs) > 0 && !vgFound {
			// Массивы есть, а группы томов нет — стек не собран
			// (например, сброс посреди удаления), пул не исправен.
			issue("группа томов " + vg + " не найдена")
			broken = true
		}
	}
	if vgFound {
		if out, err := e.query(ctx, e.Tools.LVM, "lvs", "--reportformat", "json", "--units", "b", "--nosuffix",
			"-o", "lv_name,vg_name,lv_size,lv_attr", vg); err != nil {
			unread("lvs: " + err.Error())
		} else if rows, err := ParseLVMReport(out, "lv"); err != nil {
			unread(err.Error())
		} else {
			for _, r := range rows {
				if r["lv_name"] == LVName {
					lvFound = true
					s.LVSizeBytes, _ = strconv.ParseInt(r["lv_size"], 10, 64)
				}
			}
			if !lvFound {
				issue("логический том " + vg + "/" + LVName + " не найден")
				broken = true
			}
		}
		if pvs, err := e.pvList(ctx, vg); err == nil {
			for _, pv := range pvs {
				if got, err := e.diskIDsUnder(pv); err == nil {
					for _, id := range got {
						ids[id] = true
					}
				}
			}
		}
	}

	s.Mount = e.mountPoint(p.Pool)
	if _, opts, super, ok := e.mountInfoSuper(s.Mount); ok {
		s.Mounted = true
		s.ReadOnly = contains(opts, "ro") || contains(super, "ro")
		if contains(super, "ro") && !contains(opts, "ro") {
			// Ядро перевело Btrfs в read-only после ошибки записи
			// (Transaction aborted); точка монтирования при этом rw.
			s.EmergencyReadOnly = true
			issue("Btrfs аварийно перешёл в режим только для чтения после ошибки записи; запись на пул невозможна до перемонтирования, проверьте диски и журнал ядра")
		}
		if out, err := e.query(ctx, e.Tools.Btrfs, "filesystem", "usage", "-b", s.Mount); err != nil {
			unread("btrfs filesystem usage: " + err.Error())
		} else if u, err := ParseBtrfsUsage(string(out)); err != nil {
			unread(err.Error())
		} else {
			s.Btrfs = &u
		}
		if out, err := e.query(ctx, e.Tools.Btrfs, "device", "stats", s.Mount); err != nil {
			issue("btrfs device stats: " + err.Error())
		} else {
			s.BtrfsErrors = ParseBtrfsDevStats(string(out))
		}
	} else if vgFound {
		issue("том пула не смонтирован в " + s.Mount)
	}
	for id := range ids {
		s.DiskIDs = append(s.DiskIDs, id)
	}
	sort.Strings(s.DiskIDs)
	noteIntegrityErrors(&s)
	s.Health = health(s, len(arrs) > 0 || vgFound)
	if broken {
		s.Health = HealthFailed
	}
	if len(s.Unknown) > 0 && s.Health != HealthFailed {
		// Неполные данные не дают ok/resyncing/missing: что не прочитано,
		// могло быть отказом. Доказанный отказ остаётся failed.
		s.Health = HealthUnknown
	}
	return s, nil
}

// noteIntegrityErrors — IntegrityErrors и предупреждение с разбивкой.
func noteIntegrityErrors(s *PoolStatus) {
	var parts []string
	for _, k := range sortedKeys(s.BtrfsErrors) {
		if n := s.BtrfsErrors[k]; n > 0 {
			s.IntegrityErrors += n
			parts = append(parts, fmt.Sprintf("btrfs %s=%d", k, n))
		}
	}
	for _, m := range s.Integrity {
		if m.Mismatches > 0 {
			s.IntegrityErrors += m.Mismatches
			parts = append(parts, fmt.Sprintf("dm-integrity %s=%d", m.Name, m.Mismatches))
		}
	}
	if s.IntegrityErrors > 0 {
		s.Issues = append(s.Issues, fmt.Sprintf("обнаружены ошибки чтения/целостности: %d (%s); запустите проверку данных пула",
			s.IntegrityErrors, strings.Join(parts, ", ")))
	}
}

func sortedKeys(m map[string]int64) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func health(s PoolStatus, exists bool) string {
	if !exists {
		return HealthMissing
	}
	h := HealthOK
	rank := map[string]int{HealthOK: 0, HealthResyncing: 1, HealthReshaping: 2, HealthRebuilding: 3, HealthDegraded: 4, HealthFailed: 5}
	set := func(v string) {
		if rank[v] > rank[h] {
			h = v
		}
	}
	for _, a := range s.Arrays {
		state := strings.Join(a.State, ",")
		switch {
		case contains(a.State, "inactive") || strings.Contains(state, "FAILED") || a.Level == "":
			set(HealthFailed)
		case a.Missing > 0 || contains(a.State, "degraded"):
			if a.Action == "recovery" {
				set(HealthRebuilding)
			} else {
				set(HealthDegraded)
			}
		case a.Action == "recovery":
			// Все слоты исправны, идёт копирование на замену (mdadm
			// --replace): избыточность не уменьшена.
			set(HealthResyncing)
		case a.Action == "reshape":
			set(HealthReshaping)
		case a.Action == "resync":
			set(HealthResyncing)
		}
	}
	// Фоновый пересчёт тегов dm-integrity (инициализация без полной
	// записи): до его конца часть устройства не проверяется — защита ещё
	// не полная, как при начальной синхронизации md.
	for _, m := range s.Integrity {
		if m.Recalculating {
			set(HealthResyncing)
		}
	}
	if s.Btrfs != nil && s.Btrfs.DeviceMissing > 0 {
		set(HealthFailed)
	}
	// Аварийный RO Btrfs — не ok (запись невозможна), но и не
	// отказ (данные читаются). Деградация/отказ массивов важнее.
	if s.EmergencyReadOnly && rank[h] < rank[HealthRebuilding] {
		h = HealthWarning
	}
	return h
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// initialSyncArray — массив в начальной синхронизации без отказов: raid5/6
// строится как «деградированный + recovery», raid1/10 — resync.
func initialSyncArray(a ArrayStatus) bool {
	return a.FailedDevices == 0 && (a.Action == "resync" || a.Action == "recovery") &&
		!contains(a.State, "inactive")
}

// markInitialSync — пул из записи с признаком начальной синхронизации:
// если отказов нет и синхронизация идёт, это не деградация.
func markInitialSync(st *PoolStatus) {
	if st.Health != HealthRebuilding && st.Health != HealthDegraded && st.Health != HealthResyncing {
		return
	}
	if st.Btrfs != nil && st.Btrfs.DeviceMissing > 0 {
		return
	}
	for _, a := range st.Arrays {
		if (a.Missing > 0 || a.FailedDevices > 0 || contains(a.State, "degraded")) && !initialSyncArray(a) {
			return
		}
	}
	st.InitialSync = true
	st.Health = HealthResyncing
}

// integrityLive — есть ли под отображением dm-integrity исправное устройство
// Пересчёт тегов идёт внутри dm и md не видит пропажи диска,
// поэтому смотрим нижние устройства в sysfs и состояние участника md.
func (e *Executor) integrityLive(m *IntegrityStatus, arrays []ArrayStatus) {
	node, err := filepath.EvalSymlinks(filepath.Join(e.Paths.Dev, "mapper", m.Name))
	if err != nil {
		m.LiveErr = "узел отображения: " + err.Error()
		return
	}
	kname := filepath.Base(node)
	for _, a := range arrays {
		for _, mem := range a.Members {
			if filepath.Base(mem.Device) == kname && strings.Contains(mem.State, "faulty") {
				m.Lost, m.LostWhy = true, "диск "+a.Name+" отказал"
				return
			}
		}
	}
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, kname, "slaves")
	slaves, err := os.ReadDir(dir)
	if err != nil {
		m.LiveErr = "нижние устройства: " + err.Error()
		return
	}
	live := 0
	for _, sl := range slaves {
		base := filepath.Join(e.Paths.Disks.SysClassBlock, sl.Name())
		if _, err := os.Stat(base); err != nil {
			continue // ссылка на удалённое устройство
		}
		state := filepath.Join(base, "device", "state")
		if _, err := os.Stat(filepath.Join(base, "partition")); err == nil {
			if real, err := filepath.EvalSymlinks(base); err == nil {
				state = filepath.Join(filepath.Dir(real), "device", "state") // раздел: состояние диска
			}
		}
		if b, err := os.ReadFile(state); err == nil {
			if st := strings.TrimSpace(string(b)); st != "running" && st != "live" {
				continue // SCSI offline/blocked
			}
		}
		live++
	}
	if live == 0 {
		m.Lost, m.LostWhy = true, "нет нижнего устройства"
	}
}
