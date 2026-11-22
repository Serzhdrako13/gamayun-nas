package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gamayun-nas/internal/watch"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gamayun-nas/internal/disks"
)

// OpAssemble — сборка пула при загрузке.
const OpAssemble = "home.pool.assemble"

// AssembleParams — состав пула из записи gamayund. Режим dm-integrity в
// суперблоке не хранится, поэтому передаётся из записи пула.
type AssembleParams struct {
	Pool          string      `json:"pool"`
	Integrity     bool        `json:"integrity"`
	IntegrityMode string      `json:"integrity_mode,omitempty"`
	Arrays        []ArraySpec `json:"arrays"`
	// Pending — незавершённое добавление диска: диск, записанный
	// в базу до вызова add_disk. Принимается в массив, только если его
	// суперблок md относится к этому массиву; reshape/рост тогда
	// продолжаются до целевых уровня и числа участников.
	Pending *PendingAdd `json:"pending,omitempty"`
	// Type — тип пула для манифеста на пуле; пусто — манифест не пишется.
	Type string `json:"type,omitempty"`
	// Disks — диски записи (размеры) для манифеста.
	Disks []DiskRef `json:"disks,omitempty"`
}

// PendingAdd — ожидаемое добавление диска DiskID в массивы Arrays.
type PendingAdd struct {
	DiskID string        `json:"disk_id"`
	Arrays []ExpandArray `json:"arrays"`
	// New — ожидаемые новые слои: приняты, если PV слоя в VG.
	New []NewArray `json:"new,omitempty"`
}

func (pa *PendingAdd) target(i int) (ExpandArray, bool) {
	if pa == nil {
		return ExpandArray{}, false
	}
	for _, a := range pa.Arrays {
		if a.Index == i {
			return a, true
		}
	}
	return ExpandArray{}, false
}

// Validate проверяет имя, режим и состав.
func (p AssembleParams) Validate() error {
	if err := (PoolParams{Pool: p.Pool}).Validate(); err != nil {
		return err
	}
	if err := validIntegrityMode(p.Integrity, p.IntegrityMode); err != nil {
		return err
	}
	if p.Integrity && p.IntegrityMode == "" {
		return errors.New("integrity без integrity_mode")
	}
	if len(p.Arrays) == 0 || len(p.Arrays) > 32 {
		return fmt.Errorf("число массивов %d", len(p.Arrays))
	}
	for i, a := range p.Arrays {
		if _, err := minMembers(a.Level); err != nil {
			return fmt.Errorf("массив %d: %w", i, err)
		}
		if len(a.Members) == 0 {
			return fmt.Errorf("массив %d без дисков", i)
		}
		for _, id := range a.Members {
			if !ValidDiskID(id) {
				return fmt.Errorf("недопустимая идентичность диска %q", id)
			}
		}
	}
	if pa := p.Pending; pa != nil {
		if !ValidDiskID(pa.DiskID) || len(pa.Arrays)+len(pa.New) == 0 {
			return fmt.Errorf("недопустимое ожидаемое добавление %q", pa.DiskID)
		}
		for _, a := range pa.Arrays {
			if a.Index < 0 || a.Index >= len(p.Arrays) || hasMember(p.Arrays[a.Index].Members, pa.DiskID) {
				return fmt.Errorf("ожидаемое добавление: массив %d", a.Index)
			}
			if _, err := minMembers(a.Level); err != nil || a.RaidDevices < 2 || a.RaidDevices > 64 {
				return fmt.Errorf("ожидаемое добавление: массив %d: уровень %q, дисков %d", a.Index, a.Level, a.RaidDevices)
			}
		}
		if len(pa.New) > 0 {
			if pa.New[0].Index != len(p.Arrays) {
				return fmt.Errorf("ожидаемый новый слой %d: ожидался индекс %d", pa.New[0].Index, len(p.Arrays))
			}
			if err := validateNew(DiskRef{ID: pa.DiskID}, len(p.Arrays)-1, pa.New); err != nil {
				return fmt.Errorf("ожидаемое добавление: %w", err)
			}
		}
	}
	return nil
}

// AssembleResult — итог сборки.
type AssembleResult struct {
	Result
	// MissingDisks — диски записи, которых нет в системе (пул собран
	// деградированным или не собран).
	MissingDisks []string `json:"missing_disks,omitempty"`
	// Failed — участники, которые не удалось открыть (входят в MissingDisks).
	Failed []string `json:"failed,omitempty"`
	// Readded — участники, возвращённые в md (mdadm --re-add).
	Readded []string `json:"readded,omitempty"`
	// ReaddFailed — открытые, но не вошедшие в md участники, которых
	// не удалось вернуть ни --re-add, ни --add: нужен администратор.
	ReaddFailed []string `json:"readd_failed,omitempty"`
	// FullRecovery — участники из Readded, возвращённые полным --add
	// (re-add отказал или застрял запасным): идёт полный recovery.
	FullRecovery []string `json:"full_recovery,omitempty"`
	// PendingAccepted — индексы массивов, в которые ожидаемый диск входит
	// по суперблокам md; остальные — откат ожидания.
	PendingAccepted []int `json:"pending_accepted,omitempty"`
	// PendingErrors — сбои продолжения reshape после принятия диска.
	PendingErrors []string `json:"pending_errors,omitempty"`
	// PendingReleased — ожидаемый диск не принят ни в один массив, его
	// разметка пула стёрта, диск снова свободен.
	PendingReleased bool `json:"pending_released,omitempty"`
}

// integrityReopenArgs — повторное открытие участника (загрузка): БЕЗ
// --integrity-recalculate (он «узаконил» бы порчу, см. integrity);
// незавершённый пересчёт ядро продолжит по флагу суперблока.
func integrityReopenArgs(mode, part, name string) []string {
	args := []string{"open", "--integrity", "crc32c"}
	if mode == IntegrityBitmap {
		args = append(args, "--integrity-bitmap-mode")
	}
	return append(args, part, name)
}

// Assemble поднимает пул после загрузки: dm-integrity в режиме пула →
// md (--assemble --run, деградированный тоже) → снятие auto-read-only
// (иначе resync/reshape стоят в PENDING) → LVM → монтирование Btrfs.
// Идемпотентна: уже открытые/собранные/смонтированные слои пропускаются,
// поэтому безопасна и при автосборке udev, и при повторном запуске.
// Ничего не форматирует; прерванный reshape/resync продолжает ядро.
func (e *Executor) Assemble(ctx context.Context, p AssembleParams) (AssembleResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := AssembleResult{Result: Result{Pool: p.Pool}}
	fail := func(err error) (AssembleResult, error) {
		res.Commands = j.cmds
		return res, fmt.Errorf("сборка пула %s: %w", p.Pool, err)
	}
	e.settle(ctx, j)
	all, missing, pend, err := e.openMembers(ctx, j, p, &res)
	if err != nil {
		return fail(err)
	}
	for i, a := range p.Arrays {
		members := all[i]
		pdev := pend[i]
		if a.Level == LevelSingle {
			if len(members) == 0 {
				return fail(fmt.Errorf("массив %d: нет диска", i))
			}
			continue
		}
		md := filepath.Join(e.Paths.Dev, "md", MDName(p.Pool, i))
		if _, err := os.Stat(md); err != nil {
			// Уже собран под чужим именем (<хост>:<пул>-<i>) — берём его узел.
			if arrs, aerr := e.arrays(ctx, p.Pool); aerr == nil {
				if a, ok := e.arrayByIndex(arrs, i); ok && !a.inactive {
					md = a.node
				}
			}
		}
		if _, err := os.Stat(md); err != nil {
			if len(members) == 0 {
				return fail(fmt.Errorf("массив %d: нет дисков", i))
			}
			// udev мог начать инкрементальную сборку: дожидаемся её.
			e.settle(ctx, j)
		}
		if _, err := os.Stat(md); err != nil {
			// Инкрементальная сборка udev, не дождавшаяся всех участников,
			// оставляет неактивный массив без /dev/md/<имя> и держит
			// устройства («is busy - skipping»). Неактивный массив данных не
			// обслуживает — останавливаем и собираем явно.
			if err := e.stopPartial(ctx, j, append(append([]string{}, members...), pdev...)); err != nil {
				return fail(err)
			}
			args := append([]string{"--assemble", "--run", md}, members...)
			if len(pdev) == 1 && len(members) > 0 && e.sameArray(ctx, j, members[0], pdev[0]) {
				args = append(args, pdev[0])
			}
			if _, err := e.run(ctx, j, longTimeout, e.Tools.Mdadm, args...); err != nil {
				return fail(err)
			}
			e.settle(ctx, j)
			if err := e.waitFor(ctx, md, func() bool { _, err := os.Stat(md); return err == nil }); err != nil {
				return fail(fmt.Errorf("%s не появился: %w", md, err))
			}
		}
		real, err := filepath.EvalSymlinks(md)
		if err != nil {
			return fail(err)
		}
		st, err := e.mdstat()
		if err != nil {
			return fail(err)
		}
		ms := st[filepath.Base(real)]
		if ms.Inactive || !ms.Active {
			// Неполная инкрементальная сборка udev — запуск с тем, что есть.
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--run", md); err != nil {
				return fail(err)
			}
			if st, err = e.mdstat(); err != nil {
				return fail(err)
			}
			ms = st[filepath.Base(real)]
		}
		if ms.ReadOnly {
			// auto-read-only после --assemble: resync/reshape ждут первой
			// записи (resync=PENDING) — снимаем сразу.
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--readwrite", md); err != nil {
				return fail(err)
			}
		}
		e.reAdd(ctx, j, md, ms, members, &res)
		if len(pdev) == 1 {
			e.resumePending(ctx, j, p, i, md, ms, members, pdev[0], &res)
		}
	}
	e.assembleNewLayers(ctx, j, p, &res)
	if p.Pending != nil && len(res.PendingAccepted) == 0 {
		e.releasePending(ctx, j, p, &res)
	}
	for id := range missing {
		res.MissingDisks = append(res.MissingDisks, id)
	}
	vg := VGName(p.Pool)
	lv := filepath.Join(e.Paths.Dev, vg, LVName)
	if _, err := os.Stat(lv); err != nil {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "vgchange", "-q", "-ay", vg); err != nil {
			return fail(err)
		}
		if err := e.waitFor(ctx, lv, func() bool { _, err := os.Stat(lv); return err == nil }); err != nil {
			return fail(fmt.Errorf("%s не появился: %w", lv, err))
		}
	}
	mp := e.mountPoint(p.Pool)
	if src, ok := e.mountedAt(mp); ok {
		want, err1 := filepath.EvalSymlinks(lv)
		got, err2 := filepath.EvalSymlinks(src)
		if err1 != nil || err2 != nil || want != got {
			return fail(refuse("в %s смонтирован не том пула (%s)", mp, src))
		}
	} else {
		j.add("mkdir", []string{"-p", mp})
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return fail(err)
		}
		if _, err := e.run(ctx, j, longTimeout, e.Tools.Mount, "-t", "btrfs", lv, mp); err != nil {
			return fail(err)
		}
	}
	res.Mount = mp
	if err := e.writeManifest(p); err != nil {
		res.Warnings = append(res.Warnings, "манифест пула не записан: "+err.Error())
	}
	res.Commands = j.cmds
	return res, nil
}

// openMembers находит разделы участников и открывает dm-integrity в режиме
// пула; возвращает устройства участников по массивам и отсутствующие диски.
func (e *Executor) openMembers(ctx context.Context, j *journal, p AssembleParams, res *AssembleResult) ([][]string, map[string]bool, [][]string, error) {
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("снимок дисков: %w", err)
	}
	knames := map[string]string{}
	dup := map[string]bool{}
	for _, d := range snap.Disks {
		if _, ok := knames[d.ID]; ok {
			dup[d.ID] = true
		}
		knames[d.ID] = d.KernelName
	}
	missing := map[string]bool{}
	all := make([][]string, len(p.Arrays))
	pend := make([][]string, len(p.Arrays))
	for i, a := range p.Arrays {
		var members []string
		for _, id := range a.Members {
			if dup[id] {
				return nil, nil, nil, refuse("диск %s найден несколько раз", id)
			}
			k, ok := knames[id]
			pk := ""
			if ok {
				pk = e.partKName(k, i+1)
			}
			if pk == "" {
				missing[id] = true
				continue
			}
			dev := filepath.Join(e.Paths.Dev, pk)
			if p.Integrity {
				name := IntegrityName(p.Pool, i, id)
				node := filepath.Join(e.Paths.Dev, "mapper", name)
				if err := e.dropStaleIntegrity(ctx, j, name, node, pk); err != nil {
					missing[id] = true
					res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", id, err))
					continue
				}
				if _, err := os.Stat(node); err != nil {
					if _, err := e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, integrityReopenArgs(p.IntegrityMode, dev, name)...); err != nil {
						// Повреждённый суперблок dm-integrity одного участника
						// не должен мешать собрать пул деградированным
						// Диск считается отсутствующим.
						missing[id] = true
						res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", id, err))
						continue
					}
					if err := e.waitFor(ctx, node, func() bool { _, err := os.Stat(node); return err == nil }); err != nil {
						return nil, nil, nil, fmt.Errorf("%s не появился: %w", node, err)
					}
				}
				dev = node
			}
			members = append(members, dev)
		}
		all[i] = members
		// Ожидаемый диск: открывается так же, но сбой — не
		// отсутствие участника (add_disk мог прерваться до разметки).
		if _, ok := p.Pending.target(i); ok && !dup[p.Pending.DiskID] {
			if k, ok := knames[p.Pending.DiskID]; ok {
				if pk := e.partKName(k, i+1); pk != "" {
					dev := filepath.Join(e.Paths.Dev, pk)
					if p.Integrity {
						name := IntegrityName(p.Pool, i, p.Pending.DiskID)
						node := filepath.Join(e.Paths.Dev, "mapper", name)
						if _, err := os.Stat(node); err != nil {
							if _, err := e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, integrityReopenArgs(p.IntegrityMode, dev, name)...); err != nil {
								continue
							}
							if err := e.waitFor(ctx, node, func() bool { _, err := os.Stat(node); return err == nil }); err != nil {
								continue
							}
						}
						dev = node
					}
					pend[i] = []string{dev}
				}
			}
		}
	}
	return all, missing, pend, nil
}

// dropStaleIntegrity — участник пропал и вернулся (часто под
// другим именем ядра), а отображение dm-integrity осталось на исчезнувшем
// устройстве: в md оно faulty, reAdd его пропускал (имя dm то же), пул
// оставался деградированным до перезагрузки. Отображение устарело, если
// у текущего раздела диска нет его в holders (sysfs): убираем его
// из md, закрываем — дальше обычное открытие и mdadm --re-add.
func (e *Executor) dropStaleIntegrity(ctx context.Context, j *journal, name, node, part string) error {
	real, err := filepath.EvalSymlinks(node)
	if err != nil {
		return nil // отображения нет — откроется штатно
	}
	dm := filepath.Base(real)
	if _, err := os.Stat(filepath.Join(e.Paths.Disks.SysClassBlock, dm)); err != nil {
		return nil // не блочное устройство в sysfs — не трогаем
	}
	holders := filepath.Join(e.Paths.Disks.SysClassBlock, part, "holders")
	if _, err := os.Stat(holders); err != nil {
		return nil // sysfs не говорит о связях — не трогаем
	}
	// Связь holders заводится при загрузке таблицы dm: у вернувшегося
	// раздела (даже под тем же именем) её нет — новый объект ядра.
	if _, err := os.Lstat(filepath.Join(holders, dm)); err == nil {
		return nil // отображение на текущем разделе
	}
	if st, err := e.mdstat(); err == nil {
		for md, a := range st {
			if !hasMember(a.Members, dm) {
				continue
			}
			mdDev := filepath.Join(e.Paths.Dev, md)
			// --fail может ответить ошибкой, если участник уже faulty.
			e.run(ctx, j, shortTimeout, e.Tools.Mdadm, mdDev, "--fail", real)
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, mdDev, "--remove", real); err != nil {
				return fmt.Errorf("устаревшее отображение %s (%s) не убрано из %s: %w", name, dm, md, err)
			}
		}
	}
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Dmsetup, "remove", name); err != nil {
		return fmt.Errorf("устаревшее отображение %s не закрыто: %w", name, err)
	}
	return nil
}

// mdUUID — UUID массива из суперблока md участника; пусто — суперблока нет.
func (e *Executor) mdUUID(ctx context.Context, j *journal, dev string) string {
	out, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--examine", "--brief", dev)
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "UUID=") {
			return strings.TrimPrefix(f, "UUID=")
		}
	}
	return ""
}

// sameArray — у dev суперблок того же массива, что у участника ref.
func (e *Executor) sameArray(ctx context.Context, j *journal, ref, dev string) bool {
	u := e.mdUUID(ctx, j, dev)
	return u != "" && u == e.mdUUID(ctx, j, ref)
}

// resumePending решает судьбу ожидаемого диска массива i: входит в
// массив (участник или запасной) — принят; сброс между --add и --grow
// оставил его запасным — --grow до целевых уровня и числа участников.
// Не входит и суперблока массива нет — ожидание откатывается, dm-integrity
// диска закрывается. Сбой продолжения не прерывает сборку.
func (e *Executor) resumePending(ctx context.Context, j *journal, p AssembleParams, i int, md string, ms MdstatArray, members []string, dev string, res *AssembleResult) {
	want, _ := p.Pending.target(i)
	kn := func() string {
		r, err := filepath.EvalSymlinks(dev)
		if err != nil {
			return ""
		}
		return filepath.Base(r)
	}()
	in := false
	for _, m := range append(append([]string{}, ms.Members...), ms.Spares...) {
		in = in || (kn != "" && m == kn)
	}
	if !in && len(members) > 0 && e.sameArray(ctx, j, members[0], dev) {
		// udev собрал массив без него — вернуть по bitmap.
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--re-add", md, dev); err == nil {
			in = true
		} else {
			res.PendingErrors = append(res.PendingErrors, fmt.Sprintf("массив %d: --re-add: %v", i, err))
		}
	}
	if !in {
		if p.Integrity {
			e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, "close", IntegrityName(p.Pool, i, p.Pending.DiskID))
		}
		return
	}
	res.PendingAccepted = append(res.PendingAccepted, i)
	if ms.Action == "reshape" || ms.RaidDevices >= want.RaidDevices {
		return
	}
	// Те же команды, что в AddDisk (raid5 → raid6 — с
	// --layout=preserve, без reshape раскладки).
	grow, err := growCmds(md, ms, want)
	if err != nil {
		res.PendingErrors = append(res.PendingErrors, fmt.Sprintf("массив %d: %v", i, err))
		return
	}
	for _, args := range grow {
		if _, err := e.run(ctx, j, longTimeout, e.Tools.Mdadm, args...); err != nil {
			res.PendingErrors = append(res.PendingErrors, fmt.Sprintf("массив %d: %v", i, err))
			return
		}
	}
}

// AssembleAll собирает все пулы из базы (вызывается gamayund при старте до
// применения защиты и конфигурации Samba). Ошибка одного пула не мешает
// остальным. helper может ещё запускаться — повтор при ошибке связи.
func (s *Service) AssembleAll(ctx context.Context, logf func(pool string, r AssembleResult, err error)) error {
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
		rec, err := s.getRecord(ctx, n)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var r AssembleResult
		err = s.callRetry(ctx, OpAssemble, rec.assembleParams(), &r)
		if logf != nil {
			logf(n, r, err)
		}
		if rec.Creating {
			if cerr := s.resolveCreating(ctx, rec); cerr != nil {
				errs = append(errs, fmt.Errorf("пул %s: %w", n, cerr))
			}
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("пул %s: %w", n, err))
			continue
		}
		if rec.Pending != nil && rec.Pending.Replace {
			// Сброс внутри замены — дозавершение или откат.
			if err := s.afterReplaceAssemble(ctx, rec, r.PendingAccepted, true); err != nil {
				errs = append(errs, fmt.Errorf("пул %s: %w", n, err))
			}
			continue
		}
		if rec.Pending != nil {
			// Сброс внутри add_disk — состав по суперблокам md.
			res := rec.resolve(r.PendingAccepted)
			if err := s.saveRecord(ctx, res, false); err != nil {
				errs = append(errs, fmt.Errorf("пул %s: %w", n, err))
			} else if err := s.submitResume(ctx, res); err != nil {
				errs = append(errs, fmt.Errorf("пул %s: %w", n, err))
			}
		}
		// Reshape после add_disk продолжается в ядре, а задача add_disk
		// стала interrupted и рост LVM/Btrfs не выполнит — ставим задачу
		// роста, она дождётся конца reshape.
		if cur, err := s.getRecord(ctx, n); err == nil {
			rec = cur
		}
		if st, err := s.status(ctx, n); err == nil && (reshaping(st) || len(r.PendingAccepted) > 0 || rec.GrowPending) {
			if lost := stalledDegraded(st); lost != "" {
				// Задача роста ждала бы выпавший диск вечно — пул
				// остаётся заблокированным до «Продолжить» или удаления.
				errs = append(errs, fmt.Errorf("пул %s: добавление диска не завершено (%s); рост не поставлен — верните или замените диск и нажмите «Продолжить» либо удалите пул", n, lost))
				continue
			}
			raw, _ := json.Marshal(growTask{Pool: n})
			// Пул уже занят дозавершением add_disk — оно само растит пул.
			if _, err := s.submit(ctx, n, TaskGrow, raw); err != nil && !errors.Is(err, ErrConflict) {
				errs = append(errs, fmt.Errorf("пул %s: задача роста: %w", n, err))
			}
		}
	}
	if err := s.writeBootFile(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// resolveCreating — пул, создание которого прервано сбросом или
// перезапуском gamayund. Полный стек (том данных смонтирован)
// — флаг снимается, начальную синхронизацию дождётся защита. Тома
// данных нет — данных на пуле быть не может: откат create_rollback по
// дискам записи и удаление записи. Том есть, но не смонтирован — данные
// возможны: ничего не делается, пул остаётся «создаётся» до
// удаления администратором.
func (s *Service) resolveCreating(ctx context.Context, rec PoolRecord) error {
	st, err := s.status(ctx, rec.Name)
	if err != nil {
		return fmt.Errorf("создание прервано, состояние не определено: %w — пул оставлен как есть", err)
	}
	if st.Health == HealthUnknown {
		// Непрочитанный том данных — не «тома нет»; откат по неполным
		// данным мог бы уничтожить данные.
		return fmt.Errorf("создание прервано, %s — пул оставлен как есть", unknownReason(st))
	}
	switch {
	case st.Mounted && st.LVSizeBytes > 0:
		rec.Creating = false
		return s.saveRecord(ctx, rec, false)
	case st.LVSizeBytes > 0 || st.Btrfs != nil:
		return fmt.Errorf("создание прервано: том данных есть, но пул не собран полностью (%s) — автоматический откат не выполняется; удалите пул", strings.Join(st.Issues, "; "))
	}
	done, err := s.rollbackCreate(ctx, rec)
	if err != nil {
		return fmt.Errorf("создание прервано; откат не выполнен: %w — удалите пул", err)
	}
	return fmt.Errorf("создание прервано (сброс или перезапуск): %s", done)
}

// resolvePending сверяет ожидаемое добавление с массивами (повторная
// сборка живого пула идемпотентна) и снимает ожидание.
func (s *Service) resolvePending(ctx context.Context, rec PoolRecord, resume bool) error {
	if rec.Pending == nil {
		return nil
	}
	var r AssembleResult
	if err := s.helper.Call(ctx, OpAssemble, rec.assembleParams(), &r); err != nil {
		return err
	}
	res := rec.resolve(r.PendingAccepted)
	if err := s.saveRecord(ctx, res, false); err != nil {
		return err
	}
	if !resume {
		return nil
	}
	return s.submitResume(ctx, res)
}

// submitResume ставит задачу дозавершения добавления диска.
func (s *Service) submitResume(ctx context.Context, rec PoolRecord) error {
	tp, ok := rec.resumeTask()
	if !ok {
		return nil
	}
	raw, err := json.Marshal(tp)
	if err != nil {
		return err
	}
	if _, err := s.submit(ctx, rec.Name, TaskAddDisk, raw); err != nil {
		return fmt.Errorf("дозавершение добавления диска: %w", err)
	}
	return nil
}

// BootFileName — файл раннего открытия в каталоге базы gamayund.
const BootFileName = "boot-pools.json"

// BootParams — пулы с dm-integrity из базы (состав для раннего открытия).
func (s *Service) BootParams(ctx context.Context) ([]AssembleParams, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pools WHERE integrity <> 0 ORDER BY name")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	out := []AssembleParams{}
	for _, n := range names {
		rec, err := s.getRecord(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, rec.assembleParams())
	}
	return out, nil
}

// writeBootFile атомарно переписывает BootFile по базе.
func (s *Service) writeBootFile(ctx context.Context) error {
	if s.BootFile == "" {
		return nil
	}
	ps, err := s.BootParams(ctx)
	if err != nil {
		return fmt.Errorf("файл раннего открытия: %w", err)
	}
	raw, _ := json.MarshalIndent(ps, "", "  ")
	tmp := s.BootFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("файл раннего открытия: %w", err)
	}
	if err := os.Rename(tmp, s.BootFile); err != nil {
		return fmt.Errorf("файл раннего открытия: %w", err)
	}
	return nil
}

// OpenIntegrityFile — ранний юнит загрузки: открыть dm-integrity всех пулов
// из файла gamayund. Ошибки пулов журналируются, загрузку не останавливают.
func OpenIntegrityFile(ctx context.Context, e *Executor, path string, logf func(pool string, r AssembleResult, err error)) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ps []AssembleParams
	if err := json.Unmarshal(raw, &ps); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, p := range ps {
		var r AssembleResult
		err := p.Validate()
		if err == nil {
			r, err = e.OpenIntegrity(ctx, p)
		}
		if logf != nil {
			logf(p.Pool, r, err)
		}
	}
	return nil
}

// AssembleRetries — попытки связаться с helper при старте (шаг 1 с).
var AssembleRetries = 15

// callRetry вызывает helper при старте, пока он недоступен (через
// internal/watch — недоступность есть «неизвестное состояние», окно
// AssembleRetries/2 с, затем второе окно; ошибка — с причиной).
func (s *Service) callRetry(ctx context.Context, op string, params, out any) error {
	var err error
	if err = s.helper.Call(ctx, op, params, out); err == nil || s.Unavailable == nil || !s.Unavailable(err) {
		return err
	}
	window := time.Duration(AssembleRetries) * time.Second / 2
	res := watch.Run(ctx, watch.Spec{
		Name:          "helper (" + op + ")",
		Poll:          time.Second,
		UnknownWindow: window,
		StallWindow:   window,
		Deadline:      2*window + 2*time.Second,
		Probe: func(ctx context.Context) watch.Observation {
			err = s.helper.Call(ctx, op, params, out)
			if err != nil && s.Unavailable(err) {
				return watch.Observation{Err: err}
			}
			return watch.Observation{Target: true} // ответ получен (успех или ошибка операции)
		},
	})
	if res.OK() {
		return err
	}
	if res.Result == watch.ResultCancelled && ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%s: %w", res.Reason, err)
}

func reshaping(st PoolStatus) bool {
	for _, a := range st.Arrays {
		if a.Action == "reshape" {
			return true
		}
	}
	return false
}

type growTask struct {
	Pool string `json:"pool"`
}

// runGrow ждёт конца reshape/resync и растягивает PV, LV и Btrfs (только рост).
func (s *Service) runGrow(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var gt growTask
	if err := json.Unmarshal(raw, &gt); err != nil || gt.Pool == "" {
		return nil, fmt.Errorf("параметры задачи: %v", err)
	}
	if _, err := s.waitIdle(ctx, gt.Pool, progress, 1, 90, nil); err != nil {
		return nil, err
	}
	var r Result
	if err := s.helper.Call(ctx, OpGrow, PoolParams{Pool: gt.Pool}, &r); err != nil {
		return nil, err
	}
	if err := s.endGrowPending(ctx, gt.Pool); err != nil {
		return nil, err
	}
	st, err := s.status(ctx, gt.Pool)
	if err != nil {
		return nil, err
	}
	return TaskResult{Pool: gt.Pool, Helper: []Result{r}, Status: st}, nil
}

// stopPartial останавливает неактивные массивы, в которых есть хоть один
// из участников (по имени ядра: dm-3, sdb1).
func (e *Executor) stopPartial(ctx context.Context, j *journal, members []string) error {
	st, err := e.mdstat()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, m := range members {
		if r, err := filepath.EvalSymlinks(m); err == nil {
			want[filepath.Base(r)] = true
		}
	}
	for name, a := range st {
		if !a.Inactive {
			continue
		}
		for _, m := range append(append(append([]string{}, a.Members...), a.Spares...), a.Faulty...) {
			if want[m] {
				if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--stop", filepath.Join(e.Paths.Dev, name)); err != nil {
					return err
				}
				e.settle(ctx, j)
				break
			}
		}
	}
	return nil
}

// reAdd возвращает в деградированный массив открытых участников, которых в
// нём нет (диск вернулся): mdadm --re-add досинхронизирует по write-intent
// bitmap. Если mdadm отказал (нет bitmap, события устарели) —
// полный --add с recovery: участник — диск записи пула, массив
// деградирован. Участник, оставшийся в массиве запасным (S) при
// деградации и без идущего восстановления, — recovery после re-add
// прервался (после пропажи диска под dm-integrity re-add по bitmap
// прерывается через 1–5 с), сам он не вернётся: --remove + --add.
// Не удалось — отказ в ReaddFailed. Ошибка не прерывает сборку.
func (e *Executor) reAdd(ctx context.Context, j *journal, md string, ms MdstatArray, members []string, res *AssembleResult) {
	if ms.RaidDevices == 0 || ms.InSync >= ms.RaidDevices {
		return
	}
	in := map[string]bool{}
	for _, m := range append(append([]string{}, ms.Members...), ms.Spares...) {
		in[m] = true
	}
	// Запасной в массиве, где нет действия (recovery, resync, reshape…):
	// ядро не начинает восстановление на него само — застрял.
	stuck := map[string]bool{}
	if ms.Action == "" && !ms.Delayed && !ms.ReadOnly {
		for _, s := range ms.Spares {
			stuck[s] = true
		}
	}
	add := func(dev string) {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, md, "--add", dev); err != nil {
			res.ReaddFailed = append(res.ReaddFailed, fmt.Sprintf("%s: %v", dev, err))
			return
		}
		res.Readded = append(res.Readded, dev)
		res.FullRecovery = append(res.FullRecovery, dev)
	}
	for _, dev := range members {
		r, err := filepath.EvalSymlinks(dev)
		if err != nil {
			continue
		}
		k := filepath.Base(r)
		if stuck[k] {
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, md, "--remove", dev); err != nil {
				res.ReaddFailed = append(res.ReaddFailed, fmt.Sprintf("%s: резервный не убран: %v", dev, err))
				continue
			}
			add(dev)
			continue
		}
		if in[k] {
			continue
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--re-add", md, dev); err != nil {
			add(dev)
			continue
		}
		res.Readded = append(res.Readded, dev)
	}
}

// OpenIntegrity — только открытие dm-integrity участников (ранний юнит
// загрузки gamayun-integrity.service): все участники появляются
// разом, инкрементальная сборка md не успевает запустить массив неполным
// таймером mdadm-last-resort. md/LVM/монтирование — позже, в Assemble.
func (e *Executor) OpenIntegrity(ctx context.Context, p AssembleParams) (AssembleResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := AssembleResult{Result: Result{Pool: p.Pool}}
	if !p.Integrity {
		return res, nil
	}
	e.settle(ctx, j)
	_, missing, _, err := e.openMembers(ctx, j, p, &res)
	for id := range missing {
		res.MissingDisks = append(res.MissingDisks, id)
	}
	res.Commands = j.cmds
	return res, err
}

// releasePending стирает разметку ожидаемого диска, не принятого ни в один
// массив: иначе после отката add_disk диск остаётся
// размеченным и не свободен. Условия (любое нарушено — только запись в
// PendingErrors, без записи на диск): диск в allowlist и не участник
// записи пула; каждый раздел — с меткой dn-<пул>-* и без держателей.
// Диск пользователь уже подтвердил к стиранию при add_disk.
func (e *Executor) releasePending(ctx context.Context, j *journal, p AssembleParams, res *AssembleResult) {
	id := p.Pending.DiskID
	skip := func(format string, a ...any) {
		res.PendingErrors = append(res.PendingErrors, "очистка диска "+id+": "+fmt.Sprintf(format, a...))
	}
	if !e.allowed(id) {
		skip("не в destructive_allow_ids")
		return
	}
	for _, a := range p.Arrays {
		if hasMember(a.Members, id) {
			return // диск уже в пуле (частичное принятие) — не трогать
		}
	}
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		skip("снимок дисков: %v", err)
		return
	}
	var k string
	for _, d := range snap.Disks {
		if d.ID == id {
			k = d.KernelName
		}
	}
	if k == "" {
		return // диска нет — нечего очищать
	}
	if !knameRe.MatchString(k) {
		skip("имя ядра %q", k)
		return
	}
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, k)
	ents, _ := os.ReadDir(dir)
	var parts []string
	prefix := PartLabelPrefix(p.Pool)
	for _, en := range ents {
		if _, err := os.Stat(filepath.Join(dir, en.Name(), "partition")); err != nil {
			continue
		}
		if hs, _ := os.ReadDir(filepath.Join(dir, en.Name(), "holders")); len(hs) > 0 {
			skip("раздел %s занят", en.Name())
			return
		}
		if !strings.HasPrefix(e.partName(filepath.Join(dir, en.Name())), prefix) {
			skip("раздел %s без метки %s*", en.Name(), prefix)
			return
		}
		parts = append(parts, en.Name())
	}
	if hs, _ := os.ReadDir(filepath.Join(dir, "holders")); len(hs) > 0 {
		skip("диск занят")
		return
	}
	if len(parts) == 0 {
		return
	}
	for _, pt := range parts {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", filepath.Join(e.Paths.Dev, pt)); err != nil {
			skip("%v", err)
			return
		}
	}
	disk := filepath.Join(e.Paths.Dev, k)
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Sgdisk, "--zap-all", disk); err != nil {
		skip("%v", err)
		return
	}
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", disk); err != nil {
		skip("%v", err)
		return
	}
	e.settle(ctx, j)
	res.PendingReleased = true
}

// partName — метка GPT раздела (PARTNAME из uevent в sysfs).
func (e *Executor) partName(sysdir string) string {
	b, err := os.ReadFile(filepath.Join(sysdir, "uevent"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PARTNAME="); ok {
			return v
		}
	}
	return ""
}
