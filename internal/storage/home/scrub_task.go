package home

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gamayun-nas/internal/tasks"
)

// TaskScrub — проверка пула: md check, затем btrfs scrub.
const TaskScrub = "storage.pool.scrub"

// Источник запуска проверки.
const (
	ScrubManual   = "manual"
	ScrubSchedule = "schedule"
)

type scrubTask struct {
	Pool    string `json:"pool"`
	Trigger string `json:"trigger"`
}

// ScrubResult — итог проверки пула.
type ScrubResult struct {
	Pool    string `json:"pool"`
	Trigger string `json:"trigger"`
	// Cancelled — остановлена пользователем (стадии после остановки не шли).
	Cancelled bool `json:"cancelled,omitempty"`
	// MismatchCnt — сумма md/mismatch_cnt после check: копии расходятся
	// (check не исправляет; при dm-integrity не ожидается).
	MismatchCnt int64        `json:"mismatch_cnt"`
	Arrays      []ScrubArray `json:"arrays"`
	Btrfs       *BtrfsScrub  `json:"btrfs,omitempty"`
	// IntegrityErrorsDelta — прирост счётчиков ошибок пула (dm-integrity +
	// btrfs device stats) за проверку: найденные при чтении ошибки; под md
	// с избыточностью исправлены перезаписью с другой копии.
	IntegrityErrorsDelta int64     `json:"integrity_errors_delta"`
	StartedAt            time.Time `json:"started_at"`
	FinishedAt           time.Time `json:"finished_at"`
}

// Severity — уровень события по итогу: неисправимое — critical, любые
// найденные ошибки или расхождения — warning, чисто — info.
func (r ScrubResult) Severity() string {
	switch {
	case r.Btrfs != nil && r.Btrfs.UncorrectableErrors > 0:
		return "critical"
	case r.MismatchCnt > 0 || r.IntegrityErrorsDelta > 0 || (r.Btrfs != nil && r.Btrfs.Errors() > 0):
		return "warning"
	}
	return "info"
}

// Scrub ставит проверку пула. Только исправный пул: при деградации,
// восстановлении или перестройке чтение всех копий невозможно или мешает.
func (s *Service) Scrub(ctx context.Context, pool, trigger string) (tasks.Task, error) {
	if _, err := s.getRecord(ctx, pool); err != nil {
		return tasks.Task{}, err
	}
	if trigger != ScrubSchedule {
		trigger = ScrubManual
	}
	st, err := s.status(ctx, pool)
	if err != nil {
		return tasks.Task{}, err
	}
	if m := syncBusy(pool, st, "проверка данных"); m != "" {
		return tasks.Task{}, wrap(ErrConflict, "%s", m)
	}
	if st.Health != HealthOK {
		return tasks.Task{}, wrap(ErrConflict, "пул %s: состояние %s — проверка возможна только у исправного пула", pool, st.Health)
	}
	if !st.Mounted {
		return tasks.Task{}, wrap(ErrConflict, "пул %s не смонтирован", pool)
	}
	raw, err := json.Marshal(scrubTask{Pool: pool, Trigger: trigger})
	if err != nil {
		return tasks.Task{}, err
	}
	s.mu.Lock()
	delete(s.scrubStop, pool)
	s.mu.Unlock()
	return s.submit(ctx, pool, TaskScrub, raw)
}

// ScrubCancel останавливает идущую проверку пула.
func (s *Service) ScrubCancel(ctx context.Context, pool string) error {
	s.mu.Lock()
	running := s.busy[pool] == TaskScrub
	if running {
		if s.scrubStop == nil {
			s.scrubStop = map[string]bool{}
		}
		s.scrubStop[pool] = true
	}
	s.mu.Unlock()
	if !running {
		return wrap(ErrConflict, "пул %s: проверка не идёт", pool)
	}
	var r Result
	return s.helper.Call(ctx, OpScrubCancel, PoolParams{Pool: pool}, &r)
}

func (s *Service) scrubStopped(pool string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrubStop[pool]
}

func (s *Service) runScrub(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var t scrubTask
	if err := json.Unmarshal(raw, &t); err != nil || t.Pool == "" {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	res := ScrubResult{Pool: t.Pool, Trigger: t.Trigger, Arrays: []ScrubArray{}, StartedAt: s.now().UTC()}
	progress(1)
	before, err := s.status(ctx, t.Pool)
	if err != nil {
		return nil, err
	}
	// Отмена задачи (менеджер, сторож, остановка gamayund) — остановить
	// проверку в ядре, иначе она шла бы без наблюдения.
	stop := func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var r Result
		s.helper.Call(c, OpScrubCancel, PoolParams{Pool: t.Pool}, &r)
	}
	finish := func(err error) (any, error) {
		res.FinishedAt = s.now().UTC()
		if err == nil {
			if after, serr := s.status(context.WithoutCancel(ctx), t.Pool); serr == nil && after.IntegrityErrors > before.IntegrityErrors {
				res.IntegrityErrorsDelta = after.IntegrityErrors - before.IntegrityErrors
			}
		}
		if s.OnScrub != nil {
			s.OnScrub(res, err)
		}
		if err != nil {
			return nil, err
		}
		return res, nil
	}

	// Стадия 1: md check.
	var r Result
	if err := s.helper.Call(ctx, OpScrubStart, ScrubParams{Pool: t.Pool, Stage: ScrubStageMD}, &r); err != nil {
		return finish(err)
	}
	for {
		if s.scrubStopped(t.Pool) {
			res.Cancelled = true
			return finish(nil)
		}
		var st ScrubStatus
		if err := s.helper.Call(ctx, OpScrubStatus, PoolParams{Pool: t.Pool}, &st); err != nil {
			if ctx.Err() != nil {
				stop()
			}
			return finish(err)
		}
		busy, sum, other := 0, 0.0, ""
		for _, a := range st.Arrays {
			switch a.Action {
			case "check":
				busy++
				if a.Progress != nil {
					sum += *a.Progress
				}
			case "idle", "":
				sum += 100
			default:
				other = a.Name + ": " + a.Action
			}
		}
		if other != "" {
			// Ядро начало восстановление/перестройку (отказ диска во время
			// проверки) — проверка прервана, пул разбирают защита и коллектор.
			return finish(fmt.Errorf("проверка прервана: массив %s", other))
		}
		if n := len(st.Arrays); n > 0 {
			progress(1 + int(sum/float64(n)*0.8))
		}
		if busy == 0 {
			res.Arrays = st.Arrays
			for _, a := range st.Arrays {
				if a.MismatchCnt != nil {
					res.MismatchCnt += *a.MismatchCnt
				}
			}
			break
		}
		tasks.Alive(ctx, fmt.Sprintf("md check: %d из %d массивов", busy, len(st.Arrays)))
		select {
		case <-ctx.Done():
			stop()
			return finish(ctx.Err())
		case <-time.After(s.Poll):
		}
	}
	if s.scrubStopped(t.Pool) {
		res.Cancelled = true
		return finish(nil)
	}

	// Стадия 2: btrfs scrub.
	progress(81)
	if err := s.helper.Call(ctx, OpScrubStart, ScrubParams{Pool: t.Pool, Stage: ScrubStageBtrfs}, &r); err != nil {
		return finish(err)
	}
	for {
		var st ScrubStatus
		if err := s.helper.Call(ctx, OpScrubStatus, PoolParams{Pool: t.Pool}, &st); err != nil {
			if ctx.Err() != nil {
				stop()
			}
			return finish(err)
		}
		if st.Btrfs == nil {
			return finish(fmt.Errorf("пул %s не смонтирован — проверка данных (btrfs scrub) не завершена", t.Pool))
		}
		if st.Btrfs.Status != "running" {
			b := *st.Btrfs
			res.Btrfs = &b
			if s.scrubStopped(t.Pool) || strings.HasPrefix(b.Status, "aborted") || strings.HasPrefix(b.Status, "interrupted") {
				res.Cancelled = s.scrubStopped(t.Pool)
				if !res.Cancelled {
					return finish(fmt.Errorf("btrfs scrub: %s", b.Status))
				}
			}
			break
		}
		tasks.Alive(ctx, "btrfs scrub")
		select {
		case <-ctx.Done():
			stop()
			return finish(ctx.Err())
		case <-time.After(s.Poll):
		}
	}
	progress(100)
	return finish(nil)
}

// ScrubRunning — идёт ли проверка какого-либо пула.
func (s *Service) ScrubRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.busy {
		if k == TaskScrub {
			return true
		}
	}
	return false
}

// Names — имена пулов из записей.
func (s *Service) Names(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pools ORDER BY name")
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
	return out, rows.Err()
}
