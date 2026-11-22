// Package store открывает базу состояния gamayund (SQLite, WAL) и применяет
// миграции схемы.
//
// Драйвер — modernc.org/sqlite (чистый Go, без CGO): статический бинарник и
// кросс-компиляция под arm64 без C-тулчейна. Схема версионируется списком
// migrations; применённые версии — в таблице schema_migrations. База новее
// кода (например, после отката ОС на старый слот) не открывается: демон
// не должен работать со схемой, которую не понимает.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // драйвер "sqlite"
)

// ErrSchemaTooNew — в базе применены миграции, неизвестные этой версии кода.
var ErrSchemaTooNew = errors.New("схема базы новее, чем поддерживает эта версия")

// Migration — один шаг схемы. Версии строго возрастают с 1 без пропусков;
// применённую миграцию не менять — только добавлять новую.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// migrations — история схемы. Время — INTEGER, миллисекунды Unix (UTC).
var migrations = []Migration{
	{Version: 1, Name: "tasks_events", SQL: `
CREATE TABLE tasks (
	id          TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	state       TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed','interrupted')),
	progress    INTEGER NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
	params      TEXT NOT NULL DEFAULT 'null',
	result      TEXT,
	error_code  TEXT,
	error_msg   TEXT,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	started_at  INTEGER,
	finished_at INTEGER
);
CREATE INDEX tasks_created ON tasks(created_at DESC);
CREATE INDEX tasks_state ON tasks(state);

CREATE TABLE events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      INTEGER NOT NULL,
	type    TEXT NOT NULL,
	task_id TEXT,
	data    TEXT NOT NULL DEFAULT 'null'
);
CREATE INDEX events_task ON events(task_id);
`},
	{Version: 2, Name: "pools", SQL: `
CREATE TABLE pools (
	name       TEXT PRIMARY KEY,
	type       TEXT NOT NULL,
	integrity  INTEGER NOT NULL DEFAULT 0,
	layout     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`},
	// Учётные записи и сессии (см. internal/auth/store.go). Таблица
	// users рассчитана на расширение (обычные пользователи,
	// группы — отдельными таблицами, новые столбцы — ALTER TABLE).
	{Version: 3, Name: "auth", SQL: `
CREATE TABLE users (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	name                TEXT NOT NULL UNIQUE,
	password            TEXT,
	is_admin            INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0,1)),
	disabled            INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0,1)),
	created_at          INTEGER NOT NULL,
	updated_at          INTEGER NOT NULL,
	password_changed_at INTEGER
);

CREATE TABLE sessions (
	id_hash      BLOB PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	remote_addr  TEXT NOT NULL DEFAULT '',
	user_agent   TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE login_devices (
	token_hash   BLOB PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX login_devices_user ON login_devices(user_id);
`},
	// Защита записи при исчерпании запаса избыточности. Нет строки
	// pool_protection — защита включена (значение по умолчанию). Журнал
	// pool_protection_log не очищается при удалении пула.
	{Version: 4, Name: "pool_protection", SQL: `
CREATE TABLE pool_protection (
	pool            TEXT PRIMARY KEY,
	enabled         INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
	enforced        INTEGER NOT NULL DEFAULT 0 CHECK (enforced IN (0,1)),
	override_actor  TEXT,
	override_reason TEXT,
	override_from   INTEGER,
	override_until  INTEGER,
	updated_at      INTEGER NOT NULL
);

CREATE TABLE pool_protection_log (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	pool       TEXT NOT NULL,
	ts         INTEGER NOT NULL,
	actor      TEXT NOT NULL,
	action     TEXT NOT NULL,
	reason     TEXT NOT NULL DEFAULT '',
	expires_at INTEGER
);
CREATE INDEX pool_protection_log_pool ON pool_protection_log(pool, ts);
`},
	// Локальные пользователи и группы NAS (отдельно от администраторов
	// gamayund в users), общие папки Samba и уровни доступа. Субъект доступа —
	// пользователь или группа; level: none, read, rw. share_pool_state —
	// пулы в read-only.
	{Version: 5, Name: "nas_shares", SQL: `
CREATE TABLE nas_users (
	name       TEXT PRIMARY KEY,
	enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
	created_at INTEGER NOT NULL
);
CREATE TABLE nas_groups (
	name       TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL
);
CREATE TABLE nas_group_members (
	grp  TEXT NOT NULL REFERENCES nas_groups(name) ON DELETE CASCADE,
	user TEXT NOT NULL REFERENCES nas_users(name) ON DELETE CASCADE,
	PRIMARY KEY (grp, user)
) WITHOUT ROWID;
CREATE TABLE shares (
	name       TEXT PRIMARY KEY,
	pool       TEXT NOT NULL,
	comment    TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX shares_pool ON shares(pool);
CREATE TABLE share_access (
	share   TEXT NOT NULL REFERENCES shares(name) ON DELETE CASCADE,
	kind    TEXT NOT NULL CHECK (kind IN ('user','group')),
	subject TEXT NOT NULL,
	level   TEXT NOT NULL CHECK (level IN ('none','read','rw')),
	PRIMARY KEY (share, kind, subject)
) WITHOUT ROWID;
CREATE TABLE share_pool_state (
	pool      TEXT PRIMARY KEY,
	read_only INTEGER NOT NULL CHECK (read_only IN (0,1))
);
`},
	// Вход по SSH (учётка ОС). Администраторам — включён по умолчанию.
	{Version: 6, Name: "nas_user_ssh", SQL: `
ALTER TABLE nas_users ADD COLUMN ssh INTEGER NOT NULL DEFAULT 0 CHECK (ssh IN (0,1));
UPDATE nas_users SET ssh = 1 WHERE name IN (SELECT user FROM nas_group_members WHERE grp = 'administrators');
`},
	// Настройки (ключ-значение) — «мастер пройден», домашние
	// папки, защита SSH. Уже настроенная установка (есть администратор)
	// считается прошедшей мастер.
	{Version: 7, Name: "settings", SQL: `
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
INSERT INTO settings(key, value) SELECT 'setup.done', '1' WHERE EXISTS (SELECT 1 FROM users WHERE is_admin = 1);
`},
	// Пароль задан не везде (веб / ОС и Samba) после сбоя на
	// середине — текст причины; NULL — пароль согласован.
	{Version: 8, Name: "nas_users_password_sync", SQL: `
ALTER TABLE nas_users ADD COLUMN password_sync_error TEXT;
`},
	// Признак жизни задачи (время и последнее наблюдение) —
	// для сторожа задач и для причины interrupted после перезапуска.
	{Version: 9, Name: "task_alive", SQL: `
ALTER TABLE tasks ADD COLUMN alive_at INTEGER;
ALTER TABLE tasks ADD COLUMN alive_note TEXT;
`},
	// Уровень важности и предмет события (диск/массив/пул) — для
	// фильтров журнала и поиска последнего события по предмету.
	{Version: 10, Name: "events_severity_subject", SQL: `
ALTER TABLE events ADD COLUMN severity TEXT NOT NULL DEFAULT 'info';
ALTER TABLE events ADD COLUMN subject TEXT;
CREATE INDEX events_subject ON events(subject, id);
`},
	// Последний снимок коллектора событий — после перезапуска
	// новый снимок сравнивается с ним, изменения за простой не теряются.
	{Version: 11, Name: "collector_state", SQL: `
CREATE TABLE collector_state (
	id   INTEGER PRIMARY KEY CHECK (id = 1),
	ts   INTEGER NOT NULL,
	data TEXT NOT NULL
);
`},
	// Центр уведомлений (копия события — переживает обрезку журнала)
	// и очередь внешней доставки (outbox) по каналам; курсор отбора —
	// settings 'notify.cursor' (ID последнего обработанного события).
	{Version: 12, Name: "notifications", SQL: `
CREATE TABLE notifications (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id INTEGER NOT NULL UNIQUE,
	ts       INTEGER NOT NULL,
	type     TEXT NOT NULL,
	severity TEXT NOT NULL,
	subject  TEXT,
	data     TEXT NOT NULL,
	read_at  INTEGER
);
CREATE TABLE outbox (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	notif_id   INTEGER NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
	channel    TEXT NOT NULL,
	state      TEXT NOT NULL DEFAULT 'pending',
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_at    INTEGER NOT NULL,
	last_error TEXT,
	sent_at    INTEGER,
	UNIQUE (notif_id, channel)
);
CREATE INDEX outbox_due ON outbox(state, next_at);
`},
	// Группировка серий во внешних каналах. notify_groups — окно
	// серии (тип+предмет): первое событие уходит сразу, повторы считаются
	// и по окончании окна уходят одной строкой outbox (repeats > 0) на
	// последнее уведомление серии. Тишина — settings 'notify.quiet'.
	{Version: 13, Name: "notify_groups", SQL: `
CREATE TABLE notify_groups (
	key        TEXT PRIMARY KEY,
	window_end INTEGER NOT NULL,
	repeats    INTEGER NOT NULL DEFAULT 0,
	first_ts   INTEGER,
	last_notif INTEGER
);
ALTER TABLE outbox ADD COLUMN repeats INTEGER NOT NULL DEFAULT 0;
ALTER TABLE outbox ADD COLUMN repeats_since INTEGER;
`},
	// История SMART — точка на плановый опрос (температура и
	// счётчики секторов); прореживание и срок — internal/maintenance.
	{Version: 14, Name: "smart_history", SQL: `
CREATE TABLE smart_history (
	disk_id        TEXT NOT NULL,
	ts             INTEGER NOT NULL,
	temperature_c  INTEGER,
	reallocated    INTEGER,
	pending        INTEGER,
	uncorrectable  INTEGER,
	power_on_hours INTEGER,
	PRIMARY KEY (disk_id, ts)
) WITHOUT ROWID;
`},
}

// Open открывает (создаёт) базу по пути и применяет миграции.
// Новый файл создаётся с правами 0600.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("не задан путь к базе")
	}
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("путь к базе не должен содержать '?' или '#': %q", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("база %s: %w", path, err)
	}
	f.Close()

	// synchronous(FULL): каждый коммит WAL сбрасывается на диск. При NORMAL
	// сброс питания терял последние коммиты
	// (пользователь и папка за ~25 с до qm reset пропали, учётка ОС осталась).
	// Запись в базу редкая — цена fsync на коммит несущественна.
	// busy_timeout применяется драйвером первым; _txlock=immediate —
	// транзакции сразу берут блокировку записи, что исключает SQLITE_BUSY
	// при повышении блокировки чтения до записи в WAL.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("база %s: %w", path, err)
	}
	if mode != "wal" {
		db.Close()
		return nil, fmt.Errorf("база %s: режим журнала %q вместо wal", path, mode)
	}
	if err := migrate(ctx, db, migrations); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// LatestVersion — последняя версия схемы, известная коду.
func LatestVersion() int { return migrations[len(migrations)-1].Version }

// SchemaVersion возвращает максимальную применённую версию (0 — пустая база).
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v)
	if err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func migrate(ctx context.Context, db *sql.DB, list []Migration) error {
	for i, m := range list {
		if m.Version != i+1 {
			return fmt.Errorf("миграции: версия %d на позиции %d", m.Version, i)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("миграции: %w", err)
	}
	cur, err := SchemaVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("миграции: %w", err)
	}
	if cur > len(list) {
		return fmt.Errorf("%w: в базе %d, в коде %d", ErrSchemaTooNew, cur, len(list))
	}
	for _, m := range list[cur:] {
		if err := apply(ctx, db, m); err != nil {
			return fmt.Errorf("миграция %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)",
		m.Version, m.Name, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
