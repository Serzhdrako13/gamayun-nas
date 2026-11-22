package daemon

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/snapshots"
	"gamayun-nas/internal/tasks"
)

// newSnapshots — сервис снапшотов: общие папки и служба домашних
// папок (если включена) с их пулами.
func newSnapshots(socket string, db *sql.DB, evlog *events.Log, logger *slog.Logger, mgr *tasks.Manager, nasSvc *nas.Service) (*snapshots.Service, error) {
	s := &snapshots.Service{
		DB:     db,
		Helper: &helper.Client{Socket: socket, Timeout: snapshots.RestoreTimeout + 5*time.Minute},
		Events: evlog, Logger: logger, Tasks: mgr,
		Targets: func(ctx context.Context) ([]snapshots.Target, error) {
			shares, err := nasSvc.ListShares(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]snapshots.Target, 0, len(shares)+1)
			for _, sh := range shares {
				out = append(out, snapshots.Target{Share: sh.Name, Pool: sh.Pool})
			}
			h, err := nasSvc.Homes(ctx)
			if err != nil {
				return nil, err
			}
			if h.Enabled && h.Pool != "" {
				out = append(out, snapshots.Target{Share: nas.HomesShare, Pool: h.Pool})
			}
			return out, nil
		},
	}
	return s, s.Init()
}
