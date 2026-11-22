package helper

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gamayun-nas/internal/apps"
	"gamayun-nas/internal/confbackup"
	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/smart"
	"gamayun-nas/internal/snapshots"
	"gamayun-nas/internal/storage/home"
)

// SystemPaths — источники system.info; в тестах указывают во временный каталог.
type SystemPaths struct {
	Proc      string // /proc
	OSRelease string // /etc/os-release
	DTModel   string // /sys/firmware/devicetree/base/model (ARM-платы)
}

// Deps — внешние зависимости операций.
type Deps struct {
	Disks  disks.Paths
	System SystemPaths
	// Runner — запуск внешних программ (операции пулов).
	Runner *Runner
	// Home — исполнитель операций пулов профиля Home.
	Home *home.Executor
	// NAS — учётки и Samba.
	NAS *nas.Executor
	// Config — резервные копии настроек в пуле и на USB.
	Config *confbackup.Executor
	// Smart — SMART дисков.
	Smart *smart.Executor
	// Snapshots — снапшоты общих папок.
	Snapshots *snapshots.Executor
	// Apps — подсистема приложений Docker.
	Apps *apps.Executor
}

// DefaultDeps — пути реальной системы и настройки из конфигурации.
func DefaultDeps(cfg Config) Deps {
	r := &Runner{}
	return Deps{
		Disks: disks.DefaultPaths(),
		System: SystemPaths{
			Proc:      "/proc",
			OSRelease: "/etc/os-release",
			DTModel:   "/sys/firmware/devicetree/base/model",
		},
		Runner: r,
		Home: &home.Executor{
			Runner:    RunnerAdapter{r},
			Tools:     home.DefaultTools(),
			Paths:     home.DefaultPaths(),
			AllowIDs:  append([]string{}, cfg.DestructiveAllowIDs...),
			MountRoot: cfg.MountRoot,
		},
		Config: &confbackup.Executor{MountRoot: cfg.MountRoot, Runner: confRunner{r}},
		Smart:  &smart.Executor{Runner: smartRunner{r}, Smartctl: "/usr/sbin/smartctl", Disks: disks.DefaultPaths()},
		NAS: &nas.Executor{
			Runner: nasRunner{r}, Tools: nas.DefaultTools(), MountRoot: cfg.MountRoot,
			IncludeFile: cfg.SambaInclude, SmbConf: cfg.SambaConf,
			SSHDev: append([]string{}, cfg.SSHDevAccess...),
		},
		Snapshots: &snapshots.Executor{Runner: nasRunner{r}, MountRoot: cfg.MountRoot},
		Apps: &apps.Executor{Runner: nasRunner{r}, Paths: apps.DefaultPaths(), MountRoot: cfg.MountRoot,
			ReadOnly: apps.StatfsReadOnly},
	}
}

// nasRunner приводит Runner к интерфейсу nas.Runner.
type nasRunner struct{ R *Runner }

func (a nasRunner) Run(ctx context.Context, timeout time.Duration, stdin []byte, path string, args ...string) (nas.CmdResult, error) {
	r, err := a.R.RunInput(ctx, timeout, stdin, path, args...)
	return nas.CmdResult{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}, err
}

// confRunner приводит Runner к интерфейсу confbackup.Runner.
type confRunner struct{ R *Runner }

func (a confRunner) Run(ctx context.Context, timeout time.Duration, path string, args ...string) (confbackup.CmdResult, error) {
	r, err := a.R.Run(ctx, timeout, path, args...)
	return confbackup.CmdResult{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}, err
}

// smartRunner приводит Runner к интерфейсу smart.Runner.
type smartRunner struct{ R *Runner }

func (a smartRunner) Run(ctx context.Context, timeout time.Duration, path string, args ...string) (smart.CmdResult, error) {
	r, err := a.R.Run(ctx, timeout, path, args...)
	return smart.CmdResult{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}, err
}

// RunnerAdapter приводит Runner к интерфейсу home.Runner.
type RunnerAdapter struct{ R *Runner }

// Run — см. Runner.Run.
func (a RunnerAdapter) Run(ctx context.Context, timeout time.Duration, path string, args ...string) (home.CmdResult, error) {
	r, err := a.R.Run(ctx, timeout, path, args...)
	return home.CmdResult{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr, Truncated: r.Truncated}, err
}

func mutating(op Op) Op {
	op.Mutating = true
	return op
}

// poolOpTimeout — предел изменяющей операции пула (разметка, dm-integrity
// format, mdadm, mkfs). Долгий reshape/recovery идёт в ядре и ждётся
// задачей gamayund опросом home.pool.status.
const poolOpTimeout = 30 * time.Minute

// DefaultRegistry — полный перечень операций helper. Новая операция
// добавляется только здесь, с типом параметров и тестами.
func DefaultRegistry(d Deps) *Registry {
	ops := []Op{
		Define("disks.observe", 30*time.Second, func(ctx context.Context, _ NoParams) (any, error) {
			return disks.Observe(d.Disks)
		}),
		Define("system.info", 10*time.Second, func(ctx context.Context, _ NoParams) (any, error) {
			return SystemInfo(d.System), nil
		}),
	}
	if d.Home != nil {
		h := d.Home
		ops = append(ops,
			Define(home.OpStatus, 60*time.Second, func(ctx context.Context, p home.PoolParams) (any, error) {
				return h.Status(ctx, p)
			}),
			mutating(Define(home.OpCreate, poolOpTimeout, func(ctx context.Context, p home.CreateParams) (any, error) {
				return h.Create(ctx, p)
			})),
			mutating(Define(home.OpAddDisk, poolOpTimeout, func(ctx context.Context, p home.AddDiskParams) (any, error) {
				return h.AddDisk(ctx, p)
			})),
			mutating(Define(home.OpReplace, poolOpTimeout, func(ctx context.Context, p home.ReplaceParams) (any, error) {
				return h.Replace(ctx, p)
			})),
			mutating(Define(home.OpReplaceFinish, poolOpTimeout, func(ctx context.Context, p home.ReplaceParams) (any, error) {
				return h.ReplaceFinish(ctx, p)
			})),
			mutating(Define(home.OpDestroy, 10*time.Minute, func(ctx context.Context, p home.PoolParams) (any, error) {
				return h.Destroy(ctx, p)
			})),
			mutating(Define(home.OpCreateRollback, 10*time.Minute, func(ctx context.Context, p home.CreateRollbackParams) (any, error) {
				return h.CreateRollback(ctx, p)
			})),
			mutating(Define(home.OpReleaseDisk, 5*time.Minute, func(ctx context.Context, p home.ReleaseParams) (any, error) {
				return h.ReleaseDisk(ctx, p)
			})),
			mutating(Define(home.OpSetReadOnly, time.Minute, func(ctx context.Context, p home.ReadOnlyParams) (any, error) {
				return h.SetReadOnly(ctx, p)
			})),
			Define(home.OpScan, 2*time.Minute, func(ctx context.Context, p home.ScanParams) (any, error) {
				return h.Scan(ctx, p)
			}),
			mutating(Define(home.OpImport, 10*time.Minute, func(ctx context.Context, p home.ImportParams) (any, error) {
				return h.Import(ctx, p)
			})),
			mutating(Define(home.OpAssemble, 10*time.Minute, func(ctx context.Context, p home.AssembleParams) (any, error) {
				return h.Assemble(ctx, p)
			})),
			Define(home.OpScrubStatus, 60*time.Second, func(ctx context.Context, p home.PoolParams) (any, error) {
				return h.ScrubStatus(ctx, p)
			}),
			mutating(Define(home.OpScrubStart, 2*time.Minute, func(ctx context.Context, p home.ScrubParams) (any, error) {
				return h.ScrubStart(ctx, p)
			})),
			mutating(Define(home.OpScrubCancel, 2*time.Minute, func(ctx context.Context, p home.PoolParams) (any, error) {
				return h.ScrubCancel(ctx, p)
			})),
			mutating(Define(home.OpGrow, 10*time.Minute, func(ctx context.Context, p home.PoolParams) (any, error) {
				return h.Grow(ctx, p)
			})),
		)
	}
	if d.Config != nil {
		c := d.Config
		ops = append(ops,
			mutating(Define(confbackup.OpSave, time.Minute, func(ctx context.Context, p confbackup.SaveParams) (any, error) {
				return c.Save(ctx, p)
			})),
			Define(confbackup.OpList, 30*time.Second, func(ctx context.Context, p confbackup.PoolParams) (any, error) {
				return c.List(ctx, p)
			}),
			Define(confbackup.OpRead, 30*time.Second, func(ctx context.Context, p confbackup.ReadParams) (any, error) {
				return c.Read(ctx, p)
			}),
			Define(confbackup.OpUSBList, 30*time.Second, func(ctx context.Context, _ NoParams) (any, error) {
				return c.USBList(ctx)
			}),
			mutating(Define(confbackup.OpUSBSave, 3*time.Minute, func(ctx context.Context, p confbackup.USBSaveParams) (any, error) {
				return c.USBSave(ctx, p)
			})),
		)
	}
	if d.Smart != nil {
		sm := d.Smart
		ops = append(ops,
			Define(smart.OpRead, time.Minute, func(ctx context.Context, p smart.DiskParams) (any, error) {
				return sm.Read(ctx, p)
			}),
			mutating(Define(smart.OpTest, time.Minute, func(ctx context.Context, p smart.TestParams) (any, error) {
				return sm.Test(ctx, p)
			})),
			mutating(Define(smart.OpSet, time.Minute, func(ctx context.Context, p smart.SetParams) (any, error) {
				return sm.Set(ctx, p)
			})),
		)
	}
	if d.Snapshots != nil {
		sn := d.Snapshots
		ops = append(ops,
			Define(snapshots.OpList, 30*time.Second, func(ctx context.Context, p snapshots.ShareParams) (any, error) {
				return sn.List(ctx, p)
			}),
			mutating(Define(snapshots.OpCreate, 3*time.Minute, func(ctx context.Context, p snapshots.CreateParams) (any, error) {
				return sn.Create(ctx, p)
			})),
			mutating(Define(snapshots.OpDelete, 6*time.Minute, func(ctx context.Context, p snapshots.SnapParams) (any, error) {
				return sn.Delete(ctx, p)
			})),
			Define(snapshots.OpBrowse, 30*time.Second, func(ctx context.Context, p snapshots.PathParams) (any, error) {
				return sn.Browse(ctx, p)
			}),
			mutating(Define(snapshots.OpRestore, snapshots.RestoreTimeout, func(ctx context.Context, p snapshots.PathParams) (any, error) {
				return sn.Restore(ctx, p)
			})),
		)
	}
	if d.Apps != nil {
		a := d.Apps
		ops = append(ops,
			Define(apps.OpStatus, 30*time.Second, func(ctx context.Context, p apps.NoParams) (any, error) {
				return a.Status(ctx, p)
			}),
			mutating(Define(apps.OpConfigure, 3*time.Minute, func(ctx context.Context, p apps.PoolParams) (any, error) {
				return a.Configure(ctx, p)
			})),
			mutating(Define(apps.OpUnconfigure, 10*time.Minute, func(ctx context.Context, p apps.UnconfigureParams) (any, error) {
				return a.Unconfigure(ctx, p)
			})),
			mutating(Define(apps.OpStart, 4*time.Minute, func(ctx context.Context, p apps.PoolParams) (any, error) {
				return a.Start(ctx, p)
			})),
			mutating(Define(apps.OpStop, apps.StopTimeout+time.Minute, func(ctx context.Context, p apps.NoParams) (any, error) {
				return a.Stop(ctx, p)
			})),
			Define(apps.OpPoolBusy, time.Minute, func(ctx context.Context, p apps.PoolParams) (any, error) {
				return a.PoolBusy(ctx, p)
			}),
			mutating(Define(apps.OpRemount, 8*time.Minute, func(ctx context.Context, p apps.PoolParams) (any, error) {
				return a.Remount(ctx, p)
			})),
			mutating(Define(apps.OpAppdataCreate, 2*time.Minute, func(ctx context.Context, p apps.AppdataParams) (any, error) {
				return a.AppdataCreate(ctx, p)
			})),
			mutating(Define(apps.OpAppdataDelete, 10*time.Minute, func(ctx context.Context, p apps.AppdataParams) (any, error) {
				return a.AppdataDelete(ctx, p)
			})),
			mutating(Define(apps.OpProjectWrite, time.Minute, func(ctx context.Context, p apps.ProjectWriteParams) (any, error) {
				return a.ProjectWrite(ctx, p)
			})),
			mutating(Define(apps.OpProjectRevert, time.Minute, func(ctx context.Context, p apps.ProjectParams) (any, error) {
				return a.ProjectRevert(ctx, p)
			})),
			mutating(Define(apps.OpPointCreate, 10*time.Minute, func(ctx context.Context, p apps.PointParams) (any, error) {
				return a.PointCreate(ctx, p)
			})),
			mutating(Define(apps.OpPointRestore, 20*time.Minute, func(ctx context.Context, p apps.PointParams) (any, error) {
				return a.PointRestore(ctx, p)
			})),
			mutating(Define(apps.OpPointDelete, 10*time.Minute, func(ctx context.Context, p apps.PointParams) (any, error) {
				return a.PointDelete(ctx, p)
			})),
			Define(apps.OpPointList, 30*time.Second, func(ctx context.Context, p apps.PointListParams) (any, error) {
				return a.PointList(ctx, p)
			}),
			Define(apps.OpProjectRead, 30*time.Second, func(ctx context.Context, p apps.ProjectParams) (any, error) {
				return a.ProjectRead(ctx, p)
			}),
			mutating(Define(apps.OpProjectDelete, time.Minute, func(ctx context.Context, p apps.ProjectParams) (any, error) {
				return a.ProjectDelete(ctx, p)
			})),
			mutating(Define(apps.OpProjectCompose, 16*time.Minute, func(ctx context.Context, p apps.ComposeParams) (any, error) {
				return a.ProjectCompose(ctx, p)
			})),
			Define(apps.OpProjectsPS, 30*time.Second, func(ctx context.Context, p apps.NoParams) (any, error) {
				return a.ProjectsPS(ctx, p)
			}),
			Define(apps.OpProjectLogs, time.Minute, func(ctx context.Context, p apps.LogsParams) (any, error) {
				return a.ProjectLogs(ctx, p)
			}),
			mutating(Define(apps.OpUserEnsure, time.Minute, func(ctx context.Context, p apps.UserParams) (any, error) {
				return a.UserEnsure(ctx, p)
			})),
			mutating(Define(apps.OpUserDelete, time.Minute, func(ctx context.Context, p apps.UserParams) (any, error) {
				return a.UserDelete(ctx, p)
			})),
			mutating(Define(apps.OpAppdataOwn, 30*time.Minute, func(ctx context.Context, p apps.OwnParams) (any, error) {
				return a.AppdataOwn(ctx, p)
			})),
			mutating(Define(apps.OpACLStart, time.Minute, func(ctx context.Context, p apps.ACLParams) (any, error) {
				return a.ACLStart(ctx, p)
			})),
			Define(apps.OpACLStatus, 30*time.Second, func(ctx context.Context, p apps.JobParams) (any, error) {
				return a.ACLStatus(ctx, p)
			}),
			Define(apps.OpACLCheck, 30*time.Second, func(ctx context.Context, p apps.ShareParams) (any, error) {
				return a.ACLCheck(ctx, p)
			}),
			Define(apps.OpEvents, (apps.MaxWait+30)*time.Second, func(ctx context.Context, p apps.EventsParams) (any, error) {
				return a.Events(ctx, p)
			}),
		)
	}
	if d.NAS != nil {
		n := d.NAS
		ops = append(ops,
			mutating(Define(nas.OpUserEnsure, time.Minute, func(ctx context.Context, p nas.UserParams) (any, error) {
				return n.UserEnsure(ctx, p)
			})),
			mutating(Define(nas.OpUserPassword, time.Minute, func(ctx context.Context, p nas.PasswordParams) (any, error) {
				return n.UserPassword(ctx, p)
			})),
			mutating(Define(nas.OpUserDelete, time.Minute, func(ctx context.Context, p nas.UserParams) (any, error) {
				return n.UserDelete(ctx, p)
			})),
			mutating(Define(nas.OpUserAccess, time.Minute, func(ctx context.Context, p nas.AccessParams) (any, error) {
				return n.UserAccess(ctx, p)
			})),
			mutating(Define(nas.OpAccessSetup, 2*time.Minute, func(ctx context.Context, p nas.SetupParams) (any, error) {
				return n.AccessSetup(ctx, p)
			})),
			Define(nas.OpIdentityGet, time.Minute, func(ctx context.Context, p nas.SetupParams) (any, error) {
				return n.IdentityGet(ctx, p)
			}),
			mutating(Define(nas.OpHostnameSet, time.Minute, func(ctx context.Context, p nas.HostnameParams) (any, error) {
				return n.HostnameSet(ctx, p)
			})),
			mutating(Define(nas.OpTimezoneSet, time.Minute, func(ctx context.Context, p nas.TimezoneParams) (any, error) {
				return n.TimezoneSet(ctx, p)
			})),
			mutating(Define(nas.OpHomesApply, 2*time.Minute, func(ctx context.Context, p nas.HomesParams) (any, error) {
				return n.HomesApply(ctx, p)
			})),
			mutating(Define(nas.OpGuardApply, 2*time.Minute, func(ctx context.Context, p nas.GuardParams) (any, error) {
				return n.GuardApply(ctx, p)
			})),
			Define(nas.OpGuardStatus, time.Minute, func(ctx context.Context, p nas.SetupParams) (any, error) {
				return n.GuardStatus(ctx, p)
			}),
			mutating(Define(nas.OpSSHApply, 2*time.Minute, func(ctx context.Context, p nas.SSHParams) (any, error) {
				return n.SSHApply(ctx, p)
			})),
			Define(nas.OpSSHStatus, time.Minute, func(ctx context.Context, p nas.SetupParams) (any, error) {
				return n.SSHStatusGet(ctx, p)
			}),
			mutating(Define(nas.OpPower, 30*time.Second, func(ctx context.Context, p nas.PowerParams) (any, error) {
				return n.Power(ctx, p)
			})),
			mutating(Define(nas.OpGuardUnban, time.Minute, func(ctx context.Context, p nas.UnbanParams) (any, error) {
				return n.GuardUnban(ctx, p)
			})),
			Define(nas.OpShareDirs, 30*time.Second, func(ctx context.Context, p nas.ShareDirsParams) (any, error) {
				return n.ShareDirs(ctx, p)
			}),
			mutating(Define(nas.OpShareMkdir, 30*time.Second, func(ctx context.Context, p nas.ShareMkdirParams) (any, error) {
				return n.ShareMkdir(ctx, p)
			})),
			mutating(Define(nas.OpShareRename, 30*time.Second, func(ctx context.Context, p nas.ShareRenameParams) (any, error) {
				return n.ShareRename(ctx, p)
			})),
			mutating(Define(nas.OpShareMove, 30*time.Second, func(ctx context.Context, p nas.ShareMoveParams) (any, error) {
				return n.ShareMove(ctx, p)
			})),
			mutating(Define(nas.OpShareRmdir, 30*time.Second, func(ctx context.Context, p nas.ShareRmdirParams) (any, error) {
				return n.ShareRmdir(ctx, p)
			})),
			// Файловый менеджер. Права пользователя проверяет gamayund.
			Define(nas.OpFilesList, 30*time.Second, func(ctx context.Context, p nas.FilesLocParams) (any, error) {
				return n.FilesList(ctx, p)
			}),
			Define(nas.OpFilesStat, 10*time.Second, func(ctx context.Context, p nas.FilesLocParams) (any, error) {
				return n.FilesStat(ctx, p)
			}),
			Define(nas.OpFilesOpen, 10*time.Second, func(ctx context.Context, p nas.FilesLocParams) (any, error) {
				return withFile(n.FilesOpen(ctx, p))
			}),
			mutating(Define(nas.OpFilesCreate, 10*time.Second, func(ctx context.Context, p nas.FilesCreateParams) (any, error) {
				return withFile(n.FilesCreate(ctx, p))
			})),
			mutating(Define(nas.OpFilesCommit, 10*time.Second, func(ctx context.Context, p nas.FilesCommitParams) (any, error) {
				return n.FilesCommit(ctx, p)
			})),
			mutating(Define(nas.OpFilesDiscard, 10*time.Second, func(ctx context.Context, p nas.FilesDiscardParams) (any, error) {
				return n.FilesDiscard(ctx, p)
			})),
			mutating(Define(nas.OpFilesMkdir, 10*time.Second, func(ctx context.Context, p nas.FilesNameParams) (any, error) {
				return n.FilesMkdir(ctx, p)
			})),
			mutating(Define(nas.OpFilesRename, 10*time.Second, func(ctx context.Context, p nas.FilesNameParams) (any, error) {
				return n.FilesRename(ctx, p)
			})),
			mutating(Define(nas.OpFilesMove, 10*time.Second, func(ctx context.Context, p nas.FilesMoveParams) (any, error) {
				return n.FilesMove(ctx, p)
			})),
			mutating(Define(nas.OpFilesRemove, 30*time.Minute, func(ctx context.Context, p nas.FilesLocParams) (any, error) {
				return n.FilesRemove(ctx, p)
			})),
			mutating(Define(nas.OpSharesApply, 2*time.Minute, func(ctx context.Context, p nas.SharesParams) (any, error) {
				return n.SharesApply(ctx, p)
			})),
		)
	}
	return NewRegistry(ops...)
}

// SystemInfoResult — сведения о системе. null — значение не определено.
type SystemInfoResult struct {
	Hostname      *string  `json:"hostname"`
	Kernel        *string  `json:"kernel"`
	OS            *string  `json:"os"`
	Arch          string   `json:"arch"`
	Board         *string  `json:"board"`
	CPU           CPUInfo  `json:"cpu"`
	Memory        MemInfo  `json:"memory"`
	UptimeSeconds *float64 `json:"uptime_seconds"`
}

// CPUInfo — процессор.
type CPUInfo struct {
	Model *string `json:"model"`
	Count int     `json:"count"` // логических процессоров
}

// MemInfo — оперативная память.
type MemInfo struct {
	TotalBytes     *int64 `json:"total_bytes"`
	AvailableBytes *int64 `json:"available_bytes"`
}

// SystemInfo читает сведения о системе из /proc и /etc/os-release.
func SystemInfo(p SystemPaths) SystemInfoResult {
	r := SystemInfoResult{Arch: runtime.GOARCH}
	r.Hostname = readTrimmed(p.Proc + "/sys/kernel/hostname")
	r.Kernel = readTrimmed(p.Proc + "/sys/kernel/osrelease")
	if p.DTModel != "" {
		if b, err := os.ReadFile(p.DTModel); err == nil {
			if s := strings.TrimSpace(strings.TrimRight(string(b), "\x00")); s != "" {
				r.Board = &s
			}
		}
	}
	if b, err := os.ReadFile(p.OSRelease); err == nil {
		r.OS = osPrettyName(b)
	}
	if b, err := os.ReadFile(p.Proc + "/cpuinfo"); err == nil {
		r.CPU.Model, r.CPU.Count = parseCPUInfo(b)
	}
	if r.CPU.Count == 0 {
		r.CPU.Count = runtime.NumCPU()
	}
	if b, err := os.ReadFile(p.Proc + "/meminfo"); err == nil {
		r.Memory = parseMemInfo(b)
	}
	if s := readTrimmed(p.Proc + "/uptime"); s != nil {
		if f := strings.Fields(*s); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				r.UptimeSeconds = &v
			}
		}
	}
	return r
}

func readTrimmed(path string) *string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return &s
}

func osPrettyName(b []byte) *string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok || k != "PRETTY_NAME" {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		} else {
			v = strings.Trim(v, `"'`)
		}
		if v != "" {
			return &v
		}
	}
	return nil
}

// parseCPUInfo: модель из "model name" (x86), "Model"/"Hardware" (ARM);
// число — строки "processor".
func parseCPUInfo(b []byte) (*string, int) {
	var model *string
	var fallback *string
	count := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			count++
		case "model name":
			if model == nil && v != "" {
				model = &v
			}
		case "Model", "Hardware":
			if fallback == nil && v != "" {
				fallback = &v
			}
		}
	}
	if model == nil {
		model = fallback
	}
	return model, count
}

func parseMemInfo(b []byte) MemInfo {
	var m MemInfo
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) >= 3 && f[2] == "kB" {
			v *= 1024
		}
		switch f[0] {
		case "MemTotal:":
			m.TotalBytes = &v
		case "MemAvailable:":
			m.AvailableBytes = &v
		}
	}
	return m
}

// withFile — результат nas.files.open/create с дескриптором.
func withFile(h nas.FileHandle, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return WithFile{Result: h.Info, File: h.File}, nil
}
