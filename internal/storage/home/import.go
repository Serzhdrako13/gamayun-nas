package home

// Пул GamayunNas на дисках после потери системы (переустановка, удалённая
// база) — найти по меткам разделов и подхватить: состав и уровни берутся с дисков
// (метка раздела dn-<пул>-<i>, суперблоки dm-integrity и md), сборка — тем же
// путём, что при загрузке (Assemble). Тип пула — из манифеста на пуле
// (.gamayun/pool.json, пишется при каждой сборке), без него — по составу.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gamayun-nas/internal/disks"
)

// Операции helper.
const (
	OpScan   = "home.pool.scan"
	OpImport = "home.pool.import"
)

// ManifestPath — манифест пула относительно точки монтирования.
const ManifestPath = ".gamayun/pool.json"

// Manifest — описание пула на самом пуле (для подхвата после переустановки).
type Manifest struct {
	Name          string      `json:"name"`
	Type          string      `json:"type"`
	Integrity     bool        `json:"integrity"`
	IntegrityMode string      `json:"integrity_mode,omitempty"`
	Arrays        []ArraySpec `json:"arrays"`
	Disks         []DiskRef   `json:"disks,omitempty"`
}

// FoundMember — раздел найденного пула.
type FoundMember struct {
	DiskID     string `json:"disk_id"`
	DiskName   string `json:"disk_name"`
	KernelName string `json:"kernel_name"`
	SizeBytes  int64  `json:"size_bytes"` // размер диска
	Partition  string `json:"partition"`
}

// FoundArray — массив (слой) найденного пула по меткам разделов.
type FoundArray struct {
	Index   int           `json:"index"`
	Members []FoundMember `json:"members"`
}

// FoundPool — пул GamayunNas, найденный на дисках.
type FoundPool struct {
	Name          string       `json:"name"`
	Integrity     bool         `json:"integrity"`
	IntegrityMode string       `json:"integrity_mode,omitempty"`
	Arrays        []FoundArray `json:"arrays"`
	Disks         int          `json:"disks"`
	// Busy — том пула смонтирован (пул уже работает). Собранные udev массивы
	// md и открытые dm-integrity без монтирования подхвату не мешают.
	Busy bool `json:"busy,omitempty"`
	// Known — пул с таким именем уже есть в базе (заполняет gamayund).
	Known bool `json:"known,omitempty"`
}

// ScanResult — home.pool.scan.
type ScanResult struct {
	Pools []FoundPool `json:"pools"`
	Result
}

// ImportParams — home.pool.import.
type ImportParams struct {
	Pool string `json:"pool"`
	// Degraded — подхватить, даже если части дисков массива нет (явное
	// подтверждение администратора).
	Degraded bool `json:"degraded,omitempty"`
}

// Validate проверяет имя.
func (p ImportParams) Validate() error {
	if !ValidPoolName(p.Pool) {
		return fmt.Errorf("недопустимое имя пула %q", p.Pool)
	}
	return nil
}

// ImportResult — состав подхваченного пула (gamayund пишет запись).
type ImportResult struct {
	AssembleResult
	Type          string      `json:"type"`
	TypeGuessed   bool        `json:"type_guessed,omitempty"` // манифеста нет — тип по составу
	Integrity     bool        `json:"integrity"`
	IntegrityMode string      `json:"integrity_mode,omitempty"`
	Disks         []DiskRef   `json:"disks"`
	Arrays        []ArraySpec `json:"arrays"`
	Degraded      []string    `json:"degraded,omitempty"` // «массив i: найдено n из m»
}

// ErrDegraded — массивы найдены не полностью, нужно подтверждение.
var ErrDegraded = errors.New("пул найден не полностью")

// parseLabel — «dn-<пул>-<i>» → пул, i.
func parseLabel(label string) (string, int, bool) {
	rest, ok := strings.CutPrefix(label, "dn-")
	k := strings.LastIndex(rest, "-")
	if !ok || k <= 0 {
		return "", 0, false
	}
	i, err := strconv.Atoi(rest[k+1:])
	if err != nil || i < 0 || i >= 32 || !ValidPoolName(rest[:k]) {
		return "", 0, false
	}
	return rest[:k], i, true
}

// scan — пулы по меткам разделов всех дисков.
func (e *Executor) scan(ctx context.Context) ([]FoundPool, error) {
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return nil, fmt.Errorf("снимок дисков: %w", err)
	}
	pools := map[string]*FoundPool{}
	arrs := map[string]map[int][]FoundMember{}
	diskSet := map[string]map[string]bool{}
	for _, d := range snap.Disks {
		if d.ID == "" || d.SizeBytes == nil || !knameRe.MatchString(d.KernelName) {
			continue
		}
		dir := filepath.Join(e.Paths.Disks.SysClassBlock, d.KernelName)
		ents, _ := os.ReadDir(dir)
		for _, en := range ents {
			pdir := filepath.Join(dir, en.Name())
			if _, err := os.Stat(filepath.Join(pdir, "partition")); err != nil {
				continue
			}
			pool, i, ok := parseLabel(e.partName(pdir))
			if !ok {
				continue
			}
			fp := pools[pool]
			if fp == nil {
				fp = &FoundPool{Name: pool}
				pools[pool] = fp
				arrs[pool] = map[int][]FoundMember{}
				diskSet[pool] = map[string]bool{}
			}
			m := FoundMember{DiskID: d.ID, DiskName: d.Name, KernelName: d.KernelName, SizeBytes: *d.SizeBytes, Partition: en.Name()}
			arrs[pool][i] = append(arrs[pool][i], m)
			diskSet[pool][d.ID] = true
			if out, err := e.query(ctx, e.Tools.Blkid, "-p", "-o", "value", "-s", "TYPE", filepath.Join(e.Paths.Dev, en.Name())); err == nil &&
				strings.TrimSpace(string(out)) == "DM_integrity" {
				fp.Integrity = true
				if fp.IntegrityMode == "" {
					fp.IntegrityMode = e.integrityMode(ctx, filepath.Join(e.Paths.Dev, en.Name()))
				}
			}
		}
	}
	out := []FoundPool{}
	for name, fp := range pools {
		var idx []int
		for i := range arrs[name] {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			ms := arrs[name][i]
			sort.Slice(ms, func(a, b int) bool { return ms[a].DiskID < ms[b].DiskID })
			fp.Arrays = append(fp.Arrays, FoundArray{Index: i, Members: ms})
		}
		fp.Disks = len(diskSet[name])
		fp.Busy = e.lvMounted(name)
		out = append(out, *fp)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// lvMounted — том данных пула смонтирован где-либо (mountinfo).
func (e *Executor) lvMounted(pool string) bool {
	want, err := filepath.EvalSymlinks(filepath.Join(e.Paths.Dev, VGName(pool), LVName))
	if err != nil {
		return false
	}
	b, err := os.ReadFile(e.Paths.Disks.Mountinfo)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		for i := 6; i < len(f)-2; i++ {
			if f[i] == "-" {
				if got, err := filepath.EvalSymlinks(f[i+2]); err == nil && got == want {
					return true
				}
				break
			}
		}
	}
	return false
}

// integrityMode — режим для открытия при подхвате: флаг dirty_bitmap есть —
// bitmap; нет — journal (после чистого закрытия режимы по суперблоку
// неотличимы; journal безопасен всегда). Режим пула уточняется по манифесту.
func (e *Executor) integrityMode(ctx context.Context, dev string) string {
	out, err := e.query(ctx, e.Tools.Integritysetup, "dump", dev)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f, ok := strings.CutPrefix(strings.TrimSpace(l), "flags"); ok {
			for _, x := range strings.Fields(f) {
				if x == "dirty_bitmap" {
					return IntegrityBitmap
				}
			}
			return IntegrityJournal
		}
	}
	return ""
}

// ScanParams — home.pool.scan (без параметров).
type ScanParams struct{}

// Validate — параметров нет.
func (ScanParams) Validate() error { return nil }

// Scan — найденные на дисках пулы (только чтение).
func (e *Executor) Scan(ctx context.Context, _ ScanParams) (ScanResult, error) {
	ps, err := e.scan(ctx)
	return ScanResult{Pools: ps}, err
}

// mdInfo — mdadm --examine --export участника.
type mdInfo struct {
	level   string
	devices int
	uuid    string
}

func (e *Executor) examine(ctx context.Context, dev string) (mdInfo, error) {
	out, err := e.query(ctx, e.Tools.Mdadm, "--examine", "--export", dev)
	if err != nil {
		return mdInfo{}, err
	}
	var m mdInfo
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "MD_LEVEL":
			m.level = v
		case "MD_DEVICES":
			m.devices, _ = strconv.Atoi(v)
		case "MD_UUID":
			m.uuid = v
		}
	}
	if m.level == "" || m.devices == 0 {
		return m, fmt.Errorf("%s: нет суперблока md", dev)
	}
	return m, nil
}

// Import подхватывает найденный пул: открывает участников, определяет уровни
// массивов по суперблокам, собирает пул (Assemble) и читает манифест. Отказ до
// сборки: пул не найден, занят, смонтировано другое, массив неполный без
// Degraded, разные массивы md под одним индексом. При отказе открытые
// отображения dm-integrity закрываются.
func (e *Executor) Import(ctx context.Context, p ImportParams) (ImportResult, error) {
	res := ImportResult{}
	found, err := e.scan(ctx)
	if err != nil {
		return res, err
	}
	var fp *FoundPool
	for i := range found {
		if found[i].Name == p.Pool {
			fp = &found[i]
		}
	}
	if fp == nil {
		return res, refuse("пул %s на дисках не найден", p.Pool)
	}
	if fp.Busy {
		return res, refuse("том пула %s уже смонтирован", p.Pool)
	}
	if fp.Integrity && fp.IntegrityMode == "" {
		return res, refuse("режим dm-integrity пула %s не определён", p.Pool)
	}
	if len(fp.Arrays) == 0 || fp.Arrays[0].Index != 0 || fp.Arrays[len(fp.Arrays)-1].Index != len(fp.Arrays)-1 {
		return res, refuse("пул %s: слои найдены не подряд с 0 — часть дисков отсутствует", p.Pool)
	}
	if src, ok := e.mountedAt(e.mountPoint(p.Pool)); ok {
		return res, refuse("в %s уже смонтировано %s", e.mountPoint(p.Pool), src)
	}
	ap := AssembleParams{Pool: p.Pool, Integrity: fp.Integrity, IntegrityMode: fp.IntegrityMode}
	sizes := map[string]int64{}
	for _, a := range fp.Arrays {
		spec := ArraySpec{Level: LevelRAID1}
		for _, m := range a.Members {
			spec.Members = append(spec.Members, m.DiskID)
			sizes[m.DiskID] = m.SizeBytes
			if spec.SliceBytes == 0 {
				if b, err := os.ReadFile(filepath.Join(e.Paths.Disks.SysClassBlock, m.KernelName, m.Partition, "size")); err == nil {
					n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
					spec.SliceBytes = n * 512
				}
			}
		}
		ap.Arrays = append(ap.Arrays, spec)
	}
	// Открыть участников (dm-integrity) и определить уровни.
	e.mu.Lock()
	j := &journal{}
	var ar AssembleResult
	devs, _, _, err := e.openMembers(ctx, j, ap, &ar)
	closeAll := func() {
		if !ap.Integrity {
			return
		}
		for i, a := range ap.Arrays {
			for _, id := range a.Members {
				e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, "close", IntegrityName(p.Pool, i, id))
			}
		}
	}
	fail := func(err error) (ImportResult, error) {
		closeAll()
		e.mu.Unlock()
		res.Commands = j.cmds
		return res, err
	}
	if err != nil {
		return fail(err)
	}
	for i := range ap.Arrays {
		if len(devs[i]) == 0 {
			return fail(refuse("слой %d: участники не открылись", i))
		}
		t, _ := e.query(ctx, e.Tools.Blkid, "-p", "-o", "value", "-s", "TYPE", devs[i][0])
		switch strings.TrimSpace(string(t)) {
		case "LVM2_member":
			if len(devs[i]) != 1 {
				return fail(refuse("слой %d: том LVM на %d разделах", i, len(devs[i])))
			}
			ap.Arrays[i].Level = LevelSingle
		case "linux_raid_member":
			var first mdInfo
			for k, d := range devs[i] {
				m, err := e.examine(ctx, d)
				if err != nil {
					return fail(refuse("слой %d: %v", i, err))
				}
				if k == 0 {
					first = m
				} else if m.uuid != first.uuid {
					return fail(refuse("слой %d: участники из разных массивов md", i))
				}
			}
			lvl := first.level
			if _, err := minMembers(lvl); err != nil || lvl == LevelSingle {
				return fail(refuse("слой %d: уровень %q не поддерживается", i, lvl))
			}
			ap.Arrays[i].Level = lvl
			if n := len(devs[i]); n < first.devices {
				res.Degraded = append(res.Degraded, fmt.Sprintf("слой %d (%s): найдено %d из %d", i, lvl, n, first.devices))
			}
		default:
			return fail(refuse("слой %d: на %s нет md и LVM (%q)", i, devs[i][0], strings.TrimSpace(string(t))))
		}
	}
	if len(res.Degraded) > 0 && !p.Degraded {
		return fail(fmt.Errorf("%w: %s", ErrDegraded, strings.Join(res.Degraded, "; ")))
	}
	e.mu.Unlock()
	asm, err := e.Assemble(ctx, ap)
	res.AssembleResult = asm
	res.Commands = append(j.cmds, asm.Commands...)
	if err != nil {
		return res, err
	}
	res.Integrity, res.IntegrityMode, res.Arrays = ap.Integrity, ap.IntegrityMode, ap.Arrays
	ids := map[string]bool{}
	for _, a := range ap.Arrays {
		for _, id := range a.Members {
			if !ids[id] {
				ids[id] = true
				res.Disks = append(res.Disks, DiskRef{ID: id, SizeBytes: sizes[id]})
			}
		}
	}
	sort.Slice(res.Disks, func(a, b int) bool { return res.Disks[a].ID < res.Disks[b].ID })
	// Манифест: тип, режим dm-integrity (по суперблоку неотличим после
	// чистого закрытия: открыто journal) и полный состав — с дисками, которых
	// сейчас нет (вернутся при следующей сборке).
	if b, err := os.ReadFile(filepath.Join(e.mountPoint(p.Pool), ManifestPath)); err == nil {
		var m Manifest
		if json.Unmarshal(b, &m) == nil && m.Name == p.Pool && m.Type != "" {
			res.Type = m.Type
			if res.Integrity && m.Integrity && m.IntegrityMode != "" && m.IntegrityMode != res.IntegrityMode {
				res.Warnings = append(res.Warnings, fmt.Sprintf("dm-integrity открыт в режиме %s, режим пула %s применится при следующей сборке (перезапуск)", res.IntegrityMode, m.IntegrityMode))
				res.IntegrityMode = m.IntegrityMode
			}
			if fullArrays(m.Arrays, res.Arrays) {
				res.Arrays = m.Arrays
				for _, d := range m.Disks {
					if !ids[d.ID] && d.validate() == nil {
						ids[d.ID] = true
						res.Disks = append(res.Disks, d)
					}
				}
				sort.Slice(res.Disks, func(a, b int) bool { return res.Disks[a].ID < res.Disks[b].ID })
			}
		}
	}
	if res.Type == "" {
		res.Type, res.TypeGuessed = guessType(ap.Arrays), true
	}
	return res, nil
}

// fullArrays — состав манифеста согласуется с найденным: те же слои и
// уровни, найденные диски — подмножество участников манифеста.
func fullArrays(m, found []ArraySpec) bool {
	if len(m) != len(found) {
		return false
	}
	for i := range m {
		if m[i].Level != found[i].Level {
			return false
		}
		in := map[string]bool{}
		for _, id := range m[i].Members {
			in[id] = true
		}
		for _, id := range found[i].Members {
			if !in[id] {
				return false
			}
		}
	}
	return true
}

// guessType — тип пула по составу (манифеста нет): один слой — его уровень
// (single → basic); все слои single — jbod; несколько слоёв raid1/raid5 — shr,
// raid6 — shr2.
func guessType(arrs []ArraySpec) string {
	if len(arrs) == 1 {
		if arrs[0].Level == LevelSingle {
			return "basic"
		}
		return arrs[0].Level
	}
	all := func(l string) bool {
		for _, a := range arrs {
			if a.Level != l {
				return false
			}
		}
		return true
	}
	if all(LevelSingle) {
		return "jbod"
	}
	for _, a := range arrs {
		if a.Level == LevelRAID6 {
			return "shr2"
		}
	}
	return "shr"
}

// WriteManifest — манифест пула на пуле (после сборки; ошибка не мешает работе).
func (e *Executor) writeManifest(p AssembleParams) error {
	if p.Type == "" {
		return nil
	}
	dir, err := ensureManifestDir(e.mountPoint(p.Pool))
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(Manifest{Name: p.Pool, Type: p.Type, Integrity: p.Integrity, IntegrityMode: p.IntegrityMode, Arrays: p.Arrays, Disks: p.Disks}, "", "  ")
	path := filepath.Join(dir, filepath.Base(ManifestPath))
	if old, err := os.ReadFile(path); err == nil && string(old) == string(b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ensureManifestDir — .gamayun на пуле (root 0700), без ссылок.
func ensureManifestDir(mp string) (string, error) {
	dir := filepath.Join(mp, filepath.Dir(ManifestPath))
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", err
		}
		return dir, nil
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s — не каталог", dir)
	}
	return dir, nil
}
