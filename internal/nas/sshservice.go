package nas

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Служба SSH из веба. После мастера первоначальной настройки
// выключена; входят только члены SSHGroup, root — без входа.
//
// Режим разработки (ssh_dev_access в конфигурации helper, только вручную на
// сервере): перечисленные учётки входят по ключу (root — prohibit-password),
// а служба не останавливается — «выключено» закрывает вход только учёткам NAS.
// Так тестовый сервер не теряет доступ по ключу.

// SSHParams — параметры ssh.service.apply.
type SSHParams struct {
	Enabled bool `json:"enabled"`
	// KillSessions — при выключении разорвать открытые сессии учёток NAS
	// (по умолчанию они остаются, закрываются только новые входы).
	KillSessions bool `json:"kill_sessions"`
}

// Validate всегда успешна.
func (SSHParams) Validate() error { return nil }

// SSHStatus — состояние службы (ssh.service.status).
type SSHStatus struct {
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
	// DevAccess — включён режим разработки (служба не останавливается).
	DevAccess bool `json:"dev_access"`
	Result
}

func (e *Executor) sshdInstalled() bool {
	if e.SSHInstalled != nil {
		return e.SSHInstalled()
	}
	st, err := os.Stat(e.Tools.Sshd)
	return err == nil && !st.IsDir()
}

// devGroups — первичные группы учёток режима разработки (существующих).
func (e *Executor) devGroups() ([]string, bool, error) {
	if len(e.SSHDev) == 0 {
		return nil, false, nil
	}
	gb, err := os.ReadFile(or(e.GroupFile, "/etc/group"))
	if err != nil {
		return nil, false, err
	}
	gname := map[string]string{}
	for _, l := range strings.Split(string(gb), "\n") {
		if f := strings.Split(l, ":"); len(f) >= 3 {
			gname[f[2]] = f[0]
		}
	}
	want := map[string]bool{}
	for _, u := range e.SSHDev {
		want[u] = true
	}
	pf, err := os.Open(or(e.PasswdFile, "/etc/passwd"))
	if err != nil {
		return nil, false, err
	}
	defer pf.Close()
	set := map[string]bool{}
	root := false
	sc := bufio.NewScanner(pf)
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 7 || !want[f[0]] {
			continue
		}
		if g := gname[f[3]]; g != "" && g != UnixGroup && nameRe.MatchString(g) {
			set[g] = true
			root = root || f[2] == "0"
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, root, sc.Err()
}

// sshdConf — содержимое drop-in sshd.
func sshdConf(enabled bool, dev []string, devRoot bool) string {
	allow := []string{}
	if enabled {
		allow = append(allow, SSHGroup)
	}
	allow = append(allow, dev...)
	if len(allow) == 0 {
		allow = []string{SSHGroup} // служба остановлена; строка для валидного конфига
	}
	root := "no"
	if devRoot {
		root = "prohibit-password"
	}
	// Пароль учётки NAS — единый для веба, SMB и SSH: членам SSHGroup вход по
	// паролю разрешён (у учёток NAS нет домашнего каталога и ключей); прочим —
	// только по ключу. Match действует до конца этого файла.
	return fmt.Sprintf("# Управляется gamayun-nas, не редактировать: меняется из веб-интерфейса.\n"+
		"PermitRootLogin %s\nPasswordAuthentication no\nAllowGroups %s\n"+
		"Match Group %s\n\tPasswordAuthentication yes\n", root, strings.Join(allow, " "), SSHGroup)
}

// SSHApply записывает drop-in sshd (проверка sshd -t, при ошибке прежний файл
// возвращается) и включает/выключает службу. Идемпотентно.
func (e *Executor) SSHApply(ctx context.Context, p SSHParams) (Result, error) {
	var res Result
	if !e.sshdInstalled() {
		if p.Enabled {
			return res, fmt.Errorf("%w: openssh-server не установлен", ErrRefused)
		}
		return res, nil
	}
	if err := e.ensureGroups(ctx, &res); err != nil {
		return res, err
	}
	dev, devRoot, err := e.devGroups()
	if err != nil {
		return res, err
	}
	devMode := len(e.SSHDev) > 0
	path := or(e.SshdDropIn, defaultSshdDropIn)
	if err := e.swapChecked(ctx, &res, path, []byte(sshdConf(p.Enabled, dev, devRoot)), 0o644, nil, func() error {
		return e.must(ctx, &res, "sshd -t", e.Tools.Sshd, "-t")
	}); err != nil {
		return res, err
	}
	changedConf := res.Changed
	active, err := e.unitActive(ctx, &res, "ssh")
	if err != nil {
		return res, err
	}
	run := p.Enabled || devMode
	switch {
	case run && !active:
		if err := e.must(ctx, &res, "systemctl enable --now ssh", e.Tools.Systemctl, "enable", "--now", "ssh"); err != nil {
			return res, err
		}
		res.Changed = true
	case run && changedConf:
		if err := e.must(ctx, &res, "systemctl reload ssh", e.Tools.Systemctl, "reload", "ssh"); err != nil {
			return res, err
		}
	case !run:
		if active {
			if err := e.must(ctx, &res, "systemctl disable --now ssh", e.Tools.Systemctl, "disable", "--now", "ssh"); err != nil {
				return res, err
			}
			res.Changed = true
		}
		// Активация по сокету (если включена) снова подняла бы sshd.
		if r, err := e.run(ctx, &res, nil, e.Tools.Systemctl, "is-enabled", "--quiet", "ssh.socket"); err == nil && r.ExitCode == 0 {
			if err := e.must(ctx, &res, "systemctl disable --now ssh.socket", e.Tools.Systemctl, "disable", "--now", "ssh.socket"); err != nil {
				return res, err
			}
			res.Changed = true
		}
	}
	if run {
		// sshd -T печатает prohibit-password (старые версии — without-password).
		r, err := e.run(ctx, &res, nil, e.Tools.Sshd, "-T")
		if err == nil && r.ExitCode == 0 {
			ok := bytes.Contains(r.Stdout, []byte("permitrootlogin no"))
			if devRoot {
				ok = bytes.Contains(r.Stdout, []byte("permitrootlogin prohibit-password")) ||
					bytes.Contains(r.Stdout, []byte("permitrootlogin without-password"))
			}
			if !ok {
				res.Warnings = append(res.Warnings, fmt.Sprintf("sshd не применяет %s (нет Include sshd_config.d в начале sshd_config?)", path))
			}
		}
	}
	if !p.Enabled && p.KillSessions {
		// Сессия учётки NAS — процесс sshd-session с её первичной группой
		// (UnixGroup); учётки режима разработки не затрагиваются.
		r, err := e.run(ctx, &res, nil, e.Tools.Pkill, "-KILL", "-G", UnixGroup, "-x", "sshd-session")
		if err != nil {
			return res, err
		}
		switch r.ExitCode {
		case 0:
			res.Changed = true
		case 1: // открытых сессий не было
		default:
			return res, fail("pkill sshd-session", r)
		}
	}
	return res, nil
}

// SSHStatusGet — состояние службы (только чтение).
func (e *Executor) SSHStatusGet(ctx context.Context, _ SetupParams) (SSHStatus, error) {
	st := SSHStatus{DevAccess: len(e.SSHDev) > 0}
	if !e.sshdInstalled() {
		return st, nil
	}
	st.Installed = true
	a, err := e.unitActive(ctx, &st.Result, "ssh")
	st.Active = a
	return st, err
}

// ---------- модель (gamayund) ----------

const keySSH = "ssh.service"

// SSHSettings — сохранённое состояние службы.
type SSHSettings struct {
	Enabled bool `json:"enabled"`
}

// sshSaved — сохранённое значение; ok=false — ещё не задано.
func (s *Service) sshSaved(ctx context.Context) (SSHSettings, bool, error) {
	var c SSHSettings
	v, err := s.kvGet(ctx, keySSH)
	if err != nil || v == "" {
		return c, false, err
	}
	return c, true, json.Unmarshal([]byte(v), &c)
}

// SSHState — сохранённое состояние, состояние службы и число учёток NAS с
// правом SSH (0 — включённой службой никто не сможет войти).
func (s *Service) SSHState(ctx context.Context) (SSHSettings, SSHStatus, int, error) {
	c, _, err := s.sshSaved(ctx)
	if err != nil {
		return c, SSHStatus{}, 0, err
	}
	var st SSHStatus
	if err := s.helper.Call(ctx, OpSSHStatus, SetupParams{}, &st); err != nil {
		return c, st, 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nas_users WHERE ssh = 1 AND enabled = 1").Scan(&n)
	return c, st, n, err
}

// SetSSHService применяет и сохраняет состояние службы.
func (s *Service) SetSSHService(ctx context.Context, enabled, kill bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Result
	if err := s.helper.Call(ctx, OpSSHApply, SSHParams{Enabled: enabled, KillSessions: kill}, &r); err != nil {
		return err
	}
	b, _ := json.Marshal(SSHSettings{Enabled: enabled})
	return s.kvSet(ctx, keySSH, string(b))
}

// applySSH — приведение службы к сохранённому (старт gamayund, под s.mu).
// Значение ещё не задано: до мастера первоначальной настройки служба не
// трогается (идёт установка, администратор может быть в SSH); после него
// (обновление существующей установки) сохраняется текущее состояние.
func (s *Service) applySSH(ctx context.Context) error {
	c, ok, err := s.sshSaved(ctx)
	if err != nil {
		return err
	}
	if !ok {
		done, err := s.SetupDone(ctx)
		if err != nil || !done {
			return err
		}
		var st SSHStatus
		if err := s.helper.Call(ctx, OpSSHStatus, SetupParams{}, &st); err != nil {
			return err
		}
		c.Enabled = st.Active
		b, _ := json.Marshal(c)
		if err := s.kvSet(ctx, keySSH, string(b)); err != nil {
			return err
		}
	}
	var r Result
	return s.helper.Call(ctx, OpSSHApply, SSHParams{Enabled: c.Enabled}, &r)
}

// sshAfterSetup — мастер пройден впервые: служба выключается, если состояние
// ещё не задано.
func (s *Service) sshAfterSetup(ctx context.Context) error {
	if _, ok, err := s.sshSaved(ctx); err != nil || ok {
		return err
	}
	var r Result
	if err := s.helper.Call(ctx, OpSSHApply, SSHParams{}, &r); err != nil {
		return err
	}
	return s.kvSet(ctx, keySSH, `{"enabled":false}`)
}
