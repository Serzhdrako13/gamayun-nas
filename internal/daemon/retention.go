package daemon

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/notify"
)

// pruneJournals — обрезка журнала событий и центра уведомлений:
// сначала по сроку хранения уровня (настройки retention), затем
// по пределу числа записей. Ошибки только в журнал службы: обрезка не
// должна мешать запуску и работе.
func pruneJournals(ctx context.Context, db *sql.DB, evlog *events.Log, ns *notify.Service, logger *slog.Logger, now time.Time) {
	r, err := events.LoadRetention(ctx, db)
	if err != nil {
		logger.Warn("сроки хранения не прочитаны, используются по умолчанию", "err", err, "retention", r)
	}
	report := func(what string, n int64, err error) {
		if err != nil {
			logger.Warn(what+": обрезка", "err", err)
		} else if n > 0 {
			logger.Info(what+" обрезан", "removed", n)
		}
	}
	var n, m int64
	n, err = evlog.PruneAge(ctx, r, now)
	if err == nil {
		m, err = evlog.Prune(ctx, EventsKeep)
	}
	report("журнал событий", n+m, err)
	if ns == nil {
		return
	}
	n, m = 0, 0
	n, err = ns.PruneAge(ctx, r, now)
	if err == nil {
		m, err = ns.Prune(ctx, NotificationsKeep)
	}
	report("центр уведомлений", n+m, err)
}

// runRetention вызывает prune с периодом every до отмены ctx.
func runRetention(ctx context.Context, every time.Duration, prune func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}
