package nas

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Доступ учёток NAS к ОС — SSH и sudo.

const (
	defaultSshdDropIn    = "/etc/ssh/sshd_config.d/50-gamayun-nas.conf"
	defaultSudoersDropIn = "/etc/sudoers.d/gamayun-nas"
	shellNologin         = "/usr/sbin/nologin"
	shellBash            = "/bin/bash"
)

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// UserAccess приводит оболочку и членство в SSHGroup/SudoGroup к заданному
// (идемпотентно: без расхождений команды изменения не запускаются). Только
// для учёток NAS; прочие группы учётки не трогаются.
func (e *Executor) UserAccess(ctx context.Context, p AccessParams) (Result, error) {
	var res Result
	ours, exists, err := e.isOurs(ctx, &res, p.Name)
	if err != nil {
		return res, err
	}
	if !exists || !ours {
		return res, fmt.Errorf("%w: %s не учётка NAS", ErrRefused, p.Name)
	}
	if err := e.ensureGroups(ctx, &res); err != nil {
		return res, err
	}
	r, err := e.run(ctx, &res, nil, e.Tools.Getent, "passwd", p.Name)
	if err != nil {
		return res, err
	}
	f := strings.Split(strings.TrimSpace(string(r.Stdout)), ":")
	if r.ExitCode != 0 || len(f) < 7 {
		return res, fail("getent passwd", r)
	}
	r, err = e.run(ctx, &res, nil, e.Tools.Id, "-nG", "--", p.Name)
	if err != nil {
		return res, err
	}
	if r.ExitCode != 0 {
		return res, fail("id -nG", r)
	}
	has := map[string]bool{}
	for _, g := range strings.Fields(string(r.Stdout)) {
		has[g] = true
	}
	shell := shellNologin
	if p.SSH {
		shell = shellBash
	}
	if f[6] != shell {
		if err := e.must(ctx, &res, "usermod", e.Tools.Usermod, "--shell", shell, "--", p.Name); err != nil {
			return res, err
		}
	}
	for _, g := range []struct {
		name string
		want bool
	}{{SSHGroup, p.SSH}, {SudoGroup, p.Sudo}} {
		switch {
		case g.want && !has[g.name]:
			err = e.must(ctx, &res, "usermod -aG", e.Tools.Usermod, "--append", "--groups", g.name, "--", p.Name)
		case !g.want && has[g.name]:
			err = e.must(ctx, &res, "gpasswd -d", e.Tools.Gpasswd, "--delete", p.Name, g.name)
		}
		if err != nil {
			return res, err
		}
	}
	// Samba: отключение/включение записи; ненулевой код допустим, если
	// записи Samba ещё нет (пароль не задавался) — входить всё равно нечем.
	flag := "-e"
	if p.SambaDisabled {
		flag = "-d"
	}
	if _, err := e.run(ctx, &res, nil, e.Tools.Smbpasswd, flag, "--", p.Name); err != nil {
		return res, err
	}
	if p.SambaDisabled {
		if err := e.closeSMBSessions(ctx, &res, p.Name); err != nil {
			return res, err
		}
	}
	return res, nil
}

// closeSMBSessions завершает открытые сессии Samba пользователя: PID из
// `smbstatus -p -u`, затем `smbcontrol <pid> shutdown`. pkill -u не годится:
// smbd между запросами работает с effective uid root.
func (e *Executor) closeSMBSessions(ctx context.Context, res *Result, name string) error {
	r, err := e.run(ctx, res, nil, e.Tools.Smbstatus, "-p", "-u", name)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fail("smbstatus", r)
	}
	for _, pid := range smbSessionPIDs(r.Stdout, name) {
		if err := e.must(ctx, res, "smbcontrol shutdown", e.Tools.Smbcontrol, pid, "shutdown"); err != nil {
			return err
		}
	}
	return nil
}

// smbSessionPIDs — PID строк `smbstatus -p`, где пользователь — name.
func smbSessionPIDs(out []byte, name string) []string {
	var pids []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || f[1] != name {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err == nil {
			pids = append(pids, f[0])
		}
	}
	return pids
}

// must выполняет изменяющую команду; ненулевой код — ошибка.
func (e *Executor) must(ctx context.Context, res *Result, what, path string, args ...string) error {
	r, err := e.run(ctx, res, nil, path, args...)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fail(what, r)
	}
	res.Changed = true
	return nil
}

func (e *Executor) ensureGroups(ctx context.Context, res *Result) error {
	for _, g := range []string{SSHGroup, SudoGroup} {
		r, err := e.run(ctx, res, nil, e.Tools.Getent, "group", g)
		if err != nil {
			return err
		}
		if r.ExitCode == 0 {
			continue
		}
		if err := e.must(ctx, res, "groupadd", e.Tools.Groupadd, "--force", "--system", g); err != nil {
			return err
		}
	}
	return nil
}

// AccessSetup создаёт группы доступа и drop-in sudoers (проверка visudo -c
// до замены). Drop-in sshd — SSHApply.
func (e *Executor) AccessSetup(ctx context.Context, _ SetupParams) (Result, error) {
	var res Result
	if err := e.ensureGroups(ctx, &res); err != nil {
		return res, err
	}
	sudo := fmt.Sprintf("# Управляется gamayun-nas, не редактировать.\n%%%s ALL=(ALL:ALL) ALL\n", SudoGroup)
	if err := e.swapChecked(ctx, &res, or(e.SudoersDropIn, defaultSudoersDropIn), []byte(sudo), 0o440,
		func(tmp string) error {
			return e.must(ctx, &res, "visudo -c", e.Tools.Visudo, "-c", "-q", "-f", tmp)
		}, nil); err != nil {
		return res, err
	}
	return res, nil
}

// swapChecked заменяет файл path содержимым data, если оно отличается.
// pre проверяет временный файл до замены; post — систему после замены
// (ошибка — прежнее содержимое возвращается).
func (e *Executor) swapChecked(ctx context.Context, res *Result, path string, data []byte, mode os.FileMode,
	pre func(tmp string) error, post func() error) error {
	old, err := os.ReadFile(path)
	if err == nil && bytes.Equal(old, data) {
		return nil
	}
	had := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Точка в имени: sudoers.d и sshd_config.d/*.conf временный файл не читают.
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new")
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("запись %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if pre != nil {
		if err := pre(tmp); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	res.Changed = true
	if post == nil {
		return nil
	}
	if err := post(); err != nil {
		if had {
			os.WriteFile(path, old, mode)
		} else {
			os.Remove(path)
		}
		return fmt.Errorf("%s отклонён, прежнее состояние восстановлено: %w", path, err)
	}
	return nil
}
