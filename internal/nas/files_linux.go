//go:build linux

package nas

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// FileHandle — открытый файл и сведения о нём (open/create); helper
// оборачивает его в WithFile.
type FileHandle struct {
	File *os.File
	Info any
}

func entryType(m uint32) string {
	switch m & unix.S_IFMT {
	case unix.S_IFREG:
		return "file"
	case unix.S_IFDIR:
		return "dir"
	case unix.S_IFLNK:
		return "link"
	}
	return "other"
}

func statTime(st *unix.Stat_t) time.Time {
	return time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC()
}

// openParent — дескриптор родителя объекта l.Path и имя объекта.
func (e *Executor) openParent(l FileLoc) (int, string, error) {
	base, err := e.fileBase(l)
	if err != nil {
		return -1, "", err
	}
	parent, name := splitRel(l.Path)
	fd, err := openDirChain(base, parent)
	return fd, name, err
}

// openDir — дескриптор каталога l.Path.
func (e *Executor) openDir(l FileLoc) (int, error) {
	base, err := e.fileBase(l)
	if err != nil {
		return -1, err
	}
	return openDirChain(base, l.Path)
}

func lstatAt(dfd int, name, rel string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return st, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return st, fmt.Errorf("%s: %v", rel, err)
	}
	return st, nil
}

// FilesList — содержимое каталога (без временных файлов), по имени.
func (e *Executor) FilesList(_ context.Context, p FilesLocParams) (FilesListResult, error) {
	fd, err := e.openDir(p.FileLoc)
	if err != nil {
		return FilesListResult{}, err
	}
	f := os.NewFile(uintptr(fd), p.Path)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return FilesListResult{}, fmt.Errorf("чтение каталога: %v", err)
	}
	sort.Strings(names)
	res := FilesListResult{Entries: []FileEntry{}}
	for _, n := range names {
		if len(n) > len(TempPrefix) && n[:len(TempPrefix)] == TempPrefix {
			continue
		}
		if len(res.Entries) >= MaxListEntries {
			res.Truncated = true
			break
		}
		var st unix.Stat_t
		if unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			continue // исчез между чтением и stat
		}
		res.Entries = append(res.Entries, FileEntry{Name: n, Type: entryType(st.Mode), Size: st.Size, MTime: statTime(&st)})
	}
	return res, nil
}

// FilesStat — сведения об объекте (ссылка не раскрывается).
func (e *Executor) FilesStat(_ context.Context, p FilesLocParams) (FileInfo, error) {
	if p.Path == "" {
		fd, err := e.openDir(p.FileLoc)
		if err != nil {
			return FileInfo{}, err
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return FileInfo{}, err
		}
		return FileInfo{Name: "", Type: "dir", MTime: statTime(&st)}, nil
	}
	pfd, name, err := e.openParent(p.FileLoc)
	if err != nil {
		return FileInfo{}, err
	}
	defer unix.Close(pfd)
	st, err := lstatAt(pfd, name, p.Path)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Name: name, Type: entryType(st.Mode), Size: st.Size, MTime: statTime(&st)}, nil
}

// FilesOpen открывает обычный файл на чтение (O_NOFOLLOW: ссылка — отказ).
func (e *Executor) FilesOpen(_ context.Context, p FilesLocParams) (FileHandle, error) {
	if err := p.notRoot(); err != nil {
		return FileHandle{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	pfd, name, err := e.openParent(p.FileLoc)
	if err != nil {
		return FileHandle{}, err
	}
	defer unix.Close(pfd)
	fd, err := unix.Openat(pfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return FileHandle{}, fmt.Errorf("%w: %s", ErrNotFound, p.Path)
		case errors.Is(err, unix.ELOOP):
			return FileHandle{}, fmt.Errorf("%w: %s — ссылка", ErrRefused, p.Path)
		}
		return FileHandle{}, fmt.Errorf("открытие %s: %v", p.Path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return FileHandle{}, fmt.Errorf("%w: %s — не обычный файл", ErrRefused, p.Path)
	}
	// O_NONBLOCK нужен только чтобы open не повис на FIFO; для чтения файла — снять.
	if fl, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0); err == nil {
		unix.FcntlInt(uintptr(fd), unix.F_SETFL, fl&^unix.O_NONBLOCK)
	}
	return FileHandle{File: os.NewFile(uintptr(fd), name),
		Info: FileInfo{Name: name, Type: "file", Size: st.Size, MTime: statTime(&st)}}, nil
}

// FilesCreate создаёт временный файл в каталоге p.Path (владелец —
// пользователь, режим как у Samba) и отдаёт его на запись. Нехватка места
// под p.Size — ErrNoSpace до создания.
func (e *Executor) FilesCreate(_ context.Context, p FilesCreateParams) (FileHandle, error) {
	uid, gid, fmode, _, err := e.fileOwner(p.FileLoc)
	if err != nil {
		return FileHandle{}, err
	}
	dfd, err := e.openDir(p.FileLoc)
	if err != nil {
		return FileHandle{}, err
	}
	defer unix.Close(dfd)
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(dfd, &sfs); err == nil {
		free := int64(sfs.Bavail) * int64(sfs.Bsize)
		if p.Size > free {
			return FileHandle{}, fmt.Errorf("%w: нужно %d байт, свободно %d", ErrNoSpace, p.Size, free)
		}
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	temp := TempPrefix + hex.EncodeToString(rnd[:])
	fd, err := unix.Openat(dfd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return FileHandle{}, fmt.Errorf("создание файла: %v", err)
	}
	fail := func(cause error) (FileHandle, error) {
		unix.Close(fd)
		unix.Unlinkat(dfd, temp, 0)
		return FileHandle{}, cause
	}
	if err := e.chownFile(fd, uid, gid); err != nil {
		return fail(fmt.Errorf("chown: %v", err))
	}
	if err := unix.Fchmod(fd, fmode); err != nil {
		return fail(fmt.Errorf("chmod: %v", err))
	}
	return FileHandle{File: os.NewFile(uintptr(fd), temp), Info: FilesCreateResult{Temp: temp}}, nil
}

// FilesCommit переименовывает временный файл в p.Name без перезаписи
// (renameat2 RENAME_NOREPLACE); при rename подбирает «имя (N)».
func (e *Executor) FilesCommit(_ context.Context, p FilesCommitParams) (FilesNameResult, error) {
	dfd, err := e.openDir(p.FileLoc)
	if err != nil {
		return FilesNameResult{}, err
	}
	defer unix.Close(dfd)
	st, err := lstatAt(dfd, p.Temp, p.Temp)
	if err != nil {
		return FilesNameResult{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return FilesNameResult{}, fmt.Errorf("%w: временный файл подменён", ErrRefused)
	}
	name := p.Name
	for n := 2; ; n++ {
		err = unix.Renameat2(dfd, p.Temp, dfd, name, unix.RENAME_NOREPLACE)
		if err == nil {
			return FilesNameResult{Path: path.Join(p.Path, name)}, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return FilesNameResult{}, fmt.Errorf("переименование: %v", err)
		}
		if p.OnConflict != ConflictRename || n > 999 {
			return FilesNameResult{}, fmt.Errorf("%w: %s уже существует", ErrConflict, path.Join(p.Path, name))
		}
		name = ConflictName(p.Name, n)
		if len(name) > MaxDirName {
			return FilesNameResult{}, fmt.Errorf("%w: %s уже существует", ErrConflict, path.Join(p.Path, p.Name))
		}
	}
}

// FilesDiscard удаляет временный файл (только обычный файл с именем-шаблоном).
func (e *Executor) FilesDiscard(_ context.Context, p FilesDiscardParams) (FilesNameResult, error) {
	dfd, err := e.openDir(p.FileLoc)
	if err != nil {
		return FilesNameResult{}, err
	}
	defer unix.Close(dfd)
	st, err := lstatAt(dfd, p.Temp, p.Temp)
	if err != nil {
		return FilesNameResult{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return FilesNameResult{}, fmt.Errorf("%w: не файл", ErrRefused)
	}
	if err := unix.Unlinkat(dfd, p.Temp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return FilesNameResult{}, err
	}
	return FilesNameResult{Path: path.Join(p.Path, p.Temp)}, nil
}

// FilesMkdir создаёт каталог p.Name в каталоге p.Path: в общей папке —
// root:gamayun-users 2770 (как у общих папок), в домашней — пользователь 0700.
func (e *Executor) FilesMkdir(_ context.Context, p FilesNameParams) (FilesNameResult, error) {
	base, err := e.fileBase(p.FileLoc)
	if err != nil {
		return FilesNameResult{}, err
	}
	rel := path.Join(p.Path, p.Name)
	if len(rel) > MaxDirPath {
		return FilesNameResult{}, fmt.Errorf("%w: путь слишком длинный", ErrInvalid)
	}
	chown := e.DirChown
	if chown == nil {
		chown = e.defaultDirChown
	}
	if p.Share == HomesShare {
		uid, gid, _, _, err := e.fileOwner(p.FileLoc)
		if err != nil {
			return FilesNameResult{}, err
		}
		chown = func(fd int) error {
			if err := e.chownFile(fd, uid, gid); err != nil {
				return err
			}
			return unix.Fchmod(fd, 0o700)
		}
		if err := dirMkdirMode(base, p.Path, p.Name, chown, 0o700); err != nil {
			return FilesNameResult{}, err
		}
		return FilesNameResult{Path: rel}, nil
	}
	if err := dirMkdir(base, p.Path, p.Name, chown); err != nil {
		return FilesNameResult{}, err
	}
	return FilesNameResult{Path: rel}, nil
}

// dirMkdirMode — dirMkdir с другим итоговым режимом (домашняя папка).
func dirMkdirMode(base, parent, name string, chown func(fd int) error, mode uint32) error {
	pfd, err := openDirChain(base, parent)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	rel := path.Join(parent, name)
	if err := unix.Mkdirat(pfd, name, 0o700); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s уже существует", ErrConflict, rel)
		}
		return fmt.Errorf("mkdir %s: %v", rel, err)
	}
	fd, err := unix.Openat(pfd, name, dirOpenFlags, 0)
	if err == nil {
		err = chown(fd)
		if err == nil {
			err = unix.Fchmod(fd, mode)
		}
		unix.Close(fd)
	}
	if err != nil {
		unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR)
		return fmt.Errorf("каталог %s: %v; созданный каталог удалён", rel, err)
	}
	return nil
}

// FilesRename — переименование файла, каталога или ссылки (сама ссылка) в
// пределах родителя без перезаписи.
func (e *Executor) FilesRename(_ context.Context, p FilesNameParams) (FilesNameResult, error) {
	if err := p.notRoot(); err != nil {
		return FilesNameResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	pfd, old, err := e.openParent(p.FileLoc)
	if err != nil {
		return FilesNameResult{}, err
	}
	defer unix.Close(pfd)
	parent, _ := splitRel(p.Path)
	if _, err := lstatAt(pfd, old, p.Path); err != nil {
		return FilesNameResult{}, err
	}
	dst := path.Join(parent, p.Name)
	if old == p.Name {
		return FilesNameResult{Path: dst}, nil
	}
	err = unix.Renameat2(pfd, old, pfd, p.Name, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return FilesNameResult{Path: dst}, nil
	case errors.Is(err, unix.EEXIST):
		return FilesNameResult{}, fmt.Errorf("%w: %s уже существует", ErrConflict, dst)
	case errors.Is(err, unix.ENOENT):
		return FilesNameResult{}, fmt.Errorf("%w: %s", ErrNotFound, p.Path)
	case errors.Is(err, unix.EBUSY):
		return FilesNameResult{}, fmt.Errorf("%w: %s занят", ErrRefused, p.Path)
	}
	return FilesNameResult{}, fmt.Errorf("переименование %s: %v", p.Path, err)
}

// FilesMove переносит объект в каталог p.To той же папки, имя сохраняется.
func (e *Executor) FilesMove(_ context.Context, p FilesMoveParams) (FilesNameResult, error) {
	base, err := e.fileBase(p.FileLoc)
	if err != nil {
		return FilesNameResult{}, err
	}
	parent, name := splitRel(p.Path)
	spfd, err := openDirChain(base, parent)
	if err != nil {
		return FilesNameResult{}, err
	}
	defer unix.Close(spfd)
	if _, err := lstatAt(spfd, name, p.Path); err != nil {
		return FilesNameResult{}, err
	}
	dst := path.Join(p.To, name)
	if p.To == parent {
		return FilesNameResult{Path: dst}, nil
	}
	dfd, err := openDirChain(base, p.To)
	if err != nil {
		return FilesNameResult{}, err
	}
	defer unix.Close(dfd)
	err = unix.Renameat2(spfd, name, dfd, name, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return FilesNameResult{Path: dst}, nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return FilesNameResult{}, fmt.Errorf("%w: %s уже существует", ErrConflict, dst)
	case errors.Is(err, unix.ENOENT):
		return FilesNameResult{}, fmt.Errorf("%w: %s", ErrNotFound, p.Path)
	case errors.Is(err, unix.EXDEV), errors.Is(err, unix.EBUSY), errors.Is(err, unix.EINVAL):
		return FilesNameResult{}, fmt.Errorf("%w: перенос %s в %s отвергнут: %v", ErrRefused, p.Path, p.To, err)
	}
	return FilesNameResult{}, fmt.Errorf("перенос %s: %v", p.Path, err)
}

// FilesRemove удаляет объект; каталог — рекурсивно, без перехода по
// ссылкам (ссылка удаляется сама, цель не трогается) и без выхода за
// пределы ФС (точки монтирования внутри — отказ).
func (e *Executor) FilesRemove(ctx context.Context, p FilesLocParams) (FilesRemoveResult, error) {
	if err := p.notRoot(); err != nil {
		return FilesRemoveResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	pfd, name, err := e.openParent(p.FileLoc)
	if err != nil {
		return FilesRemoveResult{}, err
	}
	defer unix.Close(pfd)
	var pst unix.Stat_t
	if err := unix.Fstat(pfd, &pst); err != nil {
		return FilesRemoveResult{}, err
	}
	var res FilesRemoveResult
	err = removeAt(ctx, pfd, name, p.Path, pst.Dev, 0, &res)
	return res, err
}

func removeAt(ctx context.Context, dfd int, name, rel string, dev uint64, depth int, res *FilesRemoveResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := lstatAt(dfd, name, rel)
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(dfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("удаление %s: %v", rel, err)
		}
		res.Files++
		return nil
	}
	if st.Dev != dev {
		return fmt.Errorf("%w: %s — точка монтирования или подтом", ErrRefused, rel)
	}
	if depth >= MaxRemoveDepth {
		return fmt.Errorf("%w: вложенность больше %d", ErrRefused, MaxRemoveDepth)
	}
	cfd, err := unix.Openat(dfd, name, dirOpenFlags, 0)
	if err != nil {
		return pathErr(rel, err)
	}
	f := os.NewFile(uintptr(cfd), rel)
	names, err := f.Readdirnames(-1)
	if err != nil {
		f.Close()
		return fmt.Errorf("чтение %s: %v", rel, err)
	}
	for _, n := range names {
		if err := removeAt(ctx, cfd, n, path.Join(rel, n), dev, depth+1, res); err != nil {
			f.Close()
			return err
		}
	}
	f.Close()
	if err := unix.Unlinkat(dfd, name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("удаление %s: %v", rel, err)
	}
	res.Dirs++
	return nil
}
