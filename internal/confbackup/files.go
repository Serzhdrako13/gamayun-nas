package confbackup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/storage/home"
)

// Операции gamayun-helper (root): копии в пуле и на USB-носителе.
const (
	OpSave    = "config.backup.save"
	OpList    = "config.backup.list"
	OpRead    = "config.backup.read"
	OpUSBList = "config.backup.usb.list"
	OpUSBSave = "config.backup.usb.save"
)

// Keep — сколько копий хранить в пуле.
const Keep = 30

// PoolDir — каталог копий относительно точки монтирования пула.
const PoolDir = ".gamayun/config-backup"

// USBDir — каталог копий на USB-носителе.
const USBDir = "gamayun-nas-config"

var (
	nameRe   = regexp.MustCompile(`^gamayun-config-[0-9]{8}-[0-9]{6}(-[a-z0-9-]{1,40})?\.json\.gz$`)
	poolRe   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	deviceRe = regexp.MustCompile(`^sd[a-z]{1,3}[0-9]{0,3}$`)
)

// FileName — имя файла копии: gamayun-config-ГГГГММДД-ЧЧММСС[-имя].json.gz (UTC).
func FileName(t time.Time, host string) string {
	n := "gamayun-config-" + t.UTC().Format("20060102-150405")
	if host != "" && regexp.MustCompile(`^[a-z0-9-]{1,40}$`).MatchString(host) {
		n += "-" + host
	}
	return n + ".json.gz"
}

// Ошибки исполнителя: ErrRefused — пул не смонтирован, носитель не USB,
// ссылка вместо каталога; ErrNotFound — нет файла; ErrExists — имя занято.
// Это ошибки nas/home — helper переводит их в коды refused/not_found/conflict.
var (
	ErrRefused  = home.ErrRefused
	ErrNotFound = nas.ErrNotFound
	ErrExists   = fmt.Errorf("%w: файл уже есть", nas.ErrConflict)
)

// SaveParams — config.backup.save.
type SaveParams struct {
	Pool string `json:"pool"`
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// Validate проверяет параметры.
func (p SaveParams) Validate() error {
	if !poolRe.MatchString(p.Pool) {
		return errors.New("недопустимое имя пула")
	}
	return validFile(p.Name, p.Data)
}

func validFile(name string, data []byte) error {
	if !nameRe.MatchString(name) {
		return errors.New("недопустимое имя файла копии")
	}
	if len(data) == 0 || len(data) > MaxSize {
		return fmt.Errorf("размер копии 1..%d байт", MaxSize)
	}
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return errors.New("данные не в gzip")
	}
	return nil
}

// PoolParams — config.backup.list.
type PoolParams struct {
	Pool string `json:"pool"`
}

// Validate проверяет параметры.
func (p PoolParams) Validate() error {
	if !poolRe.MatchString(p.Pool) {
		return errors.New("недопустимое имя пула")
	}
	return nil
}

// ReadParams — config.backup.read.
type ReadParams struct {
	Pool string `json:"pool"`
	Name string `json:"name"`
}

// Validate проверяет параметры.
func (p ReadParams) Validate() error {
	if !poolRe.MatchString(p.Pool) {
		return errors.New("недопустимое имя пула")
	}
	if !nameRe.MatchString(p.Name) {
		return errors.New("недопустимое имя файла копии")
	}
	return nil
}

// USBSaveParams — config.backup.usb.save.
type USBSaveParams struct {
	Device string `json:"device"` // имя ядра раздела или диска: sdb1
	Name   string `json:"name"`
	Data   []byte `json:"data"`
}

// Validate проверяет параметры.
func (p USBSaveParams) Validate() error {
	if !deviceRe.MatchString(p.Device) {
		return errors.New("недопустимое имя устройства")
	}
	return validFile(p.Name, p.Data)
}

// FileInfo — копия в хранилище.
type FileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// SaveResult — итог сохранения.
type SaveResult struct {
	Path    string   `json:"path"`
	Removed []string `json:"removed,omitempty"` // старые копии сверх Keep
}

// ReadResult — содержимое копии.
type ReadResult struct {
	Data []byte `json:"data"`
}

// USBDevice — носитель для выгрузки.
type USBDevice struct {
	Device string `json:"device"`
	FSType string `json:"fstype"`
	Label  string `json:"label,omitempty"`
	Size   int64  `json:"size"`
	Model  string `json:"model,omitempty"`
}

// CmdResult, Runner — запуск mount/umount/blkid.
type CmdResult struct {
	ExitCode       int
	Stdout, Stderr []byte
}

// Runner — запуск внешней программы.
type Runner interface {
	Run(ctx context.Context, timeout time.Duration, path string, args ...string) (CmdResult, error)
}

// Executor — операции helper.
type Executor struct {
	MountRoot string
	// Mounted — смонтирована ли ФС в каталоге (nil — сравнение устройств).
	Mounted func(dir string) bool
	Runner  Runner
	// Sys, Dev, RunDir, MountInfo — пути (пусто — /sys, /dev, /run, /proc/self/mountinfo).
	Sys, Dev, RunDir, MountInfo string
	// Mount, Umount, Blkid — программы (пусто — пути Debian).
	Mount, Umount, Blkid string
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (e *Executor) poolDir(pool string) (string, error) {
	mp := filepath.Join(e.MountRoot, pool)
	mounted := e.Mounted
	if mounted == nil {
		mounted = isMountPoint
	}
	if !mounted(mp) {
		return "", fmt.Errorf("%w: пул %s не смонтирован", ErrRefused, pool)
	}
	return filepath.Join(mp, PoolDir), nil
}

// ensureDir создаёт цепочку каталогов root 0700 и проверяет, что ни один
// компонент под base не ссылка.
func ensureDir(base, rel string) (string, error) {
	cur := base
	for _, c := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0o700); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s — не каталог", ErrRefused, cur)
		}
	}
	return cur, nil
}

// writeAtomic — временный файл, fsync, rename без перезаписи, fsync каталога.
func writeAtomic(dir, name string, data []byte) (string, error) {
	final := filepath.Join(dir, name)
	if _, err := os.Lstat(final); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, name)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%w: %s", ErrExists, name)
		}
		// ФС без жёстких ссылок (vfat/exfat): проверка выше + rename.
		if err := os.Rename(tmp, final); err != nil {
			return "", err
		}
	} else {
		os.Remove(tmp)
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return final, nil
}

func list(dir string) ([]FileInfo, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []FileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []FileInfo{}
	for _, e := range ents {
		if !e.Type().IsRegular() || !nameRe.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{Name: e.Name(), Size: fi.Size()})
	}
	// Имя начинается с времени UTC — сортировка по имени = по времени.
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Save сохраняет копию в пул и удаляет старые сверх Keep.
func (e *Executor) Save(_ context.Context, p SaveParams) (SaveResult, error) {
	if _, err := e.poolDir(p.Pool); err != nil {
		return SaveResult{}, err
	}
	dir, err := ensureDir(filepath.Join(e.MountRoot, p.Pool), PoolDir)
	if err != nil {
		return SaveResult{}, err
	}
	path, err := writeAtomic(dir, p.Name, p.Data)
	if err != nil {
		return SaveResult{}, err
	}
	res := SaveResult{Path: path}
	files, err := list(dir)
	if err != nil {
		return res, err
	}
	for i := Keep; i < len(files); i++ {
		if os.Remove(filepath.Join(dir, files[i].Name)) == nil {
			res.Removed = append(res.Removed, files[i].Name)
		}
	}
	return res, nil
}

// List — копии в пуле, новые первыми.
func (e *Executor) List(_ context.Context, p PoolParams) ([]FileInfo, error) {
	base, err := e.poolDir(p.Pool)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(base); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
		return nil, fmt.Errorf("%w: %s — не каталог", ErrRefused, base)
	}
	return list(base)
}

// Read читает копию из пула (не по ссылке).
func (e *Executor) Read(_ context.Context, p ReadParams) (ReadResult, error) {
	base, err := e.poolDir(p.Pool)
	if err != nil {
		return ReadResult{}, err
	}
	fi, err := os.Lstat(filepath.Join(base, p.Name))
	if errors.Is(err, os.ErrNotExist) {
		return ReadResult{}, fmt.Errorf("%w: %s", ErrNotFound, p.Name)
	}
	if err != nil {
		return ReadResult{}, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > MaxSize {
		return ReadResult{}, fmt.Errorf("%w: %s — не файл копии", ErrRefused, p.Name)
	}
	b, err := os.ReadFile(filepath.Join(base, p.Name))
	return ReadResult{Data: b}, err
}

// ---------- USB ----------

// usbFS — файловые системы носителя, на которые пишем.
var usbFS = map[string]bool{"vfat": true, "exfat": true, "ext4": true}

// isUSB — блочное устройство (диск или раздел) подключено через USB.
func (e *Executor) isUSB(dev string) (disk string, ok bool) {
	sys := or(e.Sys, "/sys")
	link, err := filepath.EvalSymlinks(filepath.Join(sys, "class", "block", dev))
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(link, "partition")); err == nil {
		disk = filepath.Base(filepath.Dir(link))
	} else {
		disk = dev
	}
	return disk, strings.Contains(link, "/usb")
}

// mountedSources — устройства, смонтированные где-либо.
func (e *Executor) mountedSources() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(or(e.MountInfo, "/proc/self/mountinfo"))
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		i := strings.Index(l, " - ")
		if i < 0 {
			continue
		}
		f := strings.Fields(l[i+3:])
		if len(f) >= 2 {
			out[filepath.Base(f[1])] = true
		}
	}
	return out
}

func (e *Executor) blkid(ctx context.Context, dev string) (fstype, label string) {
	r, err := e.Runner.Run(ctx, 10*time.Second, or(e.Blkid, "/usr/sbin/blkid"), "-o", "export", "-p", "--", filepath.Join(or(e.Dev, "/dev"), dev))
	if err != nil || r.ExitCode != 0 {
		return "", ""
	}
	for _, l := range strings.Split(string(r.Stdout), "\n") {
		if v, ok := strings.CutPrefix(l, "TYPE="); ok {
			fstype = v
		}
		if v, ok := strings.CutPrefix(l, "LABEL="); ok {
			label = v
		}
	}
	return fstype, label
}

func readInt(path string) int64 {
	b, _ := os.ReadFile(path)
	var n int64
	fmt.Sscan(string(bytes.TrimSpace(b)), &n)
	return n
}

// USBList — разделы USB-носителей с поддерживаемой ФС, нигде не смонтированные.
func (e *Executor) USBList(ctx context.Context) ([]USBDevice, error) {
	sys := or(e.Sys, "/sys")
	ents, err := os.ReadDir(filepath.Join(sys, "class", "block"))
	if err != nil {
		return nil, err
	}
	busy := e.mountedSources()
	out := []USBDevice{}
	for _, en := range ents {
		dev := en.Name()
		if !deviceRe.MatchString(dev) || busy[dev] {
			continue
		}
		disk, ok := e.isUSB(dev)
		if !ok {
			continue
		}
		fstype, label := e.blkid(ctx, dev)
		if !usbFS[fstype] {
			continue
		}
		model, _ := os.ReadFile(filepath.Join(sys, "class", "block", disk, "device", "model"))
		out = append(out, USBDevice{Device: dev, FSType: fstype, Label: label,
			Size: readInt(filepath.Join(sys, "class", "block", dev, "size")) * 512, Model: strings.TrimSpace(string(model))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

// USBSave монтирует раздел USB во временный каталог (nosuid,nodev,noexec),
// пишет копию в gamayun-nas-config/, сбрасывает и отмонтирует.
func (e *Executor) USBSave(ctx context.Context, p USBSaveParams) (SaveResult, error) {
	if _, ok := e.isUSB(p.Device); !ok {
		return SaveResult{}, fmt.Errorf("%w: %s — не USB-носитель", ErrRefused, p.Device)
	}
	if e.mountedSources()[p.Device] {
		return SaveResult{}, fmt.Errorf("%w: %s уже смонтирован", ErrRefused, p.Device)
	}
	fstype, _ := e.blkid(ctx, p.Device)
	if !usbFS[fstype] {
		return SaveResult{}, fmt.Errorf("%w: файловая система %q не поддерживается (нужна vfat, exfat или ext4)", ErrRefused, fstype)
	}
	mnt, err := os.MkdirTemp(or(e.RunDir, "/run"), "gamayun-usb-")
	if err != nil {
		return SaveResult{}, err
	}
	defer os.Remove(mnt)
	r, err := e.Runner.Run(ctx, time.Minute, or(e.Mount, "/usr/bin/mount"), "-t", fstype, "-o", "nosuid,nodev,noexec",
		"--", filepath.Join(or(e.Dev, "/dev"), p.Device), mnt)
	if err != nil {
		return SaveResult{}, err
	}
	if r.ExitCode != 0 {
		return SaveResult{}, fmt.Errorf("mount: код %d: %s", r.ExitCode, strings.TrimSpace(string(r.Stderr)))
	}
	res, werr := func() (SaveResult, error) {
		dir, err := ensureDir(mnt, USBDir)
		if err != nil {
			return SaveResult{}, err
		}
		path, err := writeAtomic(dir, p.Name, p.Data)
		return SaveResult{Path: filepath.Join(p.Device+":", USBDir, filepath.Base(path))}, err
	}()
	u, uerr := e.Runner.Run(ctx, time.Minute, or(e.Umount, "/usr/bin/umount"), "--", mnt)
	if werr != nil {
		return SaveResult{}, werr
	}
	if uerr != nil || u.ExitCode != 0 {
		return SaveResult{}, fmt.Errorf("копия записана, но носитель не отмонтирован (%v %s) — не извлекайте его до перезагрузки", uerr, strings.TrimSpace(string(u.Stderr)))
	}
	return res, nil
}

func isMountPoint(dir string) bool {
	var a, b syscall.Stat_t
	if syscall.Lstat(dir, &a) != nil || a.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return false
	}
	if syscall.Stat(filepath.Dir(dir), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}
