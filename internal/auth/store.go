package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Таблицы (миграция 3 в internal/store):
//   - users — учётные записи. Сейчас только администраторы (is_admin=1);
// добавляет обычных пользователей (is_admin=0), группы и связи
//     отдельными таблицами и столбцы (uid и т. п.) через ALTER TABLE.
//     password NULL — вход паролем невозможен (например, учётная запись
//     только для SMB).
//   - sessions — серверные сессии; хранится только SHA-256 идентификатора.
//   - login_devices — «доверенные устройства» (браузеры, где уже был вход):
//     снимают блокировку учётной записи по перебору (см. Service.login).

var (
	// ErrUserExists — имя уже занято.
	ErrUserExists = errors.New("пользователь с таким именем уже есть")
	// ErrUserNotFound — нет такого пользователя.
	ErrUserNotFound = errors.New("пользователь не найден")
	// ErrAlreadyConfigured — администратор уже создан (первичная настройка закрыта).
	ErrAlreadyConfigured = errors.New("администратор уже создан")
	errNoSession         = errors.New("нет сессии")
)

// User — учётная запись.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	IsAdmin   bool      `json:"is_admin"`
	Disabled  bool      `json:"-"`
	CreatedAt time.Time `json:"-"`
	hash      sql.NullString
}

// Session — серверная сессия.
type Session struct {
	idHash    []byte
	token     string // только у только что созданной/предъявленной сессии
	UserID    int64
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time // абсолютный срок
}

// CSRFToken — синхронизирующий токен сессии: HMAC-SHA256 с ключом —
// идентификатором сессии. В базе не хранится; по утечке базы не вычисляется.
func (s *Session) CSRFToken() string {
	m := hmac.New(sha256.New, []byte(s.token))
	m.Write([]byte("csrf"))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Store — доступ к таблицам аутентификации.
type Store struct{ DB *sql.DB }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// newToken — 32 случайных байта (crypto/rand) в base64url и SHA-256 от них.
func newToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	return tok, tokenHash(tok), nil
}

func tokenHash(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

const userCols = "id, name, password, is_admin, disabled, created_at"

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created int64
	err := row.Scan(&u.ID, &u.Name, &u.hash, &u.IsAdmin, &u.Disabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUserNotFound
	}
	u.CreatedAt = fromMS(created)
	return u, err
}

// UserByName ищет пользователя по имени.
func (s Store) UserByName(ctx context.Context, name string) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE name = ?", name))
}

// Users — все пользователи по имени.
func (s Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// AdminCount — число активных администраторов с паролем.
func (s Store) AdminCount(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0 AND password IS NOT NULL").Scan(&n)
	return n, err
}

// CreateUser добавляет пользователя с готовым хэшем пароля. firstOnly —
// только если активных администраторов ещё нет (первичная настройка),
// проверка и вставка в одной транзакции.
func (s Store) CreateUser(ctx context.Context, name, hash string, admin, firstOnly bool, now time.Time) (User, error) {
	if err := ValidateName(name); err != nil {
		return User{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if firstOnly {
		var n int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0 AND password IS NOT NULL").Scan(&n); err != nil {
			return User{}, err
		}
		if n > 0 {
			return User{}, ErrAlreadyConfigured
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users(name, password, is_admin, disabled, created_at, updated_at, password_changed_at)
		 VALUES (?, ?, ?, 0, ?, ?, ?)`, name, hash, admin, ms(now), ms(now), ms(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrUserExists
		}
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: id, Name: name, IsAdmin: admin, CreatedAt: now, hash: sql.NullString{String: hash, Valid: true}}, nil
}

// SetPassword меняет хэш пароля.
func (s Store) SetPassword(ctx context.Context, userID int64, hash string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx,
		"UPDATE users SET password = ?, updated_at = ?, password_changed_at = ? WHERE id = ?",
		hash, ms(now), ms(now), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// rehash заменяет хэш, только если он не менялся с момента проверки.
func (s Store) rehash(ctx context.Context, userID int64, old, hash string) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE users SET password = ? WHERE id = ? AND password = ?", hash, userID, old)
	return err
}

// --- сессии ---

// maxSessionsPerUser — сверх этого удаляются самые старые сессии.
const maxSessionsPerUser = 32

// CreateSession создаёт сессию; токен возвращается один раз и в базе не хранится.
func (s Store) CreateSession(ctx context.Context, userID int64, now time.Time, absolute time.Duration, remote, ua string) (*Session, error) {
	tok, h, err := newToken()
	if err != nil {
		return nil, err
	}
	now = now.UTC().Truncate(time.Millisecond) // как при чтении из базы
	if len(ua) > 256 {
		ua = ua[:256]
	}
	sess := &Session{idHash: h, token: tok, UserID: userID, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(absolute)}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions(id_hash, user_id, created_at, last_seen_at, expires_at, remote_addr, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`, h, userID, ms(now), ms(now), ms(sess.ExpiresAt), remote, ua); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id_hash NOT IN
		 (SELECT id_hash FROM sessions WHERE user_id = ? ORDER BY created_at DESC LIMIT ?)`,
		userID, userID, maxSessionsPerUser); err != nil {
		return nil, err
	}
	return sess, tx.Commit()
}

// lookupSession — сессия по токену и её пользователь (без проверки сроков).
func (s Store) lookupSession(ctx context.Context, tok string) (*Session, User, error) {
	sess, u, err := s.lookupSessionHash(ctx, tokenHash(tok))
	if sess != nil {
		sess.token = tok
	}
	return sess, u, err
}

// lookupSessionHash — сессия по SHA-256 токена (сессии приложений
// хранят только хэш родительской сессии).
func (s Store) lookupSessionHash(ctx context.Context, idHash []byte) (*Session, User, error) {
	sess := &Session{idHash: idHash}
	var created, seen, exp int64
	var u User
	var uc int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT s.user_id, s.created_at, s.last_seen_at, s.expires_at,
		        u.id, u.name, u.password, u.is_admin, u.disabled, u.created_at
		 FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id_hash = ?`, sess.idHash).
		Scan(&sess.UserID, &created, &seen, &exp, &u.ID, &u.Name, &u.hash, &u.IsAdmin, &u.Disabled, &uc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, u, errNoSession
	}
	if err != nil {
		return nil, u, err
	}
	sess.CreatedAt, sess.LastSeen, sess.ExpiresAt, u.CreatedAt = fromMS(created), fromMS(seen), fromMS(exp), fromMS(uc)
	return sess, u, nil
}

func (s Store) touchSession(ctx context.Context, sess *Session, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?", ms(now), sess.idHash)
	if err == nil {
		sess.LastSeen = now
	}
	return err
}

func (s Store) deleteSession(ctx context.Context, idHash []byte) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE id_hash = ?", idHash)
	return err
}

// RevokeSessions удаляет все сессии пользователя, кроме except (может быть
// nil). Возвращает число удалённых.
func (s Store) RevokeSessions(ctx context.Context, userID int64, except *Session) (int64, error) {
	var keep []byte
	if except != nil {
		keep = except.idHash
	}
	res, err := s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND id_hash IS NOT ?", userID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeSessions удаляет истёкшие (по абсолютному сроку или простою) сессии.
func (s Store) PurgeSessions(ctx context.Context, now time.Time, idle time.Duration) (int64, error) {
	res, err := s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?",
		ms(now), ms(now.Add(-idle)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- доверенные устройства ---

const (
	deviceTTL           = 180 * 24 * time.Hour
	maxDevicesPerUser   = 16
	deviceTouchInterval = time.Hour
)

func (s Store) createDevice(ctx context.Context, userID int64, now time.Time) (string, error) {
	tok, h, err := newToken()
	if err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx,
		"INSERT INTO login_devices(token_hash, user_id, created_at, last_used_at) VALUES (?, ?, ?, ?)",
		h, userID, ms(now), ms(now)); err != nil {
		return "", err
	}
	_, err = s.DB.ExecContext(ctx,
		`DELETE FROM login_devices WHERE (user_id = ? AND token_hash NOT IN
		 (SELECT token_hash FROM login_devices WHERE user_id = ? ORDER BY last_used_at DESC LIMIT ?))
		 OR last_used_at <= ?`, userID, userID, maxDevicesPerUser, ms(now.Add(-deviceTTL)))
	return tok, err
}

// deviceUser — владелец действующего токена устройства (0 — нет).
func (s Store) deviceUser(ctx context.Context, tok string, now time.Time) (int64, error) {
	var uid, last int64
	err := s.DB.QueryRowContext(ctx, "SELECT user_id, last_used_at FROM login_devices WHERE token_hash = ?",
		tokenHash(tok)).Scan(&uid, &last)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && now.Sub(fromMS(last)) > deviceTTL) {
		return 0, nil
	}
	return uid, err
}

func (s Store) touchDevice(ctx context.Context, tok string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE login_devices SET last_used_at = ? WHERE token_hash = ? AND last_used_at < ?",
		ms(now), tokenHash(tok), ms(now.Add(-deviceTouchInterval)))
	return err
}

func (s Store) deleteDevice(ctx context.Context, tok string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM login_devices WHERE token_hash = ?", tokenHash(tok))
	return err
}

// String — для журналов.
func (u User) String() string { return fmt.Sprintf("%s(#%d)", u.Name, u.ID) }
