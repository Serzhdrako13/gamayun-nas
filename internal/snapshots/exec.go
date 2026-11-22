// Package snapshots — снапшоты Btrfs общих папок: операции
// gamayun-helper (root) — список, создание, удаление, просмотр и восстановление
// копией — и сервис gamayund — расписание, ротация, события.
//
// Раскладка: общая папка — подтом <пул>/shares/<папка>; снапшоты — подтомы
// только для чтения <пул>/.snapshots/<папка>/@GMT-ГГГГ.ММ.ДД-ЧЧ.ММ.СС (UTC,
// формат vfs_shadow_copy2 Samba). Снапшоты по расписанию отмечены пустым
// файлом <пул>/.snapshots/<папка>/.auto/<имя>: только они участвуют в
// ротации; снапшот без отметки (ручной или неизвестный) автоматически не
// удаляется никогда.
package snapshots

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/storage/home"
)

// Операции gamayun-helper.
const (
	OpList    = "snapshot.list"
	OpCreate  = "snapshot.create"
	OpDelete  = "snapshot.delete"
	OpBrowse  = "snapshot.browse"
	OpRestore = "snapshot.restore"
)

// AutoDir — каталог отметок снапшотов по расписанию (в каталоге снапшотов папки).
const AutoDir = ".auto"

// Режимы каталогов снапшотов (root). .snapshots — только проход (без
// списка папок); каталог папки — чтение списка: Samba (vfs_shadow_copy2)
// перечисляет снапшоты и открывает файлы в них от имени пользователя SMB,
// права внутри снапшота — те же, что у папки. Запись — только root:
// пользователь не может удалить или изменить снапшот (подтомы -r).
const (
	SnapRootMode  os.FileMode = 0o711
	SnapShareMode os.FileMode = 0o755
	autoMode      os.FileMode = 0o700
)

// Пределы.
const (
	MaxBrowseEntries = 2000
	MaxPathLen       = 1024
	MaxMinFreePct    = 50
)

var (
	poolRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	nameRe = regexp.MustCompile(`^@GMT-[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9]{2}\.[0-9]{2}\.[0-9]{2}$`)
)

// Ошибки — ошибки nas/home: helper переводит их в коды refused/not_found/
// conflict/invalid_params, API — в 400/404/409.
var (
	ErrRefused  = home.ErrRefused
	ErrNotFound = nas.ErrNotFound
	ErrConflict = nas.ErrConflict
	ErrInvalid  = nas.ErrInvalid
)

// ValidName — имя снапшота в формате @GMT-ГГГГ.ММ.ДД-ЧЧ.ММ.СС.
func ValidName(s string) bool {
	if !nameRe.MatchString(s) {
		return false
	}
	_, err := time.Parse(nas.SnapshotGoLayout, s)
	return err == nil
}

// NameTime — время снапшота по имени (UTC).
func NameTime(s string) (time.Time, bool) {
	if !nameRe.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(nas.SnapshotGoLayout, s)
	return t, err == nil
}

// ValidShare — общая папка или служба домашних папок.
func ValidShare(s string) bool { return nas.ValidShare(s) }

func validPoolShare(pool, share string) error {
	if !poolRe.MatchString(pool) {
		return errors.New("недопустимое имя пула")
	}
	if !ValidShare(share) {
		return errors.New("недопустимое имя папки")
	}
	return nil
}

// ShareParams — snapshot.list.
type ShareParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
}

// Validate проверяет параметры.
func (p ShareParams) Validate() error { return validPoolShare(p.Pool, p.Share) }

// CreateParams — snapshot.create. Auto — по расписанию: отметка для ротации;
// при нехватке места (свободно меньше MinFreePct %) или ФС только для
// чтения снапшот пропускается (Skipped), а не ошибка.
type CreateParams struct {
	Pool       string `json:"pool"`
	Share      string `json:"share"`
	Auto       bool   `json:"auto"`
	MinFreePct int    `json:"min_free_pct"`
}

// Validate проверяет параметры.
func (p CreateParams) Validate() error {
	if err := validPoolShare(p.Pool, p.Share); err != nil {
		return err
	}
	if p.MinFreePct < 0 || p.MinFreePct > MaxMinFreePct {
		return fmt.Errorf("min_free_pct — от 0 до %d", MaxMinFreePct)
	}
	return nil
}

// SnapParams — snapshot.delete.
type SnapParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Name  string `json:"name"`
}

// Validate проверяет параметры.
func (p SnapParams) Validate() error {
	if err := validPoolShare(p.Pool, p.Share); err != nil {
		return err
	}
	if !ValidName(p.Name) {
		return errors.New("недопустимое имя снимка")
	}
	return nil
}

// PathParams — snapshot.browse (Path "" — корень) и snapshot.restore (Path
// обязателен: файл или папка внутри снапшота).
type PathParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}

// Validate проверяет параметры.
func (p PathParams) Validate() error {
	if err := (SnapParams{p.Pool, p.Share, p.Name}).Validate(); err != nil {
		return err
	}
	_, err := CleanRel(p.Path)
	return err
}

// CleanRel — относительный путь внутри папки/снапшота (как nas.CleanRel).
func CleanRel(p string) (string, error) {
	if len(p) > MaxPathLen {
		return "", errors.New("путь слишком длинный")
	}
	return nas.CleanRel(p)
}

// Snapshot — снапшот папки.
type Snapshot struct {
	Name string    `json:"name"`
	Time time.Time `json:"time"`
	Auto bool      `json:"auto"` // по расписанию (участвует в ротации)
}

// ListResult — snapshot.list.
type ListResult struct {
	// Subvolume — папка — подтом Btrfs (снапшоты возможны); false — обычный
	// каталог (папка создана до появления снимков) или не Btrfs.
	Subvolume bool       `json:"subvolume"`
	ReadOnly  bool       `json:"read_only"`
	Total     uint64     `json:"total"`
	Avail     uint64     `json:"avail"`
	Snapshots []Snapshot `json:"snapshots"`
}

// Значения CreateResult.Skipped.
const (
	SkipLowSpace = "low_space"
	SkipReadOnly = "read_only"
)

// CreateResult — snapshot.create.
type CreateResult struct {
	Name     string   `json:"name,omitempty"`
	Skipped  string   `json:"skipped,omitempty"`
	FreePct  float64  `json:"free_pct"`
	Warnings []string `json:"warnings,omitempty"`
}

// Entry — элемент каталога в снапшоте.
type Entry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // dir, file, link, other
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// BrowseResult — snapshot.browse.
type BrowseResult struct {
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
	Partial bool    `json:"partial,omitempty"` // показаны не все (предел)
}

// RestoreResult — snapshot.restore.
type RestoreResult struct {
	// Path — восстановленная копия относительно папки.
	Path string `json:"path"`
	// ToRoot — исходного каталога в папке уже нет: копия положена в корень.
	ToRoot  bool  `json:"to_root,omitempty"`
	Files   int   `json:"files"`
	Dirs    int   `json:"dirs"`
	Links   int   `json:"links"`
	Skipped int   `json:"skipped"` // устройства, каналы, сокеты — не копируются
	Bytes   int64 `json:"bytes"`
}

// Executor — операции helper.
type Executor struct {
	Runner    nas.Runner
	MountRoot string
	// Btrfs — путь программы btrfs; пусто — /usr/bin/btrfs.
	Btrfs string
	// Mounted — смонтирован ли пул (nil — nas.IsMountPoint).
	Mounted func(dir string) bool
	// FS — сведения о ФС каталога (nil — nas.ProbeFS).
	FS func(dir string) (nas.FSInfo, error)
	// Now — часы (nil — time.Now).
	Now func() time.Time
	// Local — часовой пояс имён восстановленных копий (nil — time.Local).
	Local *time.Location

	mu sync.Mutex // создание и удаление
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) fs(dir string) (nas.FSInfo, error) {
	if e.FS != nil {
		return e.FS(dir)
	}
	return nas.ProbeFS(dir)
}

func (e *Executor) btrfs() string {
	if e.Btrfs != "" {
		return e.Btrfs
	}
	return "/usr/bin/btrfs"
}

// shareDir — каталог папки на смонтированном пуле.
func (e *Executor) shareDir(pool, share string) (string, error) {
	mp := filepath.Join(e.MountRoot, pool)
	mounted := e.Mounted
	if mounted == nil {
		mounted = nas.IsMountPoint
	}
	if !mounted(mp) {
		return "", fmt.Errorf("%w: пул %s не смонтирован", ErrRefused, pool)
	}
	dir := nas.ShareDir(e.MountRoot, pool, share)
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: каталог папки %s", ErrNotFound, share)
	}
	return dir, nil
}

// snapDir — каталог снапшотов папки; create — создать недостающие каталоги.
// Каждый компонент — настоящий каталог (не ссылка).
func (e *Executor) snapDir(pool, share string, create bool) (string, bool, error) {
	base := filepath.Join(e.MountRoot, pool)
	cur := base
	for _, c := range []struct {
		name string
		mode os.FileMode
	}{{nas.SnapshotsDir, SnapRootMode}, {share, SnapShareMode}} {
		cur = filepath.Join(cur, c.name)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return cur, false, nil
			}
			if err := os.Mkdir(cur, c.mode); err != nil {
				return "", false, err
			}
			if err := os.Chmod(cur, c.mode); err != nil { // umask
				return "", false, err
			}
			continue
		}
		if err != nil {
			return "", false, err
		}
		if !st.IsDir() {
			return "", false, fmt.Errorf("%w: %s — ссылка или не каталог", ErrRefused, cur)
		}
	}
	return cur, true, nil
}

// snapPath — путь существующего снапшота (каталог, не ссылка).
func (e *Executor) snapPath(pool, share, name string) (string, error) {
	dir, ok, err := e.snapDir(pool, share, false)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if st, err := os.Lstat(p); !ok || err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: снимок %s папки %s", ErrNotFound, name, share)
	}
	return p, nil
}

// List — снапшоты папки (новые первыми) и сведения о ФС.
func (e *Executor) List(_ context.Context, p ShareParams) (ListResult, error) {
	if err := p.Validate(); err != nil {
		return ListResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dir, err := e.shareDir(p.Pool, p.Share)
	if err != nil {
		return ListResult{}, err
	}
	fi, err := e.fs(dir)
	if err != nil {
		return ListResult{}, err
	}
	res := ListResult{Subvolume: fi.Btrfs && fi.Subvol, ReadOnly: fi.ReadOnly, Total: fi.Total, Avail: fi.Avail, Snapshots: []Snapshot{}}
	sd, ok, err := e.snapDir(p.Pool, p.Share, false)
	if err != nil || !ok {
		return res, err
	}
	ents, err := os.ReadDir(sd)
	if err != nil {
		return res, err
	}
	for _, de := range ents {
		t, ok := NameTime(de.Name())
		if !ok || !de.IsDir() || de.Type()&fs.ModeSymlink != 0 {
			continue
		}
		_, aerr := os.Lstat(filepath.Join(sd, AutoDir, de.Name()))
		res.Snapshots = append(res.Snapshots, Snapshot{Name: de.Name(), Time: t, Auto: aerr == nil})
	}
	sort.Slice(res.Snapshots, func(i, j int) bool { return res.Snapshots[i].Name > res.Snapshots[j].Name })
	return res, nil
}

// Create — снапшот папки только для чтения (btrfs subvolume snapshot -r).
func (e *Executor) Create(ctx context.Context, p CreateParams) (CreateResult, error) {
	if err := p.Validate(); err != nil {
		return CreateResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, err := e.shareDir(p.Pool, p.Share)
	if err != nil {
		return CreateResult{}, err
	}
	fi, err := e.fs(dir)
	if err != nil {
		return CreateResult{}, err
	}
	if !fi.Btrfs || !fi.Subvol {
		return CreateResult{}, fmt.Errorf("%w: папка %s — обычный каталог, не подтом Btrfs (создана до появления снимков): снимки недоступны", ErrRefused, p.Share)
	}
	var res CreateResult
	if fi.Total > 0 {
		res.FreePct = float64(fi.Avail) * 100 / float64(fi.Total)
	}
	if fi.ReadOnly {
		if p.Auto {
			res.Skipped = SkipReadOnly
			return res, nil
		}
		return res, fmt.Errorf("%w: пул %s только для чтения — снимок не создать", ErrRefused, p.Pool)
	}
	if p.Auto && p.MinFreePct > 0 && fi.Total > 0 && res.FreePct < float64(p.MinFreePct) {
		res.Skipped = SkipLowSpace
		return res, nil
	}
	sd, _, err := e.snapDir(p.Pool, p.Share, true)
	if err != nil {
		return res, err
	}
	name := e.now().UTC().Format(nas.SnapshotGoLayout)
	dst := filepath.Join(sd, name)
	if _, err := os.Lstat(dst); err == nil {
		return res, fmt.Errorf("%w: снимок %s уже есть — повторите через секунду", ErrConflict, name)
	}
	r, err := e.Runner.Run(ctx, 2*time.Minute, nil, e.btrfs(), "subvolume", "snapshot", "-r", dir, dst)
	if err != nil {
		return res, err
	}
	if r.ExitCode != 0 {
		return res, fmt.Errorf("btrfs subvolume snapshot: код %d: %s", r.ExitCode, trim(r.Stderr))
	}
	res.Name = name
	if p.Auto {
		if err := markAuto(sd, name); err != nil {
			// Без отметки снапшот считается ручным и не ротируется — данные
			// не теряются, только место.
			res.Warnings = append(res.Warnings, "отметка снимка по расписанию не записана (снимок не будет удалён ротацией): "+err.Error())
		}
	}
	return res, nil
}

func markAuto(sd, name string) error {
	ad := filepath.Join(sd, AutoDir)
	st, err := os.Lstat(ad)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(ad, autoMode); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("%s — не каталог", ad)
	}
	f, err := os.OpenFile(filepath.Join(ad, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Delete удаляет снапшот (btrfs subvolume delete) и его отметку.
func (e *Executor) Delete(ctx context.Context, p SnapParams) (struct{}, error) {
	if err := p.Validate(); err != nil {
		return struct{}{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.shareDir(p.Pool, p.Share); err != nil && !errors.Is(err, ErrNotFound) {
		return struct{}{}, err // пул не смонтирован
	}
	sp, err := e.snapPath(p.Pool, p.Share, p.Name)
	if err != nil {
		return struct{}{}, err
	}
	r, err := e.Runner.Run(ctx, 5*time.Minute, nil, e.btrfs(), "subvolume", "delete", sp)
	if err != nil {
		return struct{}{}, err
	}
	if r.ExitCode != 0 {
		return struct{}{}, fmt.Errorf("btrfs subvolume delete: код %d: %s", r.ExitCode, trim(r.Stderr))
	}
	os.Remove(filepath.Join(filepath.Dir(sp), AutoDir, p.Name))
	return struct{}{}, nil
}

// Browse — содержимое каталога в снапшоте (один уровень). Обход — через
// os.Root: «..» и ссылки за пределы снапшота не проходят; компоненты пути —
// настоящие каталоги. Снапшот неизменяем, поэтому проверка и чтение не
// разделены гонкой.
func (e *Executor) Browse(_ context.Context, p PathParams) (BrowseResult, error) {
	if err := p.Validate(); err != nil {
		return BrowseResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := e.shareDir(p.Pool, p.Share); err != nil && !errors.Is(err, ErrNotFound) {
		return BrowseResult{}, err
	}
	sp, err := e.snapPath(p.Pool, p.Share, p.Name)
	if err != nil {
		return BrowseResult{}, err
	}
	root, err := os.OpenRoot(sp)
	if err != nil {
		return BrowseResult{}, fmt.Errorf("%w: снимок недоступен", ErrRefused)
	}
	defer root.Close()
	cur := ""
	if p.Path != "" {
		for _, c := range strings.Split(p.Path, "/") {
			cur = path.Join(cur, c)
			st, err := root.Lstat(cur)
			if err != nil || !st.IsDir() {
				return BrowseResult{}, fmt.Errorf("%w: каталог %s в снимке", ErrNotFound, p.Path)
			}
		}
	}
	open := p.Path
	if open == "" {
		open = "."
	}
	f, err := root.Open(open)
	if err != nil {
		return BrowseResult{}, fmt.Errorf("%w: каталог %s в снимке", ErrNotFound, p.Path)
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return BrowseResult{}, err
	}
	res := BrowseResult{Path: p.Path, Entries: []Entry{}}
	for _, de := range ents {
		if len(res.Entries) >= MaxBrowseEntries {
			res.Partial = true
			break
		}
		x := Entry{Name: de.Name(), Type: "other"}
		switch t := de.Type(); {
		case t.IsDir():
			x.Type = "dir"
		case t&fs.ModeSymlink != 0:
			x.Type = "link"
		case t.IsRegular():
			x.Type = "file"
		}
		if info, err := de.Info(); err == nil {
			x.MTime = info.ModTime().UTC()
			if x.Type == "file" {
				x.Size = info.Size()
			}
		}
		res.Entries = append(res.Entries, x)
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i], res.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return a.Name < b.Name
	})
	return res, nil
}

// Restore копирует файл или папку из снапшота в папку рядом с исходным
// местом под новым именем «<имя> (снапшот ГГГГ-ММ-ДД ЧЧ-ММ)» — ничего не
// перезаписывается. Исходного каталога уже нет — копия кладётся в корень
// папки. При ошибке частичная копия удаляется.
func (e *Executor) Restore(ctx context.Context, p PathParams) (RestoreResult, error) {
	if err := p.Validate(); err != nil {
		return RestoreResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if p.Path == "" {
		return RestoreResult{}, fmt.Errorf("%w: укажите файл или папку внутри снимка", ErrInvalid)
	}
	dir, err := e.shareDir(p.Pool, p.Share)
	if err != nil {
		return RestoreResult{}, err
	}
	sp, err := e.snapPath(p.Pool, p.Share, p.Name)
	if err != nil {
		return RestoreResult{}, err
	}
	t, _ := NameTime(p.Name)
	loc := e.Local
	if loc == nil {
		loc = time.Local
	}
	return restoreCopy(ctx, sp, dir, p.Path, t.In(loc))
}

// MaxNameBytes — предел имени файла (NAME_MAX).
const MaxNameBytes = 255

// RestoredName — имя копии: «отчёт (снапшот 2026-11-18 10-00).docx»;
// n > 1 — номер при занятом имени. Расширение файла сохраняется в конце,
// чтобы копия открывалась той же программой. Имя укорачивается до NAME_MAX.
func RestoredName(name string, t time.Time, dir bool, n int) string {
	base, ext := name, ""
	if !dir {
		if x := path.Ext(name); x != "" && x != name && len(x) <= 16 && !strings.HasPrefix(name, ".") {
			base, ext = strings.TrimSuffix(name, x), x
		}
	}
	tag := " (снимок " + t.Format("2006-01-02 15-04")
	if n > 1 {
		tag += fmt.Sprintf(" %d", n)
	}
	tag += ")"
	for len(base)+len(tag)+len(ext) > MaxNameBytes && base != "" {
		r := []rune(base)
		base = string(r[:len(r)-1])
	}
	return base + tag + ext
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
