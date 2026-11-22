package nas

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// HelperClient — вызов операции gamayun-helper.
type HelperClient interface {
	Call(ctx context.Context, op string, params, result any) error
}

// WebAccounts — учётки веба: одна учётка на человека, пароль
// задаётся одним действием для веба и Samba. Реализует auth.Accounts.
type WebAccounts interface {
	Validate(name, password string) error
	SetPassword(ctx context.Context, name, password string, admin, disabled bool) error
	SetFlags(ctx context.Context, name string, admin, disabled bool) error
	Delete(ctx context.Context, name string) error
	Admins(ctx context.Context) ([]string, error)
}

// Service — модель пользователей, групп и шар в SQLite gamayund. Системные
// действия выполняет только helper (операции nas.*).
type Service struct {
	db     *sql.DB
	helper HelperClient
	Web    WebAccounts // nil — вход в веб отключён (auth.disable)
	mu     sync.Mutex  // сериализует изменения модели и применение к Samba
	now    func() time.Time
}

// NewService создаёт сервис.
func NewService(h HelperClient, db *sql.DB) *Service {
	return &Service{db: db, helper: h, now: time.Now}
}

// User — пользователь NAS.
type User struct {
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	Admin     bool      `json:"admin"` // член administrators: входит в веб
	SSH       bool      `json:"ssh"`   // вход по SSH (оболочка bash)
	Groups    []string  `json:"groups"`
	CreatedAt time.Time `json:"created_at"`
	// PasswordSyncError — пароль задан не везде (веб / ОС и Samba) после
	// сбоя на середине; пусто — согласован. Снимается успешной сменой.
	PasswordSyncError string `json:"password_sync_error,omitempty"`
}

// Group — группа NAS.
type Group struct {
	Name      string    `json:"name"`
	Members   []string  `json:"members"`
	System    bool      `json:"system"` // стандартная: не удаляется
	CreatedAt time.Time `json:"created_at"`
}

// Access — уровень доступа субъекта к шаре.
type Access struct {
	Kind    string `json:"kind"` // user | group
	Subject string `json:"subject"`
	Level   string `json:"level"` // none | read | rw
}

// ShareInfo — общая папка с правами.
type ShareInfo struct {
	Name      string    `json:"name"`
	Pool      string    `json:"pool"`
	Comment   string    `json:"comment"`
	Path      string    `json:"path,omitempty"` // относительно точки монтирования пула
	ReadOnly  bool      `json:"read_only"`      // пул в read-only (защита)
	Access    []Access  `json:"access"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) ms(t int64) time.Time { return time.UnixMilli(t).UTC() }

// ---------- пользователи ----------

// CreateUser создаёт учётку ОС без входа, пароль Samba и запись в базе.
func (s *Service) CreateUser(ctx context.Context, name, password string, admin bool) (User, error) {
	return s.CreateUserSSH(ctx, name, password, admin, admin)
}

// CreateUserSSH — CreateUser с явным флагом входа по SSH.
func (s *Service) CreateUserSSH(ctx context.Context, name, password string, admin, ssh bool) (User, error) {
	if !ValidName(name) {
		return User{}, wrap(ErrInvalid, "имя: строчные латинские буквы, цифры, _ и -, до 31 символа, первая — буква")
	}
	if IsAppUser(name, -1) {
		return User{}, wrap(ErrInvalid, "имена %s… зарезервированы для учёток приложений", AppUserPrefix)
	}
	if s.Web != nil {
		if err := s.Web.Validate(name, password); err != nil {
			return User{}, wrap(ErrInvalid, "%v", err)
		}
	}
	if err := ValidPassword(password); err != nil {
		return User{}, wrap(ErrInvalid, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaults(ctx); err != nil {
		return User{}, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nas_users WHERE name = ?", name).Scan(&n); err != nil {
		return User{}, err
	}
	if n > 0 {
		return User{}, wrap(ErrConflict, "пользователь %s уже есть", name)
	}
	if admin && !ssh {
		if err := s.db.QueryRowContext(ctx, sshAdminsQuery, GroupAdmins, name).Scan(&n); err != nil {
			return User{}, err
		}
		if n == 0 {
			return User{}, wrap(ErrConflict, "хотя бы у одного включённого администратора должен остаться вход по SSH")
		}
	}
	var r Result
	if err := s.helper.Call(ctx, OpUserEnsure, UserParams{Name: name}, &r); err != nil {
		return User{}, err
	}
	if err := s.helper.Call(ctx, OpUserPassword, PasswordParams{Name: name, Password: password}, &r); err != nil {
		return User{}, err
	}
	if s.Web != nil {
		if err := s.Web.SetPassword(ctx, name, password, admin, false); err != nil {
			return User{}, err
		}
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO nas_users(name, enabled, ssh, created_at) VALUES (?, 1, ?, ?)", name, ssh, now.UnixMilli()); err != nil {
		return User{}, err
	}
	groups := []string{GroupUsers}
	if admin {
		groups = append(groups, GroupAdmins)
	}
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, "INSERT INTO nas_group_members(grp, user) VALUES (?, ?)", g, name); err != nil {
			return User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	if err := s.applyAccess(ctx, name); err != nil {
		return User{}, err
	}
	if err := s.applyHomes(ctx, []string{name}); err != nil {
		return User{}, err
	}
	sort.Strings(groups)
	return User{Name: name, Enabled: true, Admin: admin, SSH: ssh, Groups: groups, CreatedAt: now.UTC()}, nil
}

// ListUsers — пользователи по имени.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, enabled, ssh, created_at, COALESCE(password_sync_error, '') FROM nas_users ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var en, ssh int
		var ts int64
		if err := rows.Scan(&u.Name, &en, &ssh, &ts, &u.PasswordSyncError); err != nil {
			return nil, err
		}
		u.SSH = ssh != 0
		u.Enabled, u.CreatedAt, u.Groups = en != 0, s.ms(ts), []string{}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mem, err := s.memberships(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		for g, us := range mem {
			for _, x := range us {
				if x == out[i].Name {
					out[i].Groups = append(out[i].Groups, g)
				}
			}
		}
		sort.Strings(out[i].Groups)
		for _, g := range out[i].Groups {
			if g == GroupAdmins {
				out[i].Admin = true
			}
		}
	}
	return out, nil
}

func (s *Service) userExists(ctx context.Context, name string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nas_users WHERE name = ?", name).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return wrap(ErrNotFound, "пользователь %s", name)
	}
	return nil
}

// SetPassword меняет пароль одним действием для Samba и веба.
func (s *Service) SetPassword(ctx context.Context, name, password string) error {
	if s.Web != nil {
		if err := s.Web.Validate(name, password); err != nil {
			return wrap(ErrInvalid, "%v", err)
		}
	}
	if err := ValidPassword(password); err != nil {
		return wrap(ErrInvalid, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.userExists(ctx, name); err != nil {
		return err
	}
	var r Result
	if err := s.helper.Call(ctx, OpUserPassword, PasswordParams{Name: name, Password: password}, &r); err != nil {
		return err
	}
	// smbpasswd -a включает запись Samba — у отключённого вернуть отключение.
	if err := s.applyAccess(ctx, name); err != nil {
		return err
	}
	if s.Web == nil {
		return nil
	}
	admin, enabled, err := s.flags(ctx, name)
	if err == nil {
		err = s.Web.SetPassword(ctx, name, password, admin, !enabled)
	}
	if err != nil {
		s.markPassword(ctx, name, "пароль ОС и Samba новый, веба — прежний: "+err.Error())
		return err
	}
	return s.markPassword(ctx, name, "")
}

// markPassword записывает (reason ≠ "") или снимает пометку о несогласованном
// пароле. Ошибка записи пометки не скрывает исходную ошибку.
func (s *Service) markPassword(ctx context.Context, name, reason string) error {
	var v any
	if reason != "" {
		v = reason
	}
	_, err := s.db.ExecContext(ctx, "UPDATE nas_users SET password_sync_error = ? WHERE name = ?", v, name)
	return err
}

// MarkPasswordMismatch — пометка извне (auth: пароль ОС и Samba уже новый,
// а сохранить хэш веба не удалось).
func (s *Service) MarkPasswordMismatch(ctx context.Context, name, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markPassword(ctx, name, reason)
}

// SyncAfterWeb — пароль ОС и Samba после того, как пароль веба уже сменён
// (gamayund admin create/passwd): при отказе — пометка о несогласованности.
func (s *Service) SyncAfterWeb(ctx context.Context, name, password string) error {
	err := s.SetSambaPassword(ctx, name, password)
	if err == nil {
		// SetSambaPassword молча пропускает пользователя без учётки NAS —
		// здесь это ошибка: иначе CLI сообщал об успехе.
		s.mu.Lock()
		err = s.userExists(ctx, name)
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("учётки NAS %s нет: пароль ОС и Samba не задан: %w", name, err)
		}
		return nil
	}
	s.MarkPasswordMismatch(ctx, name, "пароль веба новый, ОС и Samba — прежний: "+err.Error())
	return err
}

// SetSambaPassword — пароль Samba для пароля, заданного в вебе (смена
// собственного пароля, первичная настройка). Учётка NAS при необходимости
// создаётся из администратора веба (Sync). Нет такой учётки — ничего не делает.
func (s *Service) SetSambaPassword(ctx context.Context, name, password string) error {
	if err := s.Sync(ctx); err != nil {
		return err
	}
	if err := ValidPassword(password); err != nil {
		return wrap(ErrInvalid, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.userExists(ctx, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	var r Result
	if err := s.helper.Call(ctx, OpUserPassword, PasswordParams{Name: name, Password: password}, &r); err != nil {
		return err
	}
	// ОС и Samba новые; веб меняет вызывающий (auth) — при его сбое пометку
	// поставит MarkPasswordMismatch.
	if err := s.markPassword(ctx, name, ""); err != nil {
		return err
	}
	return s.applyAccess(ctx, name)
}

// flags — состав administrators и включённость пользователя.
func (s *Service) flags(ctx context.Context, name string) (admin, enabled bool, err error) {
	var en, adm int
	err = s.db.QueryRowContext(ctx, `SELECT enabled, EXISTS(SELECT 1 FROM nas_group_members WHERE grp = ? AND user = nas_users.name)
		FROM nas_users WHERE name = ?`, GroupAdmins, name).Scan(&en, &adm)
	return adm != 0, en != 0, err
}

// otherAdmins — число включённых членов administrators, кроме except.
func (s *Service) otherAdmins(ctx context.Context, except string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nas_group_members m JOIN nas_users u ON u.name = m.user
		WHERE m.grp = ? AND u.enabled = 1 AND u.name <> ?`, GroupAdmins, except).Scan(&n)
	return n, err
}

// syncFlags переносит состав administrators и отключение в учётки веба.
func (s *Service) syncFlags(ctx context.Context) error {
	if s.Web == nil {
		return nil
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := s.Web.SetFlags(ctx, u.Name, u.Admin, !u.Enabled); err != nil {
			return err
		}
	}
	return nil
}

// ensureDefaults создаёт стандартные группы (идемпотентно). Если группа
// users создана сейчас, в неё попадают все уже существующие пользователи.
func (s *Service) ensureDefaults(ctx context.Context) error {
	now := s.now().UnixMilli()
	for _, g := range SystemGroups {
		res, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO nas_groups(name, created_at) VALUES (?, ?)", g, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 && g == GroupUsers {
			if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO nas_group_members(grp, user) SELECT ?, name FROM nas_users", g); err != nil {
				return err
			}
		}
	}
	return nil
}

// Sync (старт gamayund и чтение списка пользователей): создаёт стандартные
// группы и переносит администраторов веба, которых нет среди пользователей
// NAS, в единый список (члены administrators и users). Идемпотентно. Пароль
// Samba таким администраторам задаётся при следующей установке пароля.
func (s *Service) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDefaults(ctx); err != nil {
		return err
	}
	if s.Web == nil {
		return nil
	}
	names, err := s.Web.Admins(ctx)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !ValidName(n) {
			continue
		}
		if err := s.userExists(ctx, n); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		var r Result
		if err := s.helper.Call(ctx, OpUserEnsure, UserParams{Name: n}, &r); err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO nas_users(name, enabled, ssh, created_at) VALUES (?, 1, 1, ?)", n, s.now().UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		for _, g := range SystemGroups {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO nas_group_members(grp, user) VALUES (?, ?)", g, n); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if err := s.applyAccess(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// SetEnabled отключает пользователя (исключается из всех шар) или возвращает.
// Уже открытые сеансы SMB действуют до переподключения.
func (s *Service) SetEnabled(ctx context.Context, name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.userExists(ctx, name); err != nil {
		return err
	}
	if !enabled {
		if err := s.guardLastAdmin(ctx, name); err != nil {
			return err
		}
	}
	v := 0
	if enabled {
		v = 1
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nas_users SET enabled = ? WHERE name = ?", v, name); err != nil {
		return err
	}
	if err := s.syncFlags(ctx); err != nil {
		return err
	}
	if err := s.applyAccess(ctx, name); err != nil {
		return err
	}
	return s.applyLocked(ctx)
}

// guardLastAdmin отказывает, если name — последний включённый администратор.
func (s *Service) guardLastAdmin(ctx context.Context, name string) error {
	admin, enabled, err := s.flags(ctx, name)
	if err != nil || !admin || !enabled {
		return err
	}
	n, err := s.otherAdmins(ctx, name)
	if err != nil {
		return err
	}
	if n == 0 {
		return wrap(ErrConflict, "%s — последний администратор: сначала назначьте другого", name)
	}
	return s.guardLastSSHAdmin(ctx, name)
}

// DeleteUser удаляет пользователя из моделей и учётку ОС. Файлы пользователя
// на пулах не трогаются.
func (s *Service) DeleteUser(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.userExists(ctx, name); err != nil {
		return err
	}
	if err := s.guardLastAdmin(ctx, name); err != nil {
		return err
	}
	// Учётка ОС удаляется первой — при отказе helper модель остаётся
	// целой и удаление можно повторить (без «осиротевшей» учётки ОС).
	var r Result
	if err := s.helper.Call(ctx, OpUserDelete, UserParams{Name: name}, &r); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM nas_group_members WHERE user = ?",
		"DELETE FROM share_access WHERE kind = 'user' AND subject = ?",
		"DELETE FROM nas_users WHERE name = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.Web != nil {
		if err := s.Web.Delete(ctx, name); err != nil {
			return err
		}
	}
	return s.applyLocked(ctx)
}

// ---------- группы ----------

func (s *Service) memberships(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT grp, user FROM nas_group_members ORDER BY grp, user")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string][]string{}
	for rows.Next() {
		var g, u string
		if err := rows.Scan(&g, &u); err != nil {
			return nil, err
		}
		m[g] = append(m[g], u)
	}
	return m, rows.Err()
}

// CreateGroup создаёт пустую группу.
func (s *Service) CreateGroup(ctx context.Context, name string) (Group, error) {
	if !ValidName(name) {
		return Group{}, wrap(ErrInvalid, "имя группы: строчные латинские буквы, цифры, _ и -, до 31 символа")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO nas_groups(name, created_at) VALUES (?, ?)", name, now.UnixMilli()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Group{}, wrap(ErrConflict, "группа %s уже есть", name)
		}
		return Group{}, err
	}
	return Group{Name: name, Members: []string{}, CreatedAt: now.UTC()}, nil
}

// ListGroups — группы с составом.
func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, created_at FROM nas_groups ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		var ts int64
		if err := rows.Scan(&g.Name, &ts); err != nil {
			return nil, err
		}
		g.CreatedAt, g.Members, g.System = s.ms(ts), []string{}, IsSystemGroup(g.Name)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mem, err := s.memberships(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if m := mem[out[i].Name]; m != nil {
			out[i].Members = m
		}
	}
	return out, nil
}

// SetMembers заменяет состав группы (пользователи должны существовать).
func (s *Service) SetMembers(ctx context.Context, group string, members []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exists(ctx, "nas_groups", group, "группа"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range members {
		if seen[m] {
			return wrap(ErrInvalid, "пользователь %s указан дважды", m)
		}
		seen[m] = true
		if err := s.userExists(ctx, m); err != nil {
			return err
		}
	}
	if group == GroupAdmins {
		ok := false
		for _, m := range members {
			if _, en, err := s.flags(ctx, m); err != nil {
				return err
			} else if en {
				ok = true
			}
		}
		if !ok {
			return wrap(ErrConflict, "в группе %s должен остаться хотя бы один включённый пользователь", GroupAdmins)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if group == GroupAdmins {
		// Новый администратор получает SSH по умолчанию.
		for _, m := range members {
			if _, err := tx.ExecContext(ctx, `UPDATE nas_users SET ssh = 1 WHERE name = ?
				AND NOT EXISTS(SELECT 1 FROM nas_group_members WHERE grp = ? AND user = ?)`, m, GroupAdmins, m); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM nas_group_members WHERE grp = ?", group); err != nil {
		return err
	}
	for _, m := range members {
		if _, err := tx.ExecContext(ctx, "INSERT INTO nas_group_members(grp, user) VALUES (?, ?)", group, m); err != nil {
			return err
		}
	}
	if group == GroupAdmins {
		var n int
		if err := tx.QueryRowContext(ctx, sshAdminsQuery, GroupAdmins, "").Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return wrap(ErrConflict, "хотя бы у одного включённого администратора должен остаться вход по SSH")
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if group == GroupAdmins {
		if err := s.syncFlags(ctx); err != nil {
			return err
		}
		if err := s.applyAccessAll(ctx); err != nil {
			return err
		}
	}
	return s.applyLocked(ctx)
}

// DeleteGroup удаляет группу и её записи доступа.
func (s *Service) DeleteGroup(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exists(ctx, "nas_groups", name, "группа"); err != nil {
		return err
	}
	if IsSystemGroup(name) {
		return wrap(ErrConflict, "группа %s стандартная: удалять и переименовывать нельзя", name)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM nas_group_members WHERE grp = ?",
		"DELETE FROM share_access WHERE kind = 'group' AND subject = ?",
		"DELETE FROM nas_groups WHERE name = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.applyLocked(ctx)
}

func (s *Service) exists(ctx context.Context, table, name, what string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE name = ?", name).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return wrap(ErrNotFound, "%s %s", what, name)
	}
	return nil
}

// ---------- шары ----------

// CreateShare создаёт общую папку на смонтированном пуле. Каталог создаёт
// helper (<точка пула>/shares/<имя>). Доступ изначально ни у кого.
func (s *Service) CreateShare(ctx context.Context, name, pool, comment string) (ShareInfo, error) {
	if !ValidShare(name) {
		return ShareInfo{}, wrap(ErrInvalid, "имя папки: латиница, цифры, _ и -, до 32 символов")
	}
	if ReservedShare(name) {
		return ShareInfo{}, wrap(ErrInvalid, "имя %s зарезервировано (служебная секция Samba)", name)
	}
	// Имя устройства Windows (CON, NUL, COM1…) у ресурса SMB —
	// только для новых папок; существующие не трогаем (их проверяет
	// SharesParams.Validate без этого правила).
	if dev := WinReservedName(name); dev != "" {
		return ShareInfo{}, wrap(ErrInvalid, "имя %s недопустимо: %s — зарезервированное имя устройства Windows (в любом регистре)", name, dev)
	}
	if !commentRe.MatchString(comment) {
		return ShareInfo{}, wrap(ErrInvalid, "комментарий: до 120 символов без управляющих")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exists(ctx, "pools", pool, "пул"); err != nil {
		return ShareInfo{}, err
	}
	// Samba и каталоги не различают регистр имён ресурсов: «Docs» и «docs» —
	// одна папка.
	var dup string
	if err := s.db.QueryRowContext(ctx, "SELECT name FROM shares WHERE lower(name) = lower(?)", name).Scan(&dup); err == nil {
		return ShareInfo{}, wrap(ErrConflict, "папка %s уже есть", dup)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ShareInfo{}, err
	}
	now := s.now()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO shares(name, pool, comment, created_at) VALUES (?, ?, ?, ?)", name, pool, comment, now.UnixMilli()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ShareInfo{}, wrap(ErrConflict, "папка %s уже есть", name)
		}
		return ShareInfo{}, err
	}
	if err := s.applyLocked(ctx); err != nil {
		s.db.ExecContext(ctx, "DELETE FROM shares WHERE name = ?", name) // откат: папки нет
		return ShareInfo{}, err
	}
	return s.GetShare(ctx, name)
}

// ListShares — все папки с правами.
func (s *Service) ListShares(ctx context.Context) ([]ShareInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.name, s.pool, s.comment, s.created_at, COALESCE(p.read_only, 0)
		FROM shares s LEFT JOIN share_pool_state p ON p.pool = s.pool ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareInfo{}
	idx := map[string]int{}
	for rows.Next() {
		var x ShareInfo
		var ts int64
		var ro int
		if err := rows.Scan(&x.Name, &x.Pool, &x.Comment, &ts, &ro); err != nil {
			return nil, err
		}
		x.CreatedAt, x.ReadOnly, x.Access = s.ms(ts), ro != 0, []Access{}
		x.Path = "shares/" + x.Name
		idx[x.Name] = len(out)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ar, err := s.db.QueryContext(ctx, "SELECT share, kind, subject, level FROM share_access ORDER BY share, kind, subject")
	if err != nil {
		return nil, err
	}
	defer ar.Close()
	for ar.Next() {
		var sh string
		var a Access
		if err := ar.Scan(&sh, &a.Kind, &a.Subject, &a.Level); err != nil {
			return nil, err
		}
		if i, ok := idx[sh]; ok {
			out[i].Access = append(out[i].Access, a)
		}
	}
	return out, ar.Err()
}

// GetShare — одна папка.
func (s *Service) GetShare(ctx context.Context, name string) (ShareInfo, error) {
	all, err := s.ListShares(ctx)
	if err != nil {
		return ShareInfo{}, err
	}
	for _, x := range all {
		if x.Name == name {
			return x, nil
		}
	}
	return ShareInfo{}, wrap(ErrNotFound, "папка %s", name)
}

// SetAccess заменяет права папки списком записей. Субъект без записи —
// нет доступа. Запись пользователя важнее записей его групп; из групп
// «нет доступа» сильнее чтения/записи, иначе берётся максимум.
func (s *Service) SetAccess(ctx context.Context, share string, entries []Access) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exists(ctx, "shares", share, "папка"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, a := range entries {
		switch a.Level {
		case LevelNone, LevelRead, LevelRW:
		default:
			return wrap(ErrInvalid, "уровень %q: none, read или rw", a.Level)
		}
		k := a.Kind + "/" + a.Subject
		if seen[k] {
			return wrap(ErrInvalid, "повтор субъекта %s", k)
		}
		seen[k] = true
		var err error
		switch a.Kind {
		case KindUser:
			err = s.userExists(ctx, a.Subject)
		case KindGroup:
			err = s.exists(ctx, "nas_groups", a.Subject, "группа")
		default:
			err = wrap(ErrInvalid, "вид субъекта %q: user или group", a.Kind)
		}
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM share_access WHERE share = ?", share); err != nil {
		return err
	}
	for _, a := range entries {
		if _, err := tx.ExecContext(ctx, "INSERT INTO share_access(share, kind, subject, level) VALUES (?, ?, ?, ?)", share, a.Kind, a.Subject, a.Level); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.applyLocked(ctx)
}

// DeleteShare убирает папку из Samba и модели. Каталог с файлами остаётся
// на пуле (данные не удаляются).
func (s *Service) DeleteShare(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.exists(ctx, "shares", name, "папка"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM shares WHERE name = ?", name); err != nil {
		return err
	}
	return s.applyLocked(ctx)
}

// SharesOnPool — имена папок пула.
func (s *Service) SharesOnPool(ctx context.Context, pool string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM shares WHERE pool = ? ORDER BY name", pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if h, err := s.Homes(ctx); err != nil {
		return nil, err
	} else if h.Enabled && h.Pool == pool {
		out = append(out, HomesShare) // служба домашних папок тоже живёт на пуле
	}
	return out, nil
}

// ---------- применение к Samba ----------

// Apply пересчитывает права и отдаёт helper полный список шар (идемпотентно).
func (s *Service) Apply(ctx context.Context) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(ctx)
}

func (s *Service) applyLocked(ctx context.Context) error {
	_, err := s.apply(ctx)
	return err
}

func (s *Service) apply(ctx context.Context) (Result, error) {
	shares, err := s.build(ctx)
	if err != nil {
		return Result{}, err
	}
	sp := SharesParams{Shares: shares}
	if sp.Netbios, err = s.kvGet(ctx, keyServerName); err != nil {
		return Result{}, err
	}
	h, err := s.Homes(ctx)
	if err != nil {
		return Result{}, err
	}
	if h.Enabled {
		sp.Homes = h.Pool
		s.db.QueryRowContext(ctx, "SELECT read_only FROM share_pool_state WHERE pool = ?", h.Pool).Scan(&sp.HomesReadOnly)
	}
	var r Result
	if err := s.helper.Call(ctx, OpSharesApply, sp, &r); err != nil {
		return r, fmt.Errorf("применение шар Samba: %w", err)
	}
	return r, nil
}

// Resync применяет текущую модель при старте gamayund (если шары есть).
func (s *Service) Resync(ctx context.Context) error {
	if err := s.Sync(ctx); err != nil {
		return err
	}
	shares, err := s.ListShares(ctx)
	if err != nil {
		return err
	}
	nb, err := s.kvGet(ctx, keyServerName)
	if err != nil {
		return err
	}
	h, err := s.Homes(ctx)
	if err != nil || (len(shares) == 0 && nb == "" && !h.Enabled) {
		return err
	}
	_, err = s.Apply(ctx)
	return err
}

// SetPoolReadOnly — реализация home.ShareGuard: шары пула становятся
// read only в Samba и возвращаются после снятия запрета.
func (s *Service) SetPoolReadOnly(ctx context.Context, pool string, readOnly bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := 0
	if readOnly {
		v = 1
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO share_pool_state(pool, read_only) VALUES (?, ?)
		ON CONFLICT(pool) DO UPDATE SET read_only = excluded.read_only`, pool, v); err != nil {
		return err
	}
	on, err := s.SharesOnPool(ctx, pool)
	if err != nil || len(on) == 0 {
		return err
	}
	return s.applyLocked(ctx)
}

func (s *Service) build(ctx context.Context) ([]Share, error) {
	infos, err := s.ListShares(ctx)
	if err != nil {
		return nil, err
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	mem, err := s.memberships(ctx)
	if err != nil {
		return nil, err
	}
	var enabled []string
	for _, u := range users {
		if u.Enabled {
			enabled = append(enabled, u.Name)
		}
	}
	out := make([]Share, 0, len(infos))
	for _, x := range infos {
		r, w := Effective(enabled, mem, x.Access)
		out = append(out, Share{Name: x.Name, Pool: x.Pool, Comment: x.Comment, ReadOnly: x.ReadOnly, Read: r, Write: w})
	}
	return out, nil
}

// Effective раскрывает группы в пользователей и возвращает списки чтения
// (только чтение) и записи (чтение и запись). Правила: запись пользователя
// важнее его групп; из групп «none» сильнее остальных, иначе максимум;
// нет записей — нет доступа. Отключённые пользователи (нет в enabled)
// исключаются.
func Effective(enabled []string, members map[string][]string, acc []Access) (read, write []string) {
	rank := map[string]int{LevelNone: 0, LevelRead: 1, LevelRW: 2}
	direct := map[string]string{}
	grp := map[string]string{}
	for _, a := range acc {
		switch a.Kind {
		case KindUser:
			direct[a.Subject] = a.Level
		case KindGroup:
			for _, u := range members[a.Subject] {
				cur, ok := grp[u]
				switch {
				case !ok, a.Level == LevelNone, cur != LevelNone && rank[a.Level] > rank[cur]:
					grp[u] = a.Level
				}
			}
		}
	}
	read, write = []string{}, []string{}
	for _, u := range enabled {
		l, ok := direct[u]
		if !ok {
			l = grp[u]
		}
		switch l {
		case LevelRead:
			read = append(read, u)
		case LevelRW:
			write = append(write, u)
		}
	}
	return read, write
}

// ---------- Учётка ОС, SSH и sudo ----------

// sshAdminsQuery — число включённых администраторов с SSH, кроме ?2.
const sshAdminsQuery = `SELECT COUNT(*) FROM nas_group_members m JOIN nas_users u ON u.name = m.user
	WHERE m.grp = ? AND u.enabled = 1 AND u.ssh = 1 AND u.name <> ?`

// guardLastSSHAdmin отказывает, если name — последний включённый
// администратор со входом по SSH (доступ к ОС не должен потеряться).
func (s *Service) guardLastSSHAdmin(ctx context.Context, name string) error {
	var en, adm, ssh int
	err := s.db.QueryRowContext(ctx, `SELECT enabled, ssh, EXISTS(SELECT 1 FROM nas_group_members WHERE grp = ? AND user = nas_users.name)
		FROM nas_users WHERE name = ?`, GroupAdmins, name).Scan(&en, &ssh, &adm)
	if err != nil || en == 0 || ssh == 0 || adm == 0 {
		return err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, sshAdminsQuery, GroupAdmins, name).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return wrap(ErrConflict, "%s — последний администратор со входом по SSH", name)
	}
	return nil
}

// SetSSH включает или выключает вход по SSH (оболочка bash и группа SSHGroup).
func (s *Service) SetSSH(ctx context.Context, name string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.userExists(ctx, name); err != nil {
		return err
	}
	if !on {
		if err := s.guardLastSSHAdmin(ctx, name); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nas_users SET ssh = ? WHERE name = ?", on, name); err != nil {
		return err
	}
	return s.applyAccess(ctx, name)
}

// access — требуемый доступ к ОС: отключённый пользователь не входит по SSH
// и не получает sudo; sudo — у членов administrators.
func (s *Service) access(ctx context.Context, name string) (AccessParams, error) {
	var en, adm, ssh int
	err := s.db.QueryRowContext(ctx, `SELECT enabled, ssh, EXISTS(SELECT 1 FROM nas_group_members WHERE grp = ? AND user = nas_users.name)
		FROM nas_users WHERE name = ?`, GroupAdmins, name).Scan(&en, &ssh, &adm)
	return AccessParams{Name: name, SSH: en != 0 && ssh != 0, Sudo: en != 0 && adm != 0, SambaDisabled: en == 0}, err
}

func (s *Service) applyAccess(ctx context.Context, name string) error {
	p, err := s.access(ctx, name)
	if err != nil {
		return err
	}
	var r Result
	return s.helper.Call(ctx, OpUserAccess, p, &r)
}

func (s *Service) applyAccessAll(ctx context.Context) error {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := s.applyAccess(ctx, u.Name); err != nil {
			return err
		}
	}
	return nil
}

// MigrateOS (старт gamayund): переносит администраторов веба (Sync), готовит
// группы и drop-in sshd/sudoers и приводит каждую учётку NAS к модели —
// учётка ОС, оболочка, SSH, sudo. Идемпотентно: без расхождений helper
// ничего не меняет. Возвращает имена изменённых учёток (и "system" —
// изменены группы или drop-in). Пароль ОС у перенесённых учёток появится
// при следующей смене пароля.
func (s *Service) MigrateOS(ctx context.Context) ([]string, error) {
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var changed []string
	var r Result
	if err := s.helper.Call(ctx, OpAccessSetup, SetupParams{}, &r); err != nil {
		return nil, err
	}
	if r.Changed {
		changed = append(changed, "system")
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return changed, err
	}
	for _, u := range users {
		var r1, r2 Result
		if err := s.helper.Call(ctx, OpUserEnsure, UserParams{Name: u.Name}, &r1); err != nil {
			return changed, err
		}
		p, err := s.access(ctx, u.Name)
		if err != nil {
			return changed, err
		}
		if err := s.helper.Call(ctx, OpUserAccess, p, &r2); err != nil {
			return changed, err
		}
		if r1.Changed || r2.Changed {
			changed = append(changed, u.Name)
		}
	}
	// Домашние каталоги (если служба включена) и fail2ban — по сохранённым настройкам.
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Name
	}
	if err := s.reconcileHomes(ctx, names); err != nil {
		return changed, err
	}
	if err := s.applySSH(ctx); err != nil {
		return changed, err
	}
	return changed, s.applyGuard(ctx)
}

// ShareDirs — вложенные каталоги общей папки (только каталоги, глубина
// 1..MaxDirDepth). Чтение — через helper: у gamayund нет прав на каталоги шар.
// Путь проверяется здесь и повторно в helper.
func (s *Service) ShareDirs(ctx context.Context, share, rel string, depth int) (ShareDirsResult, error) {
	if depth == 0 {
		depth = 1
	}
	if !ValidShare(share) {
		return ShareDirsResult{}, wrap(ErrInvalid, "имя папки")
	}
	if depth < 1 || depth > MaxDirDepth {
		return ShareDirsResult{}, wrap(ErrInvalid, "глубина должна быть 1..%d", MaxDirDepth)
	}
	if _, err := CleanRel(rel); err != nil {
		return ShareDirsResult{}, wrap(ErrInvalid, "%v", err)
	}
	p := ShareDirsParams{Share: share, Path: rel, Depth: depth}
	sh, err := s.GetShare(ctx, share)
	if err != nil {
		return ShareDirsResult{}, err
	}
	p.Pool = sh.Pool
	var res ShareDirsResult
	if err := s.helper.Call(ctx, OpShareDirs, p, &res); err != nil {
		return ShareDirsResult{}, err
	}
	if res.Missing {
		return ShareDirsResult{}, wrap(ErrNotFound, "каталог %q в папке %s", rel, share)
	}
	return res, nil
}

// ShareMkdir создаёт каталог name в каталоге parent общей папки.
// Параметры проверяются здесь (до БД и helper) и повторно в helper; коды
// ошибок helper (not_found, conflict, refused, invalid_params) сопоставляет API.
func (s *Service) ShareMkdir(ctx context.Context, share, parent, name string) (ShareDirOpResult, error) {
	p := ShareMkdirParams{Share: share, Path: parent, Name: name}
	return s.shareDirOp(ctx, OpShareMkdir, share, &p.Pool, &p)
}

// ShareRename переименовывает каталог rel общей папки в name (тот же родитель).
func (s *Service) ShareRename(ctx context.Context, share, rel, name string) (ShareDirOpResult, error) {
	p := ShareRenameParams{Share: share, Path: rel, Name: name}
	return s.shareDirOp(ctx, OpShareRename, share, &p.Pool, &p)
}

// ShareMove переносит каталог rel в каталог to той же общей папки
// ("" — корень), имя сохраняется.
func (s *Service) ShareMove(ctx context.Context, share, rel, to string) (ShareDirOpResult, error) {
	p := ShareMoveParams{Share: share, Path: rel, To: to}
	return s.shareDirOp(ctx, OpShareMove, share, &p.Pool, &p)
}

// ShareRmdir удаляет пустой каталог rel общей папки (не рекурсивно).
func (s *Service) ShareRmdir(ctx context.Context, share, rel string) (ShareDirOpResult, error) {
	p := ShareRmdirParams{Share: share, Path: rel}
	return s.shareDirOp(ctx, OpShareRmdir, share, &p.Pool, &p)
}

// shareDirPoolStub — допустимое имя пула для проверки параметров до чтения
// пула из БД (сам пул проверяется после GetShare).
const shareDirPoolStub = "p"

// shareDirOp — общая часть операций над каталогами: проверка параметров с
// заглушкой пула (неверный путь/имя — ErrInvalid без обращения к БД), пул
// папки из БД, повторная проверка, вызов helper. pool указывает на поле
// Pool в params.
func (s *Service) shareDirOp(ctx context.Context, op, share string, pool *string, params interface{ Validate() error }) (ShareDirOpResult, error) {
	*pool = shareDirPoolStub
	if err := params.Validate(); err != nil {
		return ShareDirOpResult{}, wrap(ErrInvalid, "%v", err)
	}
	sh, err := s.GetShare(ctx, share)
	if err != nil {
		return ShareDirOpResult{}, err
	}
	*pool = sh.Pool
	if err := params.Validate(); err != nil {
		return ShareDirOpResult{}, wrap(ErrInvalid, "%v", err)
	}
	var res ShareDirOpResult
	if err := s.helper.Call(ctx, op, params, &res); err != nil {
		return ShareDirOpResult{}, err
	}
	return res, nil
}

// FileRoot — корень веб-файлового менеджера, доступный пользователю.
type FileRoot struct {
	Name  string `json:"name"` // имя общей папки или HomesShare
	Pool  string `json:"pool"`
	Level string `json:"level"` // read или rw
	Home  bool   `json:"home,omitempty"`
}

// FileRoots — общие папки, к которым у user есть доступ, по тем же правилам,
// что и в Samba (Effective: группы раскрыты, отключённые пользователи без
// доступа; пул в read-only — только чтение), и его домашняя папка, если
// служба включена. Администратор особых прав не получает (как в SMB).
func (s *Service) FileRoots(ctx context.Context, user string) ([]FileRoot, error) {
	shares, err := s.build(ctx)
	if err != nil {
		return nil, err
	}
	out := []FileRoot{}
	in := func(list []string) bool {
		for _, x := range list {
			if x == user {
				return true
			}
		}
		return false
	}
	enabled := false
	for _, sh := range shares {
		switch {
		case in(sh.Write) && !sh.ReadOnly:
			out = append(out, FileRoot{Name: sh.Name, Pool: sh.Pool, Level: LevelRW})
		case in(sh.Write) || in(sh.Read):
			out = append(out, FileRoot{Name: sh.Name, Pool: sh.Pool, Level: LevelRead})
		}
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.Name == user && u.Enabled {
			enabled = true
		}
	}
	h, err := s.Homes(ctx)
	if err != nil {
		return nil, err
	}
	if h.Enabled && enabled {
		lvl := LevelRW
		var ro bool
		s.db.QueryRowContext(ctx, "SELECT read_only FROM share_pool_state WHERE pool = ?", h.Pool).Scan(&ro)
		if ro {
			lvl = LevelRead
		}
		out = append(out, FileRoot{Name: HomesShare, Pool: h.Pool, Level: lvl, Home: true})
	}
	return out, nil
}
