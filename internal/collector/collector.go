// Package collector — коллектор событий оборудования.
//
// Принцип: событие получается только из разницы двух снимков состояния
// (диски, пулы). Последний снимок хранится в базе, поэтому изменения,
// случившиеся пока gamayund был остановлен, становятся событиями при первой
// сверке после старта. Источники (таймер, uevent) лишь запускают сверку.
//
// Неизвестное (VISION 2026-11-13) не трактуется ни как отказ, ни как
// исправность: если часть снимка не прочитана, для неё сохраняется прежнее
// состояние и события не порождаются.
package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/storage/home"
)

// DiskState — диск, когда-либо виденный коллектором.
type DiskState struct {
	Present bool   `json:"present"`
	Name    string `json:"name,omitempty"` // последний алиас («Диск 1»)
}

// PoolState — состояние пула для сверки.
type PoolState struct {
	Health    string           `json:"health"`
	Failed    []string         `json:"failed,omitempty"`     // ID дисков-участников в faulty
	Errors    int64            `json:"errors"`               // сумма счётчиков btrfs + dm-integrity
	ErrDetail map[string]int64 `json:"err_detail,omitempty"` // по источникам, для данных события
	// EmergencyRO — Btrfs аварийно read-only; только у смонтированного пула.
	EmergencyRO bool `json:"emergency_ro,omitempty"`
	// ProtectionBypassed — запас исчерпан при выключенной защите.
	ProtectionBypassed bool `json:"protection_bypassed,omitempty"`
}

// State — снимок коллектора.
type State struct {
	Disks map[string]DiskState `json:"disks"`
	Pools map[string]PoolState `json:"pools"`
	// Members — диски из записей пулов (ID → пул): отказ такого диска critical.
	Members map[string]string `json:"members"`
}

// Observation — сырое наблюдение для Build.
type Observation struct {
	Disks    *disks.Snapshot // nil — снимок дисков не прочитан
	Pools    []home.PoolView
	PoolsErr error // список пулов не прочитан
}

// Build строит новый снимок поверх prev: непрочитанные части берутся из prev.
func Build(prev State, o Observation) State {
	cur := State{Disks: map[string]DiskState{}, Pools: map[string]PoolState{}, Members: map[string]string{}}
	if o.Disks == nil {
		for id, d := range prev.Disks {
			cur.Disks[id] = d
		}
	} else {
		for id, d := range prev.Disks { // известные ранее остаются в памяти как отсутствующие
			d.Present = false
			cur.Disks[id] = d
		}
		for _, d := range o.Disks.Disks {
			if d.ID == "" {
				continue // без идентичности диск не отследить между снимками
			}
			cur.Disks[d.ID] = DiskState{Present: true, Name: d.Name}
		}
	}
	if o.PoolsErr != nil {
		for k, v := range prev.Pools {
			cur.Pools[k] = v
		}
		for k, v := range prev.Members {
			cur.Members[k] = v
		}
		return cur
	}
	for _, v := range o.Pools {
		for _, d := range v.Disks {
			cur.Members[d.ID] = v.Name
		}
		if v.Creating || v.Busy == home.BusyDestroying {
			// Пул создаётся (или создание прервано — карточка
			// «удалите пул») либо удаляется: разборка массива — не отказ,
			// событий по пулу нет; прежнее состояние не переносится.
			continue
		}
		st := v.Status
		if st == nil || st.Health == home.HealthUnknown {
			if p, ok := prev.Pools[v.Name]; ok {
				cur.Pools[v.Name] = p
			}
			continue
		}
		ps := PoolState{Health: st.Health, EmergencyRO: st.EmergencyReadOnly}
		ex := v.Protection.RedundancyExhausted
		ps.ProtectionBypassed = !v.Protection.Enabled && ex != nil && *ex
		for _, a := range st.Arrays {
			for _, m := range a.Members {
				if m.DiskID != "" && strings.Contains(m.State, "faulty") {
					ps.Failed = append(ps.Failed, m.DiskID)
				}
			}
		}
		sort.Strings(ps.Failed)
		for k, n := range st.BtrfsErrors {
			ps.Errors += n
			addDetail(&ps, "btrfs."+k, n)
		}
		for _, in := range st.Integrity {
			ps.Errors += in.Mismatches
			addDetail(&ps, "integrity."+in.Name, in.Mismatches)
		}
		cur.Pools[v.Name] = ps
	}
	return cur
}

func addDetail(ps *PoolState, k string, n int64) {
	if n == 0 {
		return
	}
	if ps.ErrDetail == nil {
		ps.ErrDetail = map[string]int64{}
	}
	ps.ErrDetail[k] = n
}

// Diff — события перехода prev → cur. Пустой prev (первый запуск) — без событий.
func Diff(prev, cur State) []events.Event {
	if prev.Disks == nil && prev.Pools == nil {
		return nil
	}
	var out []events.Event
	add := func(typ, sev, subj string, data map[string]any) {
		b, _ := json.Marshal(data)
		out = append(out, events.Event{Type: typ, Severity: sev, Subject: subj, Data: b})
	}
	for _, id := range sortedKeys(cur.Disks) {
		c := cur.Disks[id]
		p, known := prev.Disks[id]
		data := map[string]any{"disk_id": id, "name": c.Name}
		if pool := cur.Members[id]; pool != "" {
			data["pool"] = pool
		}
		switch {
		case !known && c.Present:
			add(events.DiskAdded, events.SevInfo, "disk:"+id, data)
		case known && p.Present && !c.Present:
			sev := events.SevWarning
			if cur.Members[id] != "" {
				sev = events.SevCritical
			}
			add(events.DiskRemoved, sev, "disk:"+id, data)
		case known && !p.Present && c.Present:
			add(events.DiskReturned, events.SevInfo, "disk:"+id, data)
		}
	}
	for _, name := range sortedKeys(cur.Pools) {
		c := cur.Pools[name]
		p, known := prev.Pools[name]
		subj := "pool:" + name
		if !known {
			if bad(c.Health) { // новый пул сразу в плохом состоянии — тоже событие
				add(events.ArrayDegraded, events.SevCritical, subj, map[string]any{"pool": name, "health": c.Health})
			}
			continue
		}
		for _, id := range c.Failed {
			if !contains(p.Failed, id) {
				add(events.DiskFailed, events.SevCritical, "disk:"+id, map[string]any{"disk_id": id, "pool": name})
			}
		}
		if c.Health != p.Health {
			data := map[string]any{"pool": name, "health": c.Health, "was": p.Health}
			switch {
			case bad(c.Health):
				add(events.ArrayDegraded, events.SevCritical, subj, data)
			case c.Health == home.HealthRebuilding:
				add(events.ArrayRecovering, events.SevWarning, subj, data)
			case c.Health == home.HealthOK && (bad(p.Health) || p.Health == home.HealthRebuilding):
				add(events.ArrayRecovered, events.SevInfo, subj, data)
			}
		}
		switch {
		case c.EmergencyRO && !p.EmergencyRO:
			add(events.PoolEmergencyRO, events.SevCritical, subj, map[string]any{"pool": name})
		case !c.EmergencyRO && p.EmergencyRO:
			add(events.PoolEmergencyROCleared, events.SevInfo, subj, map[string]any{"pool": name})
		}
		// Отдельный факт от array.degraded — защита не сработала,
		// потому что её выключили; именно это администратор должен узнать.
		switch {
		case c.ProtectionBypassed && !p.ProtectionBypassed:
			add(events.PoolProtectionBypassed, events.SevCritical, subj, map[string]any{"pool": name})
		case !c.ProtectionBypassed && p.ProtectionBypassed:
			add(events.PoolProtectionBypassedCleared, events.SevInfo, subj, map[string]any{"pool": name})
		}
		if c.Errors > p.Errors {
			add(events.IntegrityErrors, events.SevWarning, subj,
				map[string]any{"pool": name, "errors": c.Errors, "was": p.Errors, "detail": c.ErrDetail})
		}
	}
	return out
}

func bad(h string) bool {
	return h == home.HealthDegraded || h == home.HealthFailed || h == home.HealthMissing
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// Collector — сверка снимков с журналом.
type Collector struct {
	DB      *sql.DB
	Log     *events.Log
	Observe func(ctx context.Context) Observation
	Now     func() time.Time
	OnEvent func(events.Event) // каждое записанное событие (журнал systemd); nil — нет
	// Settle — пауза после Trigger перед сверкой: пачка uevent (диск и его
	// разделы, md) сливается в одну сверку, udev успевает создать ссылки.
	Settle  time.Duration
	trigger chan struct{}
}

// New создаёт коллектор.
func New(db *sql.DB, log *events.Log, observe func(context.Context) Observation) *Collector {
	return &Collector{DB: db, Log: log, Observe: observe, Now: time.Now, Settle: 2 * time.Second, trigger: make(chan struct{}, 1)}
}

// Trigger просит внеочередную сверку (uevent); не блокирует.
func (c *Collector) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

func (c *Collector) load(ctx context.Context) (State, error) {
	var data string
	err := c.DB.QueryRowContext(ctx, "SELECT data FROM collector_state WHERE id = 1").Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal([]byte(data), &s); err != nil {
		return State{}, fmt.Errorf("снимок коллектора: %w", err)
	}
	return s, nil
}

func (c *Collector) save(ctx context.Context, s State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = c.DB.ExecContext(ctx,
		"INSERT INTO collector_state(id, ts, data) VALUES (1, ?, ?) ON CONFLICT(id) DO UPDATE SET ts = excluded.ts, data = excluded.data",
		c.Now().UnixMilli(), string(b))
	return err
}

// Sync — одна сверка: наблюдение, события разницы, сохранение снимка.
// События пишутся до снимка: при сбое между ними событие может повториться
// после перезапуска, но не потеряется.
func (c *Collector) Sync(ctx context.Context) ([]events.Event, error) {
	prev, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	cur := Build(prev, c.Observe(ctx))
	var out []events.Event
	for _, ev := range Diff(prev, cur) {
		e, err := c.Log.Emit(ctx, ev, true)
		if err != nil {
			return out, err
		}
		out = append(out, e)
		if c.OnEvent != nil {
			c.OnEvent(e)
		}
	}
	return out, c.save(ctx, cur)
}

// Run — сверка при старте, затем раз в interval и по Trigger, до отмены ctx.
func (c *Collector) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := c.Sync(ctx); err != nil && onErr != nil && ctx.Err() == nil {
			onErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.trigger:
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.Settle):
			}
			select { // события во время паузы покрыты этой сверкой
			case <-c.trigger:
			default:
			}
		}
	}
}
