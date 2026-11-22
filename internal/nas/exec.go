package nas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gamayun-nas/internal/storage/home"
)

// CmdResult — итог внешней команды.
type CmdResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Runner — запуск внешних программ (в helper — обёртка над helper.Runner,
// в тестах — подмена). stdin передаётся программе как есть.
type Runner interface {
	Run(ctx context.Context, timeout time.Duration, stdin []byte, path string, args ...string) (CmdResult, error)
}

// Tools — абсолютные пути программ.
type Tools struct {
	Useradd, Userdel, Id, Smbpasswd, Smbcontrol, Install, Testparm, Groupadd string
	// Учётка ОС, SSH и sudo.
	Usermod, Gpasswd, Chpasswd, Getent, Sshd, Systemctl, Visudo, Pkill string
	Hostnamectl, Timedatectl, Fail2ban                                 string
	// сессии Samba пользователя.
	Smbstatus string
	// Подтом общей папки.
	Btrfs string
}

// DefaultTools — пути Debian.
func DefaultTools() Tools {
	return Tools{
		Useradd: "/usr/sbin/useradd", Userdel: "/usr/sbin/userdel", Groupadd: "/usr/sbin/groupadd",
		Id: "/usr/bin/id", Smbpasswd: "/usr/bin/smbpasswd", Smbcontrol: "/usr/bin/smbcontrol",
		Install: "/usr/bin/install", Testparm: "/usr/bin/testparm",
		Usermod: "/usr/sbin/usermod", Gpasswd: "/usr/bin/gpasswd", Chpasswd: "/usr/sbin/chpasswd",
		Getent: "/usr/bin/getent", Sshd: "/usr/sbin/sshd", Systemctl: "/usr/bin/systemctl", Visudo: "/usr/sbin/visudo",
		Pkill: "/usr/bin/pkill", Hostnamectl: "/usr/bin/hostnamectl", Timedatectl: "/usr/bin/timedatectl",
		Fail2ban: "/usr/bin/fail2ban-client", Smbstatus: "/usr/bin/smbstatus",
		Btrfs: "/usr/bin/btrfs",
	}
}

// Executor — операции helper над учётками и Samba.
type Executor struct {
	Runner    Runner
	Tools     Tools
	MountRoot string
	// IncludeFile — include-файл шар (по умолчанию /etc/samba/gamayun-shares.conf).
	IncludeFile string
	// SmbConf — основной smb.conf; только читается, чтобы предупредить об
	// отсутствии include. Пусто — не проверять.
	SmbConf string
	// drop-in sshd и sudoers, источники учёток для AllowGroups.
	// Пусто — пути Debian (см. osaccess.go).
	SshdDropIn, SudoersDropIn, PasswdFile, GroupFile string
	// ShadowFile — /etc/shadow (откат пароля ОС); пусто — путь Debian.
	ShadowFile string
	// SSHDev — учётки режима разработки SSH.
	SSHDev []string
	// SSHInstalled — установлен ли sshd (nil — файл Tools.Sshd); для тестов.
	SSHInstalled func() bool
	// Этап 2: /etc/hosts и drop-in fail2ban (пусто — пути Debian).
	HostsFile, GuardDropIn string
	// Mounted — смонтирована ли ФС в каталоге (nil — сравнение устройства
	// каталога и родителя, как mountpoint(1)); подменяется в тестах.
	Mounted func(dir string) bool
	// DirChown — владелец созданного вложенного каталога шары по открытому
	// дескриптору; nil — fchown root:gamayun-users. Подменяется
	// в тестах без root.
	DirChown func(fd int) error
	// FileChown — владелец файла/каталога файлового менеджера;
	// nil — fchown. Подменяется в тестах без root.
	FileChown func(fd, uid, gid int) error
	// FS — сведения о ФС каталога; nil — ProbeFS (statfs/stat).
	FS func(dir string) (FSInfo, error)
	// ShadowCopy — установлен ли модуль vfs_shadow_copy2; nil —
	// поиск файла модуля в каталогах Samba.
	ShadowCopy func() bool
}

// poolMounted — пул смонтирован в <MountRoot>/<пул>. Только существование
// каталога не годится: точка монтирования остаётся у несобранного пула, и
// каталоги шар/домашних папок создавались на корневой ФС.
func (e *Executor) poolMounted(pool string) bool {
	dir := filepath.Join(e.MountRoot, pool)
	if e.Mounted != nil {
		return e.Mounted(dir)
	}
	return isMountPoint(dir)
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

const cmdTimeout = 30 * time.Second

func (e *Executor) run(ctx context.Context, res *Result, stdin []byte, path string, args ...string) (CmdResult, error) {
	res.Commands = append(res.Commands, path+" "+strings.Join(args, " "))
	r, err := e.Runner.Run(ctx, cmdTimeout, stdin, path, args...)
	return r, err
}

func fail(what string, r CmdResult) error {
	msg := strings.TrimSpace(string(r.Stderr))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("%s: код %d: %s", what, r.ExitCode, msg)
}

// isOurs — учётка существует и принадлежит NAS: первичная группа UnixGroup
// и UID не ниже MinUID (системные учётки не трогаются никогда).
// exists=false — учётки нет.
func (e *Executor) isOurs(ctx context.Context, res *Result, name string) (ours, exists bool, err error) {
	r, err := e.run(ctx, res, nil, e.Tools.Id, "-gn", "--", name)
	if err != nil {
		return false, false, err
	}
	if r.ExitCode != 0 {
		return false, false, nil
	}
	if strings.TrimSpace(string(r.Stdout)) != UnixGroup {
		return false, true, nil
	}
	r, err = e.run(ctx, res, nil, e.Tools.Id, "-u", "--", name)
	if err != nil {
		return false, true, err
	}
	uid, perr := strconv.Atoi(strings.TrimSpace(string(r.Stdout)))
	return r.ExitCode == 0 && perr == nil && uid >= MinUID && uid < 65534 && !IsAppUser(name, uid), true, nil
}

// UserEnsure создаёт учётку ОС без входа и без домашнего каталога
// (идемпотентно). Существующую чужую учётку не трогает — отказ.
func (e *Executor) UserEnsure(ctx context.Context, p UserParams) (Result, error) {
	var res Result
	ours, exists, err := e.isOurs(ctx, &res, p.Name)
	if err != nil {
		return res, err
	}
	if exists {
		if !ours {
			return res, fmt.Errorf("%w: учётка %s уже есть в системе и не принадлежит NAS", ErrRefused, p.Name)
		}
		return res, nil
	}
	if r, err := e.run(ctx, &res, nil, e.Tools.Groupadd, "-f", UnixGroup); err != nil {
		return res, err
	} else if r.ExitCode != 0 {
		return res, fail("groupadd", r)
	}
	r, err := e.run(ctx, &res, nil, e.Tools.Useradd, "--no-create-home", "--no-user-group", "--gid", UnixGroup,
		"--home-dir", HomeNone, "--shell", "/usr/sbin/nologin", "--comment", "gamayun-nas", "--", p.Name)
	if err != nil {
		return res, err
	}
	if r.ExitCode != 0 {
		return res, fail("useradd", r)
	}
	res.Changed = true
	return res, nil
}

// ErrRefused — операция отклонена проверкой безопасности helper.
var ErrRefused = home.ErrRefused

// UserPassword задаёт пароль ОС (chpasswd) и Samba (smbpasswd -a -s);
// пароль передаётся только через stdin.
func (e *Executor) UserPassword(ctx context.Context, p PasswordParams) (Result, error) {
	var res Result
	ours, exists, err := e.isOurs(ctx, &res, p.Name)
	if err != nil {
		return res, err
	}
	if !exists || !ours {
		return res, fmt.Errorf("%w: %s не учётка NAS", ErrRefused, p.Name)
	}
	// ОС и Samba меняются вместе — при отказе smbpasswd прежний хэш
	// ОС возвращается (chpasswd -e), отказ операции = ничего не изменилось.
	oldHash, herr := shadowHash(or(e.ShadowFile, "/etc/shadow"), p.Name)
	if r, err := e.run(ctx, &res, []byte(p.Name+":"+p.Password+"\n"), e.Tools.Chpasswd); err != nil {
		return res, err
	} else if r.ExitCode != 0 {
		return res, fail("chpasswd", r)
	}
	r, err := e.run(ctx, &res, []byte(p.Password+"\n"+p.Password+"\n"), e.Tools.Smbpasswd, "-a", "-s", "--", p.Name)
	if err == nil && r.ExitCode == 0 {
		return res, nil
	}
	cause := err
	if cause == nil {
		cause = fail("smbpasswd", r)
	}
	if herr != nil {
		return res, fmt.Errorf("%w; прежний пароль ОС не восстановлен (нет хэша: %v) — пароль ОС новый, Samba прежний", cause, herr)
	}
	if rr, rerr := e.run(ctx, &res, []byte(p.Name+":"+oldHash+"\n"), e.Tools.Chpasswd, "-e"); rerr != nil || rr.ExitCode != 0 {
		return res, fmt.Errorf("%w; откат пароля ОС не удался — пароль ОС новый, Samba прежний", cause)
	}
	return res, fmt.Errorf("%w; пароль ОС возвращён прежний", cause)
}

// shadowHash — поле хэша учётки из /etc/shadow (только для отката, не журналируется).
func shadowHash(path, name string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.SplitN(l, ":", 3)
		if len(f) >= 2 && f[0] == name {
			if f[1] == "" || strings.ContainsAny(f[1], ":\n") {
				return "", fmt.Errorf("пустой хэш")
			}
			return f[1], nil
		}
	}
	return "", fmt.Errorf("учётки нет в shadow")
}

// UserDelete удаляет пароль Samba и учётку ОС (только учётку NAS; нет — не ошибка).
func (e *Executor) UserDelete(ctx context.Context, p UserParams) (Result, error) {
	var res Result
	ours, exists, err := e.isOurs(ctx, &res, p.Name)
	if err != nil {
		return res, err
	}
	if !exists {
		return res, nil
	}
	if !ours {
		return res, fmt.Errorf("%w: %s не учётка NAS", ErrRefused, p.Name)
	}
	// Сначала отзыв SSH/sudo (даже если userdel не пройдёт, входа
	// нет), затем завершение процессов учётки — иначе userdel откажет
	// («currently used by process») при открытом сеансе SSH.
	if r, err := e.UserAccess(ctx, AccessParams{Name: p.Name}); err != nil {
		res.Commands = append(res.Commands, r.Commands...)
		return res, err
	} else {
		res.Commands = append(res.Commands, r.Commands...)
	}
	if _, err := e.run(ctx, &res, nil, e.Tools.Pkill, "--signal", "KILL", "--uid", p.Name); err != nil {
		return res, err
	}
	// Ненулевой код smbpasswd -x допустим: записи Samba могло не быть.
	if _, err := e.run(ctx, &res, nil, e.Tools.Smbpasswd, "-x", "--", p.Name); err != nil {
		return res, err
	}
	r, err := e.run(ctx, &res, nil, e.Tools.Userdel, "--", p.Name)
	if err != nil {
		return res, err
	}
	if r.ExitCode != 0 {
		return res, fail("userdel", r)
	}
	return res, nil
}

// SharesApply создаёт каталоги шар, записывает include-файл и перечитывает
// конфигурацию Samba. Файл пишется атомарно и проверяется testparm до
// замены; основной smb.conf не меняется.
func (e *Executor) SharesApply(ctx context.Context, p SharesParams) (Result, error) {
	var res Result
	// Шары несмонтированных пулов не публикуются (иначе Samba писала бы в
	// корневую ФС), остальные применяются: один несобранный пул не блокирует
	// конфигурацию всех папок.
	var shares []Share
	for _, s := range p.Shares {
		if !e.poolMounted(s.Pool) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("пул %s не смонтирован — папка %s не публикуется", s.Pool, s.Name))
			continue
		}
		shares = append(shares, s)
	}
	p.Shares = shares
	if p.Homes != "" && !e.poolMounted(p.Homes) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("пул %s не смонтирован — личные папки не публикуются", p.Homes))
		p.Homes = ""
	}
	for _, s := range p.Shares {
		dir := ShareDir(e.MountRoot, s.Pool, s.Name)
		if r, err := e.run(ctx, &res, nil, e.Tools.Install, "-d", "-m", "2770", "-o", "root", "-g", UnixGroup, "--", filepath.Dir(dir)); err != nil {
			return res, err
		} else if r.ExitCode != 0 {
			return res, fail("install -d", r)
		}
		// Новая (или пустая) папка — подтом Btrfs; режим и владельца
		// корня подтома ставит install -d ниже.
		if err := e.ensureSubvolume(ctx, &res, dir); err != nil {
			return res, err
		}
		if r, err := e.run(ctx, &res, nil, e.Tools.Install, "-d", "-m", "2770", "-o", "root", "-g", UnixGroup, "--", dir); err != nil {
			return res, err
		} else if r.ExitCode != 0 {
			return res, fail("install -d", r)
		}
	}
	// «Предыдущие версии» по SMB — только при установленном модуле.
	p.ShadowCopy = e.hasShadowCopy()
	if !p.ShadowCopy && len(p.Shares) > 0 {
		res.Warnings = append(res.Warnings, "модуль Samba vfs_shadow_copy2 не найден (пакет samba-vfs-modules?) — снимки не видны по SMB как «Предыдущие версии»")
	}
	data := []byte(RenderParams(e.MountRoot, p))
	oldNB := netbiosOf(e.IncludeFile)
	tmp := e.IncludeFile + ".new"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return res, fmt.Errorf("запись %s: %w", tmp, err)
	}
	if err := e.testAndSwap(ctx, &res, tmp); err != nil {
		os.Remove(tmp)
		return res, err
	}
	if r, err := e.run(ctx, &res, nil, e.Tools.Smbcontrol, "all", "reload-config"); err != nil {
		return res, err
	} else if r.ExitCode != 0 {
		res.Warnings = append(res.Warnings, "smbcontrol reload-config: "+strings.TrimSpace(string(r.Stderr))+" (smbd не запущен?)")
	}
	// Имя сервера меняется только перезапуском (разрыв текущих соединений —
	// администратор сам сменил имя).
	if oldNB != p.Netbios {
		for _, u := range []string{"smbd", "nmbd"} {
			if r, err := e.run(ctx, &res, nil, e.Tools.Systemctl, "try-restart", u); err != nil {
				return res, err
			} else if r.ExitCode != 0 && u == "smbd" {
				res.Warnings = append(res.Warnings, "systemctl try-restart smbd: "+strings.TrimSpace(string(r.Stderr)))
			}
		}
	}
	if e.SmbConf != "" {
		b, err := os.ReadFile(e.SmbConf)
		if err != nil || !strings.Contains(string(b), e.IncludeFile) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("в %s нет строки «include = %s» — шары не видны Samba", e.SmbConf, e.IncludeFile))
		}
		// Штатный [homes] из smb.conf идёт после include и перекрывает его параметры.
		if err == nil && p.Homes != "" && strings.Contains("\n"+string(b), "\n[homes]") {
			res.Warnings = append(res.Warnings, fmt.Sprintf("в %s есть собственный раздел [homes] — он перекрывает личные папки; закомментируйте его", e.SmbConf))
		}
	}
	return res, nil
}

func (e *Executor) testAndSwap(ctx context.Context, res *Result, tmp string) error {
	// testparm разбирает только отдельный файл как smb.conf: проверяет синтаксис шар.
	r, err := e.run(ctx, res, nil, e.Tools.Testparm, "-s", "--suppress-prompt", tmp)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fail("testparm (файл шар отклонён, прежний сохранён)", r)
	}
	return os.Rename(tmp, e.IncludeFile)
}

// netbiosOf — имя сервера из прежнего include-файла (пусто — не задано).
func netbiosOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, netbiosPrefix); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
