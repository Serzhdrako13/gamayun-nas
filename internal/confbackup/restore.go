package confbackup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/notify"
)

// Target — действия восстановления (реализует *nas.Service). Всё
// системное (учётки ОС, Samba, имя, время, fail2ban) идёт через него, т. е.
// через gamayun-helper, как при обычной настройке из веба.
type Target interface {
	CreateUserSSH(ctx context.Context, name, password string, admin, ssh bool) (nas.User, error)
	SetPassword(ctx context.Context, name, password string) error
	SetEnabled(ctx context.Context, name string, enabled bool) error
	CreateGroup(ctx context.Context, name string) (nas.Group, error)
	SetMembers(ctx context.Context, group string, members []string) error
	CreateShare(ctx context.Context, name, pool, comment string) (nas.ShareInfo, error)
	SetAccess(ctx context.Context, share string, entries []nas.Access) error
	SetIdentity(ctx context.Context, hostname, timezone string, ntp bool) ([]string, error)
	SetHomes(ctx context.Context, enabled bool, pool string) (nas.HomesInfo, error)
	SetGuard(ctx context.Context, g nas.GuardSettings, clientIP string) (nas.GuardSettings, error)
	CompleteSetup(ctx context.Context) error
}

// Plan — что сделает восстановление (без изменений, dry-run).
type Plan struct {
	CreatedAt     string       `json:"created_at"`
	AppVersion    string       `json:"app_version"`
	FormatVersion int          `json:"format_version"`
	SchemaVersion int          `json:"schema_version"`
	Hostname      string       `json:"hostname,omitempty"`
	SetupDone     bool         `json:"setup_done"` // система уже настроена (восстановление из веба — только до конца мастера)
	Users         []PlanUser   `json:"users"`
	Groups        []PlanItem   `json:"groups"`
	Shares        []PlanItem   `json:"shares"`
	Secrets       []PlanSecret `json:"secrets"`
	Settings      []string     `json:"settings"`
	Warnings      []string     `json:"warnings"`
}

// PlanUser — пользователь из копии. Пароль нужно задать новый; оставить
// прежний можно только для уже существующей учётки (CanKeep).
type PlanUser struct {
	User
	Exists  bool `json:"exists"`
	CanKeep bool `json:"can_keep"`
}

// PlanItem — группа или папка: Action create | update | skip.
type PlanItem struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// PlanSecret — секрет, который задаёт пользователь (пароль SMTP, ключ
// webhook). Внутренние ключи (сертификат, сессии) в копию не входят и
// создаются системой заново автоматически.
type PlanSecret struct {
	Key     string `json:"key"` // smtp_password | webhook_secret
	Title   string `json:"title"`
	CanKeep bool   `json:"can_keep"` // на этой системе уже задан
}

// Answers — ответы мастера восстановления.
type Answers struct {
	// Passwords — новые пароли пользователей (по умолчанию для всех).
	Passwords map[string]string `json:"passwords"`
	// Keep — оставить прежний пароль существующей учётки / прежний секрет
	// (ключи: имя пользователя, smtp_password, webhook_secret). Требует
	// AcceptInsecure — небезопасно: пароль мог быть причиной восстановления.
	Keep           map[string]bool `json:"keep"`
	AcceptInsecure bool            `json:"accept_insecure"`
	// SMTPPassword, WebhookSecret — новые значения; пусто и не Keep —
	// канал восстанавливается выключенным.
	SMTPPassword  string `json:"smtp_password"`
	WebhookSecret string `json:"webhook_secret"`
	WebhookURL    string `json:"webhook_url"`
}

// String не выводит значения секретов (защита от случайной записи в журнал).
func (a Answers) String() string {
	return fmt.Sprintf("Answers{passwords:%d keep:%d}", len(a.Passwords), len(a.Keep))
}

const (
	SecretSMTP    = "smtp_password"
	SecretWebhook = "webhook_secret"
	// SecretWebhookURL — адрес webhook с токеном в параметрах/учётных данных.
	SecretWebhookURL = "webhook_url"
)

// MakePlan строит план по копии и текущей базе.
func MakePlan(ctx context.Context, db *sql.DB, b Backup, d Data) (Plan, error) {
	p := Plan{CreatedAt: b.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), AppVersion: b.AppVersion,
		FormatVersion: b.FormatVersion, SchemaVersion: b.SchemaVersion, Hostname: b.Hostname,
		Users: []PlanUser{}, Groups: []PlanItem{}, Shares: []PlanItem{}, Secrets: []PlanSecret{},
		Settings: []string{}, Warnings: []string{}}
	kv, err := settingsKV(ctx, db)
	if err != nil {
		return p, err
	}
	p.SetupDone = kv["setup.done"] == "1"
	for _, u := range d.Users {
		ex, err := has(ctx, db, "SELECT COUNT(*) FROM nas_users WHERE name = ?", u.Name)
		if err != nil {
			return p, err
		}
		p.Users = append(p.Users, PlanUser{User: u, Exists: ex, CanKeep: ex})
	}
	for _, g := range d.Groups {
		ex, err := has(ctx, db, "SELECT COUNT(*) FROM nas_groups WHERE name = ?", g.Name)
		if err != nil {
			return p, err
		}
		a := "create"
		if ex || nas.IsSystemGroup(g.Name) {
			a = "update"
		}
		p.Groups = append(p.Groups, PlanItem{Name: g.Name, Action: a})
	}
	for _, s := range d.Shares {
		it := PlanItem{Name: s.Name, Action: "create"}
		if ex, err := has(ctx, db, "SELECT COUNT(*) FROM shares WHERE lower(name) = lower(?)", s.Name); err != nil {
			return p, err
		} else if ex {
			it.Action = "update"
		} else if pe, err := has(ctx, db, "SELECT COUNT(*) FROM pools WHERE name = ?", s.Pool); err != nil {
			return p, err
		} else if !pe {
			it.Action, it.Reason = "skip", "нет пула "+s.Pool
			p.Warnings = append(p.Warnings, fmt.Sprintf("папка %s пропускается: на этой системе нет пула %s", s.Name, s.Pool))
		}
		p.Shares = append(p.Shares, it)
	}
	if d.Settings.ServerName != "" {
		tz := d.Settings.Timezone
		if tz == "" {
			tz = "(не меняется)"
		}
		p.Settings = append(p.Settings, "имя сервера: "+d.Settings.ServerName+", часовой пояс: "+tz)
	}
	if d.Settings.Homes.Enabled {
		p.Settings = append(p.Settings, "личные папки на пуле "+d.Settings.Homes.Pool)
	}
	if d.Settings.SSHGuard != nil {
		p.Settings = append(p.Settings, "защита SSH")
	}
	if d.Maintenance != nil {
		p.Settings = append(p.Settings, "расписание проверок пулов и SMART")
	}
	if d.SMTP != nil {
		p.Settings = append(p.Settings, "уведомления по почте")
		if d.Secrets.SMTPPassword {
			cur, _ := notify.LoadSMTP(ctx, db)
			p.Secrets = append(p.Secrets, PlanSecret{Key: SecretSMTP, Title: "пароль SMTP", CanKeep: cur.Password != ""})
		}
	}
	if d.Webhook != nil {
		p.Settings = append(p.Settings, "уведомления webhook")
		if d.Secrets.WebhookSecret {
			cur, _ := notify.LoadWebhook(ctx, db)
			p.Secrets = append(p.Secrets, PlanSecret{Key: SecretWebhook, Title: "ключ подписи webhook", CanKeep: cur.Secret != ""})
		}
		if d.Secrets.WebhookURL {
			cur, _ := notify.LoadWebhook(ctx, db)
			p.Secrets = append(p.Secrets, PlanSecret{Key: SecretWebhookURL, Title: "полный адрес webhook (с токеном)", CanKeep: cur.URL != ""})
		}
	}
	if d.SFTP != nil && d.SFTP.Host != "" {
		p.Settings = append(p.Settings, "выгрузка по SFTP на "+d.SFTP.Host+" (восстанавливается выключенной: у этой установки свой ключ)")
	}
	for _, x := range d.Pools {
		if ok, err := has(ctx, db, "SELECT COUNT(*) FROM pools WHERE name = ?", x.Name); err != nil {
			return p, err
		} else if !ok {
			p.Warnings = append(p.Warnings, fmt.Sprintf("пул %s из копии на этой системе не найден (пулы восстановлением не создаются)", x.Name))
		}
	}
	return p, nil
}

func has(ctx context.Context, db *sql.DB, q string, arg any) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, q, arg).Scan(&n)
	return n > 0, err
}

// ErrAnswers — ответы мастера неполны или противоречивы.
var ErrAnswers = errors.New("ответы мастера восстановления неполны")

// Check проверяет ответы до любых изменений.
func (p Plan) Check(a Answers) error {
	keep := func(k string) bool { return a.Keep[k] }
	for _, u := range p.Users {
		pw := a.Passwords[u.Name]
		switch {
		case pw != "" && keep(u.Name):
			return fmt.Errorf("%w: %s — либо новый пароль, либо прежний", ErrAnswers, u.Name)
		case keep(u.Name) && !u.CanKeep:
			return fmt.Errorf("%w: у %s нет прежнего пароля на этой системе — задайте новый", ErrAnswers, u.Name)
		case keep(u.Name) && !a.AcceptInsecure:
			return fmt.Errorf("%w: оставить прежний пароль %s можно только с явным подтверждением (небезопасно)", ErrAnswers, u.Name)
		case pw == "" && !keep(u.Name):
			return fmt.Errorf("%w: задайте новый пароль пользователя %s", ErrAnswers, u.Name)
		}
	}
	for _, s := range p.Secrets {
		if keep(s.Key) && (!s.CanKeep || !a.AcceptInsecure) {
			return fmt.Errorf("%w: %s — оставить прежний можно, только если он задан на этой системе, и с явным подтверждением", ErrAnswers, s.Title)
		}
	}
	return nil
}

// Step — результат шага восстановления.
type Step struct {
	What    string `json:"what"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// Result — итог восстановления. Error не пусто — восстановление прервано на
// первой ошибке; повторный запуск продолжает (созданное распознаётся как
// существующее).
type Result struct {
	Steps    []Step   `json:"steps"`
	Warnings []string `json:"warnings"`
	Error    string   `json:"error,omitempty"`
}

// Apply выполняет восстановление по плану и ответам. db — та же база,
// что у Target (настройки без системных действий пишутся напрямую).
func Apply(ctx context.Context, db *sql.DB, t Target, d Data, a Answers, clientIP string) (Result, error) {
	res := Result{Steps: []Step{}, Warnings: []string{}}
	plan, err := MakePlan(ctx, db, Backup{}, d)
	if err != nil {
		return res, err
	}
	if err := plan.Check(a); err != nil {
		return res, err
	}
	step := func(what string, err error) bool {
		s := Step{What: what, OK: err == nil}
		if err != nil {
			s.Error = err.Error()
			res.Error = what + ": " + err.Error()
		}
		res.Steps = append(res.Steps, s)
		return err == nil
	}
	// 1. Пользователи: учётка веба, ОС и Samba одним действием.
	for _, u := range plan.Users {
		pw := a.Passwords[u.Name]
		var err error
		switch {
		case !u.Exists:
			_, err = t.CreateUserSSH(ctx, u.Name, pw, u.Admin, u.SSH)
		case pw != "":
			err = t.SetPassword(ctx, u.Name, pw)
		default:
			res.Warnings = append(res.Warnings, "пользователь "+u.Name+": оставлен прежний пароль (небезопасно)")
		}
		if !step("пользователь "+u.Name, err) {
			return res, nil
		}
		if !u.Enabled {
			if !step("отключение "+u.Name, t.SetEnabled(ctx, u.Name, false)) {
				return res, nil
			}
		}
	}
	// 2. Группы и члены (сначала все группы, затем члены).
	for i, g := range d.Groups {
		if plan.Groups[i].Action == "create" {
			if _, err := t.CreateGroup(ctx, g.Name); !step("группа "+g.Name, err) {
				return res, nil
			}
		}
	}
	for _, g := range d.Groups {
		m := append([]string{}, g.Members...)
		if g.Name == nas.GroupAdmins {
			// Нынешних администраторов не убирать: восстановление не должно
			// лишить входа того, кто его выполняет.
			cur, err := members(ctx, db, g.Name)
			if err != nil {
				step("члены группы "+g.Name, err)
				return res, nil
			}
			m = union(m, cur)
		}
		sort.Strings(m)
		if !step("члены группы "+g.Name, t.SetMembers(ctx, g.Name, m)) {
			return res, nil
		}
	}
	// 3. Общие папки и права (только на существующих пулах).
	for i, s := range d.Shares {
		switch plan.Shares[i].Action {
		case "skip":
			res.Warnings = append(res.Warnings, "папка "+s.Name+" пропущена: "+plan.Shares[i].Reason)
			continue
		case "create":
			if _, err := t.CreateShare(ctx, s.Name, s.Pool, s.Comment); !step("папка "+s.Name, err) {
				return res, nil
			}
		}
		if !step("права папки "+s.Name, t.SetAccess(ctx, s.Name, s.Access)) {
			return res, nil
		}
	}
	// 4. Настройки сервера.
	st := d.Settings
	if st.ServerName != "" && st.Timezone != "" && st.NTP != nil {
		w, err := t.SetIdentity(ctx, st.ServerName, st.Timezone, *st.NTP)
		res.Warnings = append(res.Warnings, w...)
		if !step("имя сервера и время", err) {
			return res, nil
		}
	} else if st.ServerName != "" {
		res.Warnings = append(res.Warnings, "имя сервера не восстановлено: в копии нет часового пояса — задайте в настройках")
	}
	if st.Homes.Enabled {
		if ok, _ := has(ctx, db, "SELECT COUNT(*) FROM pools WHERE name = ?", st.Homes.Pool); ok {
			h, err := t.SetHomes(ctx, true, st.Homes.Pool)
			res.Warnings = append(res.Warnings, h.Warnings...)
			if !step("личные папки", err) {
				return res, nil
			}
		} else {
			res.Warnings = append(res.Warnings, "личные папки не включены: нет пула "+st.Homes.Pool)
		}
	}
	if st.SSHGuard != nil {
		if _, err := t.SetGuard(ctx, *st.SSHGuard, clientIP); !step("защита SSH", err) {
			return res, nil
		}
	}
	if d.Maintenance != nil {
		err := d.Maintenance.Validate()
		if err == nil {
			err = setKV(ctx, db, "maintenance.schedule", *d.Maintenance)
		}
		if !step("расписание проверок", err) {
			return res, nil
		}
	}
	// 5. Уведомления: секреты — новые, прежние (явно) или канал выключен.
	if d.SMTP != nil {
		c := *d.SMTP
		cur, _ := notify.LoadSMTP(ctx, db)
		switch {
		case !d.Secrets.SMTPPassword:
		case a.Keep[SecretSMTP]:
			c.Password = cur.Password
			res.Warnings = append(res.Warnings, "SMTP: оставлен прежний пароль (небезопасно)")
		case a.SMTPPassword != "":
			c.Password = a.SMTPPassword
		default:
			c.Enabled = false
			res.Warnings = append(res.Warnings, "SMTP: пароль не задан — канал восстановлен выключенным")
		}
		if !step("уведомления по почте", notify.SaveSMTP(ctx, db, c)) {
			return res, nil
		}
	}
	if d.Webhook != nil {
		c := *d.Webhook
		cur, _ := notify.LoadWebhook(ctx, db)
		switch {
		case !d.Secrets.WebhookSecret:
		case a.Keep[SecretWebhook]:
			c.Secret = cur.Secret
			res.Warnings = append(res.Warnings, "webhook: оставлен прежний ключ подписи (небезопасно)")
		case a.WebhookSecret != "":
			c.Secret = a.WebhookSecret
		default:
			c.Enabled = false
			res.Warnings = append(res.Warnings, "webhook: ключ подписи не задан — канал восстановлен выключенным")
		}
		switch {
		case !d.Secrets.WebhookURL:
		case a.Keep[SecretWebhookURL]:
			c.URL = cur.URL
			res.Warnings = append(res.Warnings, "webhook: оставлен прежний адрес с токеном (небезопасно)")
		case a.WebhookURL != "":
			c.URL = a.WebhookURL
		default:
			c.Enabled = false
			res.Warnings = append(res.Warnings, "webhook: полный адрес не задан — канал восстановлен выключенным")
		}
		if !step("уведомления webhook", notify.SaveWebhook(ctx, db, c)) {
			return res, nil
		}
	}
	if d.SFTP != nil && d.SFTP.Host != "" {
		c := *d.SFTP
		c.Enabled = false
		if !step("выгрузка по SFTP", SaveSFTP(ctx, db, c)) {
			return res, nil
		}
		res.Warnings = append(res.Warnings, "SFTP: назначение восстановлено выключенным — добавьте новый открытый ключ NAS на сервер и включите выгрузку")
	}
	// 6. Мастер первоначальной настройки считается пройденным.
	if !plan.SetupDone {
		if err := t.CompleteSetup(ctx); err != nil && !errors.Is(err, nas.ErrConflict) {
			step("завершение настройки", err)
			return res, nil
		}
	}
	return res, nil
}

func setKV(ctx context.Context, db *sql.DB, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(b))
	return err
}

func members(ctx context.Context, db *sql.DB, grp string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT user FROM nas_group_members WHERE grp = ?", grp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
