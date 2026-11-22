package home

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gamayun-nas/internal/tasks"
)

// TaskImport — подхват пула, найденного на дисках.
const TaskImport = "storage.pool.import"

// knownPools — имена пулов в базе.
func (s *Service) knownPools(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pools")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Found — пулы GamayunNas на дисках, которых нет в базе (собранные пулы базы
// не показываются). Known — имя занято пулом базы: подхват невозможен.
func (s *Service) Found(ctx context.Context) ([]FoundPool, error) {
	var r ScanResult
	if err := s.helper.Call(ctx, OpScan, ScanParams{}, &r); err != nil {
		return nil, err
	}
	known, err := s.knownPools(ctx)
	if err != nil {
		return nil, err
	}
	out := []FoundPool{}
	for _, p := range r.Pools {
		if known[p.Name] {
			// Диски пула базы — это он сам (собран или нет). Пул с тем же
			// именем на других дисках — конфликт имени.
			rec, err := s.getRecord(ctx, p.Name)
			if err == nil && sameDisks(rec, p) {
				continue
			}
			p.Known = true
		}
		out = append(out, p)
	}
	return out, nil
}

func sameDisks(rec PoolRecord, p FoundPool) bool {
	for _, a := range p.Arrays {
		for _, m := range a.Members {
			if rec.hasDisk(m.DiskID) {
				return true
			}
		}
	}
	return false
}

// foundPoolOf — имя найденного пула, которому принадлежит диск (для запрета
// использовать его как свободный).
func (s *Service) foundPoolOf(ctx context.Context, id string) string {
	ps, err := s.Found(ctx)
	if err != nil {
		return ""
	}
	for _, p := range ps {
		for _, a := range p.Arrays {
			for _, m := range a.Members {
				if m.DiskID == id {
					return p.Name
				}
			}
		}
	}
	return ""
}

type importTask struct {
	Pool     string `json:"pool"`
	Degraded bool   `json:"degraded,omitempty"`
}

// Import ставит задачу подхвата пула. Отказ сразу: пул с таким именем есть
// в базе, на дисках не найден, занят.
func (s *Service) Import(ctx context.Context, name string, degraded bool) (tasks.Task, error) {
	if !ValidPoolName(name) {
		return tasks.Task{}, wrap(ErrInvalid, "недопустимое имя пула %q", name)
	}
	ps, err := s.Found(ctx)
	if err != nil {
		return tasks.Task{}, err
	}
	var fp *FoundPool
	for i := range ps {
		if ps[i].Name == name {
			fp = &ps[i]
		}
	}
	switch {
	case fp == nil:
		return tasks.Task{}, wrap(ErrNotFound, "пул %s на дисках не найден", name)
	case fp.Known:
		return tasks.Task{}, wrap(ErrConflict, "пул с именем %s уже есть — подхват невозможен", name)
	case fp.Busy:
		return tasks.Task{}, wrap(ErrConflict, "том пула %s уже смонтирован", name)
	}
	raw, _ := json.Marshal(importTask{Pool: name, Degraded: degraded})
	return s.submit(ctx, name, TaskImport, raw)
}

func (s *Service) runImport(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var it importTask
	if err := json.Unmarshal(raw, &it); err != nil || it.Pool == "" {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	progress(1)
	if known, err := s.knownPools(ctx); err != nil {
		return nil, err
	} else if known[it.Pool] {
		return nil, wrap(ErrConflict, "пул с именем %s уже есть", it.Pool)
	}
	var r ImportResult
	if err := s.helper.Call(ctx, OpImport, ImportParams{Pool: it.Pool, Degraded: it.Degraded}, &r); err != nil {
		if strings.Contains(err.Error(), ErrDegraded.Error()) {
			return nil, wrap(ErrConflict, "%v — подтвердите подхват неполного пула", err)
		}
		return nil, err
	}
	progress(80)
	rec := PoolRecord{Name: it.Pool, Type: r.Type, Integrity: r.Integrity, IntegrityMode: r.IntegrityMode, Disks: r.Disks, Arrays: r.Arrays}
	if err := s.saveRecord(ctx, rec, true); err != nil {
		return nil, fmt.Errorf("пул собран, запись не сохранена: %w", err)
	}
	st, err := s.status(ctx, it.Pool)
	if err != nil {
		return nil, err
	}
	return ImportTaskResult{TaskResult: TaskResult{Pool: it.Pool, Status: st}, TypeGuessed: r.TypeGuessed, Degraded: r.Degraded, Warnings: r.Warnings}, nil
}

// ImportTaskResult — итог задачи подхвата.
type ImportTaskResult struct {
	TaskResult
	TypeGuessed bool     `json:"type_guessed,omitempty"`
	Degraded    []string `json:"degraded,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}
