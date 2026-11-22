package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Хранение журнала событий и центра уведомлений по сроку.
// Сроки задаются по уровням важности; предел по числу записей (EventsKeep,
// NotificationsKeep в daemon) остаётся страховкой сверху.

// RetentionKey — ключ настроек сроков хранения в таблице settings (JSON).
const RetentionKey = "retention"

// MaxRetentionDays — верхняя граница срока хранения (10 лет).
const MaxRetentionDays = 3650

// ErrInvalidRetention — недопустимые сроки хранения.
var ErrInvalidRetention = errors.New("недопустимые сроки хранения")

// Retention — сроки хранения в днях по уровням важности.
type Retention struct {
	InfoDays     int `json:"info_days"`
	WarningDays  int `json:"warning_days"`
	CriticalDays int `json:"critical_days"`
}

// DefaultRetention — info/warning 90 дней, critical 365 дней.
func DefaultRetention() Retention {
	return Retention{InfoDays: 90, WarningDays: 90, CriticalDays: 365}
}

// Validate — каждый срок от 1 до MaxRetentionDays дней.
func (r Retention) Validate() error {
	for _, x := range []struct {
		name string
		v    int
	}{{"info_days", r.InfoDays}, {"warning_days", r.WarningDays}, {"critical_days", r.CriticalDays}} {
		if x.v < 1 || x.v > MaxRetentionDays {
			return fmt.Errorf("%w: %s должно быть от 1 до %d", ErrInvalidRetention, x.name, MaxRetentionDays)
		}
	}
	return nil
}

// Cutoffs — границы (Unix мс): записи уровня старше границы удаляются.
// Неизвестный уровень считается info.
func (r Retention) Cutoffs(now time.Time) (info, warning, critical int64) {
	day := 24 * time.Hour
	return now.Add(-time.Duration(r.InfoDays) * day).UnixMilli(),
		now.Add(-time.Duration(r.WarningDays) * day).UnixMilli(),
		now.Add(-time.Duration(r.CriticalDays) * day).UnixMilli()
}

// AgeCondition — условие SQL «запись старше срока своего уровня» по
// столбцам ts и severity и его аргументы.
func (r Retention) AgeCondition(now time.Time) (string, []any) {
	i, w, c := r.Cutoffs(now)
	return "((severity = 'critical' AND ts < ?) OR (severity = 'warning' AND ts < ?)" +
		" OR (severity NOT IN ('critical', 'warning') AND ts < ?))", []any{c, w, i}
}

// LoadRetention — сохранённые сроки; нет записи — DefaultRetention.
func LoadRetention(ctx context.Context, db *sql.DB) (Retention, error) {
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", RetentionKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultRetention(), nil
	}
	if err != nil {
		return DefaultRetention(), err
	}
	r := DefaultRetention()
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return DefaultRetention(), err
	}
	if err := r.Validate(); err != nil {
		return DefaultRetention(), err
	}
	return r, nil
}

// SaveRetention проверяет и сохраняет сроки.
func SaveRetention(ctx context.Context, db *sql.DB, r Retention) error {
	if err := r.Validate(); err != nil {
		return err
	}
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", RetentionKey, string(v))
	return err
}

// PruneAge удаляет события старше срока своего уровня. Последнее событие
// каждого предмета (диск, массив, пул) сохраняется при любом возрасте: по
// нему API показывает состояние пропавших дисков и последнее событие пула.
func (l *Log) PruneAge(ctx context.Context, r Retention, now time.Time) (int64, error) {
	cond, args := r.AgeCondition(now)
	res, err := l.db.ExecContext(ctx, "DELETE FROM events WHERE "+cond+
		" AND id NOT IN (SELECT MAX(id) FROM events WHERE subject IS NOT NULL GROUP BY subject)", args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
