package home

// Новый верхний слой пула при добавлении диска: SHR/SHR-2
// диском большего размера (md raid1 на хвостах больших дисков) и JBOD +1
// (слой single на новом диске).
//
// Порядок: раздел на новом диске (вместе с разделами расширяемых массивов)
// и на свободном хвосте каждого прежнего участника → dm-integrity → md
// (кроме single) → pvcreate → vgextend. Рост LV/Btrfs — home.pool.grow.
//
// Сброс посреди операции разрешает Assemble по ожиданию:
// слой принят, если его PV входит в VG пула; иначе он откатывается (метка
// LVM, md, dm-integrity, хвостовые разделы прежних дисков), а дозавершение
// создаёт его заново.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gamayun-nas/internal/disks"
)

// tailPart — раздел нового слоя на прежнем диске пула.
type tailPart struct {
	kname  string
	num    int   // номер раздела = индекс слоя + 1
	start  int64 // байты
	size   int64
	label  string
	exists bool // раздел уже есть (дозавершение) — переиспользуется
}

func tailKey(i int, id string) string { return strconv.Itoa(i) + "/" + id }

// checkNewArrays — проверки новых слоёв до любой записи: массива и
// отображений dm-integrity слоя ещё нет; каждый прежний участник — диск
// пула из allowlist с тем же размером, разделы нижних слоёв 1..i на месте
// с метками пула, после них — только свободное место (или раздел слоя от
// прерванной попытки: та же метка, геометрия, без держателей).
func (e *Executor) checkNewArrays(ctx context.Context, p AddDiskParams, arrs []poolArray) (map[string]tailPart, error) {
	out := map[string]tailPart{}
	if len(p.NewArrays) == 0 {
		return out, nil
	}
	poolIDs := map[string]bool{}
	pvs, err := e.pvList(ctx, VGName(p.Pool))
	if err != nil {
		return nil, refuse("тома пула %s: %v", p.Pool, err)
	}
	for _, pv := range pvs {
		ids, err := e.diskIDsUnder(pv)
		if err != nil {
			return nil, refuse("диски пула %s под %s: %v", p.Pool, pv, err)
		}
		for _, id := range ids {
			poolIDs[id] = true
		}
	}
	maps, err := e.integrityMaps(ctx)
	if err != nil {
		return nil, refuse("dmsetup: %v", err)
	}
	mapped := map[string]bool{}
	for _, m := range maps {
		mapped[m.Name] = true
	}
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return nil, refuse("снимок дисков: %v", err)
	}
	for _, na := range p.NewArrays {
		if _, ok := e.arrayByIndex(arrs, na.Index); ok {
			return nil, refuse("массив %s уже существует", MDName(p.Pool, na.Index))
		}
		for _, m := range na.Members {
			if p.Integrity && mapped[IntegrityName(p.Pool, na.Index, m.ID)] {
				return nil, refuse("отображение %s уже открыто", IntegrityName(p.Pool, na.Index, m.ID))
			}
			if m.ID == p.Disk.ID {
				continue
			}
			if !e.allowed(m.ID) {
				return nil, refuse("диск %s не входит в destructive_allow_ids", m.ID)
			}
			if !poolIDs[m.ID] {
				return nil, refuse("диск %s не входит в пул %s", m.ID, p.Pool)
			}
			var found []disks.Disk
			for _, d := range snap.Disks {
				if d.ID == m.ID {
					found = append(found, d)
				}
			}
			if len(found) != 1 || found[0].SizeBytes == nil || *found[0].SizeBytes != m.SizeBytes || !knameRe.MatchString(found[0].KernelName) {
				return nil, refuse("диск %s: не найден однозначно или размер не совпадает с планом", m.ID)
			}
			t, err := e.checkTail(ctx, p.Pool, found[0].KernelName, m.SizeBytes, na)
			if err != nil {
				return nil, refuse("диск %s: %v", m.ID, err)
			}
			out[tailKey(na.Index, m.ID)] = t
		}
	}
	return out, nil
}

// checkTail — место раздела слоя na на диске k: сразу за разделом слоя
// na.Index-1, в пределах диска (1 МиБ в конце — резервная GPT).
func (e *Executor) checkTail(ctx context.Context, pool, k string, diskSize int64, na NewArray) (tailPart, error) {
	t := tailPart{kname: k, num: na.Index + 1, size: na.SliceBytes, label: PartLabel(pool, na.Index)}
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, k)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return t, err
	}
	nums := map[int]string{}
	for _, en := range ents {
		n, err := e.readInt(filepath.Join(dir, en.Name(), "partition"))
		if err != nil {
			continue
		}
		nums[int(n)] = en.Name()
	}
	for n := range nums {
		if n > t.num {
			return t, fmt.Errorf("лишний раздел %d", n)
		}
	}
	for n := 1; n < t.num; n++ {
		pk, ok := nums[n]
		if !ok {
			return t, fmt.Errorf("нет раздела %d нижнего слоя", n)
		}
		if l := e.partLabel(ctx, k, pk, n); l != PartLabel(pool, n-1) {
			return t, fmt.Errorf("раздел %s: метка %q, ожидалась %q", pk, l, PartLabel(pool, n-1))
		}
	}
	prev := filepath.Join(dir, nums[t.num-1])
	st, err1 := e.readInt(filepath.Join(prev, "start"))
	sz, err2 := e.readInt(filepath.Join(prev, "size"))
	if t.num == 1 || err1 != nil || err2 != nil {
		return t, fmt.Errorf("геометрия раздела %d не читается", t.num-1)
	}
	t.start = (st + sz) * 512
	if t.start%Align != 0 || t.start+t.size > diskSize-Align {
		return t, fmt.Errorf("раздел слоя %d (%d+%d) не помещается на диск %d", na.Index, t.start, t.size, diskSize)
	}
	if pk, ok := nums[t.num]; ok {
		base := filepath.Join(dir, pk)
		st, err1 := e.readInt(filepath.Join(base, "start"))
		sz, err2 := e.readInt(filepath.Join(base, "size"))
		if err1 != nil || err2 != nil || st*512 != t.start || sz*512 != t.size {
			return t, fmt.Errorf("раздел %s: геометрия не совпадает с планом", pk)
		}
		if l := e.partLabel(ctx, k, pk, t.num); l != t.label {
			return t, fmt.Errorf("раздел %s: метка %q, ожидалась %q", pk, l, t.label)
		}
		if hs, _ := os.ReadDir(filepath.Join(base, "holders")); len(hs) > 0 {
			return t, fmt.Errorf("раздел %s занят", pk)
		}
		t.exists = true
	}
	return t, nil
}

// partLabel — метка GPT раздела num диска k. Раздел, добавленный в ядро
// через partx (диск занят — ядро не перечитывает таблицу), PARTNAME в
// uevent не имеет — тогда метка читается из GPT (sgdisk -i).
func (e *Executor) partLabel(ctx context.Context, k, pk string, num int) string {
	if l := e.partName(filepath.Join(e.Paths.Disks.SysClassBlock, k, pk)); l != "" {
		return l
	}
	out, err := e.query(ctx, e.Tools.Sgdisk, "-i", strconv.Itoa(num), filepath.Join(e.Paths.Dev, k))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "Partition name:"); ok {
			return strings.Trim(strings.TrimSpace(v), "'")
		}
	}
	return ""
}

// makeTail создаёт (или переиспользует) раздел слоя на прежнем диске и
// обнуляет его начало и конец. Диск занят разделами нижних слоёв: ядро
// таблицу не перечитывает, раздел добавляется partx --add (BLKPG).
func (e *Executor) makeTail(ctx context.Context, j *journal, t tailPart) (string, error) {
	if !t.exists {
		lbs, err := e.readInt(filepath.Join(e.Paths.Disks.SysClassBlock, t.kname, "queue", "logical_block_size"))
		if err != nil || lbs <= 0 || Align%lbs != 0 {
			lbs = 512
		}
		n := strconv.Itoa(t.num)
		disk := filepath.Join(e.Paths.Dev, t.kname)
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Sgdisk, "-n", fmt.Sprintf("%s:%d:%d", n, t.start/lbs, (t.start+t.size)/lbs-1),
			"-t", n+":FD00", "-c", n+":"+t.label, disk); err != nil {
			return "", err
		}
		e.settle(ctx, j)
		if e.partKName(t.kname, t.num) == "" {
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Partx, "--add", "--nr", n, disk); err != nil {
				return "", err
			}
			e.settle(ctx, j)
		}
	}
	var node string
	if err := e.waitFor(ctx, fmt.Sprintf("раздел %d диска %s", t.num, t.kname), func() bool {
		pk := e.partKName(t.kname, t.num)
		if pk == "" {
			return false
		}
		node = filepath.Join(e.Paths.Dev, pk)
		_, err := os.Stat(node)
		return err == nil
	}); err != nil {
		return "", fmt.Errorf("раздел %d на %s не появился: %w", t.num, t.kname, err)
	}
	if err := e.zeroStale(ctx, j, node, t.size); err != nil {
		return "", err
	}
	return node, nil
}

// createNewArrays — новые слои после расширения нижних (nodes — разделы
// нового диска по номерам).
func (e *Executor) createNewArrays(ctx context.Context, j *journal, p AddDiskParams, nodes map[int]string, tails map[string]tailPart) error {
	vg := VGName(p.Pool)
	for _, na := range p.NewArrays {
		var members []string
		for _, m := range na.Members {
			var dev string
			if m.ID == p.Disk.ID {
				dev = nodes[na.Index+1]
				if p.Resume {
					// Раздел от прерванной попытки: остатки суперблоков.
					if err := e.zeroStale(ctx, j, dev, na.SliceBytes); err != nil {
						return err
					}
				}
			} else {
				var err error
				if dev, err = e.makeTail(ctx, j, tails[tailKey(na.Index, m.ID)]); err != nil {
					return err
				}
			}
			if p.Integrity {
				var err error
				if dev, err = e.integrity(ctx, j, dev, IntegrityName(p.Pool, na.Index, m.ID), p.IntegrityMode); err != nil {
					return err
				}
			}
			members = append(members, dev)
		}
		pv := members[0]
		if na.Level != LevelSingle {
			md := filepath.Join(e.Paths.Dev, "md", MDName(p.Pool, na.Index))
			args := []string{"--create", md, "--run", "--level=" + strings.TrimPrefix(na.Level, "raid"),
				"--raid-devices=" + strconv.Itoa(len(members)), "--metadata=1.2", "--bitmap=" + mdBitmap(na.Level),
				"--name=" + MDName(p.Pool, na.Index)}
			if _, err := e.run(ctx, j, longTimeout, e.Tools.Mdadm, append(args, members...)...); err != nil {
				return err
			}
			e.settle(ctx, j)
			if err := e.waitFor(ctx, md, func() bool { _, err := os.Stat(md); return err == nil }); err != nil {
				return fmt.Errorf("%s не появился: %w", md, err)
			}
			pv = md
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "pvcreate", "-q", "-y", pv); err != nil {
			return err
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.LVM, "vgextend", "-q", vg, pv); err != nil {
			return err
		}
	}
	return nil
}

// pvInVG — входит ли устройство dev в VG; unknown — в VG есть PV, которого
// нет в системе (не собран md слоя): решать по такому состоянию нельзя.
func (e *Executor) pvInVG(ctx context.Context, vg, dev string) (in, unknown bool, err error) {
	pvs, err := e.pvList(ctx, vg)
	if err != nil {
		return false, false, err
	}
	want := ""
	if dev != "" {
		if r, err := filepath.EvalSymlinks(dev); err == nil {
			want = r
		}
	}
	for _, pv := range pvs {
		r, err := filepath.EvalSymlinks(pv)
		if err != nil {
			unknown = true
			continue
		}
		if want != "" && r == want {
			in = true
		}
	}
	return in, unknown, nil
}

// assembleNewLayers — сверка ожидаемых новых слоёв после сбоя:
// открыть dm-integrity участников, собрать md; PV слоя в VG —
// слой принят (PendingAccepted), иначе — откат слоя. Если в VG есть
// неизвестный PV — ничего не трогать (PendingErrors).
func (e *Executor) assembleNewLayers(ctx context.Context, j *journal, p AssembleParams, res *AssembleResult) {
	if p.Pending == nil || len(p.Pending.New) == 0 {
		return
	}
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		res.PendingErrors = append(res.PendingErrors, "новый слой: снимок дисков: "+err.Error())
		return
	}
	knames := map[string]string{}
	for _, d := range snap.Disks {
		knames[d.ID] = d.KernelName
	}
	vg := VGName(p.Pool)
	lowerOK := true
	for _, na := range p.Pending.New {
		var devs []string
		for _, m := range na.Members {
			k, ok := knames[m.ID]
			if !ok {
				continue
			}
			pk := e.partKName(k, na.Index+1)
			if pk == "" {
				continue
			}
			dev := filepath.Join(e.Paths.Dev, pk)
			if p.Integrity {
				name := IntegrityName(p.Pool, na.Index, m.ID)
				node := filepath.Join(e.Paths.Dev, "mapper", name)
				if _, err := os.Stat(node); err != nil {
					if _, err := e.run(ctx, j, shortTimeout, e.Tools.Integritysetup, integrityReopenArgs(p.IntegrityMode, dev, name)...); err != nil {
						continue // суперблока нет: integrity не создавался
					}
					if err := e.waitFor(ctx, node, func() bool { _, err := os.Stat(node); return err == nil }); err != nil {
						continue
					}
				}
				dev = node
			}
			devs = append(devs, dev)
		}
		pv := ""
		md := ""
		if na.Level == LevelSingle {
			if len(devs) == 1 {
				pv = devs[0]
			}
		} else {
			md = filepath.Join(e.Paths.Dev, "md", MDName(p.Pool, na.Index))
			if arrs, err := e.arrays(ctx, p.Pool); err == nil {
				if a, ok := e.arrayByIndex(arrs, na.Index); ok && !a.inactive {
					md = a.node
				}
			}
			// Суперблоки md слоя: нет ни у одного участника — сброс до
			// --create, собирать и обнулять нечего.
			var sb []string
			for _, d := range devs {
				if e.mdUUID(ctx, j, d) != "" {
					sb = append(sb, d)
				}
			}
			if _, err := os.Stat(md); err != nil && len(sb) > 0 {
				e.stopPartial(ctx, j, sb)
				// Ошибка сборки — откат ниже.
				e.run(ctx, j, longTimeout, e.Tools.Mdadm, append([]string{"--assemble", "--run", md}, sb...)...)
				e.settle(ctx, j)
			}
			devs = sb
			if r, err := filepath.EvalSymlinks(md); err == nil {
				pv = md
				if st, err := e.mdstat(); err == nil && st[filepath.Base(r)].ReadOnly {
					e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--readwrite", md)
				}
			}
		}
		in, unknown, err := e.pvInVG(ctx, vg, pv)
		switch {
		case err != nil:
			res.PendingErrors = append(res.PendingErrors, fmt.Sprintf("новый слой %d: тома VG: %v", na.Index, err))
			return
		case unknown:
			res.PendingErrors = append(res.PendingErrors, fmt.Sprintf("новый слой %d: в %s есть отсутствующий PV — слой не трогается", na.Index, vg))
			return
		case in && lowerOK:
			res.PendingAccepted = append(res.PendingAccepted, na.Index)
			continue
		case in:
			res.PendingErrors = append(res.PendingErrors, fmt.Sprintf("новый слой %d в VG, а нижний новый слой — нет", na.Index))
			return
		}
		lowerOK = false
		e.rollbackNewLayer(ctx, j, p, na, pv, md, devs, knames, res)
	}
}

// rollbackNewLayer убирает непринятый новый слой: метку LVM (PV не в VG),
// md, суперблоки md участников, dm-integrity, хвостовые разделы прежних
// дисков (только с меткой слоя и без держателей). Разделы нового диска
// остаются: их переиспользует дозавершение или стирает releasePending.
func (e *Executor) rollbackNewLayer(ctx context.Context, j *journal, p AssembleParams, na NewArray, pv, md string, devs []string, knames map[string]string, res *AssembleResult) {
	errf := func(format string, a ...any) {
		res.PendingErrors = append(res.PendingErrors, fmt.Sprintf("откат нового слоя %d: ", na.Index)+fmt.Sprintf(format, a...))
	}
	if pv != "" {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", pv); err != nil {
			errf("%v", err)
			return
		}
	}
	if md != "" {
		if _, err := os.Stat(md); err == nil {
			if _, err := e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--stop", md); err != nil {
				errf("%v", err)
				return
			}
			e.settle(ctx, j)
		}
		for _, d := range devs {
			// Нет суперблока — не ошибка.
			e.run(ctx, j, shortTimeout, e.Tools.Mdadm, "--zero-superblock", d)
		}
	}
	if p.Integrity {
		for _, m := range na.Members {
			name := IntegrityName(p.Pool, na.Index, m.ID)
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
				errf("%v", err)
				return
			}
		}
	}
	for _, m := range na.Members {
		if m.ID == p.Pending.DiskID {
			continue
		}
		k := knames[m.ID]
		if k == "" || !knameRe.MatchString(k) || !e.allowed(m.ID) {
			continue
		}
		pk := e.partKName(k, na.Index+1)
		if pk == "" {
			continue
		}
		if l := e.partLabel(ctx, k, pk, na.Index+1); l != PartLabel(p.Pool, na.Index) {
			errf("раздел %s: метка %q", pk, l)
			continue
		}
		if hs, _ := os.ReadDir(filepath.Join(e.Paths.Disks.SysClassBlock, k, pk, "holders")); len(hs) > 0 {
			errf("раздел %s занят", pk)
			continue
		}
		disk := filepath.Join(e.Paths.Dev, k)
		n := strconv.Itoa(na.Index + 1)
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", filepath.Join(e.Paths.Dev, pk)); err != nil {
			errf("%v", err)
			continue
		}
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Sgdisk, "-d", n, disk); err != nil {
			errf("%v", err)
			continue
		}
		// Ядро могло уже убрать раздел (перечитало таблицу) — не ошибка.
		e.run(ctx, j, shortTimeout, e.Tools.Partx, "--delete", "--nr", n, disk)
		e.settle(ctx, j)
	}
}
