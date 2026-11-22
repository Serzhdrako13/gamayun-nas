package home

// Замена диска (горячая замена, дозавершение после
// сброса, вывод и очистка старого диска).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gamayun-nas/internal/disks"
)

// diskKNames — имена ядра дисков по идентичности (неоднозначные — пусто).
func (e *Executor) diskKNames() (map[string]string, error) {
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	dup := map[string]bool{}
	for _, d := range snap.Disks {
		if _, ok := out[d.ID]; ok {
			dup[d.ID] = true
		}
		out[d.ID] = d.KernelName
	}
	for id := range dup {
		out[id] = ""
	}
	return out, nil
}

// memberDev — устройство участника id массива i пула (dm-integrity или
// раздел) и его имя ядра; пусто — устройства нет.
func (e *Executor) memberDev(pool string, i int, id string, integrity bool, knames map[string]string) (string, string) {
	if integrity {
		node := filepath.Join(e.Paths.Dev, "mapper", IntegrityName(pool, i, id))
		r, err := filepath.EvalSymlinks(node)
		if err != nil {
			return "", ""
		}
		return node, filepath.Base(r)
	}
	k := knames[id]
	if k == "" || !knameRe.MatchString(k) {
		return "", ""
	}
	pk := e.partKName(k, i+1)
	if pk == "" {
		return "", ""
	}
	return filepath.Join(e.Paths.Dev, pk), pk
}

// mdRole — роль устройства kn в массиве по /proc/mdstat: faulty,
// replacing, spare, member; пусто — не входит.
func mdRole(ms MdstatArray, kn string) string {
	if kn == "" {
		return ""
	}
	switch {
	case contains(ms.Faulty, kn):
		return "faulty"
	case contains(ms.Replacing, kn):
		return "replacing"
	case contains(ms.Spares, kn):
		return "spare"
	case contains(ms.Members, kn):
		return "member"
	}
	return ""
}

// Replace ставит новый диск в массивы пула: в деградированные — на место
// отказавшего (восстановление в фоне), при горячей замене (p.Old) — как
// замену исправного участника (mdadm --replace). Проверки — до записи.
func (e *Executor) Replace(ctx context.Context, p ReplaceParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Pool: p.Pool}
	if !e.allowed(p.Disk.ID) {
		return res, refuse("диск %s не входит в destructive_allow_ids", p.Disk.ID)
	}
	hot := p.Old != nil
	if hot && !e.allowed(p.Old.ID) {
		return res, refuse("старый диск %s не входит в destructive_allow_ids (после замены он очищается)", p.Old.ID)
	}
	if err := e.checkPoolAllowed(ctx, p.Pool); err != nil {
		return res, err
	}
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return res, refuse("массивы пула: %v", err)
	}
	st, err := e.mdstat()
	if err != nil {
		return res, refuse("/proc/mdstat: %v", err)
	}
	knames, err := e.diskKNames()
	if err != nil {
		return res, refuse("снимок дисков: %v", err)
	}
	type target struct {
		arr  poolArray
		spec ReplaceArray
		y    string // роль нового диска в массиве (дозавершение)
		xdev string // участник старого диска (горячая замена)
	}
	var targets []target
	for _, spec := range p.Arrays {
		a, ok := e.arrayByIndex(arrs, spec.Index)
		if !ok {
			return res, refuse("массив %d пула %s не найден", spec.Index, p.Pool)
		}
		ms, ok := st[a.kname]
		if !ok || !ms.Active {
			return res, refuse("массив %s не активен", a.name)
		}
		_, ykn := e.memberDev(p.Pool, spec.Index, p.Disk.ID, p.Integrity, knames)
		t := target{arr: a, spec: spec, y: mdRole(ms, ykn)}
		switch {
		case t.y != "" && !p.Resume:
			return res, refuse("диск %s уже в массиве %s (%s)", p.Disk.ID, a.name, t.y)
		case t.y == "faulty":
			return res, refuse("новый диск %s отказал в массиве %s", p.Disk.ID, a.name)
		}
		if hot {
			xdev, xkn := e.memberDev(p.Pool, spec.Index, p.Old.ID, p.Integrity, knames)
			t.xdev = xdev
			if t.y == "" || t.y == "spare" {
				if mdRole(ms, xkn) != "member" {
					return res, refuse("старый диск %s не исправный диск массива %s", p.Old.ID, a.name)
				}
				spares := len(ms.Spares)
				if t.y == "spare" {
					spares--
				}
				if ms.Action != "" || len(ms.Faulty) > 0 || ms.InSync != ms.RaidDevices || spares > 0 || len(ms.Replacing) > 0 {
					return res, refuse("массив %s не в исправном покое: %s %s, отказавших %d, резервных %d, замен %d",
						a.name, ms.Status, ms.Action, len(ms.Faulty), len(ms.Spares), len(ms.Replacing))
				}
			}
		} else if t.y == "" {
			if ms.InSync >= ms.RaidDevices && len(ms.Faulty) == 0 {
				return res, refuse("в массиве %s нет выбывшего диска (%s)", a.name, ms.Status)
			}
			if ms.Action == "recovery" || len(ms.Spares) > 0 {
				return res, refuse("массив %s уже восстанавливается или в нём есть резервный диск", a.name)
			}
		}
		integ, err := e.isIntegrityArray(a.kname)
		if err != nil {
			return res, refuse("%v", err)
		}
		if integ != p.Integrity {
			return res, refuse("массив %s: integrity=%v, в плане %v", a.name, integ, p.Integrity)
		}
		targets = append(targets, t)
	}
	var idx []int
	var slices []int64
	for _, t := range targets {
		if t.y == "" {
			idx = append(idx, t.spec.Index)
			slices = append(slices, t.spec.SliceBytes)
		}
	}
	var nodes map[int]string
	var k string
	if p.Resume {
		if len(idx) > 0 {
			if nodes, err = e.resumeParts(p.Disk, idx, slices); err != nil {
				return res, err
			}
		}
	} else if k, err = e.CheckFreeDisk(ctx, p.Disk); err != nil {
		return res, err
	}
	fail := func(err error) (Result, error) {
		res.Commands = j.cmds
		return res, fmt.Errorf("замена диска в пуле %s прервана: %w", p.Pool, err)
	}
	if !hot && !p.Resume {
		// Отказавшие и отсоединённые участники — из массивов; ошибки не
		// фатальны (их может не быть).
		for _, t := range targets {
			e.run(ctx, j, shortTimeout, e.Tools.Mdadm, t.arr.node, "--remove", "failed")
			e.run(ctx, j, shortTimeout, e.Tools.Mdadm, t.arr.node, "--remove", "detached")
		}
		if p.Integrity {
			if err := e.closeStaleIntegrity(ctx, j, p.Pool); err != nil {
				return fail(err)
			}
		}
	}
	if !p.Resume {
		if nodes, err = e.partition(ctx, j, k, layout(p.Pool, idx, slices)); err != nil {
			return fail(err)
		}
	}
	for _, t := range targets {
		switch t.y {
		case "member", "replacing":
			continue
		case "spare":
			// Сброс между --add и --replace: диск собрался запасным.
			if hot {
				ydev, _ := e.memberDev(p.Pool, t.spec.Index, p.Disk.ID, p.Integrity, knames)
				if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, t.arr.node, "--replace", t.xdev, "--with", ydev); err != nil {
					return fail(err)
				}
			}
			continue
		}
		dev := nodes[t.spec.Index+1]
		if p.Resume {
			// Раздел от прерванной попытки: закрыть его dm-integrity и
			// обнулить остатки суперблоков.
			if p.Integrity {
				name := IntegrityName(p.Pool, t.spec.Index, p.Disk.ID)
				if _, err := os.Stat(filepath.Join(e.Paths.Dev, "mapper", name)); err == nil {
					if _, err := e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, "close", name); err != nil {
						return fail(err)
					}
				}
			}
			if err := e.zeroStale(ctx, j, dev, t.spec.SliceBytes); err != nil {
				return fail(err)
			}
		}
		if p.Integrity {
			if dev, err = e.integrity(ctx, j, dev, IntegrityName(p.Pool, t.spec.Index, p.Disk.ID), p.IntegrityMode); err != nil {
				return fail(err)
			}
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, t.arr.node, "--add", dev); err != nil {
			return fail(err)
		}
		if hot {
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, t.arr.node, "--replace", t.xdev, "--with", dev); err != nil {
				return fail(err)
			}
		}
	}
	res.Commands = j.cmds
	return res, nil
}

// ReplaceFinish — после копирования на замену: старый диск (отказавший вне
// слотов) удаляется из массивов, его dm-integrity закрывается, разметка
// пула стирается (иначе ранняя сборка udev может собрать по его суперблоку
// устаревший массив). Пока старый диск — исправный участник, отказ.
func (e *Executor) ReplaceFinish(ctx context.Context, p ReplaceParams) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Pool: p.Pool}
	if p.Old == nil {
		return res, refuse("нет старого диска")
	}
	if !e.allowed(p.Old.ID) {
		return res, refuse("старый диск %s не входит в destructive_allow_ids", p.Old.ID)
	}
	if err := e.checkPoolAllowed(ctx, p.Pool); err != nil {
		return res, err
	}
	arrs, err := e.arrays(ctx, p.Pool)
	if err != nil {
		return res, refuse("массивы пула: %v", err)
	}
	st, err := e.mdstat()
	if err != nil {
		return res, refuse("/proc/mdstat: %v", err)
	}
	knames, err := e.diskKNames()
	if err != nil {
		return res, refuse("снимок дисков: %v", err)
	}
	type rm struct{ md, dev string }
	var rms []rm
	for _, spec := range p.Arrays {
		a, ok := e.arrayByIndex(arrs, spec.Index)
		if !ok {
			return res, refuse("массив %d пула %s не найден", spec.Index, p.Pool)
		}
		ms := st[a.kname]
		_, ykn := e.memberDev(p.Pool, spec.Index, p.Disk.ID, p.Integrity, knames)
		if mdRole(ms, ykn) != "member" || ms.Action != "" || ms.InSync != ms.RaidDevices {
			return res, refuse("массив %s: копирование на %s не завершено (%s %s)", a.name, p.Disk.ID, ms.Status, ms.Action)
		}
		xdev, xkn := e.memberDev(p.Pool, spec.Index, p.Old.ID, p.Integrity, knames)
		switch mdRole(ms, xkn) {
		case "member", "replacing", "spare":
			return res, refuse("массив %s: старый диск %s ещё в массиве", a.name, p.Old.ID)
		case "faulty":
			rms = append(rms, rm{a.node, xdev})
		}
	}
	fail := func(err error) (Result, error) {
		res.Commands = j.cmds
		return res, fmt.Errorf("вывод старого диска %s из пула %s: %w", p.Old.ID, p.Pool, err)
	}
	for _, r := range rms {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, r.md, "--remove", r.dev); err != nil {
			return fail(err)
		}
	}
	e.settle(ctx, j)
	if p.Integrity {
		for _, spec := range p.Arrays {
			name := IntegrityName(p.Pool, spec.Index, p.Old.ID)
			if _, err := os.Stat(filepath.Join(e.Paths.Dev, "mapper", name)); err != nil {
				continue
			}
			var err error
			for try := 0; try < 5; try++ {
				e.settle(ctx, j)
				if _, err = e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, "close", name); err == nil {
					break
				}
			}
			if err != nil {
				return fail(err)
			}
		}
	}
	if k := knames[p.Old.ID]; k != "" {
		if err := e.wipePoolDisk(ctx, j, p.Pool, k); err != nil {
			return fail(err)
		}
	}
	res.Commands = j.cmds
	return res, nil
}

// wipePoolDisk стирает разметку пула с диска k: только если каждый раздел
// с меткой dn-<пул>-* и ни раздел, ни диск не заняты.
func (e *Executor) wipePoolDisk(ctx context.Context, j *journal, pool, k string) error {
	if !knameRe.MatchString(k) {
		return fmt.Errorf("имя ядра %q", k)
	}
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, k)
	ents, _ := os.ReadDir(dir)
	var parts []string
	prefix := PartLabelPrefix(pool)
	for _, en := range ents {
		n, err := e.readInt(filepath.Join(dir, en.Name(), "partition"))
		if err != nil {
			continue
		}
		if hs, _ := os.ReadDir(filepath.Join(dir, en.Name(), "holders")); len(hs) > 0 {
			return fmt.Errorf("раздел %s занят", en.Name())
		}
		if l := e.partLabel(ctx, k, en.Name(), int(n)); len(l) < len(prefix) || l[:len(prefix)] != prefix {
			return fmt.Errorf("раздел %s без метки %s*", en.Name(), prefix)
		}
		parts = append(parts, en.Name())
	}
	if hs, _ := os.ReadDir(filepath.Join(dir, "holders")); len(hs) > 0 {
		return fmt.Errorf("диск %s занят", k)
	}
	if len(parts) == 0 {
		return nil
	}
	for _, pt := range parts {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", filepath.Join(e.Paths.Dev, pt)); err != nil {
			return err
		}
	}
	disk := filepath.Join(e.Paths.Dev, k)
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Sgdisk, "--zap-all", disk); err != nil {
		return err
	}
	if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", disk); err != nil {
		return err
	}
	e.settle(ctx, j)
	return nil
}

// growParts — разделы на диске пула p.Disk для роста после замены (Grow):
// разделы его прежних слоёв 1..m на месте с метками пула; новые (номера
// idx+1 подряд над m) — хвостами за последним, раздел от прерванной
// попытки (та же метка и геометрия, без держателей) переиспользуется.
// Только проверки; создание — makeTail.
func (e *Executor) growParts(ctx context.Context, p AddDiskParams, idx []int, slices []int64) ([]tailPart, error) {
	knames, err := e.diskKNames()
	if err != nil {
		return nil, refuse("снимок дисков: %v", err)
	}
	k := knames[p.Disk.ID]
	if k == "" || !knameRe.MatchString(k) {
		return nil, refuse("диск %s не найден однозначно", p.Disk.ID)
	}
	if !e.allowed(p.Disk.ID) {
		return nil, refuse("диск %s не входит в destructive_allow_ids", p.Disk.ID)
	}
	size, err := e.readInt(filepath.Join(e.Paths.Disks.SysClassBlock, k, "size"))
	if err != nil || size*512 != p.Disk.SizeBytes {
		return nil, refuse("диск %s: размер не совпадает с планом", p.Disk.ID)
	}
	if len(idx) == 0 {
		return nil, nil
	}
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, k)
	ents, _ := os.ReadDir(dir)
	nums := map[int]string{}
	for _, en := range ents {
		if n, err := e.readInt(filepath.Join(dir, en.Name(), "partition")); err == nil {
			nums[int(n)] = en.Name()
		}
	}
	first := idx[0] + 1
	for n := range nums {
		if n > first+len(idx)-1 {
			return nil, refuse("диск %s: лишний раздел %d", p.Disk.ID, n)
		}
	}
	for n := 1; n < first; n++ {
		pk, ok := nums[n]
		if !ok {
			return nil, refuse("диск %s: нет раздела %d нижнего слоя", p.Disk.ID, n)
		}
		if l := e.partLabel(ctx, k, pk, n); l != PartLabel(p.Pool, n-1) {
			return nil, refuse("раздел %s: метка %q, ожидалась %q", pk, l, PartLabel(p.Pool, n-1))
		}
	}
	pk := nums[first-1]
	st, err1 := e.readInt(filepath.Join(dir, pk, "start"))
	sz, err2 := e.readInt(filepath.Join(dir, pk, "size"))
	if first == 1 || err1 != nil || err2 != nil {
		return nil, refuse("диск %s: геометрия раздела %d не читается", p.Disk.ID, first-1)
	}
	start := (st + sz) * 512
	var out []tailPart
	for n, i := range idx {
		if i+1 != first+n {
			return nil, refuse("слои роста не подряд: %v", idx)
		}
		t := tailPart{kname: k, num: i + 1, start: start, size: slices[n], label: PartLabel(p.Pool, i)}
		if t.start%Align != 0 || t.start+t.size > p.Disk.SizeBytes-Align {
			return nil, refuse("диск %s: раздел слоя %d не помещается", p.Disk.ID, i)
		}
		if pk, ok := nums[t.num]; ok {
			base := filepath.Join(dir, pk)
			s1, e1 := e.readInt(filepath.Join(base, "start"))
			s2, e2 := e.readInt(filepath.Join(base, "size"))
			if e1 != nil || e2 != nil || s1*512 != t.start || s2*512 != t.size {
				return nil, refuse("раздел %s: геометрия не совпадает с планом", pk)
			}
			if l := e.partLabel(ctx, k, pk, t.num); l != t.label {
				return nil, refuse("раздел %s: метка %q, ожидалась %q", pk, l, t.label)
			}
			if hs, _ := os.ReadDir(filepath.Join(base, "holders")); len(hs) > 0 {
				return nil, refuse("раздел %s занят", pk)
			}
			t.exists = true
		}
		out = append(out, t)
		start += t.size
	}
	return out, nil
}
