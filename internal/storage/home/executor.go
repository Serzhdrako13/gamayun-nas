package home

import (
	"context"
	"errors"
	"fmt"
	"gamayun-nas/internal/watch"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gamayun-nas/internal/disks"
)

// CmdResult — итог внешней команды (зеркало helper.CommandResult без
// импорта helper: helper сам импортирует этот пакет).
type CmdResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool
}

// Runner — запуск внешней программы: абсолютный путь, массив аргументов,
// без shell (реализация — helper.Runner через адаптер).
type Runner interface {
	Run(ctx context.Context, timeout time.Duration, path string, args ...string) (CmdResult, error)
}

// Tools — абсолютные пути программ.
type Tools struct {
	Sgdisk         string
	Mdadm          string
	Integritysetup string
	LVM            string
	MkfsBtrfs      string
	Btrfs          string
	Mount          string
	Umount         string
	Wipefs         string
	Blkid          string
	Udevadm        string
	Dmsetup        string
	Blkdiscard     string
	Partx          string
}

// DefaultTools — пути Debian 13.
func DefaultTools() Tools {
	return Tools{
		Sgdisk:         "/usr/sbin/sgdisk",
		Mdadm:          "/usr/sbin/mdadm",
		Integritysetup: "/usr/sbin/integritysetup",
		LVM:            "/usr/sbin/lvm",
		MkfsBtrfs:      "/usr/sbin/mkfs.btrfs",
		Btrfs:          "/usr/bin/btrfs",
		Mount:          "/usr/bin/mount",
		Umount:         "/usr/bin/umount",
		Wipefs:         "/usr/sbin/wipefs",
		Blkid:          "/usr/sbin/blkid",
		Udevadm:        "/usr/bin/udevadm",
		Dmsetup:        "/usr/sbin/dmsetup",
		Blkdiscard:     "/usr/sbin/blkdiscard",
		Partx:          "/usr/bin/partx",
	}
}

// Paths — источники состояния.
type Paths struct {
	Disks  disks.Paths // sysfs, mountinfo, swaps, stat
	Dev    string      // /dev
	Mdstat string      // /proc/mdstat
}

// DefaultPaths — пути реальной системы.
func DefaultPaths() Paths {
	return Paths{Disks: disks.DefaultPaths(), Dev: "/dev", Mdstat: "/proc/mdstat"}
}

// Executor выполняет операции пула внутри gamayun-helper.
type Executor struct {
	Runner Runner
	Tools  Tools
	Paths  Paths
	// AllowIDs — идентичности дисков, с которыми разрешены разрушающие и
	// изменяющие операции (конфиг helper destructive_allow_ids). Пусто —
	// без ограничения списком: защита — проверки перед записью
	// (CheckFreeDisk, принадлежность пулу). Непустой — только эти диски (стенд).
	AllowIDs []string
	// MountRoot — каталог точек монтирования пулов.
	MountRoot string
	// Wait — ожидание появления узлов после sgdisk/mdadm; 0 — 15 с.
	Wait time.Duration
	// Poll — шаг ожидания; 0 — 200 мс.
	Poll time.Duration

	mu sync.Mutex // одна изменяющая операция за раз
}

// ErrRefused — операция отклонена проверками безопасности до записи.
var ErrRefused = errors.New("отказ")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// CmdError — внешняя программа завершилась с ненулевым кодом.
type CmdError struct {
	Cmd    string
	Code   int
	Stderr string
}

func (e *CmdError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 600 {
		msg = msg[len(msg)-600:]
	}
	return fmt.Sprintf("%s: код %d: %s", e.Cmd, e.Code, msg)
}

const (
	shortTimeout = 30 * time.Second
	longTimeout  = 30 * time.Minute
)

// journal — выполненные команды операции.
type journal struct{ cmds []string }

func (j *journal) add(path string, args []string) {
	if j != nil {
		j.cmds = append(j.cmds, strings.TrimSpace(filepath.Base(path)+" "+strings.Join(args, " ")))
	}
}

// run выполняет команду; ненулевой код — *CmdError (stdout возвращается).
func (e *Executor) run(ctx context.Context, j *journal, timeout time.Duration, path string, args ...string) ([]byte, error) {
	j.add(path, args)
	r, err := e.Runner.Run(ctx, timeout, path, args...)
	if err != nil {
		return r.Stdout, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if r.ExitCode != 0 {
		return r.Stdout, &CmdError{Cmd: filepath.Base(path) + " " + strings.Join(args, " "), Code: r.ExitCode, Stderr: string(r.Stderr)}
	}
	return r.Stdout, nil
}

// query — команда только чтения (не журналируется).
func (e *Executor) query(ctx context.Context, path string, args ...string) ([]byte, error) {
	return e.run(ctx, nil, shortTimeout, path, args...)
}

func (e *Executor) allowed(id string) bool {
	if id == "" {
		return false
	}
	if len(e.AllowIDs) == 0 {
		return true
	}
	for _, a := range e.AllowIDs {
		if a == id && id != "" {
			return true
		}
	}
	return false
}

var knameRe = regexp.MustCompile(`^[a-z][a-z0-9]{1,31}$`)

// CheckFreeDisk — проверка непосредственно перед записью на диск ref.
// Возвращает имя ядра. Любое сомнение — ErrRefused.
func (e *Executor) CheckFreeDisk(ctx context.Context, ref DiskRef) (string, error) {
	if !e.allowed(ref.ID) {
		return "", refuse("диск %s не входит в destructive_allow_ids", ref.ID)
	}
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return "", refuse("снимок дисков: %v", err)
	}
	if !snap.Complete {
		return "", refuse("снимок дисков неполный: %d неясностей", len(snap.Issues))
	}
	var found []disks.Disk
	for _, d := range snap.Disks {
		if d.ID == ref.ID {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		return "", refuse("диск %s найден %d раз", ref.ID, len(found))
	}
	d := found[0]
	if d.SystemRole != disks.SummaryUnknown || len(d.SystemRoles) != 0 {
		return "", refuse("диск %s (%s) занят: роль %s %v", ref.ID, d.KernelName, d.SystemRole, d.SystemRoles)
	}
	if d.SizeBytes == nil || *d.SizeBytes != ref.SizeBytes {
		return "", refuse("диск %s: размер не совпадает с планом", ref.ID)
	}
	k := d.KernelName
	if !knameRe.MatchString(k) {
		return "", refuse("недопустимое имя ядра %q", k)
	}
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, k)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", refuse("sysfs %s: %v", k, err)
	}
	for _, en := range entries {
		if _, err := os.Stat(filepath.Join(dir, en.Name(), "partition")); err == nil {
			return "", refuse("на диске %s есть раздел %s", k, en.Name())
		}
	}
	holders, err := os.ReadDir(filepath.Join(dir, "holders"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", refuse("holders %s: %v", k, err)
	}
	if len(holders) > 0 {
		return "", refuse("у диска %s есть holders", k)
	}
	devnum, err := os.ReadFile(filepath.Join(dir, "dev"))
	if err != nil {
		return "", refuse("номер устройства %s: %v", k, err)
	}
	node := filepath.Join(e.Paths.Dev, k)
	if e.Paths.Disks.Stat == nil {
		return "", refuse("нет функции stat")
	}
	st, err := e.Paths.Disks.Stat(node)
	if err != nil || !st.Block || st.Rdev.String() != strings.TrimSpace(string(devnum)) {
		return "", refuse("узел %s не совпадает с устройством ядра %s", node, strings.TrimSpace(string(devnum)))
	}
	out, err := e.query(ctx, e.Tools.Wipefs, "--no-act", node)
	if err != nil {
		return "", refuse("wipefs %s: %v", k, err)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return "", refuse("на диске %s есть сигнатуры: %s", k, s)
	}
	_, err = e.query(ctx, e.Tools.Blkid, "-p", node)
	var ce *CmdError
	if !errors.As(err, &ce) || ce.Code != 2 {
		return "", refuse("blkid -p %s: сигнатура найдена или ошибка (%v)", k, err)
	}
	return k, nil
}

// ---------- разметка ----------

type partSpec struct {
	num   int // номер раздела = индекс массива + 1
	start int64
	size  int64
	label string
}

// layout — разделы диска: подряд с 1 МиБ, номера по индексам массивов.
func layout(pool string, idx []int, slices []int64) []partSpec {
	off := Align
	out := make([]partSpec, len(idx))
	for i := range idx {
		out[i] = partSpec{num: idx[i] + 1, start: off, size: slices[i], label: PartLabel(pool, idx[i])}
		off += slices[i]
	}
	return out
}

func (e *Executor) readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// partition создаёт разделы и ждёт их узлы; возвращает пути /dev по номерам.
func (e *Executor) partition(ctx context.Context, j *journal, kname string, parts []partSpec) (map[int]string, error) {
	lbs, err := e.readInt(filepath.Join(e.Paths.Disks.SysClassBlock, kname, "queue", "logical_block_size"))
	if err != nil || lbs <= 0 || Align%lbs != 0 {
		lbs = 512
	}
	args := []string{}
	for _, p := range parts {
		s := p.start / lbs
		end := (p.start+p.size)/lbs - 1
		n := strconv.Itoa(p.num)
		args = append(args, "-n", fmt.Sprintf("%s:%d:%d", n, s, end), "-t", n+":FD00", "-c", n+":"+p.label)
	}
	args = append(args, filepath.Join(e.Paths.Dev, kname))
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Sgdisk, args...); err != nil {
		return nil, err
	}
	e.settle(ctx, j)
	out := map[int]string{}
	for _, p := range parts {
		var node string
		err := e.waitFor(ctx, fmt.Sprintf("раздел %d диска %s", p.num, kname), func() bool {
			pk := e.partKName(kname, p.num)
			if pk == "" {
				return false
			}
			node = filepath.Join(e.Paths.Dev, pk)
			_, err := os.Stat(node)
			return err == nil
		})
		if err != nil {
			return nil, fmt.Errorf("раздел %d на %s не появился: %w", p.num, kname, err)
		}
		out[p.num] = node
	}
	for _, p := range parts {
		if err := e.zeroStale(ctx, j, out[p.num], p.size); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Области нового раздела, которые обнуляются перед созданием слоёв.
const (
	staleHead int64 = 256 << 20
	staleTail int64 = 16 << 20
)

// zeroStale обнуляет начало (256 МиБ) и конец (16 МиБ) нового раздела.
//
// Зачем: сторож проверяет сигнатуры только диска, а внутри раздела могут
// остаться метаданные прежнего пула. dm-integrity с --no-wipe и md при той
// же геометрии показывают их на новом устройстве по тем же смещениям, и
// udev успевает автоматически собрать старый md-массив или активировать
// старую VG до наших команд. Начало покрывает
// суперблок и журнал dm-integrity, суперблок md 1.2, смещение данных md
// (до 128 МиБ на больших дисках), метку LVM и суперблоки Btrfs (64 КиБ и
// 64 МиБ); конец — суперблоки md 0.90/1.0 и ZFS. Раздел новый, диск
// прошёл проверку перед записью, пользователь подтвердил стирание.
func (e *Executor) zeroStale(ctx context.Context, j *journal, node string, size int64) error {
	head := min(size, staleHead)
	if _, err := e.run(ctx, j, longTimeout, e.Tools.Blkdiscard, "--zeroout", "--offset", "0", "--length", strconv.FormatInt(head, 10), node); err != nil {
		return err
	}
	if size > head {
		tail := min(size-head, staleTail)
		if _, err := e.run(ctx, j, longTimeout, e.Tools.Blkdiscard, "--zeroout", "--offset", strconv.FormatInt(size-tail, 10),
			"--length", strconv.FormatInt(tail, 10), node); err != nil {
			return err
		}
	}
	return nil
}

// partKName — имя ядра раздела num диска kname по sysfs.
func (e *Executor) partKName(kname string, num int) string {
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, kname)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, en := range entries {
		if n, err := e.readInt(filepath.Join(dir, en.Name(), "partition")); err == nil && int(n) == num {
			return en.Name()
		}
	}
	return ""
}

func (e *Executor) settle(ctx context.Context, j *journal) {
	// Ошибка settle не фатальна: дальше всё равно ждём узлы.
	e.run(ctx, j, shortTimeout, e.Tools.Udevadm, "settle", "--timeout=30")
}

// waitFor ждёт появления узла: окно — половина
// Wait, затем udevadm settle и второе окно; ошибка — с именем узла и причиной.
func (e *Executor) waitFor(ctx context.Context, what string, cond func() bool) error {
	wait, poll := e.Wait, e.Poll
	if wait <= 0 {
		wait = 15 * time.Second
	}
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	out := watch.Run(ctx, watch.Spec{
		Name:        "ожидание " + what,
		Poll:        poll,
		StallWindow: wait / 2,
		Deadline:    wait + 2*poll,
		Probe: func(context.Context) watch.Observation {
			if cond() {
				return watch.Observation{Target: true}
			}
			return watch.Observation{Elements: []watch.Element{{ID: what, State: watch.Working, Pos: -1, Alive: true, Detail: "не появился"}}}
		},
		Recover: func(ctx context.Context) {
			e.Runner.Run(ctx, shortTimeout, e.Tools.Udevadm, "settle", "--timeout=5")
		},
	})
	if out.Result == watch.ResultCancelled && ctx.Err() != nil {
		return ctx.Err()
	}
	return out.Err()
}

// ---------- dm-integrity ----------

// Параметры dm-integrity: crc32c, сектор 4 КиБ (тег 4 байта на 4 КиБ).
//
// Инициализация без полной записи: format --no-wipe пишет только
// суперблок и журнал (доли секунды), open --integrity-recalculate
// запускает в ядре фоновый пересчёт тегов по содержимому раздела.
// Устройство доступно сразу; до границы пересчёта (recalc_sector в
// dmsetup status) чтения не проверяются, поэтому старые теги не дают
// ошибок, а md может синхронизировать массив обычным resync/recovery.
// Без пересчёта md не создаётся: каждое чтение — ошибка контрольной суммы.
//
// --integrity-recalculate — только при первом открытии нового участника:
// на устройстве без флага recalculating в суперблоке (например, после
// format с очисткой) он запускает пересчёт с нуля и «узаконивает» накопленную порчу (проверено на стенде, loop в
// /dev/shm). На устройстве после --no-wipe флаг recalculating остаётся в
// суперблоке и после завершения, integritysetup open сам добавляет
// recalculate — повторного пересчёта нет; незавершённый продолжится.
var integrityFormatArgs = []string{"format", "--batch-mode", "--no-wipe", "--sector-size", "4096", "--integrity", "crc32c"}

// integrityOpenArgs — открытие нового участника в режиме mode.
func integrityOpenArgs(mode, part, name string) []string {
	args := []string{"open", "--integrity", "crc32c", "--integrity-recalculate"}
	if mode == IntegrityBitmap {
		args = append(args, "--integrity-bitmap-mode")
	}
	return append(args, part, name)
}

func (e *Executor) integrity(ctx context.Context, j *journal, part, name, mode string) (string, error) {
	if mode != IntegrityJournal && mode != IntegrityBitmap {
		return "", fmt.Errorf("режим dm-integrity %q", mode)
	}
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, append(append([]string{}, integrityFormatArgs...), part)...); err != nil {
		return "", err
	}
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, integrityOpenArgs(mode, part, name)...); err != nil {
		return "", err
	}
	node := filepath.Join(e.Paths.Dev, "mapper", name)
	if err := e.waitFor(ctx, node, func() bool { _, err := os.Stat(node); return err == nil }); err != nil {
		return "", fmt.Errorf("%s не появился: %w", node, err)
	}
	return node, nil
}

// integrityMaps — отображения dm-integrity: имя → (mismatches, …).
func (e *Executor) integrityMaps(ctx context.Context) ([]IntegrityStatus, error) {
	out, err := e.query(ctx, e.Tools.Dmsetup, "status", "--target", "integrity")
	if err != nil {
		return nil, err
	}
	return ParseIntegrityStatus(string(out), ""), nil
}

// ---------- обнаружение пула ----------

// poolArray — md-массив пула.
type poolArray struct {
	index    int
	name     string // p-i
	kname    string // md127
	node     string // /dev/md/p-i; без ссылки udev — /dev/mdN
	inactive bool   // в /proc/mdstat «inactive» (данных не обслуживает)
}

// arrays — md-массивы пула: по ссылкам /dev/md/p-<i> и по массивам ядра
// (/proc/mdstat) без такой ссылки — по имени в суперблоке (нет
// ссылки udev ≠ «массив не собран»). Имя массива ядра без ссылки не
// прочитано — ошибка (состояние неизвестно), а не «массива нет».
func (e *Executor) arrays(ctx context.Context, pool string) ([]poolArray, error) {
	// «<хост>:<пул>-<i>» — массив, собранный как чужой (homehost в
	// суперблоке ≠ текущему имени: переименование сервера, ранняя сборка
	// udev до установки hostname).
	re := regexp.MustCompile(`^(?:[A-Za-z0-9._-]+:)?` + regexp.QuoteMeta(pool) + `-(\d{1,2})$`)
	st, err := e.mdstat()
	if err != nil {
		return nil, fmt.Errorf("/proc/mdstat: %w", err)
	}
	dir := filepath.Join(e.Paths.Dev, "md")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []poolArray
	linked := map[string]bool{} // массивы ядра, на которые есть ссылка /dev/md/*
	for _, en := range entries {
		real, err := filepath.EvalSymlinks(filepath.Join(dir, en.Name()))
		if err != nil {
			continue
		}
		kname := filepath.Base(real)
		linked[kname] = true
		m := re.FindStringSubmatch(en.Name())
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		node := filepath.Join(dir, en.Name())
		if strings.Contains(en.Name(), ":") {
			// mdadm не принимает «/dev/md/<хост>:<имя>» как устройство
			// («Not POSIX compatible») — узел ядра /dev/mdN.
			node = filepath.Join(e.Paths.Dev, kname)
		}
		out = append(out, poolArray{index: i, name: en.Name(), kname: kname, node: node, inactive: st[kname].Inactive})
	}
	knames := make([]string, 0, len(st))
	for k := range st {
		knames = append(knames, k)
	}
	sort.Strings(knames)
	var unread []string
	for _, k := range knames {
		if linked[k] {
			continue
		}
		name, err := e.mdName(ctx, k, st[k])
		if err != nil {
			unread = append(unread, k+": "+err.Error())
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		i, _ := strconv.Atoi(m[1])
		out = append(out, poolArray{index: i, name: name, kname: k, node: filepath.Join(e.Paths.Dev, k), inactive: st[k].Inactive})
	}
	if len(unread) > 0 {
		return nil, fmt.Errorf("массив без ссылки /dev/md, имя не прочитано: %s", strings.Join(unread, "; "))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].index < out[b].index })
	for k := 1; k < len(out); k++ {
		if out[k].index == out[k-1].index {
			return nil, fmt.Errorf("два массива с номером %d: %s и %s", out[k].index, out[k-1].kname, out[k].kname)
		}
	}
	return out, nil
}

// mdName — имя массива ядра kname из суперблока («<хост>:<имя>» или «<имя>»):
// mdadm --detail --export, для неактивного без имени — --examine --export
// участников. "" без ошибки — суперблок прочитан, имени нет (0.90, не наш).
func (e *Executor) mdName(ctx context.Context, kname string, ms MdstatArray) (string, error) {
	name, meta, derr := e.exportName(ctx, "--detail", filepath.Join(e.Paths.Dev, kname))
	if derr == nil && (name != "" || !ms.Inactive) {
		return name, nil
	}
	if derr == nil && meta {
		derr = errors.New("mdadm --detail: нет MD_NAME у неактивного массива")
	}
	errs := []string{derr.Error()}
	for _, m := range append(append(append([]string{}, ms.Members...), ms.Spares...), ms.Faulty...) {
		name, _, err := e.exportName(ctx, "--examine", filepath.Join(e.Paths.Dev, m))
		if err == nil {
			return name, nil
		}
		errs = append(errs, err.Error())
	}
	return "", errors.New(strings.Join(errs, "; "))
}

// exportName — MD_NAME из «mdadm <mode> --export dev»; meta — вывод
// опознан (есть MD_METADATA/MD_LEVEL), иначе ошибка (мусор, пусто).
func (e *Executor) exportName(ctx context.Context, mode, dev string) (name string, meta bool, err error) {
	out, err := e.query(ctx, e.Tools.Mdadm, mode, "--export", dev)
	if err != nil {
		return "", false, err
	}
	kv := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			kv[k] = v
		}
	}
	if kv["MD_METADATA"] == "" && kv["MD_LEVEL"] == "" {
		return "", false, fmt.Errorf("mdadm %s %s: вывод не распознан", mode, filepath.Base(dev))
	}
	if kv["MD_METADATA"] == "0.90" {
		return "", true, nil
	}
	return kv["MD_NAME"], true, nil
}

func (e *Executor) arrayByIndex(arrs []poolArray, i int) (poolArray, bool) {
	for _, a := range arrs {
		if a.index == i {
			return a, true
		}
	}
	return poolArray{}, false
}

// vgExists — есть ли группа томов.
func (e *Executor) vgList(ctx context.Context) ([]map[string]string, error) {
	out, err := e.query(ctx, e.Tools.LVM, "vgs", "--reportformat", "json", "--units", "b", "--nosuffix",
		"-o", "vg_name,vg_size,vg_free,pv_count,vg_extent_size")
	if err != nil {
		return nil, err
	}
	return ParseLVMReport(out, "vg")
}

func (e *Executor) pvList(ctx context.Context, vg string) ([]string, error) {
	out, err := e.query(ctx, e.Tools.LVM, "pvs", "--reportformat", "json", "--units", "b", "--nosuffix",
		"-o", "pv_name,vg_name,pv_size,pv_free,dev_size")
	if err != nil {
		return nil, err
	}
	rows, err := ParseLVMReport(out, "pv")
	if err != nil {
		return nil, err
	}
	var pvs []string
	for _, r := range rows {
		if r["vg_name"] == vg {
			pvs = append(pvs, r["pv_name"])
		}
	}
	return pvs, nil
}

// diskIDsUnder — идентичности физических дисков под узлом /dev (md, dm,
// раздел, диск) по sysfs slaves.
func (e *Executor) diskIDsUnder(node string) ([]string, error) {
	ids, err := e.disksUnder(node)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// disksUnder — физические диски под узлом: идентичность → имя ядра.
func (e *Executor) disksUnder(node string) (map[string]string, error) {
	real, err := filepath.EvalSymlinks(node)
	if err != nil {
		return nil, err
	}
	start := filepath.Join(e.Paths.Disks.SysClassBlock, filepath.Base(real))
	ids := map[string]string{}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > 8 {
			return errors.New("слишком глубокая вложенность устройств")
		}
		r, err := filepath.EvalSymlinks(dir)
		if errors.Is(err, os.ErrNotExist) && depth > 0 {
			// Участник исчез (диск удалён/отказал): в slaves осталась
			// висячая ссылка. Такой диск уже не входит в пул.
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(r, "partition")); err == nil {
			return walk(filepath.Dir(r), depth+1)
		}
		if _, err := os.Stat(filepath.Join(r, "device")); err == nil {
			id, _ := disks.Identity(r)
			if id == "" {
				return fmt.Errorf("у диска %s нет идентичности", filepath.Base(r))
			}
			ids[id] = filepath.Base(r)
			return nil
		}
		slaves, err := os.ReadDir(filepath.Join(r, "slaves"))
		if err != nil {
			return err
		}
		for _, s := range slaves {
			if err := walk(filepath.Join(r, "slaves", s.Name()), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(start, 0); err != nil {
		return nil, err
	}
	return ids, nil
}

// checkPoolAllowed — все доступные физические диски пула входят в allowlist.
func (e *Executor) checkPoolAllowed(ctx context.Context, pool string) error {
	pvs, err := e.pvList(ctx, VGName(pool))
	if err != nil {
		return refuse("тома пула %s: %v", pool, err)
	}
	if len(pvs) == 0 {
		return refuse("пул %s не найден (нет физических томов в %s)", pool, VGName(pool))
	}
	n := 0
	for _, pv := range pvs {
		ids, err := e.diskIDsUnder(pv)
		if err != nil {
			return refuse("диски пула %s под %s: %v", pool, pv, err)
		}
		for _, id := range ids {
			if !e.allowed(id) {
				return refuse("диск пула %s (%s) не входит в destructive_allow_ids", pool, id)
			}
			n++
		}
	}
	if n == 0 {
		return refuse("у пула %s не найдено ни одного диска", pool)
	}
	return nil
}

func (e *Executor) mountPoint(pool string) string { return filepath.Join(e.MountRoot, pool) }

// mountedAt — что смонтировано в точке (источник) по mountinfo.
func (e *Executor) mountedAt(mp string) (string, bool) {
	src, _, ok := e.mountInfo(mp)
	return src, ok
}

// mountInfo — источник и параметры монтирования (rw/ro…) точки mp;
// при нескольких записях — последняя (верхняя).
func (e *Executor) mountInfo(mp string) (src string, opts []string, ok bool) {
	src, opts, _, ok = e.mountInfoSuper(mp)
	return src, opts, ok
}

// mountInfoSuper — как mountInfo, плюс параметры суперблока (последнее поле
// mountinfo). Аварийный read-only Btrfs (Transaction aborted) виден только
// там: у точки монтирования остаётся rw.
func (e *Executor) mountInfoSuper(mp string) (src string, opts, super []string, ok bool) {
	b, err := os.ReadFile(e.Paths.Disks.Mountinfo)
	if err != nil {
		return "", nil, nil, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[4] != mp {
			continue
		}
		for i := 6; i < len(f)-2; i++ {
			if f[i] == "-" {
				src, opts, ok = f[i+2], strings.Split(f[5], ","), true
				super = nil
				if i+3 < len(f) {
					super = strings.Split(f[i+3], ",")
				}
				break
			}
		}
	}
	return src, opts, super, ok
}

// isIntegrityArray — участники md — устройства dm-integrity.
func (e *Executor) isIntegrityArray(kname string) (bool, error) {
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, kname, "slaves")
	slaves, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	integ, plain := 0, 0
	for _, s := range slaves {
		uuid, err := os.ReadFile(filepath.Join(dir, s.Name(), "dm", "uuid"))
		if err == nil && strings.HasPrefix(string(uuid), "CRYPT-INTEGRITY-") {
			integ++
		} else {
			plain++
		}
	}
	if integ > 0 && plain > 0 {
		return false, fmt.Errorf("массив %s: диски вперемешку: с dm-integrity и без", kname)
	}
	return integ > 0, nil
}

// ---------- операции ----------

// Create создаёт пул по плану.
func (e *Executor) Create(ctx context.Context, p CreateParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Pool: p.Pool}

	// Allowlist — до любых обращений к системе.
	for _, d := range p.Disks {
		if !e.allowed(d.ID) {
			return res, refuse("диск %s не входит в destructive_allow_ids", d.ID)
		}
	}

	// Пул с таким именем не должен существовать ни в одном слое.
	if arrs, err := e.arrays(ctx, p.Pool); err != nil || len(arrs) > 0 {
		return res, refuse("массивы пула %s уже существуют или не читаются (%v)", p.Pool, err)
	}
	vgs, err := e.vgList(ctx)
	if err != nil {
		return res, refuse("список VG: %v", err)
	}
	for _, v := range vgs {
		if v["vg_name"] == VGName(p.Pool) {
			return res, refuse("группа томов %s уже существует", VGName(p.Pool))
		}
	}
	maps, err := e.integrityMaps(ctx)
	if err != nil {
		return res, refuse("dmsetup: %v", err)
	}
	for _, m := range maps {
		if strings.HasPrefix(m.Name, IntegrityPrefix(p.Pool)) {
			return res, refuse("отображение %s уже существует", m.Name)
		}
	}
	mp := e.mountPoint(p.Pool)
	if src, ok := e.mountedAt(mp); ok {
		return res, refuse("%s уже смонтирован (%s)", mp, src)
	}

	// Проверка всех дисков непосредственно перед первой записью.
	knames := map[string]string{}
	for _, d := range p.Disks {
		k, err := e.CheckFreeDisk(ctx, d)
		if err != nil {
			return res, err
		}
		knames[d.ID] = k
	}

	// Откат: каждый созданный объект кладёт отмену в стек; при сбое
	// стек выполняется в обратном порядке, ошибки отката не останавливают
	// остальные шаги. Контекст отката не зависит от отмены задачи.
	var undo []func(context.Context) error
	fail := func(err error) (Result, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), longTimeout)
		defer cancel()
		var rbErrs []string
		for i := len(undo) - 1; i >= 0; i-- {
			if uerr := undo[i](rctx); uerr != nil {
				rbErrs = append(rbErrs, uerr.Error())
			}
		}
		res.Commands = j.cmds
		if len(rbErrs) > 0 {
			return res, fmt.Errorf("создание пула %s прервано; откат неполный (%s), см. runbook: %w", p.Pool, strings.Join(rbErrs, "; "), err)
		}
		return res, fmt.Errorf("создание пула %s прервано, частично созданные объекты удалены: %w", p.Pool, err)
	}
	rbRun := func(path string, args ...string) func(context.Context) error {
		return func(c context.Context) error {
			_, err := e.run(c, j, shortTimeout, path, args...)
			return err
		}
	}

	// Разделы.
	parts := map[string]map[int]string{} // диск → номер раздела → /dev
	for _, d := range p.Disks {
		var idx []int
		var slices []int64
		for i, a := range p.Arrays {
			for _, m := range a.Members {
				if m == d.ID {
					idx = append(idx, i)
					slices = append(slices, a.SliceBytes)
				}
			}
		}
		disk := filepath.Join(e.Paths.Dev, knames[d.ID])
		// Отмена регистрируется до разметки: сбой посреди partition тоже
		// оставляет таблицу. Разделы стираются по фактическому sysfs.
		kname := knames[d.ID]
		undo = append(undo, func(c context.Context) error {
			var errs []string
			ents, _ := os.ReadDir(filepath.Join(e.Paths.Disks.SysClassBlock, kname))
			for _, pt := range ents {
				if _, err := os.Stat(filepath.Join(e.Paths.Disks.SysClassBlock, kname, pt.Name(), "partition")); err != nil {
					continue
				}
				if _, err := e.run(c, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", filepath.Join(e.Paths.Dev, pt.Name())); err != nil {
					errs = append(errs, err.Error())
				}
			}
			for _, args := range [][]string{{e.Tools.Sgdisk, "--zap-all", disk}, {e.Tools.Wipefs, "-q", "-a", disk}} {
				if _, err := e.run(c, j, shortTimeout, args[0], args[1:]...); err != nil {
					errs = append(errs, err.Error())
				}
			}
			e.settle(c, j)
			if len(errs) > 0 {
				return errors.New(strings.Join(errs, "; "))
			}
			return nil
		})
		nodes, err := e.partition(ctx, j, kname, layout(p.Pool, idx, slices))
		if err != nil {
			return fail(err)
		}
		parts[d.ID] = nodes
	}

	// dm-integrity, md, физические тома.
	var pvs []string
	for i, a := range p.Arrays {
		var members []string
		for _, id := range a.Members {
			dev := parts[id][i+1]
			if p.Integrity {
				name := IntegrityName(p.Pool, i, id)
				if dev, err = e.integrity(ctx, j, dev, name, p.IntegrityMode); err != nil {
					return fail(err)
				}
				undo = append(undo, func(c context.Context) error {
					var err error
					for try := 0; try < 5; try++ {
						e.settle(c, j)
						if _, err = e.run(c, j, shortTimeout, e.Tools.Integritysetup, "close", name); err == nil {
							return nil
						}
					}
					return err
				})
			}
			members = append(members, dev)
		}
		if a.Level == LevelSingle {
			pvs = append(pvs, members[0])
			continue
		}
		md := filepath.Join(e.Paths.Dev, "md", MDName(p.Pool, i))
		args := []string{"--create", md, "--run", "--level=" + strings.TrimPrefix(a.Level, "raid"),
			"--raid-devices=" + strconv.Itoa(len(members)), "--metadata=1.2", "--bitmap=" + mdBitmap(a.Level),
			"--name=" + MDName(p.Pool, i)}
		mdMembers := append([]string{}, members...)
		undo = append(undo, func(c context.Context) error {
			var errs []string
			// Ссылки udev может не быть — тогда массив ищется по
			// имени в суперблоке и останавливается по узлу ядра /dev/mdN.
			node := md
			if _, err := os.Stat(md); err != nil {
				arrs, aerr := e.arrays(c, p.Pool)
				if aerr != nil {
					errs = append(errs, fmt.Sprintf("массив %s без ссылки: %v", MDName(p.Pool, i), aerr))
				}
				for _, a := range arrs {
					if a.index == i {
						node = a.node
					}
				}
			}
			// --stop падает, если массив не создан, — это не ошибка отката.
			_, _ = e.run(c, j, shortTimeout, e.Tools.Mdadm, "--stop", node)
			e.settle(c, j)
			for _, m := range mdMembers {
				if _, err := e.run(c, j, shortTimeout, e.Tools.Mdadm, "--zero-superblock", m); err != nil {
					errs = append(errs, err.Error())
				}
			}
			if len(errs) > 0 {
				return errors.New(strings.Join(errs, "; "))
			}
			return nil
		})
		if _, err := e.run(ctx, j, longTimeout, e.Tools.Mdadm, append(args, members...)...); err != nil {
			return fail(err)
		}
		e.settle(ctx, j)
		if err := e.waitFor(ctx, md, func() bool { _, err := os.Stat(md); return err == nil }); err != nil {
			return fail(fmt.Errorf("%s не появился: %w", md, err))
		}
		pvs = append(pvs, md)
	}
	vg := VGName(p.Pool)
	for _, pv := range pvs {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "pvcreate", "-q", "-y", pv); err != nil {
			return fail(err)
		}
		undo = append(undo, rbRun(e.Tools.LVM, "pvremove", "-q", "-y", "-ff", pv))
	}
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, append([]string{"vgcreate", "-q", vg}, pvs...)...); err != nil {
		return fail(err)
	}
	undo = append(undo, rbRun(e.Tools.LVM, "vgremove", "-q", "-f", "-y", vg))
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "lvcreate", "-q", "-y", "-l", "100%FREE", "-n", LVName, vg); err != nil {
		return fail(err)
	}
	lv := filepath.Join(e.Paths.Dev, vg, LVName)
	if err := e.waitFor(ctx, lv, func() bool { _, err := os.Stat(lv); return err == nil }); err != nil {
		return fail(fmt.Errorf("%s не появился: %w", lv, err))
	}
	// Метаданные Btrfs: DUP без integrity (самоисправление метаданных хотя бы
	// внутри ФС), single с integrity (исправляет нижний слой).
	meta := "dup"
	if p.Integrity {
		meta = "single"
	}
	if _, err := e.run(ctx, j, longTimeout, e.Tools.MkfsBtrfs, "-q", "-L", p.Pool, "-d", "single", "-m", meta, lv); err != nil {
		return fail(err)
	}
	j.add("mkdir", []string{"-p", mp})
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return fail(err)
	}
	undo = append(undo, func(context.Context) error {
		j.add("rmdir", []string{mp})
		_ = os.Remove(mp)
		return nil
	})
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mount, "-t", "btrfs", lv, mp); err != nil {
		return fail(err)
	}
	res.Mount = mp
	res.Commands = j.cmds
	return res, nil
}

// mdBitmap — write-intent bitmap md: internal для уровней с
// избыточностью — после некорректного выключения или временного выпадения
// участника синхронизируются только отмеченные области, а не весь массив.
// raid0 bitmap не поддерживает. Размер блока bitmap — по умолчанию mdadm
// (64 МиБ или больше, чтобы поместиться в суперблок).
func mdBitmap(level string) string {
	if level == LevelRAID0 {
		return "none"
	}
	return "internal"
}

// layout6 — раскладка raid6 «*-6» (md: left/right-asymmetric-6,
// left/right-symmetric-6, parity-first-6 = 16…20): Q на одном диске, как
// после перехода raid5 → raid6 с --layout=preserve.
func layout6(alg int) bool { return alg >= 16 && alg <= 20 }

// growCmds — команды mdadm --grow, переводящие массив ms (новый участник
// уже добавлен --add) к уровню и числу участников want.
//   - тот же уровень raid1/5/6: --raid-devices (raid6 с раскладкой *-6 —
//     с --layout=preserve, иначе mdadm отказывает: «non-standard layout»);
//   - raid1 из 2 → raid5: --level=5, затем --raid-devices;
//   - raid5 → raid6: одной командой --level=6
//     --raid-devices=N+1 --layout=preserve — Q целиком на новом диске, данные
//     не перемещаются, идёт только recovery нового диска. Без preserve mdadm
//     запускает reshape раскладки со сдвигом data offset; его прерывание
//     (mdadm 4.4, ядро 6.12.115) портит данные между записанной и
//     фактической позицией reshape (~37 МиБ на стенде, check md не видит) —
//     Раскладку не нормализуем по той же причине.
func growCmds(md string, ms MdstatArray, want ExpandArray) ([][]string, error) {
	n := "--raid-devices=" + strconv.Itoa(want.RaidDevices)
	switch {
	case ms.Level == want.Level && (ms.Level == LevelRAID1 || ms.Level == LevelRAID5 || ms.Level == LevelRAID6):
		if ms.Level == LevelRAID6 && layout6(ms.Algorithm) {
			return [][]string{{"--grow", md, n, "--layout=preserve"}}, nil
		}
		return [][]string{{"--grow", md, n}}, nil
	case ms.Level == LevelRAID1 && want.Level == LevelRAID5 && ms.RaidDevices == 2:
		return [][]string{{"--grow", md, "--level=5"}, {"--grow", md, n}}, nil
	case ms.Level == LevelRAID5 && want.Level == LevelRAID6 && ms.RaidDevices >= 3 && want.RaidDevices == ms.RaidDevices+1:
		return [][]string{{"--grow", md, "--level=6", n, "--layout=preserve"}}, nil
	}
	return nil, fmt.Errorf("переход %s (%d) → %s (%d) не поддерживается", ms.Level, ms.RaidDevices, want.Level, want.RaidDevices)
}

// mdState — состояние массива по /proc/mdstat.
func (e *Executor) mdstat() (map[string]MdstatArray, error) {
	b, err := os.ReadFile(e.Paths.Mdstat)
	if err != nil {
		return nil, err
	}
	out := map[string]MdstatArray{}
	for _, a := range ParseMdstat(string(b)) {
		out[a.Device] = a
	}
	return out, nil
}

// AddDisk добавляет диск в массивы пула и запускает reshape.
func (e *Executor) AddDisk(ctx context.Context, p AddDiskParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Pool: p.Pool}
	if !e.allowed(p.Disk.ID) {
		return res, refuse("диск %s не входит в destructive_allow_ids", p.Disk.ID)
	}
	if err := e.checkPoolAllowed(ctx, p.Pool); err != nil {
		return res, err
	}
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return res, refuse("массивы пула: %v", err)
	}
	st, err := e.mdstat()
	if err != nil {
		return res, refuse("/proc/mdstat: %v", err)
	}
	type step struct {
		arr  poolArray
		spec ExpandArray
		grow [][]string // mdadm --grow после --add
	}
	var steps []step
	for _, spec := range p.Arrays {
		a, ok := e.arrayByIndex(arrs, spec.Index)
		if !ok {
			return res, refuse("массив %d пула %s не найден", spec.Index, p.Pool)
		}
		ms, ok := st[a.kname]
		if !ok || !ms.Active {
			return res, refuse("массив %s не активен", a.name)
		}
		if ms.Action != "" || len(ms.Faulty) > 0 || ms.InSync != ms.RaidDevices || len(ms.Spares) > 0 {
			return res, refuse("массив %s не в исправном покое: %s %s, отказавших %d, резервных %d",
				a.name, ms.Status, ms.Action, len(ms.Faulty), len(ms.Spares))
		}
		if spec.RaidDevices != ms.RaidDevices+1 {
			return res, refuse("массив %s: ожидалось raid_devices %d, план %d", a.name, ms.RaidDevices+1, spec.RaidDevices)
		}
		grow, err := growCmds(a.node, ms, spec)
		if err != nil {
			return res, refuse("массив %s: %v", a.name, err)
		}
		integ, err := e.isIntegrityArray(a.kname)
		if err != nil {
			return res, refuse("%v", err)
		}
		if integ != p.Integrity {
			return res, refuse("массив %s: integrity=%v, в плане %v", a.name, integ, p.Integrity)
		}
		steps = append(steps, step{arr: a, spec: spec, grow: grow})
	}

	// Новые слои: проверки прежних дисков — до любой записи.
	tails, err := e.checkNewArrays(ctx, p, arrs)
	if err != nil {
		return res, err
	}

	fail := func(err error) (Result, error) {
		res.Commands = j.cmds
		return res, fmt.Errorf("добавление диска в пул %s прервано: %w", p.Pool, err)
	}
	// Разделы нового диска: расширяемые массивы, затем новые слои.
	var idx []int
	var slices []int64
	for _, s := range steps {
		idx = append(idx, s.spec.Index)
		slices = append(slices, s.spec.SliceBytes)
	}
	for _, na := range p.NewArrays {
		idx = append(idx, na.Index)
		slices = append(slices, na.SliceBytes)
	}
	var nodes map[int]string
	var growTails []tailPart
	if p.Grow {
		// Рост после замены: диск уже в пуле — его новые
		// разделы создаются хвостами; повтор после сброса переиспользует их.
		if growTails, err = e.growParts(ctx, p, idx, slices); err != nil {
			return res, err
		}
		nodes = map[int]string{}
	} else if p.Resume {
		if nodes, err = e.resumeParts(p.Disk, idx, slices); err != nil {
			return res, err
		}
	} else {
		k, err := e.CheckFreeDisk(ctx, p.Disk)
		if err != nil {
			return res, err
		}
		if nodes, err = e.partition(ctx, j, k, layout(p.Pool, idx, slices)); err != nil {
			return fail(err)
		}
	}
	for _, t := range growTails {
		node, err := e.makeTail(ctx, j, t)
		if err != nil {
			return fail(err)
		}
		nodes[t.num] = node
	}
	for _, s := range steps {
		dev := nodes[s.spec.Index+1]
		if p.Integrity {
			if dev, err = e.integrity(ctx, j, dev, IntegrityName(p.Pool, s.spec.Index, p.Disk.ID), p.IntegrityMode); err != nil {
				return fail(err)
			}
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, s.arr.node, "--add", dev); err != nil {
			return fail(err)
		}
		for _, args := range s.grow {
			if _, err := e.run(ctx, j, longTimeout, e.Tools.Mdadm, args...); err != nil {
				return fail(err)
			}
		}
	}
	if err := e.createNewArrays(ctx, j, p, nodes, tails); err != nil {
		return fail(err)
	}
	res.Commands = j.cmds
	return res, nil
}

// Grow растягивает PV, LV и Btrfs до размера массивов. Только рост.
func (e *Executor) Grow(ctx context.Context, p PoolParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Pool: p.Pool}
	if err := e.checkPoolAllowed(ctx, p.Pool); err != nil {
		return res, err
	}
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return res, refuse("массивы пула: %v", err)
	}
	st, err := e.mdstat()
	if err != nil {
		return res, refuse("/proc/mdstat: %v", err)
	}
	for _, a := range arrs {
		if ms := st[a.kname]; ms.Action == "reshape" {
			return res, refuse("массив %s ещё перестраивается (%s)", a.name, ms.Action)
		}
	}
	vg := VGName(p.Pool)
	pvs, err := e.pvList(ctx, vg)
	if err != nil {
		return res, err
	}
	for _, pv := range pvs {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "pvresize", "-q", pv); err != nil {
			res.Commands = j.cmds
			return res, err
		}
	}
	vgs, err := e.vgList(ctx)
	if err != nil {
		return res, err
	}
	free := int64(0)
	for _, v := range vgs {
		if v["vg_name"] == vg {
			free, _ = strconv.ParseInt(v["vg_free"], 10, 64)
		}
	}
	if free > 0 {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "lvextend", "-q", "-l", "+100%FREE", vg+"/"+LVName); err != nil {
			res.Commands = j.cmds
			return res, err
		}
	}
	mp := e.mountPoint(p.Pool)
	if _, ok := e.mountedAt(mp); ok {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Btrfs, "filesystem", "resize", "max", mp); err != nil {
			res.Commands = j.cmds
			return res, err
		}
		res.Mount = mp
	}
	res.Commands = j.cmds
	return res, nil
}

// closeStaleIntegrity закрывает отображения dm-integrity пула, которые не
// используются (нет holders): остались от отказавшего диска.
func (e *Executor) closeStaleIntegrity(ctx context.Context, j *journal, pool string) error {
	maps, err := e.integrityMaps(ctx)
	if err != nil {
		return err
	}
	for _, m := range maps {
		if !strings.HasPrefix(m.Name, IntegrityPrefix(pool)) {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Join(e.Paths.Dev, "mapper", m.Name))
		if err != nil {
			continue
		}
		h, err := os.ReadDir(filepath.Join(e.Paths.Disks.SysClassBlock, filepath.Base(real), "holders"))
		if err != nil || len(h) > 0 {
			continue
		}
		var lastErr error
		for try := 0; try < 5; try++ {
			e.settle(ctx, j)
			if _, lastErr = e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, "close", m.Name); lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			return fmt.Errorf("не закрыто отображение %s: %w", m.Name, lastErr)
		}
	}
	return nil
}

// resumeParts — разделы диска p.Disk для дозавершения добавления:
// диск из allowlist, размер как в плане, раздел массива i — номер i+1 с
// меткой пула и размером среза, без держателей (не в md и не открыт
// integrity). Иначе — отказ до любой записи.
func (e *Executor) resumeParts(disk DiskRef, idx []int, slices []int64) (map[int]string, error) {
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return nil, refuse("снимок дисков: %v", err)
	}
	var k string
	for _, d := range snap.Disks {
		if d.ID == disk.ID {
			if k != "" {
				return nil, refuse("диск %s найден дважды", disk.ID)
			}
			if d.SizeBytes == nil || *d.SizeBytes != disk.SizeBytes {
				return nil, refuse("диск %s: размер не совпадает с планом", disk.ID)
			}
			k = d.KernelName
		}
	}
	if k == "" || !knameRe.MatchString(k) {
		return nil, refuse("диск %s не найден", disk.ID)
	}
	nodes := map[int]string{}
	for n, i := range idx {
		part := e.partKName(k, i+1)
		if part == "" {
			return nil, refuse("диск %s: нет раздела %d для массива %d", disk.ID, i+1, i)
		}
		base := filepath.Join(e.Paths.Disks.SysClassBlock, k, part)
		if hs, _ := os.ReadDir(filepath.Join(base, "holders")); len(hs) > 0 {
			return nil, refuse("раздел %s занят", part)
		}
		sec, err := e.readInt(filepath.Join(base, "size"))
		if err != nil || sec*512 != slices[n] {
			return nil, refuse("раздел %s: размер %d×512 не совпадает со срезом %d", part, sec, slices[n])
		}
		nodes[i+1] = filepath.Join(e.Paths.Dev, part)
	}
	return nodes, nil
}
