package home

import (
	"gamayun-nas/internal/disks"

	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Операции helper.
const (
	OpDestroy     = "home.pool.destroy"
	OpSetReadOnly = "home.pool.set_readonly"
	// OpCreateRollback — откат прерванного создания: как
	// destroy, но только по дискам записи создаваемого пула.
	OpCreateRollback = "home.pool.create_rollback"
)

// CreateRollbackParams — пул и диски из его записи «создаётся».
type CreateRollbackParams struct {
	Pool    string   `json:"pool"`
	DiskIDs []string `json:"disk_ids"`
}

// Validate проверяет имя пула и идентичности дисков.
func (p CreateRollbackParams) Validate() error {
	if err := (PoolParams{Pool: p.Pool}).Validate(); err != nil {
		return err
	}
	if len(p.DiskIDs) == 0 {
		return fmt.Errorf("нет дисков записи пула %s", p.Pool)
	}
	for _, id := range p.DiskIDs {
		if !ValidDiskID(id) {
			return fmt.Errorf("недопустимый диск %q", id)
		}
	}
	return nil
}

// CreateRollback разбирает остатки прерванного создания пула. Диск,
// найденный в стеке пула или по меткам, но не входящий в DiskIDs, —
// отказ до любой записи (чужие остатки с тем же именем пула не трогаются).
func (e *Executor) CreateRollback(ctx context.Context, p CreateRollbackParams) (Result, error) {
	only := map[string]bool{}
	for _, id := range p.DiskIDs {
		only[id] = true
	}
	res, err := e.destroy(ctx, PoolParams{Pool: p.Pool}, only)
	if err != nil {
		return res, err
	}
	// Диск записи, недоступный при откате (выпал), не стёрт —
	// откат частичный, а не «диски свободны».
	knames, kerr := e.diskKNames()
	if kerr != nil {
		res.Warnings = append(res.Warnings, "наличие дисков после отката не проверено: "+kerr.Error())
		return res, nil
	}
	for _, id := range p.DiskIDs {
		if knames[id] == "" {
			res.Warnings = append(res.Warnings, "диск "+id+" недоступен: остатки пула на нём не стёрты — после возврата освободите диск")
		}
	}
	return res, nil
}

// ReadOnlyParams — перевод смонтированного пула в read-only и обратно.
type ReadOnlyParams struct {
	Pool     string `json:"pool"`
	ReadOnly bool   `json:"read_only"`
}

// Validate проверяет имя пула.
func (p ReadOnlyParams) Validate() error { return PoolParams{Pool: p.Pool}.Validate() }

// SetReadOnly перемонтирует Btrfs пула (mount -o remount,ro|rw). Не
// разрушающая операция: allowlist не требуется, но точка монтирования
// должна быть точкой пула и источником — логический том пула.
func (e *Executor) SetReadOnly(ctx context.Context, p ReadOnlyParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Pool: p.Pool, Commands: []string{}}
	mp := e.mountPoint(p.Pool)
	src, opts, ok := e.mountInfo(mp)
	if !ok {
		return res, refuse("пул %s не смонтирован в %s", p.Pool, mp)
	}
	lv := filepath.Join(e.Paths.Dev, VGName(p.Pool), LVName)
	want, err1 := filepath.EvalSymlinks(lv)
	got, err2 := filepath.EvalSymlinks(src)
	if err1 != nil || err2 != nil || want != got {
		return res, refuse("в %s смонтирован не том пула (%s)", mp, src)
	}
	res.Mount = mp
	if contains(opts, "ro") == p.ReadOnly {
		return res, nil
	}
	mode := "rw"
	if p.ReadOnly {
		mode = "ro"
	}
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mount, "-o", "remount,"+mode, mp); err != nil {
		res.Commands = j.cmds
		return res, err
	}
	res.Commands = j.cmds
	return res, nil
}

// Destroy разбирает стек пула: umount → vgremove/pvremove → mdadm --stop и
// --zero-superblock → integritysetup close → wipefs разделов и sgdisk
// --zap-all дисков. Все найденные физические диски пула должны входить в
// destructive_allow_ids; иначе — отказ до любой записи. Пул, от которого
// в системе ничего не осталось, — успешная операция без команд.
func (e *Executor) Destroy(ctx context.Context, p PoolParams) (Result, error) {
	return e.destroy(ctx, p, nil)
}

// destroy — разборка стека; only != nil — разрешённые диски.
func (e *Executor) destroy(ctx context.Context, p PoolParams, only map[string]bool) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Pool: p.Pool, Commands: []string{}}
	vg := VGName(p.Pool)

	// Сбор стека (только чтение).
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return res, refuse("массивы пула %s: %v", p.Pool, err)
	}
	pvs, err := e.pvList(ctx, vg)
	if err != nil {
		return res, refuse("тома пула %s: %v", p.Pool, err)
	}
	vgFound := len(pvs) > 0
	if !vgFound {
		vgs, err := e.vgList(ctx)
		if err != nil {
			return res, refuse("список VG: %v", err)
		}
		for _, v := range vgs {
			if v["vg_name"] == vg {
				vgFound = true
			}
		}
	}
	allMaps, err := e.integrityMaps(ctx)
	if err != nil {
		return res, refuse("dmsetup: %v", err)
	}
	var maps []string
	for _, m := range allMaps {
		if strings.HasPrefix(m.Name, IntegrityPrefix(p.Pool)) {
			maps = append(maps, m.Name)
		}
	}
	sort.Strings(maps)

	nodes := append([]string{}, pvs...)
	for _, a := range arrs {
		nodes = append(nodes, a.node)
	}
	for _, m := range maps {
		nodes = append(nodes, filepath.Join(e.Paths.Dev, "mapper", m))
	}
	knames := map[string]string{} // идентичность → имя ядра
	for _, n := range nodes {
		ds, err := e.disksUnder(n)
		if err != nil {
			return res, refuse("диски пула %s под %s: %v", p.Pool, n, err)
		}
		for id, k := range ds {
			knames[id] = k
		}
	}
	// Участники md исчезли (диски отключены или вернулись под
	// другими именами) — дисков по слоям не найти. Верхние слои разбираются
	// без записи на диски; вернувшиеся диски находятся по меткам ниже.
	// Остатки без верхних слоёв (одни разделы dn-<пул>-*, например
	// после сбоя создания до md или отказа участника) — диски по меткам.
	labeled, err := e.labeledDisks(p.Pool)
	if err != nil {
		return res, refuse("метки разделов пула %s: %v", p.Pool, err)
	}
	for id, k := range labeled {
		knames[id] = k
	}
	for id, k := range knames {
		if !e.allowed(id) {
			return res, refuse("диск пула %s (%s) не входит в destructive_allow_ids", p.Pool, id)
		}
		if only != nil && !only[id] {
			return res, refuse("диск %s с разметкой пула %s не входит в запись создаваемого пула", id, p.Pool)
		}
		if !knameRe.MatchString(k) {
			return res, refuse("недопустимое имя ядра %q", k)
		}
	}
	// Участники md — до остановки массивов (после остановки slaves пусты).
	members := map[string][]string{}
	for _, a := range arrs {
		slaves, err := os.ReadDir(filepath.Join(e.Paths.Disks.SysClassBlock, a.kname, "slaves"))
		if err != nil {
			return res, refuse("диски массива %s: %v", a.name, err)
		}
		for _, s := range slaves {
			members[a.name] = append(members[a.name], filepath.Join(e.Paths.Dev, s.Name()))
		}
	}
	mp := e.mountPoint(p.Pool)
	_, mounted := e.mountedAt(mp)
	if len(nodes) == 0 && !vgFound && !mounted && len(labeled) == 0 {
		return res, nil
	}

	fail := func(err error) (Result, error) {
		res.Commands = j.cmds
		return res, fmt.Errorf("удаление пула %s прервано: %w", p.Pool, err)
	}

	// Разборка сверху вниз.
	if mounted {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Umount, mp); err != nil {
			return fail(err)
		}
	}
	if vgFound {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "vgremove", "-q", "-f", "-y", vg); err != nil {
			// Сломанный массив (нет участников): метаданные VG не записать —
			// только деактивировать, иначе md не остановить.
			if _, err2 := e.run(ctx, j, shortTimeout, e.Tools.LVM, "vgchange", "-q", "-an", vg); err2 != nil {
				return fail(err)
			}
			res.Warnings = append(res.Warnings, "группа томов "+vg+" деактивирована, но не удалена (в массиве нет дисков)")
			pvs = nil // PV на том же сломанном массиве — не записать
		}
		for _, pv := range pvs {
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "pvremove", "-q", "-y", pv); err != nil {
				return fail(err)
			}
		}
	}
	// Отображения LV, которые LVM уже не видит (PV на сломанном массиве),
	// держат md — снять их напрямую.
	if lvs, err := e.query(ctx, e.Tools.Dmsetup, "ls"); err == nil {
		for _, l := range strings.Split(string(lvs), "\n") {
			f := strings.Fields(l)
			if len(f) > 0 && strings.HasPrefix(f[0], vg+"-") {
				if _, err := e.run(ctx, j, shortTimeout, e.Tools.Dmsetup, "remove", f[0]); err != nil {
					return fail(err)
				}
			}
		}
	}
	for _, a := range arrs {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--stop", a.node); err != nil {
			return fail(err)
		}
		e.settle(ctx, j)
		for _, m := range members[a.name] {
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--zero-superblock", m); err != nil {
				// Участник исчез или вернулся под другим именем: его
				// раздел, если диск на месте, сотрёт wipefs по меткам ниже.
				res.Warnings = append(res.Warnings, "суперблок "+m+" не стёрт: "+err.Error())
			}
		}
	}
	for _, m := range maps {
		var lastErr error
		for try := 0; try < 5; try++ {
			e.settle(ctx, j)
			if _, lastErr = e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, "close", m); lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			return fail(fmt.Errorf("не закрыто отображение %s: %w", m, lastErr))
		}
	}
	ids := make([]string, 0, len(knames))
	for id := range knames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		k := knames[id]
		parts, _ := os.ReadDir(filepath.Join(e.Paths.Disks.SysClassBlock, k))
		for _, pt := range parts {
			if _, err := os.Stat(filepath.Join(e.Paths.Disks.SysClassBlock, k, pt.Name(), "partition")); err != nil {
				continue
			}
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", filepath.Join(e.Paths.Dev, pt.Name())); err != nil {
				return fail(err)
			}
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Sgdisk, "--zap-all", filepath.Join(e.Paths.Dev, k)); err != nil {
			return fail(err)
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", filepath.Join(e.Paths.Dev, k)); err != nil {
			return fail(err)
		}
		// Ядро могло не перечитать таблицу (раздел был занят в момент
		// --zap-all): удалить устаревшие разделы, иначе по старой метке
		// раздела диск остаётся «найденным пулом». Ошибка не мешает: разделов
		// может уже не быть.
		if len(parts) > 0 {
			e.run(ctx, j, shortTimeout, e.Tools.Partx, "--delete", filepath.Join(e.Paths.Dev, k))
		}
	}
	e.settle(ctx, j)
	// Точка монтирования удаляется и у несмонтированного пула (остатки
	// после сбоя); os.Remove не удаляет непустой каталог.
	if fi, err := os.Lstat(mp); err == nil && fi.IsDir() {
		j.add("rmdir", []string{mp})
		_ = os.Remove(mp)
	}
	res.Commands = j.cmds
	return res, nil
}

// labeledDisks — диски (идентичность → имя ядра) с разделами, помеченными
// dn-<пул>-*. Раздел с держателем (открыт integrity, в md) не мешает:
// верхние слои разбираются раньше по их собственному поиску.
func (e *Executor) labeledDisks(pool string) (map[string]string, error) {
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return nil, err
	}
	prefix := PartLabelPrefix(pool)
	out := map[string]string{}
	for _, d := range snap.Disks {
		if d.ID == "" || !knameRe.MatchString(d.KernelName) {
			continue
		}
		dir := filepath.Join(e.Paths.Disks.SysClassBlock, d.KernelName)
		ents, _ := os.ReadDir(dir)
		for _, en := range ents {
			if _, err := os.Stat(filepath.Join(dir, en.Name(), "partition")); err != nil {
				continue
			}
			if strings.HasPrefix(e.partName(filepath.Join(dir, en.Name())), prefix) {
				out[d.ID] = d.KernelName
			}
		}
	}
	return out, nil
}
