package home

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gamayun-nas/internal/disks"
)

// OpReleaseDisk — «освободить диск»: стереть разметку,
// оставшуюся от пула, которого больше нет (сбой отката создания, удалённая
// запись) — иначе диск не свободен, а DELETE пула отвечает 404.
const OpReleaseDisk = "home.disk.release"

// ReleaseParams — диск и пулы, которые gamayund знает (их диски не трогаются).
type ReleaseParams struct {
	Disk       DiskRef  `json:"disk"`
	KnownPools []string `json:"known_pools"`
}

// Validate проверяет параметры.
func (p ReleaseParams) Validate() error {
	if err := p.Disk.validate(); err != nil {
		return err
	}
	for _, n := range p.KnownPools {
		if !ValidPoolName(n) {
			return fmt.Errorf("недопустимое имя пула %q", n)
		}
	}
	return nil
}

// ReleaseDisk стирает разметку диска, если все его разделы — от неизвестного
// пула (метка dn-<пул>-<i>, пул не в KnownPools) и ничем не заняты. Отказ
// до любой записи: диск вне allowlist, не найден, размер не совпадает,
// системная роль, держатели, чужая/старая метка («dn»), раздел известного пула.
func (e *Executor) ReleaseDisk(ctx context.Context, p ReleaseParams) (Result, error) {
	// Разделы заняты стеком пула, которого нет в базе (сбой helper посреди
	// создания): сначала разобрать этот пул целиком — Destroy находит его
	// слои по имени и диски по меткам.
	if orphan := e.orphanPool(p); orphan != "" {
		r, err := e.Destroy(ctx, PoolParams{Pool: orphan})
		if err != nil {
			return r, fmt.Errorf("разборка остатков пула %s: %w", orphan, err)
		}
		r.Warnings = append(r.Warnings, "разобраны остатки пула "+orphan)
		return r, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &journal{}
	res := Result{Commands: []string{}}
	if !e.allowed(p.Disk.ID) {
		return res, refuse("диск %s не входит в destructive_allow_ids", p.Disk.ID)
	}
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return res, refuse("снимок дисков: %v", err)
	}
	var d *disks.Disk
	for i := range snap.Disks {
		if snap.Disks[i].ID == p.Disk.ID {
			d = &snap.Disks[i]
		}
	}
	if d == nil || !knameRe.MatchString(d.KernelName) {
		return res, refuse("диск %s не найден", p.Disk.ID)
	}
	if d.SizeBytes == nil || *d.SizeBytes != p.Disk.SizeBytes {
		return res, refuse("диск %s: размер не совпадает", p.Disk.ID)
	}
	known := map[string]bool{}
	for _, n := range p.KnownPools {
		known[n] = true
	}
	k := d.KernelName
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, k)
	if hs, _ := os.ReadDir(filepath.Join(dir, "holders")); len(hs) > 0 {
		return res, refuse("диск %s занят", k)
	}
	ents, _ := os.ReadDir(dir)
	var parts []string
	for _, en := range ents {
		if _, err := os.Stat(filepath.Join(dir, en.Name(), "partition")); err != nil {
			continue
		}
		if hs, _ := os.ReadDir(filepath.Join(dir, en.Name(), "holders")); len(hs) > 0 {
			return res, refuse("раздел %s занят (открыт integrity или в массиве)", en.Name())
		}
		label := e.partName(filepath.Join(dir, en.Name()))
		rest, ok := strings.CutPrefix(label, "dn-")
		i := strings.LastIndex(rest, "-")
		if !ok || i <= 0 {
			return res, refuse("раздел %s с меткой %q — не от пула gamayun-nas нового формата; удалите пул или очистите диск вручную", en.Name(), label)
		}
		if pool := rest[:i]; known[pool] {
			return res, refuse("раздел %s принадлежит пулу %s — удалите пул", en.Name(), pool)
		}
		parts = append(parts, en.Name())
	}
	if len(parts) == 0 {
		return res, refuse("на диске %s нет разделов пула — освобождать нечего", k)
	}
	if d.SystemRole != disks.SummaryUnknown && len(d.SystemRoles) != 0 {
		return res, refuse("диск %s: системная роль %v", k, d.SystemRoles)
	}
	fail := func(err error) (Result, error) {
		res.Commands = j.cmds
		return res, fmt.Errorf("освобождение диска %s прервано: %w", k, err)
	}
	for _, pt := range parts {
		if _, err := e.run(ctx, j, shortTimeout, e.Tools.Wipefs, "-q", "-a", filepath.Join(e.Paths.Dev, pt)); err != nil {
			return fail(err)
		}
	}
	for _, args := range [][]string{{e.Tools.Sgdisk, "--zap-all"}, {e.Tools.Wipefs, "-q", "-a"}} {
		if _, err := e.run(ctx, j, shortTimeout, args[0], append(args[1:], filepath.Join(e.Paths.Dev, k))...); err != nil {
			return fail(err)
		}
	}
	e.settle(ctx, j)
	res.Commands = j.cmds
	return res, nil
}

// orphanPool — имя неизвестного пула, если разделы диска заняты и все
// помечены одним таким пулом; иначе "" (обычный путь ReleaseDisk).
func (e *Executor) orphanPool(p ReleaseParams) string {
	if !e.allowed(p.Disk.ID) {
		return ""
	}
	snap, err := disks.Observe(e.Paths.Disks)
	if err != nil {
		return ""
	}
	k := ""
	for _, d := range snap.Disks {
		if d.ID == p.Disk.ID && d.SizeBytes != nil && *d.SizeBytes == p.Disk.SizeBytes {
			k = d.KernelName
		}
	}
	if k == "" {
		return ""
	}
	known := map[string]bool{}
	for _, n := range p.KnownPools {
		known[n] = true
	}
	dir := filepath.Join(e.Paths.Disks.SysClassBlock, k)
	ents, _ := os.ReadDir(dir)
	pool, held := "", false
	for _, en := range ents {
		if _, err := os.Stat(filepath.Join(dir, en.Name(), "partition")); err != nil {
			continue
		}
		rest, ok := strings.CutPrefix(e.partName(filepath.Join(dir, en.Name())), "dn-")
		i := strings.LastIndex(rest, "-")
		if !ok || i <= 0 || known[rest[:i]] || (pool != "" && pool != rest[:i]) {
			return ""
		}
		pool = rest[:i]
		if hs, _ := os.ReadDir(filepath.Join(dir, en.Name(), "holders")); len(hs) > 0 {
			held = true
		}
	}
	if held && ValidPoolName(pool) {
		return pool
	}
	return ""
}

// ReleaseDisk (gamayund): диск не из записей пулов; известные пулы передаются
// helper, чтобы он не тронул их разделы.
func (s *Service) ReleaseDisk(ctx context.Context, id string) (Result, error) {
	snap, err := s.observe(ctx)
	if err != nil {
		return Result{}, err
	}
	var ref *DiskRef
	for _, d := range snap.Disks {
		if d.ID == id && d.SizeBytes != nil {
			ref = &DiskRef{ID: id, SizeBytes: *d.SizeBytes}
		}
	}
	if ref == nil {
		return Result{}, wrap(ErrNotFound, "диск %s", id)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pools")
	if err != nil {
		return Result{}, err
	}
	var known []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		known = append(known, n)
	}
	rows.Close()
	for _, n := range known {
		if rec, err := s.getRecord(ctx, n); err == nil && rec.hasDisk(id) {
			return Result{}, wrap(ErrConflict, "диск %s входит в пул %s — удалите пул или замените диск", id, n)
		}
	}
	var r Result
	err = s.helper.Call(ctx, OpReleaseDisk, ReleaseParams{Disk: *ref, KnownPools: known}, &r)
	return r, err
}
