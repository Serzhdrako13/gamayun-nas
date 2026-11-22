package home

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Проверка пула: сначала md check по всем массивам (чтение всех
// копий; ошибку чтения dm-integrity md исправляет перезаписью с другой
// копии, расхождения копий — md/mismatch_cnt), затем btrfs scrub
// (контрольные суммы ФС; на одиночном профиле поверх md только находит).
const (
	OpScrubStart  = "home.pool.scrub_start"
	OpScrubCancel = "home.pool.scrub_cancel"
	OpScrubStatus = "home.pool.scrub_status"
)

// Стадии проверки.
const (
	ScrubStageMD    = "md"
	ScrubStageBtrfs = "btrfs"
)

// ScrubParams — запуск стадии проверки.
type ScrubParams struct {
	Pool  string `json:"pool"`
	Stage string `json:"stage"`
}

// Validate — имя пула и стадия.
func (p ScrubParams) Validate() error {
	if !ValidPoolName(p.Pool) {
		return errors.New("pool: недопустимое имя")
	}
	if p.Stage != ScrubStageMD && p.Stage != ScrubStageBtrfs {
		return errors.New("stage: md или btrfs")
	}
	return nil
}

// ScrubArray — состояние проверки массива md.
type ScrubArray struct {
	Name        string   `json:"name"`
	Device      string   `json:"device"`
	Action      string   `json:"action"` // idle, check, resync…
	Progress    *float64 `json:"progress,omitempty"`
	MismatchCnt *int64   `json:"mismatch_cnt,omitempty"`
}

// BtrfsScrub — итог/состояние btrfs scrub (btrfs scrub status -R).
type BtrfsScrub struct {
	Status              string `json:"status"` // none, running, finished, aborted, interrupted
	DataBytesScrubbed   int64  `json:"data_bytes_scrubbed"`
	TreeBytesScrubbed   int64  `json:"tree_bytes_scrubbed"`
	ReadErrors          int64  `json:"read_errors"`
	CsumErrors          int64  `json:"csum_errors"`
	VerifyErrors        int64  `json:"verify_errors"`
	SuperErrors         int64  `json:"super_errors"`
	CorrectedErrors     int64  `json:"corrected_errors"`
	UncorrectableErrors int64  `json:"uncorrectable_errors"`
}

// Errors — сумма найденных ошибок.
func (b BtrfsScrub) Errors() int64 {
	return b.ReadErrors + b.CsumErrors + b.VerifyErrors + b.SuperErrors
}

// ScrubStatus — состояние проверки пула.
type ScrubStatus struct {
	Pool   string       `json:"pool"`
	Arrays []ScrubArray `json:"arrays"`
	Btrfs  *BtrfsScrub  `json:"btrfs,omitempty"` // nil — ФС не смонтирована
}

// ParseBtrfsScrubStatus разбирает вывод `btrfs scrub status -R`.
func ParseBtrfsScrubStatus(out string) (BtrfsScrub, error) {
	b := BtrfsScrub{Status: "none"}
	fields := map[string]*int64{
		"data_bytes_scrubbed": &b.DataBytesScrubbed, "tree_bytes_scrubbed": &b.TreeBytesScrubbed,
		"read_errors": &b.ReadErrors, "csum_errors": &b.CsumErrors, "verify_errors": &b.VerifyErrors,
		"super_errors": &b.SuperErrors, "corrected_errors": &b.CorrectedErrors,
		"uncorrectable_errors": &b.UncorrectableErrors,
	}
	seen := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "Status" {
			b.Status = v
			continue
		}
		if p, ok := fields[k]; ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return b, fmt.Errorf("btrfs scrub status: %s: %q", k, v)
			}
			*p = n
			seen = true
		}
	}
	if !seen {
		return b, errors.New("btrfs scrub status: счётчики не найдены")
	}
	return b, nil
}

// ScrubStatus — состояние проверки: действие md и счётчики btrfs scrub.
func (e *Executor) ScrubStatus(ctx context.Context, p PoolParams) (ScrubStatus, error) {
	s := ScrubStatus{Pool: p.Pool, Arrays: []ScrubArray{}}
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return s, err
	}
	st, err := e.mdstat()
	if err != nil {
		return s, err
	}
	for _, a := range arrs {
		sa := ScrubArray{Name: a.name, Device: a.kname}
		sys, _ := e.readSysfs(a.kname, "sync_action")
		ms, ok := st[a.kname]
		sa.Action, sa.Progress = scrubAction(sys, ms, ok)
		if v, err := e.readInt(filepath.Join(e.Paths.Disks.SysClassBlock, a.kname, "md", "mismatch_cnt")); err == nil {
			sa.MismatchCnt = &v
		}
		s.Arrays = append(s.Arrays, sa)
	}
	mp := e.mountPoint(p.Pool)
	if _, _, ok := e.mountInfo(mp); ok {
		out, err := e.query(ctx, e.Tools.Btrfs, "scrub", "status", "-R", mp)
		if err != nil {
			return s, err
		}
		b, err := ParseBtrfsScrubStatus(string(out))
		if err != nil {
			return s, err
		}
		s.Btrfs = &b
	}
	return s, nil
}

// scrubAction — действие массива: из md/sync_action (точное), иначе из
// /proc/mdstat. Отложенное действие mdstat показывает как «resync=DELAYED»
// при любом виде (в т. ч. check, ждущий соседний массив на тех же дисках),
// поэтому прогресс берётся только у идущего действия.
func scrubAction(sys string, ms MdstatArray, inMdstat bool) (string, *float64) {
	action := sys
	if action == "" && inMdstat {
		action = ms.Action
	}
	if action == "" {
		action = "idle"
	}
	if inMdstat && !ms.Delayed && ms.Action != "" && (sys == "" || ms.Action == sys) {
		return action, ms.Progress
	}
	return action, nil
}

func (e *Executor) readSysfs(kname, attr string) (string, error) {
	b, err := os.ReadFile(filepath.Join(e.Paths.Disks.SysClassBlock, kname, "md", attr))
	return strings.TrimSpace(string(b)), err
}

// ScrubStart запускает стадию проверки. md: `mdadm --action=check` для
// каждого массива в покое (занятый массив — отказ: идёт resync/reshape/
// восстановление). btrfs: `btrfs scrub start` (в фоне, итог — ScrubStatus).
// Проверка только читает (md check не исправляет расхождения копий;
// исправление ошибок чтения — штатная перезапись md), поэтому allowlist
// дисков не требуется.
func (e *Executor) ScrubStart(ctx context.Context, p ScrubParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := Result{Pool: p.Pool}
	j := &journal{}
	defer func() { r.Commands = j.cmds }()
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return r, err
	}
	if len(arrs) == 0 {
		return r, refuse("пул %s: массивы не найдены", p.Pool)
	}
	switch p.Stage {
	case ScrubStageMD:
		st, err := e.mdstat()
		if err != nil {
			return r, err
		}
		for _, a := range arrs {
			ms, ok := st[a.kname]
			if !ok || ms.Inactive {
				return r, refuse("массив %s не активен", a.name)
			}
			if ms.RaidDevices > 0 && ms.InSync < ms.RaidDevices {
				return r, refuse("массив %s неполный (выбыл диск) — проверка данных отложена до восстановления", a.name)
			}
			sys, _ := e.readSysfs(a.kname, "sync_action")
			if act, _ := scrubAction(sys, ms, ok); act != "idle" && act != "check" {
				return r, refuse("массив %s занят: %s", a.name, act)
			}
		}
		for _, a := range arrs {
			sys, _ := e.readSysfs(a.kname, "sync_action")
			act, _ := scrubAction(sys, st[a.kname], true)
			if ms := st[a.kname]; act == "check" || ms.Level == "raid0" || ms.Level == "linear" {
				continue // уже идёт; raid0/linear — без избыточности, check не поддерживается
			}
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--action=check", a.node); err != nil {
				return r, err
			}
		}
	case ScrubStageBtrfs:
		mp := e.mountPoint(p.Pool)
		if _, _, ok := e.mountInfo(mp); !ok {
			return r, refuse("пул %s не смонтирован", p.Pool)
		}
		out, err := e.query(ctx, e.Tools.Btrfs, "scrub", "status", "-R", mp)
		if err == nil {
			if b, perr := ParseBtrfsScrubStatus(string(out)); perr == nil && b.Status == "running" {
				return r, nil
			}
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Btrfs, "scrub", "start", mp); err != nil {
			return r, err
		}
	}
	return r, nil
}

// ScrubCancel останавливает проверку: md check → idle, btrfs scrub cancel.
// Чужие действия md (resync, reshape, recovery) не трогает.
func (e *Executor) ScrubCancel(ctx context.Context, p PoolParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := Result{Pool: p.Pool}
	j := &journal{}
	defer func() { r.Commands = j.cmds }()
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return r, err
	}
	st, err := e.mdstat()
	if err != nil {
		return r, err
	}
	for _, a := range arrs {
		sys, _ := e.readSysfs(a.kname, "sync_action")
		ms, ok := st[a.kname]
		if act, _ := scrubAction(sys, ms, ok); act != "check" {
			continue
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--action=idle", a.node); err != nil {
			return r, err
		}
	}
	mp := e.mountPoint(p.Pool)
	if _, _, ok := e.mountInfo(mp); ok {
		out, err := e.query(ctx, e.Tools.Btrfs, "scrub", "status", "-R", mp)
		if err == nil {
			if b, perr := ParseBtrfsScrubStatus(string(out)); perr == nil && b.Status == "running" {
				if _, err := e.run(ctx, j, shortTimeout, e.Tools.Btrfs, "scrub", "cancel", mp); err != nil {
					return r, err
				}
			}
		}
	}
	return r, nil
}
