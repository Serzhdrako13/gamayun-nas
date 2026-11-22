package maintenance

// История SMART (температура и счётчики секторов) в таблице
// smart_history, пороги температуры и события перегрева с антидребезгом.
// Краткий итог SMART диска для списка дисков и «Панели».
//
// Запись на SD-карту минимальна: точка — при плановом опросе (не чаще
// HistoryEvery на диск), одна транзакция на опрос; через HistoryFull
// точки прореживаются до одной на HistoryBucket (остаётся самая горячая),
// старше HistoryKeep — удаляются (раз в сутки).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/smart"
)

// Типы событий температуры (Subject "disk:<id>").
const (
	EvOverheat   = "disk.overheat"    // температура выше порога (warning/critical)
	EvTempNormal = "disk.temp_normal" // температура вернулась в норму
)

// Параметры истории.
const (
	HistoryEvery  = 10 * time.Minute    // не чаще точки на диск
	HistoryFull   = 7 * 24 * time.Hour  // все точки за последнюю неделю
	HistoryBucket = 6 * time.Hour       // старше — одна точка на 6 ч
	HistoryKeep   = 90 * 24 * time.Hour // срок хранения
	historyPrune  = 24 * time.Hour      // период прореживания
	overheatQuiet = time.Hour           // не чаще события на диск
	tempHyst      = 3                   // °C ниже порога — возврат уровня
	keyTemp       = "maintenance.temperature"
	keyTempState  = "maintenance.smart.temp_state"
)

// Limits — пороги температуры, °C.
type Limits struct {
	WarningC  int `json:"warning_c"`
	CriticalC int `json:"critical_c"`
}

// Thresholds — пороги для HDD и SSD/NVMe.
type Thresholds struct {
	HDD Limits `json:"hdd"`
	SSD Limits `json:"ssd"`
}

// DefaultThresholds — HDD 55/65 °C (решение по умолчанию); SSD и
// NVMe рассчитаны на более высокую температуру — 70/80 °C.
func DefaultThresholds() Thresholds {
	return Thresholds{HDD: Limits{55, 65}, SSD: Limits{70, 80}}
}

// Validate — 30…100 °C, warning < critical.
func (t Thresholds) Validate() error {
	for name, l := range map[string]Limits{"hdd": t.HDD, "ssd": t.SSD} {
		if l.WarningC < 30 || l.CriticalC > 100 || l.WarningC >= l.CriticalC {
			return fmt.Errorf("%w: %s — 30 ≤ warning_c < critical_c ≤ 100", ErrInvalid, name)
		}
	}
	return nil
}

// Thresholds — текущие пороги (по умолчанию, если не заданы или запись испорчена).
func (s *Service) Thresholds(ctx context.Context) (Thresholds, error) {
	v, ok, err := s.get(ctx, keyTemp)
	if err != nil || !ok {
		return DefaultThresholds(), err
	}
	t := DefaultThresholds()
	if err := json.Unmarshal([]byte(v), &t); err != nil || t.Validate() != nil {
		return DefaultThresholds(), fmt.Errorf("пороги температуры в базе испорчены: %v", err)
	}
	return t, nil
}

// SetThresholds сохраняет пороги.
func (s *Service) SetThresholds(ctx context.Context, t Thresholds) (Thresholds, error) {
	if err := t.Validate(); err != nil {
		return Thresholds{}, err
	}
	b, _ := json.Marshal(t)
	return t, s.set(ctx, keyTemp, string(b))
}

// limitsFor — пороги по виду диска: NVMe и SSD — ssd, прочее (HDD,
// неизвестный) — hdd (ниже, безопаснее).
func (t Thresholds) limitsFor(in smart.Info, kind string) Limits {
	if strings.EqualFold(in.Protocol, "NVMe") || strings.EqualFold(kind, "SSD") {
		return t.SSD
	}
	return t.HDD
}

// Уровни температуры.
const (
	tempNormal   = ""
	tempWarning  = events.SevWarning
	tempCritical = events.SevCritical
)

var tempRank = map[string]int{tempNormal: 0, tempWarning: 1, tempCritical: 2}

// tempLevel — уровень с гистерезисом: объявленный уровень держится, пока
// температура не опустится на tempHyst ниже его порога.
func tempLevel(c int64, l Limits, announced string) string {
	crit, warn := int64(l.CriticalC), int64(l.WarningC)
	if announced == tempCritical {
		crit -= tempHyst
	}
	if announced != tempNormal {
		warn -= tempHyst
	}
	switch {
	case c >= crit:
		return tempCritical
	case c >= warn:
		return tempWarning
	}
	return tempNormal
}

// TempState — объявленный уровень температуры диска (переживает перезапуск).
type TempState struct {
	Level    string    `json:"level,omitempty"`
	LastSev  string    `json:"last_sev,omitempty"` // уровень последнего события перегрева
	LastHeat time.Time `json:"last_heat,omitempty"`
}

func (s *Service) loadTempLocked(ctx context.Context) error {
	if s.temp != nil {
		return nil
	}
	s.temp = map[string]TempState{}
	v, ok, err := s.get(ctx, keyTempState)
	if err != nil || !ok {
		return err
	}
	if err := json.Unmarshal([]byte(v), &s.temp); err != nil {
		s.temp = map[string]TempState{}
		s.opt.Logger.Warn("обслуживание: состояние температуры в базе повреждено, начато заново", "err", err)
	}
	return nil
}

// checkTempLocked — события перегрева/нормализации. Перегрев — не чаще
// overheatQuiet на диск, кроме повышения до critical. Возврат в норму —
// только после объявленного перегрева.
func (s *Service) checkTempLocked(ctx context.Context, id string, in smart.Info, th Thresholds, emit func(string, string, map[string]any)) {
	if !in.Supported || in.Standby || in.TemperatureC == nil {
		return
	}
	c := *in.TemperatureC
	l := th.limitsFor(in, s.kinds[id])
	st := s.temp[id]
	cur := tempLevel(c, l, st.Level)
	data := func() map[string]any {
		return map[string]any{"temperature_c": c, "warning_c": l.WarningC, "critical_c": l.CriticalC}
	}
	now := s.now()
	switch {
	case tempRank[cur] > tempRank[st.Level]:
		quiet := !st.LastHeat.IsZero() && now.Sub(st.LastHeat) < overheatQuiet
		if quiet && !(cur == tempCritical && st.LastSev != tempCritical) {
			return // антидребезг: уровень не объявлен, повтор — следующим опросом
		}
		emit(EvOverheat, cur, data())
		st = TempState{Level: cur, LastSev: cur, LastHeat: now}
	case cur == st.Level:
		return
	case cur == tempNormal:
		emit(EvTempNormal, events.SevInfo, data())
		st.Level = tempNormal
	default: // critical → warning: без события, повторное critical — по антидребезгу
		st.Level = cur
	}
	s.temp[id] = st
	s.tempDirty = true
}

// Point — точка истории SMART.
type Point struct {
	Time          time.Time `json:"t"`
	TemperatureC  *int64    `json:"temperature_c,omitempty"`
	Reallocated   *int64    `json:"reallocated,omitempty"`
	Pending       *int64    `json:"pending,omitempty"`
	Uncorrectable *int64    `json:"uncorrectable,omitempty"`
	PowerOnHours  *int64    `json:"power_on_hours,omitempty"`
}

// histPointLocked — точка для записи (nil — не нужна: нет данных, спит,
// недавно уже записана).
func (s *Service) histPointLocked(id string, in smart.Info) *Point {
	if !in.Supported || in.Standby {
		return nil
	}
	if in.TemperatureC == nil && in.Reallocated == nil && in.Pending == nil && in.Uncorrectable == nil {
		return nil
	}
	now := s.now()
	if t, ok := s.histAt[id]; ok && now.Sub(t) < HistoryEvery {
		return nil
	}
	return &Point{Time: now, TemperatureC: in.TemperatureC, Reallocated: in.Reallocated, Pending: in.Pending,
		Uncorrectable: in.Uncorrectable, PowerOnHours: in.PowerOnHours}
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// persistLocked — одна транзакция: точки истории, состояние SMART и
// (если менялось) состояние температуры; раз в сутки — прореживание.
func (s *Service) persistLocked(ctx context.Context, pts map[string]*Point) error {
	state, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, p := range pts {
		if p == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO smart_history(disk_id, ts, temperature_c, reallocated, pending, uncorrectable, power_on_hours)
VALUES (?, ?, ?, ?, ?, ?, ?)`, id, p.Time.UnixMilli(), nullInt(p.TemperatureC), nullInt(p.Reallocated), nullInt(p.Pending),
			nullInt(p.Uncorrectable), nullInt(p.PowerOnHours)); err != nil {
			return fmt.Errorf("история SMART: %w", err)
		}
	}
	up := `INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.ExecContext(ctx, up, keySmartState, string(state)); err != nil {
		return err
	}
	if s.tempDirty {
		b, _ := json.Marshal(s.temp)
		if _, err := tx.ExecContext(ctx, up, keyTempState, string(b)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tempDirty = false
	for id, p := range pts {
		if p != nil {
			s.histAt[id] = p.Time
		}
	}
	if now := s.now(); now.Sub(s.prunedAt) >= historyPrune {
		if _, err := s.PruneHistory(ctx); err != nil {
			s.opt.Logger.Warn("обслуживание: прореживание истории SMART", "err", err)
		} else {
			s.prunedAt = now
		}
	}
	return nil
}

// PruneHistory удаляет точки старше HistoryKeep и прореживает точки
// старше HistoryFull до одной на HistoryBucket (с наибольшей
// температурой, при равенстве — последнюю). Повтор ничего не меняет.
func (s *Service) PruneHistory(ctx context.Context) (int64, error) {
	now := s.now()
	keep := now.Add(-HistoryKeep).UnixMilli()
	full := now.Add(-HistoryFull).UnixMilli()
	res, err := s.db.ExecContext(ctx, "DELETE FROM smart_history WHERE ts < ?", keep)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `DELETE FROM smart_history WHERE ts < ?1 AND (disk_id, ts) NOT IN (
	SELECT disk_id, ts FROM (
		SELECT disk_id, ts, ROW_NUMBER() OVER (PARTITION BY disk_id, ts / ?2
			ORDER BY COALESCE(temperature_c, -1000) DESC, ts DESC) AS rn
		FROM smart_history WHERE ts < ?1) WHERE rn = 1)`, full, HistoryBucket.Milliseconds())
	if err != nil {
		return n, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}

// History — точки диска за период since…сейчас по возрастанию времени.
func (s *Service) History(ctx context.Context, id string, since time.Time) ([]Point, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, temperature_c, reallocated, pending, uncorrectable, power_on_hours
FROM smart_history WHERE disk_id = ? AND ts >= ? ORDER BY ts`, id, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var ts int64
		var t, r, p, u, h sql.NullInt64
		if err := rows.Scan(&ts, &t, &r, &p, &u, &h); err != nil {
			return nil, err
		}
		ptr := func(v sql.NullInt64) *int64 {
			if !v.Valid {
				return nil
			}
			x := v.Int64
			return &x
		}
		out = append(out, Point{Time: time.UnixMilli(ts).UTC(), TemperatureC: ptr(t), Reallocated: ptr(r), Pending: ptr(p),
			Uncorrectable: ptr(u), PowerOnHours: ptr(h)})
	}
	return out, rows.Err()
}

// Summary — краткий итог SMART диска по последнему опросу.
type Summary struct {
	// Level: ok, warning, critical; unknown — SMART нет или не опрашивался.
	Level     string `json:"level"`
	Supported bool   `json:"supported"`
	// SMART есть, но выключен в диске; ManualOff — оператором.
	Disabled     bool     `json:"disabled,omitempty"`
	ManualOff    bool     `json:"manual_off,omitempty"`
	Standby      bool     `json:"standby,omitempty"`
	TemperatureC *int64   `json:"temperature_c,omitempty"`
	Limits       *Limits  `json:"limits,omitempty"`
	Reasons      []string `json:"reasons,omitempty"`
}

// Summarize — итог: critical — самооценка FAILED, последний самотест с
// ошибкой, температура ≥ critical; warning — ненулевые плохие секторы,
// температура ≥ warning; иначе ok.
func Summarize(in smart.Info, kind string, th Thresholds) Summary {
	if !in.Supported {
		return Summary{Level: "unknown", Disabled: in.Disabled, ManualOff: in.ManualOff}
	}
	l := th.limitsFor(in, kind)
	sm := Summary{Level: "ok", Supported: true, Standby: in.Standby, TemperatureC: in.TemperatureC, Limits: &l}
	if in.Passed == nil && in.TemperatureC == nil && in.Reallocated == nil && in.Pending == nil && in.Uncorrectable == nil && in.LastTest == nil {
		sm.Level = "unknown" // спал с первого опроса — данных нет
		return sm
	}
	raise := func(lv, why string) {
		if sm.Level == "ok" || tempRank[lv] > tempRank[sm.Level] {
			sm.Level = lv
		}
		sm.Reasons = append(sm.Reasons, why)
	}
	if in.Passed != nil && !*in.Passed {
		raise(events.SevCritical, "самооценка: неисправен")
	}
	if t := in.LastTest; t != nil && !t.Passed && !in.TestRunning {
		raise(events.SevCritical, "самотест с ошибкой")
	}
	if c := in.TemperatureC; c != nil {
		switch {
		case *c >= int64(l.CriticalC):
			raise(events.SevCritical, fmt.Sprintf("температура %d °C", *c))
		case *c >= int64(l.WarningC):
			raise(events.SevWarning, fmt.Sprintf("температура %d °C", *c))
		}
	}
	for _, x := range []struct {
		v    *int64
		what string
	}{{in.Reallocated, "переназначенных секторов"}, {in.Pending, "ожидающих секторов"}, {in.Uncorrectable, "нечитаемых секторов"}} {
		if val(x.v) > 0 {
			raise(events.SevWarning, fmt.Sprintf("%s: %d", x.what, *x.v))
		}
	}
	return sm
}

// Summaries — итоги по ID дисков (kinds — вид диска HDD/SSD из снимка).
func (s *Service) Summaries(ctx context.Context, kinds map[string]string) (map[string]Summary, error) {
	th, err := s.Thresholds(ctx)
	if err != nil {
		s.opt.Logger.Warn("обслуживание: пороги температуры", "err", err)
	}
	st, err := s.Smart(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Summary, len(st))
	for id, in := range st {
		out[id] = Summarize(in, kinds[id], th)
	}
	return out, nil
}
