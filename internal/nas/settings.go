package nas

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
)

// Настройки сервера в таблице settings — мастер первоначальной
// настройки, имя сервера, домашние папки, защита SSH. Системные действия —
// только операции helper.

const (
	keySetupDone  = "setup.done"
	keyServerName = "server.name"
	keyHomesOn    = "homes.enabled"
	keyHomesPool  = "homes.pool"
	keyGuard      = "sshguard"
)

func (s *Service) kvGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Service) kvSet(ctx context.Context, key, val string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, val)
	return err
}

// ---------- мастер первоначальной настройки ----------

// SetupDone — мастер уже пройден.
func (s *Service) SetupDone(ctx context.Context) (bool, error) {
	v, err := s.kvGet(ctx, keySetupDone)
	return v == "1", err
}

// CompleteSetup отмечает мастер пройденным. Повторно — ErrConflict: второго
// прохода нет, имя и часовой пояс меняются в настройках.
func (s *Service) CompleteSetup(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO settings(key, value) VALUES (?, '1')", keySetupDone)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return wrap(ErrConflict, "первоначальная настройка уже выполнена")
	}
	return s.sshAfterSetup(ctx)
}

// ---------- имя сервера и время ----------

// IdentityInfo — имя сервера и время.
type IdentityInfo struct {
	Hostname  string   `json:"hostname"`
	Timezone  string   `json:"timezone"`
	NTP       bool     `json:"ntp"`
	NTPSynced bool     `json:"ntp_synced"`
	Timezones []string `json:"timezones"`
}

// Identity читает текущие значения через helper.
func (s *Service) Identity(ctx context.Context) (IdentityInfo, error) {
	var id Identity
	if err := s.helper.Call(ctx, OpIdentityGet, SetupParams{}, &id); err != nil {
		return IdentityInfo{}, err
	}
	if id.Timezones == nil {
		id.Timezones = []string{}
	}
	return IdentityInfo{Hostname: id.Hostname, Timezone: id.Timezone, NTP: id.NTP, NTPSynced: id.NTPSynced, Timezones: id.Timezones}, nil
}

// SetIdentity задаёт имя сервера (hostnamectl, имя Samba, avahi), часовой
// пояс и NTP. Возвращает предупреждения (например, имя Samba не применено
// из-за несмонтированного пула — оно применится при следующем применении).
func (s *Service) SetIdentity(ctx context.Context, hostname, timezone string, ntp bool) ([]string, error) {
	if !ValidHostname(hostname) {
		return nil, wrap(ErrInvalid, "имя сервера: строчные латинские буквы, цифры и дефис, до 15 символов, первая — буква")
	}
	tz := TimezoneParams{Timezone: timezone, NTP: ntp}
	if err := tz.Validate(); err != nil {
		return nil, wrap(ErrInvalid, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var warns []string
	var r Result
	if err := s.helper.Call(ctx, OpTimezoneSet, tz, &r); err != nil {
		return nil, err
	}
	if err := s.helper.Call(ctx, OpHostnameSet, HostnameParams{Hostname: hostname}, &r); err != nil {
		return nil, err
	}
	warns = append(warns, r.Warnings...)
	if err := s.kvSet(ctx, keyServerName, hostname); err != nil {
		return warns, err
	}
	if res, err := s.apply(ctx); err != nil {
		warns = append(warns, "имя сервера Samba не применено: "+err.Error())
	} else {
		warns = append(warns, res.Warnings...)
	}
	return warns, nil
}

// ---------- домашние папки ----------

// HomesInfo — служба «домашние папки».
type HomesInfo struct {
	Enabled bool   `json:"enabled"`
	Pool    string `json:"pool"`
	// Warnings — после изменения: например, у кого домашний каталог ОС не
	// изменён из-за открытого сеанса (догоняется при следующем запуске).
	Warnings []string `json:"warnings,omitempty"`
}

// Homes — текущее состояние службы.
func (s *Service) Homes(ctx context.Context) (HomesInfo, error) {
	on, err := s.kvGet(ctx, keyHomesOn)
	if err != nil {
		return HomesInfo{}, err
	}
	pool, err := s.kvGet(ctx, keyHomesPool)
	return HomesInfo{Enabled: on == "1" && pool != "", Pool: pool}, err
}

func (s *Service) userNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM nas_users ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// applyHomes приводит домашние каталоги указанных пользователей к состоянию
// службы (выключена — ничего не делает). Вызывать под s.mu.
func (s *Service) applyHomes(ctx context.Context, users []string) error {
	h, err := s.Homes(ctx)
	if err != nil || !h.Enabled || len(users) == 0 {
		return err
	}
	var r Result
	return s.helper.Call(ctx, OpHomesApply, HomesParams{Enabled: true, Pool: h.Pool, Users: users}, &r)
}

// reconcileHomes (старт gamayund) приводит домашние каталоги всех учёток к
// состоянию службы в обе стороны: включена — homes/<user>, выключена — без
// дома (догоняет учётки, которых не удалось изменить из-за открытого сеанса).
func (s *Service) reconcileHomes(ctx context.Context, users []string) error {
	h, err := s.Homes(ctx)
	if err != nil || len(users) == 0 {
		return err
	}
	var r Result
	return s.helper.Call(ctx, OpHomesApply, HomesParams{Enabled: h.Enabled, Pool: h.Pool, Users: users}, &r)
}

// SetHomes включает службу на пуле или выключает. Включение: каталоги
// homes/<user> у всех пользователей, домашний каталог учёток ОС — туда,
// ресурс [homes] в Samba. Выключение: учётки снова без дома; данные на
// пуле остаются. Смена пула при включённой службе — только через выключение
// (данные не переносятся).
func (s *Service) SetHomes(ctx context.Context, enabled bool, pool string) (HomesInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.Homes(ctx)
	if err != nil {
		return cur, err
	}
	users, err := s.userNames(ctx)
	if err != nil {
		return cur, err
	}
	if enabled {
		if !poolRe.MatchString(pool) {
			return cur, wrap(ErrInvalid, "выберите пул для личных папок")
		}
		if err := s.exists(ctx, "pools", pool, "пул"); err != nil {
			return cur, err
		}
		if cur.Enabled && cur.Pool != pool {
			return cur, wrap(ErrConflict, "служба включена на пуле %s: сначала выключите её (данные не переносятся)", cur.Pool)
		}
		var r Result
		if err := s.helper.Call(ctx, OpHomesApply, HomesParams{Enabled: true, Pool: pool, Users: users}, &r); err != nil {
			return cur, err
		}
		if err := s.kvSet(ctx, keyHomesPool, pool); err != nil {
			return cur, err
		}
		if err := s.kvSet(ctx, keyHomesOn, "1"); err != nil {
			return cur, err
		}
		if err := s.applyLocked(ctx); err != nil {
			s.kvSet(ctx, keyHomesOn, "0") // откат: службы нет
			return HomesInfo{Pool: pool}, err
		}
		return HomesInfo{Enabled: true, Pool: pool, Warnings: r.Warnings}, nil
	}
	if !cur.Enabled {
		return cur, nil
	}
	if err := s.kvSet(ctx, keyHomesOn, "0"); err != nil {
		return cur, err
	}
	if err := s.applyLocked(ctx); err != nil {
		s.kvSet(ctx, keyHomesOn, "1")
		return cur, err
	}
	var r Result
	if err := s.helper.Call(ctx, OpHomesApply, HomesParams{Enabled: false, Pool: cur.Pool, Users: users}, &r); err != nil {
		return HomesInfo{Pool: cur.Pool}, err
	}
	return HomesInfo{Pool: cur.Pool, Warnings: r.Warnings}, nil
}

// ---------- защита SSH от перебора ----------

// GuardSettings — параметры защиты (по умолчанию выключена).
type GuardSettings struct {
	Enabled  bool     `json:"enabled"`
	MaxRetry int      `json:"maxretry"`
	FindTime int      `json:"findtime"` // секунды
	BanTime  int      `json:"bantime"`  // секунды
	Ignore   []string `json:"ignore"`
}

// DefaultGuard — значения по умолчанию: выключена, 5 попыток за 10 минут, блокировка на 10 минут.
func DefaultGuard() GuardSettings {
	return GuardSettings{MaxRetry: 5, FindTime: 600, BanTime: 600, Ignore: []string{}}
}

// Guard — сохранённые параметры.
func (s *Service) Guard(ctx context.Context) (GuardSettings, error) {
	g := DefaultGuard()
	v, err := s.kvGet(ctx, keyGuard)
	if err != nil || v == "" {
		return g, err
	}
	if err := json.Unmarshal([]byte(v), &g); err != nil {
		return DefaultGuard(), err
	}
	if g.Ignore == nil {
		g.Ignore = []string{}
	}
	return g, nil
}

// GuardState — параметры и текущее состояние fail2ban.
func (s *Service) GuardState(ctx context.Context) (GuardSettings, GuardStatus, error) {
	g, err := s.Guard(ctx)
	if err != nil {
		return g, GuardStatus{}, err
	}
	var st GuardStatus
	if err := s.helper.Call(ctx, OpGuardStatus, SetupParams{}, &st); err != nil {
		return g, st, err
	}
	if st.Banned == nil {
		st.Banned = []string{}
	}
	return g, st, nil
}

// SetGuard применяет параметры через helper и сохраняет их. При включении
// адрес клиента, который это делает, добавляется в исключения, чтобы
// администратор не заблокировал сам себя.
func (s *Service) SetGuard(ctx context.Context, g GuardSettings, clientIP string) (GuardSettings, error) {
	if g.Ignore == nil {
		g.Ignore = []string{}
	}
	seen := map[string]bool{}
	var ig []string
	for _, a := range g.Ignore {
		a = strings.TrimSpace(a)
		if a != "" && !seen[a] {
			seen[a] = true
			ig = append(ig, a)
		}
	}
	if ip, err := netip.ParseAddr(clientIP); g.Enabled && err == nil && !ip.IsLoopback() && !seen[ip.String()] {
		ig = append(ig, ip.String())
	}
	g.Ignore = ig
	if g.Ignore == nil {
		g.Ignore = []string{}
	}
	p := GuardParams{Enabled: g.Enabled, MaxRetry: g.MaxRetry, FindTime: g.FindTime, BanTime: g.BanTime, Ignore: g.Ignore}
	if err := p.Validate(); err != nil {
		return g, wrap(ErrInvalid, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Result
	if err := s.helper.Call(ctx, OpGuardApply, p, &r); err != nil {
		return g, err
	}
	b, _ := json.Marshal(g)
	return g, s.kvSet(ctx, keyGuard, string(b))
}

// Unban снимает блокировку адреса.
func (s *Service) Unban(ctx context.Context, ip string) error {
	p := UnbanParams{IP: ip}
	if err := p.Validate(); err != nil {
		return wrap(ErrInvalid, "%v", err)
	}
	var r Result
	return s.helper.Call(ctx, OpGuardUnban, p, &r)
}

// applyGuard — приведение fail2ban к сохранённым параметрам (старт gamayund).
func (s *Service) applyGuard(ctx context.Context) error {
	g, err := s.Guard(ctx)
	if err != nil {
		return err
	}
	var r Result
	return s.helper.Call(ctx, OpGuardApply, GuardParams{Enabled: g.Enabled, MaxRetry: g.MaxRetry, FindTime: g.FindTime, BanTime: g.BanTime, Ignore: g.Ignore}, &r)
}
