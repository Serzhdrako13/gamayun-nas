package nas

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Имя сервера, часовой пояс, домашние каталоги в хранилище
// и защита SSH (fail2ban). Все операции — фиксированные, параметры строго
// проверяются; конфигурации проверяются до применения, при ошибке прежнее
// состояние возвращается.

const (
	// HomesShare — имя ресурса и каталога служб домашних папок; общая папка
	// с таким именем создаваться не может.
	HomesShare = "homes"
	// HomeNone — домашний каталог учётки без дома (службы нет).
	HomeNone = "/nonexistent"

	defaultHostsFile   = "/etc/hosts"
	defaultGuardDropIn = "/etc/fail2ban/jail.d/gamayun-nas.local"
)

var hostRe = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,13}[a-z0-9])?$`)

// ValidHostname — имя сервера: латиница в нижнем регистре, цифры и дефис,
// до 15 символов (предел имени NetBIOS), начинается с буквы.
func ValidHostname(s string) bool { return hostRe.MatchString(s) }

var tzRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9][A-Za-z0-9_+-]*){0,2}$`)

// HostnameParams — параметры system.hostname.set.
type HostnameParams struct {
	Hostname string `json:"hostname"`
}

// Validate проверяет имя.
func (p HostnameParams) Validate() error {
	if !ValidHostname(p.Hostname) {
		return fmt.Errorf("недопустимое имя сервера %q", p.Hostname)
	}
	return nil
}

// TimezoneParams — параметры system.timezone.set.
type TimezoneParams struct {
	Timezone string `json:"timezone"`
	NTP      bool   `json:"ntp"`
}

// Validate проверяет форму имени (принадлежность списку tz — в helper).
func (p TimezoneParams) Validate() error {
	if len(p.Timezone) > 64 || !tzRe.MatchString(p.Timezone) {
		return fmt.Errorf("недопустимый часовой пояс %q", p.Timezone)
	}
	return nil
}

// Identity — имя сервера и время (system.identity.get).
type Identity struct {
	Hostname  string   `json:"hostname"`
	Timezone  string   `json:"timezone"`
	NTP       bool     `json:"ntp"`
	NTPSynced bool     `json:"ntp_synced"`
	Timezones []string `json:"timezones,omitempty"`
	Result
}

func (e *Executor) out(ctx context.Context, res *Result, path string, args ...string) (string, error) {
	r, err := e.run(ctx, res, nil, path, args...)
	if err != nil {
		return "", err
	}
	if r.ExitCode != 0 {
		return "", fail(filepath.Base(path), r)
	}
	return strings.TrimSpace(string(r.Stdout)), nil
}

func (e *Executor) timedate(ctx context.Context, res *Result) (tz string, ntp, synced bool, err error) {
	o, err := e.out(ctx, res, e.Tools.Timedatectl, "show", "--property=Timezone", "--property=NTP", "--property=NTPSynchronized")
	if err != nil {
		return
	}
	for _, l := range strings.Split(o, "\n") {
		k, v, _ := strings.Cut(l, "=")
		switch k {
		case "Timezone":
			tz = v
		case "NTP":
			ntp = v == "yes"
		case "NTPSynchronized":
			synced = v == "yes"
		}
	}
	return
}

// IdentityGet — текущие имя сервера, часовой пояс, NTP и список поясов (только чтение).
func (e *Executor) IdentityGet(ctx context.Context, _ SetupParams) (Identity, error) {
	var id Identity
	var err error
	if id.Hostname, err = e.out(ctx, &id.Result, e.Tools.Hostnamectl, "--static"); err != nil {
		return id, err
	}
	if id.Timezone, id.NTP, id.NTPSynced, err = e.timedate(ctx, &id.Result); err != nil {
		return id, err
	}
	l, err := e.out(ctx, &id.Result, e.Tools.Timedatectl, "list-timezones")
	if err != nil {
		return id, err
	}
	id.Timezones = strings.Fields(l)
	return id, nil
}

// HostnameSet задаёт статическое имя хоста (hostnamectl), строку 127.0.1.1 в
// /etc/hosts и перезапускает avahi, если он запущен. Без изменений ничего
// не запускает. Если hostnamectl отказал, /etc/hosts возвращается.
func (e *Executor) HostnameSet(ctx context.Context, p HostnameParams) (Result, error) {
	var res Result
	cur, err := e.out(ctx, &res, e.Tools.Hostnamectl, "--static")
	if err != nil {
		return res, err
	}
	if cur == p.Hostname {
		return res, nil
	}
	hosts := or(e.HostsFile, defaultHostsFile)
	old, err := os.ReadFile(hosts)
	if err != nil && !os.IsNotExist(err) {
		return res, err
	}
	if err := e.swapChecked(ctx, &res, hosts, withHostsLine(old, p.Hostname), 0o644, nil, nil); err != nil {
		return res, err
	}
	if err := e.must(ctx, &res, "hostnamectl set-hostname", e.Tools.Hostnamectl, "set-hostname", "--static", p.Hostname); err != nil {
		if old != nil {
			os.WriteFile(hosts, old, 0o644) // откат /etc/hosts
		}
		return res, err
	}
	// avahi берёт имя из системы: перезапуск только если служба активна.
	if r, err := e.run(ctx, &res, nil, e.Tools.Systemctl, "is-active", "--quiet", "avahi-daemon"); err != nil {
		return res, err
	} else if r.ExitCode == 0 {
		if r, err := e.run(ctx, &res, nil, e.Tools.Systemctl, "restart", "avahi-daemon"); err != nil {
			return res, err
		} else if r.ExitCode != 0 {
			res.Warnings = append(res.Warnings, "avahi-daemon не перезапущен: "+strings.TrimSpace(string(r.Stderr)))
		}
	}
	return res, nil
}

// withHostsLine заменяет (или добавляет) строку 127.0.1.1, остальное не трогает.
func withHostsLine(old []byte, name string) []byte {
	var out []string
	done := false
	for _, l := range strings.Split(strings.TrimRight(string(old), "\n"), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "127.0.1.1" {
			if !done {
				out = append(out, "127.0.1.1\t"+name)
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	if len(old) == 0 {
		out = nil
	}
	if !done {
		out = append(out, "127.0.1.1\t"+name)
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// TimezoneSet задаёт часовой пояс (только из списка timedatectl) и NTP.
func (e *Executor) TimezoneSet(ctx context.Context, p TimezoneParams) (Result, error) {
	var res Result
	l, err := e.out(ctx, &res, e.Tools.Timedatectl, "list-timezones")
	if err != nil {
		return res, err
	}
	known := false
	for _, z := range strings.Fields(l) {
		if z == p.Timezone {
			known = true
			break
		}
	}
	if !known {
		return res, fmt.Errorf("%w: неизвестный часовой пояс %q", ErrRefused, p.Timezone)
	}
	tz, ntp, _, err := e.timedate(ctx, &res)
	if err != nil {
		return res, err
	}
	if tz != p.Timezone {
		if err := e.must(ctx, &res, "timedatectl set-timezone", e.Tools.Timedatectl, "set-timezone", p.Timezone); err != nil {
			return res, err
		}
	}
	if ntp != p.NTP {
		if err := e.must(ctx, &res, "timedatectl set-ntp", e.Tools.Timedatectl, "set-ntp", strconv.FormatBool(p.NTP)); err != nil {
			return res, err
		}
	}
	return res, nil
}

// ---------- домашние каталоги в хранилище ----------

// HomesDir — каталог службы домашних папок: <mount_root>/<пул>/shares/homes.
func HomesDir(mountRoot, pool string) string { return ShareDir(mountRoot, pool, HomesShare) }

// HomesParams — параметры nas.homes.apply.
type HomesParams struct {
	Enabled bool     `json:"enabled"`
	Pool    string   `json:"pool"`
	Users   []string `json:"users"`
}

// Validate проверяет значения.
func (p HomesParams) Validate() error {
	if p.Enabled && !poolRe.MatchString(p.Pool) {
		return errors.New("личные папки: недопустимое имя пула")
	}
	if !p.Enabled && p.Pool != "" && !poolRe.MatchString(p.Pool) {
		return errors.New("личные папки: недопустимое имя пула")
	}
	if len(p.Users) > 1024 {
		return errors.New("слишком много пользователей")
	}
	for _, u := range p.Users {
		if !ValidName(u) {
			return fmt.Errorf("недопустимое имя пользователя %q", u)
		}
	}
	return nil
}

func (e *Executor) homeOf(ctx context.Context, res *Result, name string) (string, error) {
	r, err := e.run(ctx, res, nil, e.Tools.Getent, "passwd", name)
	if err != nil {
		return "", err
	}
	f := strings.Split(strings.TrimSpace(string(r.Stdout)), ":")
	if r.ExitCode != 0 || len(f) < 7 {
		return "", fail("getent passwd", r)
	}
	return f[5], nil
}

// HomesApply включает или выключает службу. Включение: каталог homes
// (0711, root:gamayun-users) и homes/<user> (0700, владелец — пользователь)
// на пуле, домашний каталог учётки ОС — туда. Выключение: домашний каталог
// учёток снова HomeNone; данные в homes не удаляются. Чужие учётки не
// трогаются; несуществующие пропускаются с предупреждением.
func (e *Executor) HomesApply(ctx context.Context, p HomesParams) (Result, error) {
	var res Result
	var base string
	if p.Enabled {
		pool := filepath.Join(e.MountRoot, p.Pool)
		if !e.poolMounted(p.Pool) {
			return res, fmt.Errorf("%w: пул %s не смонтирован (%s)", ErrRefused, p.Pool, pool)
		}
		base = HomesDir(e.MountRoot, p.Pool)
		if _, err := os.Stat(base); err != nil {
			res.Changed = true
		}
		if err := e.must2(ctx, &res, "install -d", e.Tools.Install, "-d", "-m", "2770", "-o", "root", "-g", UnixGroup, "--", filepath.Dir(base)); err != nil {
			return res, err
		}
		// Каталог службы — подтом Btrfs (снапшоты домашних папок).
		if err := e.ensureSubvolume(ctx, &res, base); err != nil {
			return res, err
		}
		if err := e.must2(ctx, &res, "install -d", e.Tools.Install, "-d", "-m", "0711", "-o", "root", "-g", UnixGroup, "--", base); err != nil {
			return res, err
		}
	}
	for _, u := range p.Users {
		ours, exists, err := e.isOurs(ctx, &res, u)
		if err != nil {
			return res, err
		}
		if !exists || !ours {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: нет учётки NAS, пропущена", u))
			continue
		}
		want := HomeNone
		if p.Enabled {
			want = filepath.Join(base, u)
			if _, err := os.Stat(want); err != nil {
				res.Changed = true
			}
			if err := e.must2(ctx, &res, "install -d", e.Tools.Install, "-d", "-m", "0700", "-o", u, "-g", UnixGroup, "--", want); err != nil {
				return res, err
			}
		}
		cur, err := e.homeOf(ctx, &res, u)
		if err != nil {
			return res, err
		}
		if cur != want {
			// usermod отказывает (код 8), пока у учётки есть процессы — открыт
			// сеанс SSH или соединение SMB. Это не ошибка службы: Samba берёт
			// путь из своей конфигурации, а домашний каталог ОС выровняется
			// при следующем применении (старт gamayund, повторное включение).
			r, err := e.run(ctx, &res, nil, e.Tools.Usermod, "--home", want, "--", u)
			if err != nil {
				return res, err
			}
			switch r.ExitCode {
			case 0:
				res.Changed = true
			case 8:
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: домашний каталог ОС не изменён — пользователь сейчас работает в системе; применится при следующем запуске", u))
			default:
				return res, fail("usermod --home", r)
			}
		}
	}
	return res, nil
}

// must2 — команда без отметки «изменено» (идемпотентные install -d).
func (e *Executor) must2(ctx context.Context, res *Result, what, path string, args ...string) error {
	r, err := e.run(ctx, res, nil, path, args...)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fail(what, r)
	}
	return nil
}

// ---------- защита SSH от перебора (fail2ban) ----------

// GuardParams — параметры ssh.guard.apply.
type GuardParams struct {
	Enabled  bool     `json:"enabled"`
	MaxRetry int      `json:"maxretry"` // 1..20
	FindTime int      `json:"findtime"` // секунды, 60..86400
	BanTime  int      `json:"bantime"`  // секунды, 60..604800
	Ignore   []string `json:"ignore"`   // IP и сети, которые не блокируются
}

// Validate проверяет диапазоны и адреса: значения попадают в конфигурацию fail2ban.
func (p GuardParams) Validate() error {
	if p.MaxRetry < 1 || p.MaxRetry > 20 {
		return errors.New("число попыток: от 1 до 20")
	}
	if p.FindTime < 60 || p.FindTime > 86400 {
		return errors.New("окно: от 60 до 86400 секунд")
	}
	if p.BanTime < 60 || p.BanTime > 604800 {
		return errors.New("время блокировки: от 60 секунд до 7 суток")
	}
	if len(p.Ignore) > 32 {
		return errors.New("слишком много исключений")
	}
	for _, a := range p.Ignore {
		if !validNet(a) {
			return fmt.Errorf("недопустимый адрес или сеть %q", a)
		}
	}
	return nil
}

func validNet(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	_, err := netip.ParsePrefix(s)
	return err == nil
}

// UnbanParams — параметры ssh.guard.unban.
type UnbanParams struct {
	IP string `json:"ip"`
}

// Validate — только одиночный адрес.
func (p UnbanParams) Validate() error {
	if _, err := netip.ParseAddr(p.IP); err != nil {
		return fmt.Errorf("недопустимый адрес %q", p.IP)
	}
	return nil
}

// GuardStatus — состояние защиты (ssh.guard.status).
type GuardStatus struct {
	Installed bool     `json:"installed"`
	Active    bool     `json:"active"`
	Banned    []string `json:"banned"`
	Result
}

func (e *Executor) guardInstalled() bool {
	st, err := os.Stat(e.Tools.Fail2ban)
	return err == nil && !st.IsDir()
}

func (e *Executor) unitActive(ctx context.Context, res *Result, unit string) (bool, error) {
	r, err := e.run(ctx, res, nil, e.Tools.Systemctl, "is-active", "--quiet", unit)
	return err == nil && r.ExitCode == 0, err
}

// GuardApply записывает drop-in jail sshd и включает/выключает службу.
// Адреса 127.0.0.0/8 и ::1 всегда в исключениях. Конфигурация проверяется
// fail2ban-client -t после записи; при ошибке прежний файл возвращается.
// Выключено и fail2ban не установлен — ничего не делает.
func (e *Executor) GuardApply(ctx context.Context, p GuardParams) (Result, error) {
	var res Result
	if !e.guardInstalled() {
		if p.Enabled {
			return res, fmt.Errorf("%w: fail2ban не установлен", ErrRefused)
		}
		return res, nil
	}
	ig := append([]string{"127.0.0.0/8", "::1"}, p.Ignore...)
	conf := fmt.Sprintf("# Управляется gamayun-nas, не редактировать: меняется из веб-интерфейса.\n[sshd]\nenabled = %t\n"+
		"maxretry = %d\nfindtime = %ds\nbantime = %ds\nignoreip = %s\n"+
		// OpenSSH 10 (Debian 13) пишет вход из процесса sshd-session; штатный
		// journalmatch Debian знает только sshd и не видит ни одной неудачи.
		"backend = systemd\njournalmatch = _SYSTEMD_UNIT=ssh.service _COMM=sshd _COMM=sshd-session\n", p.Enabled, p.MaxRetry, p.FindTime, p.BanTime, strings.Join(ig, " "))
	path := or(e.GuardDropIn, defaultGuardDropIn)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, err
	}
	if err := e.swapChecked(ctx, &res, path, []byte(conf), 0o644, nil, func() error {
		return e.must(ctx, &res, "fail2ban-client -t", e.Tools.Fail2ban, "-t")
	}); err != nil {
		return res, err
	}
	active, err := e.unitActive(ctx, &res, "fail2ban")
	if err != nil {
		return res, err
	}
	switch {
	case p.Enabled && !active:
		if err := e.must(ctx, &res, "systemctl enable --now fail2ban", e.Tools.Systemctl, "enable", "--now", "fail2ban"); err != nil {
			return res, err
		}
	case p.Enabled && res.Changed:
		if err := e.must(ctx, &res, "fail2ban-client reload", e.Tools.Fail2ban, "reload"); err != nil {
			return res, err
		}
	}
	if p.Enabled {
		// Служба стартует не мгновенно: ждём, пока jail ответит (до ~10 с).
		up := false
		for i := 0; i < 20 && !up; i++ {
			if r, err := e.run(ctx, &res, nil, e.Tools.Fail2ban, "status", "sshd"); err == nil && r.ExitCode == 0 {
				up = true
			} else {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
		if !up {
			res.Warnings = append(res.Warnings, "fail2ban запущен, но jail sshd пока не отвечает")
		}
	}
	switch {
	case !p.Enabled && active:
		if err := e.must(ctx, &res, "systemctl disable --now fail2ban", e.Tools.Systemctl, "disable", "--now", "fail2ban"); err != nil {
			return res, err
		}
	}
	return res, nil
}

// GuardStatus — состояние службы и список заблокированных адресов (только чтение).
func (e *Executor) GuardStatus(ctx context.Context, _ SetupParams) (GuardStatus, error) {
	var st GuardStatus
	st.Banned = []string{}
	if !e.guardInstalled() {
		return st, nil
	}
	st.Installed = true
	active, err := e.unitActive(ctx, &st.Result, "fail2ban")
	if err != nil || !active {
		return st, err
	}
	r, err := e.run(ctx, &st.Result, nil, e.Tools.Fail2ban, "status", "sshd")
	if err != nil {
		return st, err
	}
	if r.ExitCode != 0 { // jail не поднят
		return st, nil
	}
	st.Active = true
	for _, l := range strings.Split(string(r.Stdout), "\n") {
		if _, v, ok := strings.Cut(l, "Banned IP list:"); ok {
			for _, a := range strings.Fields(v) {
				if _, err := netip.ParseAddr(a); err == nil {
					st.Banned = append(st.Banned, a)
				}
			}
		}
	}
	return st, nil
}

// GuardUnban снимает блокировку адреса.
func (e *Executor) GuardUnban(ctx context.Context, p UnbanParams) (Result, error) {
	var res Result
	if !e.guardInstalled() {
		return res, fmt.Errorf("%w: fail2ban не установлен", ErrRefused)
	}
	return res, e.must(ctx, &res, "fail2ban-client unbanip", e.Tools.Fail2ban, "set", "sshd", "unbanip", p.IP)
}

// PowerParams — параметры system.power.
type PowerParams struct {
	Action string `json:"action"` // poweroff | reboot
}

// Validate допускает только выключение и перезагрузку.
func (p PowerParams) Validate() error {
	if p.Action != "poweroff" && p.Action != "reboot" {
		return fmt.Errorf("action: poweroff|reboot, получено %q", p.Action)
	}
	return nil
}

// Power ставит в systemd задание выключения или перезагрузки и сразу
// возвращается (--no-block): ответ успевает уйти до остановки служб.
func (e *Executor) Power(ctx context.Context, p PowerParams) (Result, error) {
	var res Result
	err := e.must(ctx, &res, "systemctl --no-block "+p.Action, e.Tools.Systemctl, "--no-block", p.Action)
	return res, err
}

// Power — выключение или перезагрузка через helper.
func (s *Service) Power(ctx context.Context, action string) error {
	p := PowerParams{Action: action}
	if err := p.Validate(); err != nil {
		return wrap(ErrInvalid, "%v", err)
	}
	var r Result
	return s.helper.Call(ctx, OpPower, p, &r)
}
