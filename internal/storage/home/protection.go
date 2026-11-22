package home

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gamayun-nas/internal/tasks"
)

// ---------- Read-only при исчерпании запаса избыточности ----------

// ShareGuard — применение запрета записи к общим папкам пула. Реализация —
// nas.Service; если не задан, запрет
// действует на уровне Btrfs (remount ro), что уже блокирует запись любых
// клиентов.
type ShareGuard interface {
	SetPoolReadOnly(ctx context.Context, pool string, readOnly bool) error
}

// Действия журнала защиты.
const (
	ActAutoReadOnly    = "auto_readonly"    // gamayund перевёл пул в read-only
	ActAutoReadWrite   = "auto_readwrite"   // gamayund вернул запись
	ActOverride        = "override"         // администратор временно разрешил запись
	ActOverrideRevoked = "override_revoked" // администратор отменил разрешение
	ActOverrideExpired = "override_expired" // срок разрешения истёк
	ActEnable          = "enable"           // защита включена
	ActDisable         = "disable"          // защита отключена
)

// ActorSystem — автор автоматических записей журнала.
const ActorSystem = "system"

// Пределы ручного разрешения записи.
const (
	MinOverride = time.Minute
	MaxOverride = 7 * 24 * time.Hour
)

// Override — действующее ручное разрешение записи.
type Override struct {
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Protection — состояние защиты пула (для API и будущего баннера).
type Protection struct {
	Enabled bool `json:"enabled"`
	// RedundancyExhausted — отказ ещё одного диска приведёт к потере данных
	// (или уже привёл); nil — состояние пула неизвестно.
	RedundancyExhausted *bool  `json:"redundancy_exhausted"`
	Reason              string `json:"reason,omitempty"`
	// WriteBlocked — защита требует read-only сейчас.
	WriteBlocked bool `json:"write_blocked"`
	// ReadOnly — фактически смонтирован только для чтения.
	ReadOnly bool `json:"read_only"`
	// Enforced — read-only установлен защитой gamayund.
	Enforced bool      `json:"enforced"`
	Override *Override `json:"override"`
}

// ProtectionLogEntry — запись журнала защиты.
type ProtectionLogEntry struct {
	ID        int64      `json:"id"`
	Pool      string     `json:"pool"`
	At        time.Time  `json:"at"`
	Actor     string     `json:"actor"`
	Action    string     `json:"action"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// tolerance — сколько участников массива может отказать без потери данных.
func tolerance(level string, n int) int {
	switch level {
	case LevelRAID1:
		return n - 1
	case LevelRAID5:
		return 1
	case LevelRAID6:
		return 2
	case LevelRAID10:
		return 1 // консервативно: второй отказ в той же паре фатален
	}
	return 0
}

// Exhausted — запас избыточности пула исчерпан: после уже случившихся
// отказов пул не переживёт отказ любого следующего диска. Исправный
// RAID0/single запасом не располагает изначально и не считается
// исчерпанным (политика VISION 2026-11-03). Отсутствующий пул — false.
func Exhausted(st PoolStatus) (bool, string) {
	switch st.Health {
	case HealthMissing:
		return false, ""
	case HealthFailed:
		return true, "массив пула неактивен или потерян"
	}
	if st.Btrfs != nil && st.Btrfs.DeviceMissing > 0 {
		return true, "в Btrfs отсутствует устройство"
	}
	for _, a := range st.Arrays {
		// Отказавший участник вне слотов (заменённый mdadm --replace)
		// запаса не уменьшает: считаются пустые слоты.
		lost := a.Missing
		if lost == 0 && contains(a.State, "degraded") {
			lost = 1
		}
		if lost == 0 || (st.InitialSync && initialSyncArray(a)) {
			continue
		}
		if tolerance(a.Level, a.RaidDevices)-lost <= 0 {
			return true, fmt.Sprintf("массив %s (%s) неполный (выбыл диск): отказ ещё одного диска приведёт к потере данных", a.Name, a.Level)
		}
	}
	return false, ""
}

type protRow struct {
	enabled, enforced bool
	override          *Override
}

func (s *Service) protRow(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, pool string) (protRow, error) {
	r := protRow{enabled: true}
	var en, enf int
	var actor, reason sql.NullString
	var from, until sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT enabled, enforced, override_actor, override_reason, override_from, override_until
		FROM pool_protection WHERE pool = ?`, pool).Scan(&en, &enf, &actor, &reason, &from, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.enabled, r.enforced = en != 0, enf != 0
	if until.Valid {
		r.override = &Override{Actor: actor.String, Reason: reason.String,
			CreatedAt: time.UnixMilli(from.Int64).UTC(), ExpiresAt: time.UnixMilli(until.Int64).UTC()}
	}
	return r, nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Service) ensureProtRow(ctx context.Context, x execer, pool string) error {
	_, err := x.ExecContext(ctx, "INSERT OR IGNORE INTO pool_protection(pool, updated_at) VALUES (?, ?)", pool, s.now().UnixMilli())
	return err
}

func (s *Service) logProt(ctx context.Context, x execer, pool, actor, action, reason string, expires *time.Time) error {
	var exp any
	if expires != nil {
		exp = expires.UnixMilli()
	}
	_, err := x.ExecContext(ctx, "INSERT INTO pool_protection_log(pool, ts, actor, action, reason, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
		pool, s.now().UnixMilli(), actor, action, reason, exp)
	return err
}

// protection — состояние защиты по записи и (если есть) состоянию пула.
func (s *Service) protection(ctx context.Context, pool string, st *PoolStatus) (Protection, error) {
	r, err := s.protRow(ctx, s.db, pool)
	if err != nil {
		return Protection{}, err
	}
	p := Protection{Enabled: r.enabled, Enforced: r.enforced}
	if r.override != nil && s.now().Before(r.override.ExpiresAt) {
		p.Override = r.override
	}
	if st != nil {
		ex, why := Exhausted(*st)
		p.RedundancyExhausted, p.Reason, p.ReadOnly = &ex, why, st.ReadOnly
		p.WriteBlocked = p.Enabled && ex && p.Override == nil
		if st.Health == HealthUnknown && !ex {
			// По прочитанному запас есть, но прочитано не всё.
			p.RedundancyExhausted, p.Reason = nil, unknownReason(*st)
		}
	}
	return p, nil
}

func (s *Service) checkActor(actor, reason string) error {
	if strings.TrimSpace(actor) == "" {
		return wrap(ErrInvalid, "не указан автор действия")
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return wrap(ErrInvalid, "причина обязательна (до 500 байт)")
	}
	return nil
}

// SetProtectionEnabled включает или отключает защиту пула (журналируется).
func (s *Service) SetProtectionEnabled(ctx context.Context, pool string, enabled bool, actor, reason string) (Protection, error) {
	if err := s.checkActor(actor, reason); err != nil {
		return Protection{}, err
	}
	if _, err := s.getRecord(ctx, pool); err != nil {
		return Protection{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Protection{}, err
	}
	defer tx.Rollback()
	if err := s.ensureProtRow(ctx, tx, pool); err != nil {
		return Protection{}, err
	}
	en, act := 0, ActDisable
	if enabled {
		en, act = 1, ActEnable
	}
	if _, err := tx.ExecContext(ctx, "UPDATE pool_protection SET enabled = ?, updated_at = ? WHERE pool = ?", en, s.now().UnixMilli(), pool); err != nil {
		return Protection{}, err
	}
	if err := s.logProt(ctx, tx, pool, actor, act, reason, nil); err != nil {
		return Protection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Protection{}, err
	}
	return s.enforcePool(ctx, pool)
}

// SetWriteOverride — ручное временное разрешение записи администратором:
// кто, когда, причина и срок пишутся в журнал. По истечении срока защита
// снова переводит пул в read-only, если запас всё ещё исчерпан.
func (s *Service) SetWriteOverride(ctx context.Context, pool, actor, reason string, d time.Duration) (Protection, error) {
	if err := s.checkActor(actor, reason); err != nil {
		return Protection{}, err
	}
	if d < MinOverride || d > MaxOverride {
		return Protection{}, wrap(ErrInvalid, "срок разрешения — от %s до %s", MinOverride, MaxOverride)
	}
	if _, err := s.getRecord(ctx, pool); err != nil {
		return Protection{}, err
	}
	now := s.now()
	until := now.Add(d)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Protection{}, err
	}
	defer tx.Rollback()
	if err := s.ensureProtRow(ctx, tx, pool); err != nil {
		return Protection{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pool_protection SET override_actor = ?, override_reason = ?, override_from = ?,
		override_until = ?, updated_at = ? WHERE pool = ?`, actor, reason, now.UnixMilli(), until.UnixMilli(), now.UnixMilli(), pool); err != nil {
		return Protection{}, err
	}
	if err := s.logProt(ctx, tx, pool, actor, ActOverride, reason, &until); err != nil {
		return Protection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Protection{}, err
	}
	return s.enforcePool(ctx, pool)
}

// ClearWriteOverride — досрочная отмена ручного разрешения.
func (s *Service) ClearWriteOverride(ctx context.Context, pool, actor, reason string) (Protection, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "отменено администратором"
	}
	if err := s.checkActor(actor, reason); err != nil {
		return Protection{}, err
	}
	if _, err := s.getRecord(ctx, pool); err != nil {
		return Protection{}, err
	}
	r, err := s.protRow(ctx, s.db, pool)
	if err != nil {
		return Protection{}, err
	}
	if r.override == nil {
		return Protection{}, wrap(ErrNotFound, "у пула %s нет ручного разрешения записи", pool)
	}
	if err := s.clearOverride(ctx, pool, actor, ActOverrideRevoked, reason); err != nil {
		return Protection{}, err
	}
	return s.enforcePool(ctx, pool)
}

func (s *Service) clearOverride(ctx context.Context, pool, actor, action, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE pool_protection SET override_actor = NULL, override_reason = NULL,
		override_from = NULL, override_until = NULL, updated_at = ? WHERE pool = ?`, s.now().UnixMilli(), pool); err != nil {
		return err
	}
	if err := s.logProt(ctx, tx, pool, actor, action, reason, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// ProtectionLog — журнал защиты пула, новые записи первыми.
func (s *Service) ProtectionLog(ctx context.Context, pool string, limit int) ([]ProtectionLogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, pool, ts, actor, action, reason, expires_at FROM pool_protection_log
		WHERE pool = ? ORDER BY id DESC LIMIT ?`, pool, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProtectionLogEntry{}
	for rows.Next() {
		var e ProtectionLogEntry
		var ts int64
		var exp sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Pool, &ts, &e.Actor, &e.Action, &e.Reason, &exp); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(ts).UTC()
		if exp.Valid {
			t := time.UnixMilli(exp.Int64).UTC()
			e.ExpiresAt = &t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnforceAll — один проход защиты по всем пулам базы; ошибки отдельных
// пулов собираются, проход не прерывают.
func (s *Service) EnforceAll(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pools ORDER BY name")
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	var errs []error
	for _, n := range names {
		if _, err := s.enforcePool(ctx, n); err != nil {
			errs = append(errs, fmt.Errorf("пул %s: %w", n, err))
		}
	}
	return errors.Join(errs...)
}

// RunProtection — проверка защиты до отмены ctx: полный проход раз в
// interval и немедленно при изменении состава/состояния массивов в
// /proc/mdstat (опрос MdstatPoll): при опросе только раз в 30 с
// пул без запаса оставался доступным на запись до 30 с после отказа.
func (s *Service) RunProtection(ctx context.Context, interval time.Duration, onErr func(error)) {
	poll := s.MdstatPoll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	path := s.MdstatPath
	if path == "" {
		path = "/proc/mdstat"
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	last := mdstatSignature(path)
	next := time.Now().Add(interval)
	// Сторож прохода — отдельно от цикла (зависший проход сам о себе
	// не сообщит): просрочка → событие, health unknown, операции не ставятся.
	s.protStart(interval)
	go func() {
		wt := time.NewTicker(poll)
		defer wt.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wt.C:
			}
			if why := s.protCheck(); why != "" && onErr != nil {
				onErr(errors.New(why))
			}
		}
	}()
	for {
		if err := s.EnforceAll(ctx); err != nil && onErr != nil && ctx.Err() == nil {
			onErr(err)
		}
		s.protPassed()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			sig := mdstatSignature(path)
			if sig != last && s.OnMdstatChange != nil {
				s.OnMdstatChange() // отказ md не даёт uevent — коллектору нужен этот сигнал
			}
			if sig != last || !time.Now().Before(next) {
				last = sig
				next = time.Now().Add(interval)
				break
			}
		}
	}
}

// mdstatSignature — /proc/mdstat без строк прогресса и bitmap (они
// меняются постоянно во время синхронизации): меняется при отказе,
// добавлении, сборке, остановке массива и смене действия.
func mdstatSignature(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "err"
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "bitmap:") || strings.HasPrefix(t, "[") {
			// строка прогресса «[==>...]  recovery = 12.1% …» — только действие
			if i := strings.Index(t, "] "); strings.HasPrefix(t, "[") && i > 0 {
				f := strings.Fields(t[i+2:])
				if len(f) > 0 {
					out = append(out, "action:"+f[0])
				}
			}
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, "\n")
}

// enforcePool приводит монтирование пула в соответствие с защитой.
func (s *Service) enforcePool(ctx context.Context, pool string) (Protection, error) {
	s.protMu.Lock()
	defer s.protMu.Unlock()
	r, err := s.protRow(ctx, s.db, pool)
	if err != nil {
		return Protection{}, err
	}
	if r.override != nil && !s.now().Before(r.override.ExpiresAt) {
		if err := s.clearOverride(ctx, pool, ActorSystem, ActOverrideExpired,
			fmt.Sprintf("истёк срок разрешения (%s, %s)", r.override.Actor, r.override.Reason)); err != nil {
			return Protection{}, err
		}
	}
	st, err := s.status(ctx, pool)
	if err != nil {
		// Опрос не удался — решение защиты не принято; это не «как
		// было», а отдельный исход с причиной (журнал gamayund).
		p, _ := s.protection(ctx, pool, nil)
		return p, fmt.Errorf("состояние неизвестно, решение защиты не принято: %w", err)
	}
	switch st.Health {
	case HealthOK:
		// Синхронизация закончилась без задачи create (перезапуск gamayund).
		if err := s.endInitialSync(ctx, pool); err != nil {
			return Protection{}, err
		}
	case HealthDegraded:
		// Вернувшийся диск: повторная сборка откроет dm-integrity и вернёт
		// участника в md по bitmap.
		if s.reassemble(ctx, pool) {
			if st, err = s.status(ctx, pool); err != nil {
				p, _ := s.protection(ctx, pool, nil)
				return p, err
			}
		}
	}
	p, err := s.protection(ctx, pool, &st)
	if err != nil {
		return p, err
	}
	switch {
	case p.WriteBlocked && st.Mounted && !st.ReadOnly:
		if err := s.applyReadOnly(ctx, pool, true, p.Reason); err != nil {
			return p, err
		}
		p.ReadOnly, p.Enforced = true, true
	case !p.WriteBlocked && p.Enforced:
		why := "запас избыточности восстановлен"
		switch {
		case p.Override != nil:
			why = fmt.Sprintf("ручное разрешение до %s (%s)", p.Override.ExpiresAt.Format(time.RFC3339), p.Override.Actor)
		case !p.Enabled:
			why = "защита отключена"
		}
		if st.Mounted && st.ReadOnly {
			if err := s.applyReadOnly(ctx, pool, false, why); err != nil {
				return p, err
			}
		} else {
			// ФС уже rw (например, смонтирована заново после перезагрузки),
			// но общие папки остались только на чтение — снять и с них.
			if s.Shares != nil {
				if err := s.Shares.SetPoolReadOnly(ctx, pool, false); err != nil {
					return p, fmt.Errorf("общие папки: %w", err)
				}
			}
			if err := s.setEnforced(ctx, pool, false, why); err != nil {
				return p, err
			}
		}
		p.ReadOnly, p.Enforced = false, false
	}
	if st.Health == HealthUnknown {
		// Read-only по прочитанному применён (если нужен); снять его или
		// счесть пул исправным по неполным данным нельзя — сообщаем.
		return p, errors.New(unknownReason(st) + " — решение защиты принято только по прочитанному")
	}
	return p, nil
}

func (s *Service) applyReadOnly(ctx context.Context, pool string, ro bool, why string) error {
	if ro && s.BeforeReadOnly != nil {
		if err := s.BeforeReadOnly(ctx, pool); err != nil {
			return err
		}
	}
	var r Result
	if err := s.helper.Call(ctx, OpSetReadOnly, ReadOnlyParams{Pool: pool, ReadOnly: ro}, &r); err != nil {
		return err
	}
	if s.Shares != nil {
		if err := s.Shares.SetPoolReadOnly(ctx, pool, ro); err != nil {
			return fmt.Errorf("общие папки: %w", err)
		}
	}
	return s.setEnforced(ctx, pool, ro, why)
}

func (s *Service) setEnforced(ctx context.Context, pool string, ro bool, why string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.ensureProtRow(ctx, tx, pool); err != nil {
		return err
	}
	v, act := 0, ActAutoReadWrite
	if ro {
		v, act = 1, ActAutoReadOnly
	}
	if _, err := tx.ExecContext(ctx, "UPDATE pool_protection SET enforced = ?, updated_at = ? WHERE pool = ?", v, s.now().UnixMilli(), pool); err != nil {
		return err
	}
	if err := s.logProt(ctx, tx, pool, ActorSystem, act, why, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- Удаление пула ----------

// TaskDestroy — вид задачи удаления пула.
const TaskDestroy = "storage.pool.destroy"

type destroyTask struct {
	Pool string `json:"pool"`
}

// Delete ставит задачу удаления пула. confirmName должен совпадать с
// именем пула — явное подтверждение уничтожения данных.
func (s *Service) Delete(ctx context.Context, name, confirmName string) (tasks.Task, error) {
	if !ValidPoolName(name) {
		return tasks.Task{}, wrap(ErrNotFound, "пул %s", name)
	}
	// Занятый пул (в том числе ещё создаётся — записи нет) — «занят», а не 404.
	if b := s.Busy(name); b != "" {
		return tasks.Task{}, wrap(ErrConflict, "пул %s занят: выполняется %s — дождитесь окончания", name, b)
	}
	if confirmName != name {
		return tasks.Task{}, wrap(ErrInvalid, "для удаления укажите имя пула в confirm_name")
	}
	if _, err := s.getRecord(ctx, name); err != nil {
		return tasks.Task{}, err
	}
	// Пул с общими папками не удаляется, пока папки не удалены.
	if names, err := s.sharesOn(ctx, name); err != nil {
		return tasks.Task{}, err
	} else if len(names) > 0 {
		return tasks.Task{}, wrap(ErrConflict, "на пуле %s есть общие папки (%s): удалите их перед удалением пула", name, strings.Join(names, ", "))
	}
	if s.InUse != nil {
		if what := s.InUse(ctx, name); what != "" {
			return tasks.Task{}, wrap(ErrConflict, "пул %s используется: %s — сначала выключите", name, what)
		}
	}
	raw, err := json.Marshal(destroyTask{Pool: name})
	if err != nil {
		return tasks.Task{}, err
	}
	return s.submit(ctx, name, TaskDestroy, raw)
}

func (s *Service) sharesOn(ctx context.Context, pool string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM shares WHERE pool = ? ORDER BY name", pool)
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

func (s *Service) runDestroy(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var tp destroyTask
	if err := json.Unmarshal(raw, &tp); err != nil || !ValidPoolName(tp.Pool) {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	progress(5)
	r, err := s.destroyPoolResult(ctx, tp.Pool)
	if err != nil {
		return nil, err
	}
	return TaskResult{Pool: tp.Pool, Helper: []Result{r}}, nil
}

// destroyPool разбирает стек пула и удаляет его запись (destroy, откат
// create).
func (s *Service) destroyPool(ctx context.Context, pool string) error {
	_, err := s.destroyPoolResult(ctx, pool)
	return err
}

// rollbackCreate — откат создания: helper разбирает
// остатки только на дисках записи, запись удаляется.
func (s *Service) rollbackCreate(ctx context.Context, rec PoolRecord) (string, error) {
	r, err := s.removePool(ctx, rec.Name, OpCreateRollback, rec.rollbackParams())
	if err != nil {
		return "", err
	}
	return rollbackDone(r), nil
}

// rollbackDone — итог отката для задачи: «диски свободны» —
// только без предупреждений helper (недоступный диск, нестёртый суперблок).
func rollbackDone(r Result) string {
	if len(r.Warnings) == 0 {
		return "выполнен откат: пул разобран, диски свободны"
	}
	return "выполнен частичный откат: пул разобран, но " + strings.Join(r.Warnings, "; ")
}

func (r PoolRecord) rollbackParams() CreateRollbackParams {
	p := CreateRollbackParams{Pool: r.Name}
	for _, d := range r.Disks {
		p.DiskIDs = append(p.DiskIDs, d.ID)
	}
	return p
}

func (s *Service) destroyPoolResult(ctx context.Context, pool string) (Result, error) {
	// Пул, создание которого прервано, удаляется откатом по дискам записи.
	if rec, err := s.getRecord(ctx, pool); err == nil && rec.Creating {
		return s.removePool(ctx, pool, OpCreateRollback, rec.rollbackParams())
	}
	return s.removePool(ctx, pool, OpDestroy, PoolParams{Pool: pool})
}

// removePool — helper-разборка op и удаление записи пула.
func (s *Service) removePool(ctx context.Context, pool, op string, params any) (Result, error) {
	s.protMu.Lock() // не перемонтировать пул во время разборки
	defer s.protMu.Unlock()
	var r Result
	if err := s.helper.Call(ctx, op, params, &r); err != nil {
		return r, err
	}
	if err := s.dropRecord(ctx, pool); err != nil {
		return r, fmt.Errorf("стек пула разобран, но запись не удалена из базы: %w", err)
	}
	return r, nil
}

// dropRecord — удаление записи пула и связанных строк из базы (без helper).
func (s *Service) dropRecord(ctx context.Context, pool string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer s.writeBootFile(ctx) // после Rollback/Commit: отдельное соединение
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM pools WHERE name = ?",
		"DELETE FROM pool_protection WHERE pool = ?",
		"DELETE FROM share_pool_state WHERE pool = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, pool); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// refusedBeforeWrite — helper отказал проверками до любой записи
// (refused / invalid_params): на дисках ничего не создано.
func refusedBeforeWrite(err error) bool {
	var r interface{ RefusedBeforeWrite() bool }
	return errors.As(err, &r) && r.RefusedBeforeWrite()
}

// reassemble повторяет сборку деградированного пула; true — какой-то
// участник возвращён в md.
func (s *Service) reassemble(ctx context.Context, pool string) bool {
	rec, err := s.getRecord(ctx, pool)
	if err != nil {
		return false
	}
	var r AssembleResult
	err = s.helper.Call(ctx, OpAssemble, AssembleParams{Pool: rec.Name, Integrity: rec.Integrity, IntegrityMode: rec.IntegrityMode, Arrays: rec.Arrays, Type: rec.Type, Disks: rec.Disks}, &r)
	if s.OnReassemble != nil && (err != nil || len(r.Readded) > 0 || len(r.ReaddFailed) > 0) {
		s.OnReassemble(pool, r, err)
	}
	return err == nil && len(r.Readded) > 0
}
