// Package apps — подсистема приложений Docker: операции
// gamayun-helper (root) и сервис gamayund (без root).
//
// gamayund к сокету Docker не подключается: доступ к docker.sock равен root.
// Всё привилегированное — фиксированные операции helper apps.* с проверкой
// параметров; события Docker — долгим опросом apps.events.
//
// Раскладка на пуле приложений:
//
//	<пул>/.apps/             субтом
//	<пул>/.apps/docker/      субтом, data-root dockerd
//	<пул>/.apps/containerd/  субтом, root containerd (слои образов Docker 29)
//	<пул>/.apps/appdata/<id> субтом приложения; каталоги БД — без CoW (chattr +C)
//
// Docker-стек — из пакета gamayun-nas-apps, версии закреплены (versions.lock,
// не из репозитория docker.com). Юниты
// gamayun-docker.service и gamayun-containerd.service стартуют только при флаге
// /run/gamayun-nas-apps/start и смонтированном пуле (drop-in gamayun-pool.conf:
// RequiresMountsFor + ConditionPathIsMountPoint); apps.start сверяет версию
// Docker с versions.lock.
// Флаг ставит apps.start по команде gamayund после проверки пула; /run
// очищается при загрузке — после перезагрузки без gamayund Docker не стартует.
package apps

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/storage/home"
)

// Операции gamayun-helper.
const (
	OpStatus        = "apps.status"
	OpConfigure     = "apps.configure"
	OpUnconfigure   = "apps.unconfigure"
	OpStart         = "apps.start"
	OpStop          = "apps.stop"
	OpRemount       = "apps.pool_remount"
	OpPoolBusy      = "apps.pool_busy"
	OpAppdataCreate = "apps.appdata.create"
	OpAppdataDelete = "apps.appdata.delete"
	OpEvents        = "apps.events"
)

// Каталоги на пуле.
const (
	AppsDir    = ".apps"
	DockerDir  = "docker"
	CtrDir     = "containerd"
	AppdataDir = "appdata"
)

// Юниты пакета gamayun-nas-apps (Docker-стек закреплённых версий, versions.lock).
const (
	DockerUnit = "gamayun-docker.service"
	CtrUnit    = "gamayun-containerd.service"
)

// UnitMarker — метка файлов, которые пишет helper.
const UnitMarker = "# gamayun-nas-apps"

// Пределы.
const (
	MaxEvents    = 500
	MaxWait      = 60 // с, долгий опрос событий
	StartTimeout = 90 * time.Second
	StopTimeout  = 3 * time.Minute
)

// Ошибки — как у остальных операций helper (коды refused/not_found/conflict/invalid_params).
var (
	ErrRefused  = home.ErrRefused
	ErrConflict = nas.ErrConflict
	ErrInvalid  = nas.ErrInvalid
)

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

var (
	poolRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	idRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	dirRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ValidPool — имя пула (как home.ValidPoolName).
func ValidPool(s string) bool { return poolRe.MatchString(s) }

// ValidID — идентификатор приложения.
func ValidID(s string) bool { return idRe.MatchString(s) && !strings.HasSuffix(s, "-") }

// Paths — пути системы (в тестах — временный каталог).
type Paths struct {
	Etc        string // /etc
	Run        string // /run/gamayun-nas-apps
	VarLib     string // /var/lib (проверка: Docker не пишет на системный диск)
	Mountinfo  string // /proc/self/mountinfo
	Dev        string // /dev
	DockerSock string // /run/docker.sock
	Dockerd    string // /usr/lib/gamayun-nas-apps/bin/dockerd
	DockerCLI  string // docker CLI (compose — плагин); пусто — рядом с Dockerd
	UnitDir    string // /usr/lib/systemd/system (юниты пакета)
	Lock       string // versions.lock пакета: закреплённая версия Docker
	Systemctl  string
	Btrfs      string
	Mount      string
	Umount     string
	Journalctl string
	// Proc — /proc: остатки контейнеров и занятость пула после остановки;
	// пусто — проверка отключена.
	Proc string
	// Учётки приложений; пусто — пути по умолчанию.
	Passwd, Group                        string
	Useradd, Userdel, Groupadd, Groupdel string
}

// DefaultPaths — реальная система.
func DefaultPaths() Paths {
	return Paths{
		Etc: "/etc", Run: "/run/gamayun-nas-apps", VarLib: "/var/lib", Mountinfo: "/proc/self/mountinfo", Dev: "/dev",
		DockerSock: "/run/docker.sock", Dockerd: "/usr/lib/gamayun-nas-apps/bin/dockerd", UnitDir: "/usr/lib/systemd/system",
		Lock:      "/usr/share/doc/gamayun-nas-apps/versions.lock",
		Systemctl: "/usr/bin/systemctl", Btrfs: "/usr/bin/btrfs", Mount: "/usr/bin/mount", Umount: "/usr/bin/umount",
		Journalctl: "/usr/bin/journalctl", Proc: "/proc",
	}
}

func (p Paths) unit() string { return filepath.Join(p.UnitDir, DockerUnit) }
func (p Paths) dropin(u string) string {
	return filepath.Join(p.Etc, "systemd/system", u+".service.d/gamayun-pool.conf")
}
func (p Paths) daemonJSON() string { return filepath.Join(p.Etc, "gamayun-nas/apps/daemon.json") }
func (p Paths) ctrTOML() string    { return filepath.Join(p.Etc, "gamayun-nas/apps/containerd.toml") }
func (p Paths) state() string      { return filepath.Join(p.Etc, "gamayun-nas/apps.json") }
func (p Paths) backup() string     { return filepath.Join(p.Etc, "gamayun-nas/apps-backup") }
func (p Paths) flag() string       { return filepath.Join(p.Run, "start") }

// Executor — операции helper.
type Executor struct {
	Runner    nas.Runner
	Paths     Paths
	MountRoot string
	// ReadOnly — ФС смонтирована только для чтения (statfs ST_RDONLY: видит и
	// принудительный RO после ошибки Btrfs, которого нет в параметрах точки).
	ReadOnly func(path string) (bool, error)
	// SetNoCOW — chattr +C каталога; nil — ioctl FS_IOC_SETFLAGS.
	SetNoCOW func(path string) error
	// Subvol — корень субтома Btrfs; nil — по номеру inode (тесты подменяют).
	Subvol func(path string) (bool, error)
	// Docker — клиент Engine API; nil — по Paths.DockerSock.
	Docker *DockerClient
	// Kill — сигнал процессу; nil — syscall.Kill (тесты подменяют).
	Kill func(pid int, sig syscall.Signal) error
	mu   sync.Mutex
	// задания выдачи доступа к общим папкам
	jobsOnce sync.Once
	aclJobs  *aclJobs
}

func (e *Executor) docker() *DockerClient {
	if e.Docker != nil {
		return e.Docker
	}
	return NewDockerClient(e.Paths.DockerSock)
}

func (e *Executor) isSubvol(path string) (bool, error) {
	if e.Subvol != nil {
		return e.Subvol(path)
	}
	return isSubvolume(path)
}

func (e *Executor) mountPoint(pool string) string { return filepath.Join(e.MountRoot, pool) }

// appsRoot — <пул>/.apps.
func (e *Executor) appsRoot(pool string) string { return filepath.Join(e.mountPoint(pool), AppsDir) }

// NoParams — операции без параметров.
type NoParams struct{}

// Validate — без проверок (лишние поля отсекает строгий разбор helper).
func (NoParams) Validate() error { return nil }

// PoolParams — apps.configure, apps.start, apps.pool_remount.
type PoolParams struct {
	Pool string `json:"pool"`
}

// Validate проверяет имя пула.
func (p PoolParams) Validate() error {
	if !ValidPool(p.Pool) {
		return errors.New("недопустимое имя пула")
	}
	return nil
}

// UnconfigureParams — apps.unconfigure. PurgeCreated — удалить .apps, если
// его создала apps.configure (откат неудачного первого включения).
type UnconfigureParams struct {
	PurgeCreated bool `json:"purge_created"`
}

// Validate — без проверок.
func (UnconfigureParams) Validate() error { return nil }

// AppdataParams — apps.appdata.create/delete. NoCOWDirs — каталоги данных
// БД внутри субтома приложения: создаются пустыми с chattr +C (+45–75 %
// tps PostgreSQL; контрольные суммы даёт dm-integrity ниже).
type AppdataParams struct {
	ID        string   `json:"id"`
	NoCOWDirs []string `json:"nocow_dirs,omitempty"`
}

// Validate проверяет идентификатор и имена каталогов.
func (p AppdataParams) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("недопустимый идентификатор приложения")
	}
	if len(p.NoCOWDirs) > 8 {
		return errors.New("слишком много каталогов nocow_dirs")
	}
	for _, d := range p.NoCOWDirs {
		if !dirRe.MatchString(d) || d == "." || d == ".." {
			return fmt.Errorf("недопустимое имя каталога %q", d)
		}
	}
	return nil
}

// EventsParams — apps.events: события после SinceNano (unix, нс) с
// ожиданием до WaitS секунд.
type EventsParams struct {
	SinceNano int64 `json:"since_nano"`
	WaitS     int   `json:"wait_s"`
}

// Validate проверяет пределы.
func (p EventsParams) Validate() error {
	if p.SinceNano < 0 || p.WaitS < 0 || p.WaitS > MaxWait {
		return fmt.Errorf("since_nano ≥ 0, wait_s в [0, %d]", MaxWait)
	}
	return nil
}

// state — /etc/gamayun-nas/apps.json: на каком пуле настроен Docker.
type state struct {
	Pool string `json:"pool"`
	// Created — .apps создан apps.configure (не существовал до включения).
	Created bool `json:"created,omitempty"`
}

func (e *Executor) loadState() (state, error) {
	var s state
	b, err := os.ReadFile(e.Paths.state())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", e.Paths.state(), err)
	}
	if s.Pool != "" && !ValidPool(s.Pool) {
		return s, fmt.Errorf("%s: недопустимый пул %q", e.Paths.state(), s.Pool)
	}
	return s, nil
}

// Status — apps.status.
type Status struct {
	// Installed — пакет gamayun-nas-apps установлен (свой docker.service и dockerd).
	Installed bool `json:"installed"`
	// Pool — пул, на котором настроен Docker; пусто — не настроен.
	Pool string `json:"pool"`
	// AppsExists — на пуле есть .apps (данные приложений).
	AppsExists   bool   `json:"apps_exists"`
	PoolMounted  bool   `json:"pool_mounted"`
	PoolReadOnly bool   `json:"pool_read_only"`
	Docker       string `json:"docker"`     // systemctl is-active: active, inactive, failed, activating…
	Containerd   string `json:"containerd"` // то же
	StartFlag    bool   `json:"start_flag"`
	Version      string `json:"version,omitempty"`
	RootDir      string `json:"root_dir,omitempty"`
	Running      int    `json:"containers_running"`
	Total        int    `json:"containers_total"`
	// Leftover — процессы контейнеров и shim при остановленных службах
	// gamayund останавливает их, как работающий Docker.
	Leftover int `json:"leftover_processes,omitempty"`
	// MountRoot — каталог точек монтирования пулов (пути bind в compose).
	MountRoot string `json:"mount_root"`
}

// Status — состояние подсистемы; только чтение.
func (e *Executor) Status(ctx context.Context, _ NoParams) (Status, error) {
	var st Status
	st.MountRoot = e.MountRoot
	st.Installed = e.installed()
	s, err := e.loadState()
	if err != nil {
		return st, err
	}
	st.Pool = s.Pool
	if s.Pool != "" {
		st.PoolMounted, st.PoolReadOnly = e.poolMount(s.Pool)
		if st.PoolMounted {
			_, err := os.Stat(e.appsRoot(s.Pool))
			st.AppsExists = err == nil
		}
	}
	_, err = os.Stat(e.Paths.flag())
	st.StartFlag = err == nil
	st.Docker, st.Containerd = e.active(ctx)
	if st.Docker != "active" && st.Containerd != "active" {
		st.Leftover = len(e.leftovers())
	}
	if st.Docker == "active" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if info, err := e.docker().Info(cctx); err == nil {
			st.Version, st.RootDir, st.Running, st.Total = info.ServerVersion, info.DockerRootDir, info.ContainersRunning, info.Containers
		}
	}
	return st, nil
}

func (e *Executor) installed() bool {
	if _, err := os.Stat(e.Paths.unit()); err != nil {
		return false
	}
	_, err := os.Stat(e.Paths.Dockerd)
	return err == nil
}

// active — состояние docker и containerd (systemctl is-active).
func (e *Executor) active(ctx context.Context) (docker, ctr string) {
	r, err := e.Runner.Run(ctx, 15*time.Second, nil, e.Paths.Systemctl, "is-active", DockerUnit, CtrUnit)
	if err != nil {
		return "unknown", "unknown"
	}
	f := strings.Fields(string(r.Stdout))
	if len(f) != 2 {
		return "unknown", "unknown"
	}
	return f[0], f[1]
}

// mountEntry — точка mp из mountinfo: источник, тип, параметры точки и суперблока.
type mountEntry struct {
	src, fstype string
	opts        []string
}

func (e *Executor) mountEntry(mp string) (mountEntry, bool) {
	b, err := os.ReadFile(e.Paths.Mountinfo)
	if err != nil {
		return mountEntry{}, false
	}
	var m mountEntry
	found := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[4] != mp {
			continue
		}
		for i := 6; i < len(f)-3; i++ {
			if f[i] == "-" {
				m = mountEntry{src: f[i+2], fstype: f[i+1], opts: append(strings.Split(f[5], ","), strings.Split(f[i+3], ",")...)}
				found = true
				break
			}
		}
	}
	return m, found
}

// poolMount — пул смонтирован (Btrfs, том пула) и только ли для чтения.
func (e *Executor) poolMount(pool string) (mounted, ro bool) {
	mp := e.mountPoint(pool)
	m, ok := e.mountEntry(mp)
	if !ok || m.fstype != "btrfs" {
		return false, false
	}
	for _, o := range m.opts {
		if o == "ro" {
			ro = true
		}
	}
	if e.ReadOnly != nil {
		if r, err := e.ReadOnly(mp); err != nil || r {
			ro = true // неизвестно — не считать пригодным для записи
		}
	}
	return true, ro
}

// needPoolRW — пул смонтирован на запись, иначе отказ.
func (e *Executor) needPoolRW(pool string) error {
	mounted, ro := e.poolMount(pool)
	switch {
	case !mounted:
		return refuse("пул %s не смонтирован в %s", pool, e.mountPoint(pool))
	case ro:
		return refuse("пул %s смонтирован только для чтения", pool)
	}
	return nil
}

func (e *Executor) run(ctx context.Context, timeout time.Duration, path string, args ...string) (nas.CmdResult, error) {
	r, err := e.Runner.Run(ctx, timeout, nil, path, args...)
	if err != nil {
		return r, err
	}
	if r.ExitCode != 0 {
		return r, fmt.Errorf("%s %s: код %d: %s", filepath.Base(path), strings.Join(args, " "), r.ExitCode, firstLines(r.Stderr, 3))
	}
	return r, nil
}

func firstLines(b []byte, n int) string {
	ls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.Join(ls, " | ")
}

// isSubvolume — корень субтома Btrfs (номер inode 256).
func isSubvolume(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Ino == 256, nil
}

// ensureSubvol создаёт субтом path, если его нет; существующий — только субтом.
func (e *Executor) ensureSubvol(ctx context.Context, path string, mode os.FileMode) (created bool, err error) {
	sv, err := e.isSubvol(path)
	if err == nil {
		if !sv {
			return false, refuse("%s существует и не является субтомом Btrfs", path)
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := e.run(ctx, time.Minute, e.Paths.Btrfs, "subvolume", "create", path); err != nil {
		return false, err
	}
	return true, os.Chmod(path, mode)
}

// writeFile пишет атомарно (временный файл + rename). Режимы файла и
// созданного каталога задаются явно: у helper UMask=0077.
func writeFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".gamayun-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if os.Geteuid() == 0 {
		// Группа процесса helper — gamayun; файлы в /etc — root:root.
		if err := os.Chown(tmp, 0, 0); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// DaemonJSON — /etc/docker/daemon.json: data-root на пуле, журналы
// контейнеров драйвером local с ротацией.
func DaemonJSON(appsRoot string) []byte {
	b, _ := json.MarshalIndent(map[string]any{
		"data-root":  filepath.Join(appsRoot, DockerDir),
		"log-driver": "local",
		"log-opts":   map[string]string{"max-size": "10m", "max-file": "3"},
	}, "", "  ")
	return append(b, '\n')
}

// ContainerdTOML — конфиг containerd (version 3): root на пуле, state в /run.
func ContainerdTOML(appsRoot string) []byte {
	return []byte(fmt.Sprintf("%s: root на пуле приложений\nversion = 3\nroot = %q\nstate = \"/run/containerd\"\n\n[grpc]\n  address = \"/run/containerd/containerd.sock\"\n",
		UnitMarker, filepath.Join(appsRoot, CtrDir)))
}

// LockedDocker — закреплённая версия Docker из versions.lock (строка docker-static).
func LockedDocker(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "docker-static" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("%s: нет строки docker-static", path)
}

// DropIn — drop-in docker/containerd: не стартовать без смонтированного пула.
func DropIn(mp string) []byte {
	return []byte(fmt.Sprintf("%s: запуск только при смонтированном пуле приложений\n[Unit]\nRequiresMountsFor=%s\nConditionPathIsMountPoint=%s\n", UnitMarker, mp, mp))
}

// ConfigureResult — итог apps.configure.
type ConfigureResult struct {
	Pool     string   `json:"pool"`
	AppsRoot string   `json:"apps_root"`
	Created  []string `json:"created"`
}

// Configure настраивает Docker на пул: субтомы, конфиги, drop-in. Docker
// должен быть остановлен. Сбой — откат сделанного в этом вызове.
func (e *Executor) Configure(ctx context.Context, p PoolParams) (res ConfigureResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res = ConfigureResult{Pool: p.Pool, AppsRoot: e.appsRoot(p.Pool), Created: []string{}}
	if !e.installed() {
		return res, refuse("пакет gamayun-nas-apps не установлен")
	}
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	if s.Pool != "" && s.Pool != p.Pool {
		return res, fmt.Errorf("%w: приложения уже настроены на пул %s (перенос — отдельная операция)", ErrConflict, s.Pool)
	}
	if d, c := e.active(ctx); d != "inactive" && d != "failed" || c != "inactive" && c != "failed" {
		return res, refuse("docker/containerd не остановлены (%s/%s)", d, c)
	}
	if err := e.needPoolRW(p.Pool); err != nil {
		return res, err
	}
	var undo []func()
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
			res.Created = []string{}
		}
	}()
	root := res.AppsRoot
	created := s.Created
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{root, 0o711}, {filepath.Join(root, DockerDir), 0o710}, {filepath.Join(root, CtrDir), 0o711}, {filepath.Join(root, AppdataDir), 0o755}} {
		c, err := e.ensureSubvol(ctx, d.path, d.mode)
		if err != nil {
			return res, err
		}
		if c {
			path := d.path
			res.Created = append(res.Created, path)
			undo = append(undo, func() { e.run(context.Background(), time.Minute, e.Paths.Btrfs, "subvolume", "delete", path) })
			if path == root {
				created = true
			}
		}
	}
	toml := ContainerdTOML(root)
	mp := e.mountPoint(p.Pool)
	files := []struct {
		path string
		data []byte
	}{
		{e.Paths.daemonJSON(), DaemonJSON(root)},
		{e.Paths.ctrTOML(), toml},
		{e.Paths.dropin("gamayun-docker"), DropIn(mp)},
		{e.Paths.dropin("gamayun-containerd"), DropIn(mp)},
	}
	for _, f := range files {
		u, err := e.place(f.path, f.data, s.Pool == "")
		if err != nil {
			return res, err
		}
		undo = append(undo, u)
	}
	sb, _ := json.Marshal(state{Pool: p.Pool, Created: created})
	if err := writeFile(e.Paths.state(), append(sb, '\n'), 0o644); err != nil {
		return res, err
	}
	undo = append(undo, func() { os.Remove(e.Paths.state()) })
	if _, err := e.run(ctx, time.Minute, e.Paths.Systemctl, "daemon-reload"); err != nil {
		return res, err
	}
	return res, nil
}

// place пишет свой файл; чужой существующий (первая настройка) — сначала в
// резервную копию /etc/gamayun-nas/apps-backup. Возвращает откат.
func (e *Executor) place(path string, data []byte, first bool) (func(), error) {
	old, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	bk := filepath.Join(e.Paths.backup(), strings.ReplaceAll(strings.TrimPrefix(path, e.Paths.Etc+"/"), "/", "_"))
	if exists && first && !bytes.Equal(old, data) {
		if _, err := os.Stat(bk); errors.Is(err, os.ErrNotExist) {
			if err := writeFile(bk, old, 0o600); err != nil {
				return nil, err
			}
		}
	}
	if err := writeFile(path, data, 0o644); err != nil {
		return nil, err
	}
	return func() {
		if exists {
			writeFile(path, old, 0o644)
		} else {
			os.Remove(path)
			os.Remove(filepath.Dir(path)) // пустой *.service.d
		}
	}, nil
}

// restore возвращает файл из резервной копии или удаляет свой.
func (e *Executor) restore(path string) error {
	bk := filepath.Join(e.Paths.backup(), strings.ReplaceAll(strings.TrimPrefix(path, e.Paths.Etc+"/"), "/", "_"))
	if b, err := os.ReadFile(bk); err == nil {
		if err := writeFile(path, b, 0o644); err != nil {
			return err
		}
		if err := os.Remove(bk); err != nil {
			return err
		}
		os.Remove(e.Paths.backup()) // пустой
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// UnconfigureResult — итог apps.unconfigure.
type UnconfigureResult struct {
	Pool   string   `json:"pool"`
	Purged []string `json:"purged"`
}

// Unconfigure снимает настройку Docker с пула (данные на пуле остаются,
// кроме PurgeCreated). Docker должен быть остановлен.
func (e *Executor) Unconfigure(ctx context.Context, p UnconfigureParams) (UnconfigureResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := UnconfigureResult{Purged: []string{}}
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	res.Pool = s.Pool
	if d, c := e.active(ctx); d != "inactive" && d != "failed" || c != "inactive" && c != "failed" {
		return res, refuse("docker/containerd не остановлены (%s/%s)", d, c)
	}
	for _, f := range []string{e.Paths.daemonJSON(), e.Paths.ctrTOML()} {
		if err := e.restore(f); err != nil {
			return res, err
		}
	}
	for _, u := range []string{"gamayun-docker", "gamayun-containerd"} {
		if err := os.Remove(e.Paths.dropin(u)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return res, err
		}
		os.Remove(filepath.Dir(e.Paths.dropin(u)))
	}
	if p.PurgeCreated && s.Pool != "" && s.Created {
		if err := e.needPoolRW(s.Pool); err != nil {
			return res, err
		}
		purged, err := e.purge(ctx, e.appsRoot(s.Pool))
		res.Purged = purged
		if err != nil {
			return res, err
		}
	}
	if err := os.Remove(e.Paths.state()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	_, err = e.run(ctx, time.Minute, e.Paths.Systemctl, "daemon-reload")
	return res, err
}

// purge удаляет субтом root со всеми вложенными субтомами и файлами.
func (e *Executor) purge(ctx context.Context, root string) ([]string, error) {
	out := []string{}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	// Вложенные субтомы (снизу вверх): btrfs subvolume list -o даёт пути от корня ФС.
	r, err := e.run(ctx, time.Minute, e.Paths.Btrfs, "subvolume", "list", "-o", root)
	if err != nil {
		return out, err
	}
	mp := filepath.Dir(root)
	var subs []string
	for _, l := range strings.Split(string(r.Stdout), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		rel := strings.TrimPrefix(f[len(f)-1], "<FS_TREE>/")
		subs = append(subs, filepath.Join(mp, rel))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(subs)))
	for _, sv := range subs {
		if !strings.HasPrefix(sv, root+"/") {
			return out, fmt.Errorf("субтом вне %s: %s", root, sv)
		}
		if _, err := e.run(ctx, 5*time.Minute, e.Paths.Btrfs, "subvolume", "delete", sv); err != nil {
			return out, err
		}
		out = append(out, sv)
	}
	// Остаток — обычные файлы (каркас docker/containerd) в корне .apps.
	ents, _ := os.ReadDir(root)
	for _, en := range ents {
		if err := os.RemoveAll(filepath.Join(root, en.Name())); err != nil {
			return out, err
		}
	}
	if _, err := e.run(ctx, 5*time.Minute, e.Paths.Btrfs, "subvolume", "delete", root); err != nil {
		return out, err
	}
	return append(out, root), nil
}

// StartResult — итог apps.start.
type StartResult struct {
	Version string `json:"version"`
	RootDir string `json:"root_dir"`
}

// Start ставит флаг запуска и запускает containerd и docker. Готово — только
// если Docker отвечает и его data-root на пуле, а /var/lib/{docker,containerd}
// не созданы; иначе остановка и ошибка с причиной.
func (e *Executor) Start(ctx context.Context, p PoolParams) (res StartResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.installed() {
		return res, refuse("пакет gamayun-nas-apps не установлен")
	}
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	if s.Pool != p.Pool {
		return res, refuse("приложения настроены на пул %q, а не %s", s.Pool, p.Pool)
	}
	if err := e.needPoolRW(p.Pool); err != nil {
		return res, err
	}
	for _, f := range []string{e.Paths.daemonJSON(), e.Paths.ctrTOML(), e.Paths.dropin("gamayun-docker"), e.Paths.dropin("gamayun-containerd")} {
		if _, err := os.Stat(f); err != nil {
			return res, refuse("нет файла настройки %s", f)
		}
	}
	defer func() {
		if err != nil {
			e.stop(context.Background())
		}
	}()
	if err := os.MkdirAll(e.Paths.Run, 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(e.Paths.flag(), nil, 0o644); err != nil {
		return res, err
	}
	e.Runner.Run(ctx, 15*time.Second, nil, e.Paths.Systemctl, "reset-failed", DockerUnit, CtrUnit)
	if _, err := e.run(ctx, StartTimeout, e.Paths.Systemctl, "start", CtrUnit, DockerUnit); err != nil {
		// start-limit-hit после череды остановок (пул пропадал): сброс и одна повторная попытка.
		e.Runner.Run(ctx, 15*time.Second, nil, e.Paths.Systemctl, "reset-failed", DockerUnit, CtrUnit)
		time.Sleep(3 * time.Second)
		if _, err2 := e.run(ctx, StartTimeout, e.Paths.Systemctl, "start", CtrUnit, DockerUnit); err2 != nil {
			return res, fmt.Errorf("%w; %s", err, e.journal(ctx))
		}
	}
	if d, c := e.active(ctx); d != "active" || c != "active" {
		// ConditionPathIsMountPoint/Exists не выполнено: start «успешен», служба не запущена.
		return res, fmt.Errorf("docker/containerd не запустились (%s/%s); %s", d, c, e.journal(ctx))
	}
	var info Info
	for i := 0; ; i++ {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err = e.docker().Info(cctx)
		cancel()
		if err == nil || i >= 30 || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return res, fmt.Errorf("Docker не отвечает: %w", err)
	}
	locked, err := LockedDocker(e.Paths.Lock)
	if err != nil {
		return res, fmt.Errorf("закреплённая версия Docker не прочитана: %w", err)
	}
	if info.ServerVersion != locked {
		return res, fmt.Errorf("версия Docker %s, закреплена %s (versions.lock)", info.ServerVersion, locked)
	}
	want := filepath.Join(e.appsRoot(p.Pool), DockerDir)
	if info.DockerRootDir != want {
		return res, fmt.Errorf("data-root Docker %s, ожидался %s", info.DockerRootDir, want)
	}
	for _, d := range []string{"docker", "containerd"} {
		if _, err := os.Stat(filepath.Join(e.Paths.VarLib, d)); err == nil {
			return res, fmt.Errorf("создан %s: Docker пишет на системный диск", filepath.Join(e.Paths.VarLib, d))
		}
	}
	return StartResult{Version: info.ServerVersion, RootDir: info.DockerRootDir}, nil
}

// journal — последние строки журнала docker/containerd для причины сбоя.
func (e *Executor) journal(ctx context.Context) string {
	r, err := e.Runner.Run(ctx, 15*time.Second, nil, e.Paths.Journalctl, "-u", DockerUnit, "-u", CtrUnit,
		"-n", "4", "--no-pager", "-o", "cat")
	if err != nil || len(bytes.TrimSpace(r.Stdout)) == 0 {
		return "журнал пуст"
	}
	return "журнал: " + firstLines(r.Stdout, 4)
}

// StopResult — итог apps.stop.
type StopResult struct {
	Docker     string `json:"docker"`
	Containerd string `json:"containerd"`
	// Reaped — оставшиеся после остановки служб процессы контейнеров и shim,
	// остановленные принудительно.
	Reaped []string `json:"reaped,omitempty"`
	// Unmounted — снятые монтирования внутри <пул>/.apps (overlay контейнеров,
	// оставшиеся без dockerd).
	Unmounted []string `json:"unmounted,omitempty"`
}

// Stop снимает флаг и останавливает docker и containerd (контейнеры
// останавливает dockerd). Идемпотентна.
func (e *Executor) Stop(ctx context.Context, _ NoParams) (StopResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stop(ctx)
}

func (e *Executor) stop(ctx context.Context) (StopResult, error) {
	if err := os.Remove(e.Paths.flag()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return StopResult{}, err
	}
	_, err := e.run(ctx, StopTimeout, e.Paths.Systemctl, "stop", DockerUnit, CtrUnit)
	d, c := e.active(ctx)
	res := StopResult{Docker: d, Containerd: c}
	if err != nil {
		return res, err
	}
	if d == "active" || c == "active" || d == "deactivating" || c == "deactivating" {
		return res, fmt.Errorf("docker/containerd не остановлены (%s/%s)", d, c)
	}
	// dockerd мог не остановить контейнеры (упал раньше containerd) —
	// shim и процессы контейнеров переживают systemctl stop (KillMode=process,
	// свои scope) и держат пул.
	for _, p := range e.reapLeftovers(ctx) {
		res.Reaped = append(res.Reaped, p.String())
	}
	s, _ := e.loadState()
	res.Unmounted = e.umountNested(ctx, s.Pool)
	if busy := e.poolBusy(s.Pool); len(busy) > 0 {
		return res, fmt.Errorf("Docker остановлен, но пул %s занят: %s", s.Pool, strings.Join(busy, "; "))
	}
	return res, nil
}

// PoolBusyResult — итог apps.pool_busy: чем занят пул приложений.
type PoolBusyResult struct {
	// Apps — что мешает остановке Docker (остатки контейнеров, .apps).
	Apps []string `json:"apps,omitempty"`
	// Holders — процессы с cwd/root/открытыми файлами на всём пуле
	// (мешают перемонтированию).
	Holders []string `json:"holders,omitempty"`
}

// PoolBusy — проверка занятости пула приложений; только чтение
// (gamayund сбрасывает состояние «пул занят», когда пул освободился).
func (e *Executor) PoolBusy(_ context.Context, p PoolParams) (PoolBusyResult, error) {
	var res PoolBusyResult
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	if s.Pool == "" || s.Pool != p.Pool {
		return res, refuse("проверка — только пула приложений (%q)", s.Pool)
	}
	res.Apps = e.poolBusy(p.Pool)
	if mounted, _ := e.poolMount(p.Pool); mounted {
		res.Holders = e.poolHolders(e.mountPoint(p.Pool))
	}
	return res, nil
}

// RemountResult — итог apps.pool_remount.
type RemountResult struct {
	Pool     string `json:"pool"`
	ReadOnly bool   `json:"read_only"`
}

// Remount перемонтирует пул приложений (umount + mount): принудительный
// read-only Btrfs после ошибки записи снимается только так, remount,rw не
// годится. Только пул приложений, только при остановленном Docker.
func (e *Executor) Remount(ctx context.Context, p PoolParams) (RemountResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := RemountResult{Pool: p.Pool}
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	if s.Pool != p.Pool {
		return res, refuse("перемонтирование — только пула приложений (%q)", s.Pool)
	}
	if d, c := e.active(ctx); d == "active" || c == "active" || d == "activating" || c == "activating" {
		return res, refuse("docker/containerd не остановлены (%s/%s)", d, c)
	}
	// Остатки контейнеров держат суперблок в своих пространствах
	// имён: umount пройдёт, а mount вернёт тот же суперблок в аварийном RO.
	e.reapLeftovers(ctx)
	e.umountNested(ctx, p.Pool)
	if busy := e.poolBusy(p.Pool); len(busy) > 0 {
		return res, refuse("пул %s занят, перемонтирование невозможно: %s", p.Pool, strings.Join(busy, "; "))
	}
	mp := e.mountPoint(p.Pool)
	lv := filepath.Join(e.Paths.Dev, home.VGName(p.Pool), home.LVName)
	m, ok := e.mountEntry(mp)
	if ok {
		// umount ответил бы «target is busy» без имени —
		// назвать процессы, держащие пул (cwd/root/открытые файлы).
		if h := e.poolHolders(mp); len(h) > 0 {
			return res, refuse("пул %s занят, перемонтирование невозможно: %s", p.Pool, strings.Join(h, "; "))
		}
		want, err1 := filepath.EvalSymlinks(lv)
		got, err2 := filepath.EvalSymlinks(m.src)
		if err1 != nil || err2 != nil || want != got {
			return res, refuse("в %s смонтирован не том пула (%s)", mp, m.src)
		}
		if _, err := e.run(ctx, 2*time.Minute, e.Paths.Umount, mp); err != nil {
			return res, refuse("пул занят, перемонтирование невозможно: %v", err)
		}
	}
	if _, err := e.run(ctx, 5*time.Minute, e.Paths.Mount, "-t", "btrfs", lv, mp); err != nil {
		return res, fmt.Errorf("пул %s не смонтирован обратно: %w", p.Pool, err)
	}
	mounted, ro := e.poolMount(p.Pool)
	res.ReadOnly = ro
	if !mounted || ro {
		return res, fmt.Errorf("после перемонтирования пул %s не доступен на запись", p.Pool)
	}
	return res, nil
}

// AppdataResult — итог apps.appdata.create/delete.
type AppdataResult struct {
	Path    string   `json:"path"`
	Created bool     `json:"created"`
	NoCOW   []string `json:"nocow"`
}

// AppdataCreate создаёт субтом приложения и каталоги БД без CoW. Идемпотентна;
// существующий каталог БД без +C с файлами — отказ (+C на непустом каталоге
// не действует на имеющиеся файлы).
func (e *Executor) AppdataCreate(ctx context.Context, p AppdataParams) (AppdataResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := AppdataResult{NoCOW: []string{}}
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	if s.Pool == "" {
		return res, refuse("приложения не настроены")
	}
	if err := e.needPoolRW(s.Pool); err != nil {
		return res, err
	}
	res.Path = filepath.Join(e.appsRoot(s.Pool), AppdataDir, p.ID)
	if sv, err := e.isSubvol(filepath.Dir(res.Path)); err != nil || !sv {
		return res, refuse("нет субтома %s", filepath.Dir(res.Path))
	}
	if res.Created, err = e.ensureSubvol(ctx, res.Path, 0o755); err != nil {
		return res, err
	}
	setNoCOW := e.SetNoCOW
	if setNoCOW == nil {
		setNoCOW = chattrC
	}
	for _, d := range p.NoCOWDirs {
		dir := filepath.Join(res.Path, d)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return res, err
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				return res, err
			}
		case err != nil:
			return res, err
		case !fi.IsDir():
			return res, refuse("%s не каталог", dir)
		default:
			if ents, _ := os.ReadDir(dir); len(ents) > 0 {
				if on, err := hasNoCOW(dir); err != nil || !on {
					return res, refuse("каталог %s не пуст и без +C", dir)
				}
				res.NoCOW = append(res.NoCOW, dir)
				continue
			}
		}
		if err := setNoCOW(dir); err != nil {
			return res, fmt.Errorf("chattr +C %s: %w", dir, err)
		}
		res.NoCOW = append(res.NoCOW, dir)
	}
	return res, nil
}

// AppdataDelete удаляет субтом приложения (идемпотентна).
func (e *Executor) AppdataDelete(ctx context.Context, p AppdataParams) (AppdataResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	res := AppdataResult{NoCOW: []string{}}
	s, err := e.loadState()
	if err != nil {
		return res, err
	}
	if s.Pool == "" {
		return res, refuse("приложения не настроены")
	}
	if err := e.needPoolRW(s.Pool); err != nil {
		return res, err
	}
	res.Path = filepath.Join(e.appsRoot(s.Pool), AppdataDir, p.ID)
	// Точки восстановления приложения удаляются вместе с данными.
	if err := e.dropAllPoints(ctx, filepath.Join(e.appsRoot(s.Pool), PointsDir, p.ID)); err != nil {
		return res, fmt.Errorf("точки восстановления: %w", err)
	}
	sv, err := e.isSubvol(res.Path)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if !sv {
		return res, refuse("%s не субтом", res.Path)
	}
	_, err = e.purge(ctx, res.Path)
	return res, err
}

// EventsResult — итог apps.events.
type EventsResult struct {
	// Running — Docker отвечал; false — события не читались (Docker остановлен).
	Running bool          `json:"running"`
	Events  []DockerEvent `json:"events"`
	// UntilNano — до какого момента события прочитаны (курсор следующего опроса).
	UntilNano int64 `json:"until_nano"`
}

// Events — события контейнеров после SinceNano, ожидание до WaitS секунд.
func (e *Executor) Events(ctx context.Context, p EventsParams) (EventsResult, error) {
	res := EventsResult{Events: []DockerEvent{}}
	now := time.Now()
	since := p.SinceNano
	if since == 0 || since > now.UnixNano() {
		since = now.UnixNano()
	}
	until := now.Add(time.Duration(p.WaitS) * time.Second)
	evs, end, err := e.docker().Events(ctx, since, until.UnixNano(), MaxEvents)
	res.UntilNano = end
	if err != nil {
		if errors.Is(err, ErrDockerDown) {
			res.UntilNano = since
			return res, nil
		}
		return res, err
	}
	res.Running = true
	res.Events = evs
	return res, nil
}
