// Package disks определяет роли дисков (системный / используемый / неизвестный)
// по mountinfo, /proc/swaps и sysfs, ничего не меняя в системе.
//
// Порт прототипа disks.py с исправлением дефекта: у Btrfs (и ZFS)
// номер устройства в mountinfo анонимный (0:NN), поэтому кроме maj:min
// используется источник монтирования (поле после разделителя " - ").
//
// Принцип: отсутствие роли не означает, что диск свободен. Любая неясность
// (нераспознанный источник, ZFS, ошибки чтения) делает снимок неполным
// (Snapshot.Complete=false) и попадает в Issues; диски без найденных
// зависимостей получают роль unknown с причиной, но никогда «свободен».
package disks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Роли диска.
const (
	RoleRoot        = "root"
	RoleBoot        = "boot"
	RoleEFI         = "efi"
	RoleSwap        = "swap"
	RoleMounted     = "mounted"
	RoleStackMember = "stack_member"
)

// Сводная роль диска (Disk.SystemRole).
const (
	SummaryRoot    = "root"
	SummarySystem  = "system"
	SummaryInUse   = "in_use"
	SummaryUnknown = "unknown"
)

// DevNum — номер блочного устройства major:minor.
type DevNum struct{ Major, Minor uint32 }

func (d DevNum) String() string { return fmt.Sprintf("%d:%d", d.Major, d.Minor) }

// decodeDev — как major()/minor() из glibc.
func decodeDev(d uint64) DevNum {
	return DevNum{
		Major: uint32(((d >> 8) & 0xfff) | ((d >> 32) &^ 0xfff)),
		Minor: uint32((d & 0xff) | ((d >> 12) &^ 0xff)),
	}
}

// FileStat — нужные поля stat(2) для файла или устройства подкачки.
type FileStat struct {
	Block bool   // блочное устройство (тогда значим Rdev)
	Rdev  DevNum // номер самого устройства
	Dev   DevNum // устройство файловой системы, где лежит файл
}

// StatFunc — подменяемый stat для тестов.
type StatFunc func(path string) (FileStat, error)

// Paths — все внешние источники; в тестах указывают на дерево в t.TempDir().
type Paths struct {
	SysClassBlock string // /sys/class/block
	SysDevBlock   string // /sys/dev/block
	SysFsBtrfs    string // /sys/fs/btrfs
	SysModule     string // /sys/module (наличие zfs); пусто — не проверять
	Mountinfo     string // /proc/self/mountinfo
	Swaps         string // /proc/swaps
	// Dev — корень /dev для разрешения источника монтирования
	// (/dev/mapper/*, /dev/md/*, /dev/disk/by-uuid/* и т.п. — символьные ссылки
	// на узел ядра). Пусто или недоступно — используется базовое имя источника.
	Dev  string
	Stat StatFunc // для /proc/swaps; nil — системный stat
}

// DefaultPaths — пути реальной системы.
func DefaultPaths() Paths {
	return Paths{
		SysClassBlock: "/sys/class/block",
		SysDevBlock:   "/sys/dev/block",
		SysFsBtrfs:    "/sys/fs/btrfs",
		SysModule:     "/sys/module",
		Mountinfo:     "/proc/self/mountinfo",
		Swaps:         "/proc/swaps",
		Dev:           "/dev",
		Stat:          systemStat,
	}
}

// Issue — обнаруженная неясность; любая делает снимок неполным.
type Issue struct {
	Kind       string `json:"kind"` // mountinfo, mount, swap, zfs, holders, sysfs
	Mountpoint string `json:"mountpoint,omitempty"`
	FSType     string `json:"fstype,omitempty"`
	Source     string `json:"source,omitempty"`
	Reason     string `json:"reason"`
}

// Disk — наблюдение одного физического диска.
type Disk struct {
	// Name — временный алиас («Диск 1», «SSD 1»), меняется при переобнаружении.
	Name string `json:"name"`
	// KernelName — нестабильное имя ядра (sda); только для диагностики, не для UI.
	KernelName string `json:"kernel_name"`
	// ID — стабильная идентичность диска (WWN, EUI, WWID или серийный
	// номер; см. Identity). Пусто — не определена: разрушающие операции с
	// таким диском запрещены.
	ID            string   `json:"id,omitempty"`
	WWN           *string  `json:"wwn,omitempty"` // 0x…, только для NAA
	Kind          string   `json:"kind"`          // HDD, SSD, unknown
	SizeBytes     *int64   `json:"size_bytes"`
	Model         *string  `json:"model"`
	SystemRoles   []string `json:"system_roles"`
	SystemRole    string   `json:"system_role"`
	UnknownReason string   `json:"unknown_reason,omitempty"`
}

// Snapshot — результат обхода.
type Snapshot struct {
	Disks []Disk `json:"disks"`
	// Complete=false: часть зависимостей не определена; диски с ролью unknown
	// могут использоваться.
	Complete bool    `json:"complete"`
	Issues   []Issue `json:"issues"`
}

// ErrNoSource — каталог блочных устройств недоступен.
var ErrNoSource = errors.New("источник блочных устройств недоступен")

// RoleMap — реальный путь sysfs диска → множество ролей.
type RoleMap map[string]map[string]bool

func (r RoleMap) add(disk, role string) {
	if r[disk] == nil {
		r[disk] = map[string]bool{}
	}
	r[disk][role] = true
}

type collector struct {
	p          Paths
	candidates map[string]bool // реальные пути sysfs физических дисков
	roles      RoleMap
	issues     []Issue
	zfsNoted   bool
}

func newCollector(p Paths) (*collector, error) {
	c := &collector{p: p, candidates: map[string]bool{}, roles: RoleMap{}}
	entries, err := os.ReadDir(p.SysClassBlock)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNoSource, p.SysClassBlock, err)
	}
	for _, e := range entries {
		dir := filepath.Join(p.SysClassBlock, e.Name())
		if exists(filepath.Join(dir, "partition")) || !exists(filepath.Join(dir, "device")) {
			continue
		}
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			c.candidates[real] = true
		}
	}
	return c, nil
}

// issue добавляет неясность без повторов (дубли монтирований дают одинаковые).
func (c *collector) issue(i Issue) {
	for _, have := range c.issues {
		if have == i {
			return
		}
	}
	c.issues = append(c.issues, i)
}

// SystemBackingDisks — диски под точками монтирования и активным swap.
// Возвращает роли по реальному пути sysfs диска и обнаруженные неясности.
func SystemBackingDisks(p Paths) (RoleMap, []Issue, error) {
	c, err := newCollector(p)
	if err != nil {
		return nil, nil, err
	}
	c.collectMounts()
	return c.roles, c.issues, nil
}

// ActiveStackMembers — диски, на которых (или на разделах которых) есть
// активные holders: md, device-mapper (LVM, dm-integrity, dm-crypt).
func ActiveStackMembers(p Paths) (map[string]bool, []Issue, error) {
	c, err := newCollector(p)
	if err != nil {
		return nil, nil, err
	}
	c.collectHolders()
	out := map[string]bool{}
	for disk, roles := range c.roles {
		if roles[RoleStackMember] {
			out[disk] = true
		}
	}
	return out, c.issues, nil
}

// Observe строит снимок дисков с ролями.
func Observe(p Paths) (Snapshot, error) {
	c, err := newCollector(p)
	if err != nil {
		return Snapshot{}, err
	}
	c.collectMounts()
	c.collectHolders()
	c.checkZFSModule()

	entries, err := os.ReadDir(p.SysClassBlock)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrNoSource, err)
	}
	snap := Snapshot{Complete: len(c.issues) == 0, Issues: c.issues, Disks: []Disk{}}
	if snap.Issues == nil {
		snap.Issues = []Issue{}
	}
	counters := map[string]int{}
	for _, e := range entries { // ReadDir уже сортирует по имени
		name := e.Name()
		if hasAnyPrefix(name, "loop", "ram", "dm-", "md", "zram", "sr") {
			continue
		}
		dir := filepath.Join(p.SysClassBlock, name)
		if exists(filepath.Join(dir, "partition")) || !exists(filepath.Join(dir, "device")) {
			continue
		}
		if t, ok := readTrim(filepath.Join(dir, "device", "type")); ok && t == "5" {
			continue // оптический привод
		}
		d := Disk{KernelName: name, Kind: "unknown", SystemRoles: []string{}}
		if s, ok := readTrim(filepath.Join(dir, "size")); ok && isDecimal(s) {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				size := n * 512
				d.SizeBytes = &size
			}
		}
		switch r, _ := readTrim(filepath.Join(dir, "queue", "rotational")); r {
		case "1":
			d.Kind = "HDD"
		case "0":
			d.Kind = "SSD"
		}
		if m, ok := readTrim(filepath.Join(dir, "device", "model")); ok {
			d.Model = &m
		}
		d.ID, d.WWN = Identity(dir)
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			for role := range c.roles[real] {
				d.SystemRoles = append(d.SystemRoles, role)
			}
		}
		sort.Strings(d.SystemRoles)
		d.SystemRole = summary(d.SystemRoles)
		if d.SystemRole == SummaryUnknown {
			if snap.Complete {
				d.UnknownReason = "зависимости не обнаружены; это не доказывает, что диск свободен"
			} else {
				d.UnknownReason = "снимок неполный (см. issues): диск может использоваться"
			}
		}
		label := "Диск"
		if d.Kind == "SSD" {
			label = "SSD"
		}
		counters[label]++
		d.Name = fmt.Sprintf("%s %d", label, counters[label])
		snap.Disks = append(snap.Disks, d)
	}
	return snap, nil
}

func summary(roles []string) string {
	set := map[string]bool{}
	for _, r := range roles {
		set[r] = true
	}
	switch {
	case set[RoleRoot]:
		return SummaryRoot
	case set[RoleBoot] || set[RoleEFI] || set[RoleSwap]:
		return SummarySystem
	case len(roles) > 0:
		return SummaryInUse
	}
	return SummaryUnknown
}

// ---------- mountinfo ----------

type mountEntry struct {
	dev        DevNum
	mountpoint string
	fstype     string
	source     string
}

var systemMounts = map[string]string{
	"/":         RoleRoot,
	"/boot":     RoleBoot,
	"/boot/efi": RoleEFI,
	"/efi":      RoleEFI,
}

func isSystemRole(role string) bool {
	return role == RoleRoot || role == RoleBoot || role == RoleEFI || role == RoleSwap
}

// parseMountinfo разбирает /proc/self/mountinfo:
// id parent maj:min root mountpoint options [optional...] - fstype source superopts
func parseMountinfo(text string) (entries []mountEntry, bad []int) {
	for n, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if len(f) < 6 || sep < 0 || sep+2 >= len(f) {
			bad = append(bad, n+1)
			continue
		}
		dev, ok := parseDevNum(f[2])
		if !ok {
			bad = append(bad, n+1)
			continue
		}
		entries = append(entries, mountEntry{
			dev:        dev,
			mountpoint: unescape(f[4]),
			fstype:     unescape(f[sep+1]),
			source:     unescape(f[sep+2]),
		})
	}
	return entries, bad
}

func parseDevNum(s string) (DevNum, bool) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return DevNum{}, false
	}
	ma, err1 := strconv.ParseUint(a, 10, 32)
	mi, err2 := strconv.ParseUint(b, 10, 32)
	if err1 != nil || err2 != nil {
		return DevNum{}, false
	}
	return DevNum{uint32(ma), uint32(mi)}, true
}

// unescape декодирует восьмеричные экранирования ядра (\040 пробел,
// \011 табуляция, \012 перевод строки, \134 обратная косая черта).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1:i+4]) {
			v, _ := strconv.ParseUint(s[i+1:i+4], 8, 8)
			b.WriteByte(byte(v))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(s string) bool {
	if len(s) != 3 || s[0] > '3' {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

func (c *collector) collectMounts() {
	data, err := os.ReadFile(c.p.Mountinfo)
	var entries []mountEntry
	if err != nil {
		c.issue(Issue{Kind: "mountinfo", Source: c.p.Mountinfo, Reason: "mountinfo не прочитан: " + err.Error()})
	} else {
		var bad []int
		entries, bad = parseMountinfo(string(data))
		for _, n := range bad {
			c.issue(Issue{Kind: "mountinfo", Reason: fmt.Sprintf("строка %d не разобрана", n)})
		}
	}

	// Для системных точек: найден ли хотя бы один диск и была ли уже причина.
	sysFound := map[string]bool{}
	sysExplained := map[string]bool{}
	rootSeen := false
	for _, m := range entries {
		role, ok := systemMounts[m.mountpoint]
		if !ok {
			role = RoleMounted
		}
		if m.mountpoint == "/" {
			rootSeen = true
		}
		if isSystemRole(role) {
			if _, ok := sysFound[m.mountpoint]; !ok {
				sysFound[m.mountpoint] = false
			}
		}
		nodes, problem := c.mountNodes(m)
		if problem != "" {
			c.issue(Issue{Kind: "mount", Mountpoint: m.mountpoint, FSType: m.fstype, Source: m.source, Reason: problem})
			sysExplained[m.mountpoint] = true
		}
		for _, n := range nodes {
			if c.walk(n, role, map[string]bool{}) > 0 && isSystemRole(role) {
				sysFound[m.mountpoint] = true
			}
		}
	}
	if err == nil && !rootSeen {
		c.issue(Issue{Kind: "mountinfo", Mountpoint: "/", Reason: "корень не найден в mountinfo"})
	}
	points := make([]string, 0, len(sysFound))
	for mp := range sysFound {
		points = append(points, mp)
	}
	sort.Strings(points)
	for _, mp := range points {
		if !sysFound[mp] && !sysExplained[mp] {
			c.issue(Issue{Kind: "mount", Mountpoint: mp, Reason: "системная точка монтирования не сопоставлена с диском"})
		}
	}
	c.collectSwaps(entries)
}

// mountNodes возвращает узлы sysfs, на которых лежит файловая система.
// problem непуст, если определение неполное (узлы при этом могут быть частичными).
func (c *collector) mountNodes(m mountEntry) (nodes []string, problem string) {
	if m.dev.Major != 0 {
		if n, err := filepath.EvalSymlinks(filepath.Join(c.p.SysDevBlock, m.dev.String())); err == nil {
			return []string{n}, ""
		}
		if strings.HasPrefix(m.source, "/dev/") {
			if n, err := c.sourceNode(m.source); err == nil {
				return []string{n}, ""
			}
		}
		return nil, fmt.Sprintf("устройство %s не найдено в sysfs", m.dev)
	}
	// Анонимный номер (0:NN): устройство берётся из источника монтирования.
	switch m.fstype {
	case "zfs":
		c.noteZFS()
		return nil, "ZFS не поддержан: диски пула не определяются"
	case "btrfs":
		return c.btrfsNodes(m.source)
	}
	if strings.HasPrefix(m.source, "/dev/") {
		n, err := c.sourceNode(m.source)
		if err != nil {
			return nil, "источник не сопоставлен с устройством: " + err.Error()
		}
		return []string{n}, ""
	}
	return nil, "" // не блочная ФС (proc, tmpfs, overlay, nfs…)
}

// sourceNode сопоставляет путь /dev/… с реальным узлом в sysfs.
func (c *collector) sourceNode(source string) (string, error) {
	name := filepath.Base(source)
	if c.p.Dev != "" {
		rel := strings.TrimPrefix(source, "/dev/")
		if real, err := filepath.EvalSymlinks(filepath.Join(c.p.Dev, rel)); err == nil {
			name = filepath.Base(real)
		}
	}
	if real, err := filepath.EvalSymlinks(filepath.Join(c.p.SysClassBlock, name)); err == nil {
		return real, nil
	}
	// /dev/mapper/<имя> без /dev: поиск по /sys/class/block/dm-*/dm/name.
	if strings.HasPrefix(source, "/dev/mapper/") {
		want := strings.TrimPrefix(source, "/dev/mapper/")
		matches, _ := filepath.Glob(filepath.Join(c.p.SysClassBlock, "dm-*"))
		for _, dm := range matches {
			if got, ok := readTrim(filepath.Join(dm, "dm", "name")); ok && got == want {
				return filepath.EvalSymlinks(dm)
			}
		}
	}
	return "", fmt.Errorf("%s не найден в %s", name, c.p.SysClassBlock)
}

// btrfsNodes — все устройства файловой системы Btrfs, смонтированной из source.
//
// fsid определяется перебором /sys/fs/btrfs/<fsid>/devices/* и сравнением
// реальных путей sysfs с узлом источника. Выбрано вместо UUID через
// /dev/disk/by-uuid: перебор опирается только на состояние ядра (не нужен udev,
// работает в initramfs и без /dev/disk), не зависит от устаревших ссылок после
// переформатирования и сразу даёт полный состав многодискового Btrfs —
// by-uuid указывает лишь на одно произвольное устройство.
// Каталог /sys/fs/btrfs/<fsid> есть только у смонтированных ФС; при временном
// fsid (клон, ядро ≥6.7) имя каталога — временный fsid, сопоставление по
// устройству от этого не зависит.
func (c *collector) btrfsNodes(source string) ([]string, string) {
	src, err := c.sourceNode(source)
	if err != nil {
		return nil, "источник btrfs не сопоставлен с устройством: " + err.Error()
	}
	fsids, err := os.ReadDir(c.p.SysFsBtrfs)
	if err != nil {
		return []string{src}, "состав btrfs не определён (учтено только устройство-источник): " + err.Error()
	}
	var matched [][]string
	for _, f := range fsids {
		devDir := filepath.Join(c.p.SysFsBtrfs, f.Name(), "devices")
		devs, err := os.ReadDir(devDir)
		if err != nil {
			continue // features, другие служебные каталоги
		}
		var members []string
		hit := false
		for _, d := range devs {
			real, err := filepath.EvalSymlinks(filepath.Join(devDir, d.Name()))
			if err != nil {
				continue
			}
			members = append(members, real)
			if real == src {
				hit = true
			}
		}
		if hit {
			matched = append(matched, members)
		}
	}
	switch len(matched) {
	case 0:
		return []string{src}, "устройство-источник не найдено в /sys/fs/btrfs/*/devices; учтено только оно"
	case 1:
		return matched[0], ""
	}
	// Неоднозначно: консервативно берём объединение.
	var all []string
	for _, m := range matched {
		all = append(all, m...)
	}
	return all, "устройство-источник входит в несколько btrfs; взято объединение"
}

func (c *collector) noteZFS() {
	if c.zfsNoted {
		return
	}
	c.zfsNoted = true
	c.issue(Issue{Kind: "zfs", Reason: "обнаружен ZFS: определение дисков пулов не поддержано, свободными диски не считаются"})
}

func (c *collector) checkZFSModule() {
	if c.p.SysModule != "" && exists(filepath.Join(c.p.SysModule, "zfs")) {
		c.noteZFS()
	}
}

// ---------- swap ----------

func (c *collector) collectSwaps(mounts []mountEntry) {
	data, err := os.ReadFile(c.p.Swaps)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.issue(Issue{Kind: "swap", Source: c.p.Swaps, Reason: "список swap не прочитан: " + err.Error()})
		}
		return
	}
	stat := c.p.Stat
	if stat == nil {
		stat = systemStat
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		path := unescape(f[0])
		if strings.HasPrefix(filepath.Base(path), "zram") {
			continue // подкачка в памяти
		}
		st, err := stat(path)
		if err != nil {
			c.issue(Issue{Kind: "swap", Source: path, Reason: "stat не выполнен: " + err.Error()})
			continue
		}
		var nodes []string
		problem := ""
		if st.Block {
			nodes, problem = c.mountNodes(mountEntry{dev: st.Rdev, source: path})
		} else if st.Dev.Major != 0 {
			nodes, problem = c.mountNodes(mountEntry{dev: st.Dev})
		} else {
			// Файл подкачки на ФС с анонимным номером (btrfs): ищем её монтирование.
			problem = "файл подкачки на ФС " + st.Dev.String() + " без сопоставленного монтирования"
			for _, m := range mounts {
				if m.dev == st.Dev {
					n, p := c.mountNodes(m)
					nodes = append(nodes, n...)
					problem = p
				}
			}
		}
		found := 0
		for _, n := range nodes {
			found += c.walk(n, RoleSwap, map[string]bool{})
		}
		if problem == "" && found == 0 {
			problem = "swap не сопоставлен с диском"
		}
		if problem != "" {
			c.issue(Issue{Kind: "swap", Source: path, Reason: problem})
		}
	}
}

// ---------- holders ----------

func (c *collector) collectHolders() {
	entries, err := os.ReadDir(c.p.SysClassBlock)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(c.p.SysClassBlock, e.Name())
		holders, err := os.ReadDir(filepath.Join(dir, "holders"))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.issue(Issue{Kind: "holders", Source: e.Name(), Reason: "holders не прочитан: " + err.Error()})
			}
			continue
		}
		if len(holders) > 0 {
			c.walk(dir, RoleStackMember, map[string]bool{})
		}
	}
}

// ---------- обход sysfs ----------

// walk спускается от узла к физическим дискам: раздел → родительский диск,
// md/dm → slaves/. Возвращает число найденных дисков.
func (c *collector) walk(node, role string, visited map[string]bool) int {
	real, err := filepath.EvalSymlinks(node)
	if err != nil {
		c.issue(Issue{Kind: "sysfs", Source: node, Reason: "узел sysfs недоступен: " + err.Error()})
		return 0
	}
	if visited[real] {
		return 0
	}
	visited[real] = true
	if c.candidates[real] {
		c.roles.add(real, role)
		return 1
	}
	if exists(filepath.Join(real, "partition")) {
		return c.walk(filepath.Dir(real), role, visited)
	}
	slaves, err := os.ReadDir(filepath.Join(real, "slaves"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.issue(Issue{Kind: "sysfs", Source: real, Reason: "slaves не прочитан: " + err.Error()})
		}
		return 0
	}
	found := 0
	for _, s := range slaves {
		link := filepath.Join(real, "slaves", s.Name())
		if vanished(link) {
			// Нижнее устройство удалено из системы (отказ/отключение диска),
			// а dm/md ещё держит ссылку на него. Оно не может быть ни одним из
			// текущих дисков, поэтому неясностью не считается.
			continue
		}
		found += c.walk(link, role, visited)
	}
	return found
}

// ---------- идентичность ----------

// Identity — стабильная идентичность диска по каталогу sysfs (class/block/X):
//   - device/wwid или wwid вида "naa.5000…" → "wwn-0x5000…" (как
//     /dev/disk/by-id/wwn-*), wwn = "0x5000…";
//   - "eui.…" → "eui-…"; прочие wwid (t10.…, nvme.…) → "wwid-…";
//   - иначе device/serial или serial (virtio, часть NVMe) → "serial-…".
//
// Пробелы схлопываются в "_", допустимы только [A-Za-z0-9._:-]; что не
// приводится к этому набору — пусто (идентичность не определена).
func Identity(dir string) (id string, wwn *string) {
	for _, f := range []string{"device/wwid", "wwid"} {
		v, ok := readTrim(filepath.Join(dir, f))
		if !ok || v == "" {
			continue
		}
		switch {
		case strings.HasPrefix(v, "naa."):
			hex := strings.ToLower(strings.TrimPrefix(v, "naa."))
			if !isHex(hex) {
				return "", nil
			}
			w := "0x" + hex
			return "wwn-" + w, &w
		case strings.HasPrefix(v, "eui."):
			return sanitizeID("eui-" + strings.ToLower(strings.TrimPrefix(v, "eui."))), nil
		default:
			return sanitizeID("wwid-" + v), nil
		}
	}
	for _, f := range []string{"device/serial", "serial"} {
		if v, ok := readTrim(filepath.Join(dir, f)); ok && v != "" {
			return sanitizeID("serial-" + v), nil
		}
	}
	return "", nil
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func sanitizeID(s string) string {
	s = strings.Join(strings.Fields(s), "_")
	if len(s) > 128 {
		return ""
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == ':' || r == '-'
		if !ok {
			return ""
		}
	}
	return s
}

// ---------- утилиты ----------

// vanished — символьная ссылка есть, а её цель исчезла (устройство удалено).
func vanished(link string) bool {
	st, err := os.Lstat(link)
	if err != nil || st.Mode()&fs.ModeSymlink == 0 {
		return false
	}
	_, err = os.Stat(link)
	return errors.Is(err, fs.ErrNotExist)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readTrim(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
