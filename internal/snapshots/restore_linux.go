//go:build linux

package snapshots

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// maxDepth — предел вложенности при копировании.
const maxDepth = 256

// openChain открывает base и затем по одному компоненту rel через
// openat(O_NOFOLLOW): ссылка в любом компоненте — ELOOP/ENOTDIR.
func openChain(base, rel string) (int, error) {
	fd, err := unix.Open(base, dirFlags, 0)
	if err != nil {
		return -1, err
	}
	if rel == "" {
		return fd, nil
	}
	for _, c := range strings.Split(rel, "/") {
		nfd, err := unix.Openat(fd, c, dirFlags, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = nfd
	}
	return fd, nil
}

func splitRel(rel string) (string, string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

// restoreCopy — см. Executor.Restore. Источник открывается от корня
// снапшота, назначение — от корня папки, всё — *at-вызовами с O_NOFOLLOW:
// ссылка, подставленная по SMB в путь назначения, не уводит запись за
// пределы папки; внутри новой копии всё создаётся с O_EXCL.
func restoreCopy(ctx context.Context, snapRoot, shareRoot, rel string, stamp time.Time) (RestoreResult, error) {
	var res RestoreResult
	parent, name := splitRel(rel)
	sfd, err := openChain(snapRoot, parent)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return res, fmt.Errorf("%w: %s в снимке", ErrNotFound, rel)
		}
		return res, fmt.Errorf("%w: %s в снимке — ссылка или не каталог", ErrRefused, parent)
	}
	defer unix.Close(sfd)
	var st unix.Stat_t
	if err := unix.Fstatat(sfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return res, fmt.Errorf("%w: %s в снимке", ErrNotFound, rel)
	}
	dparent := parent
	dfd, err := openChain(shareRoot, parent)
	if errors.Is(err, unix.ENOENT) {
		dparent, res.ToRoot = "", true
		dfd, err = openChain(shareRoot, "")
	}
	if err != nil {
		return res, fmt.Errorf("%w: каталог %q в папке — ссылка или не каталог", ErrRefused, parent)
	}
	defer unix.Close(dfd)
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	target := ""
	for n := 1; n <= 100; n++ {
		c := RestoredName(name, stamp, isDir, n)
		var x unix.Stat_t
		if err := unix.Fstatat(dfd, c, &x, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			target = c
			break
		}
	}
	if target == "" {
		return res, fmt.Errorf("%w: нет свободного имени для копии %s", ErrConflict, name)
	}
	if err := copyAt(ctx, sfd, name, &st, dfd, target, &res, 0); err != nil {
		if rerr := removeAt(dfd, target); rerr != nil && !errors.Is(rerr, unix.ENOENT) {
			return RestoreResult{}, fmt.Errorf("восстановление %s не выполнено: %w; частичная копия %s не удалена: %v", rel, err, path.Join(dparent, target), rerr)
		}
		return RestoreResult{}, fmt.Errorf("восстановление %s не выполнено (частичная копия удалена): %w", rel, err)
	}
	res.Path = path.Join(dparent, target)
	return res, nil
}

// copyAt копирует sname из каталога sdir в ddir под именем dname.
func copyAt(ctx context.Context, sdir int, sname string, st *unix.Stat_t, ddir int, dname string, res *RestoreResult, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > maxDepth {
		return fmt.Errorf("вложенность больше %d", maxDepth)
	}
	times := []unix.Timespec{st.Atim, st.Mtim}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if err := copyFile(sdir, sname, st, ddir, dname); err != nil {
			return err
		}
		res.Files++
		res.Bytes += st.Size
	case unix.S_IFLNK:
		buf := make([]byte, unix.PathMax)
		n, err := unix.Readlinkat(sdir, sname, buf)
		if err != nil {
			return fmt.Errorf("readlink %s: %v", sname, err)
		}
		if err := unix.Symlinkat(string(buf[:n]), ddir, dname); err != nil {
			return fmt.Errorf("symlink %s: %v", dname, err)
		}
		if err := unix.Fchownat(ddir, dname, int(st.Uid), int(st.Gid), unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("chown %s: %v", dname, err)
		}
		res.Links++
	case unix.S_IFDIR:
		if err := copyDir(ctx, sdir, sname, st, ddir, dname, res, depth); err != nil {
			return err
		}
		res.Dirs++
	default:
		res.Skipped++ // устройство, канал, сокет
		return nil
	}
	if err := unix.UtimesNanoAt(ddir, dname, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("время %s: %v", dname, err)
	}
	return nil
}

func copyFile(sdir int, sname string, st *unix.Stat_t, ddir int, dname string) error {
	sfd, err := unix.Openat(sdir, sname, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("открытие %s: %v", sname, err)
	}
	src := os.NewFile(uintptr(sfd), sname)
	defer src.Close()
	dfd, err := unix.Openat(ddir, dname, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("создание %s: %v", dname, err)
	}
	dst := os.NewFile(uintptr(dfd), dname)
	defer dst.Close()
	// Btrfs: снапшот и папка — одна ФС, клон (reflink) — без копирования
	// данных; иначе — обычное копирование (copy_file_range).
	if err := unix.IoctlFileClone(dfd, sfd); err != nil {
		if _, err := io.Copy(dst, src); err != nil {
			return fmt.Errorf("копирование %s: %v", dname, err)
		}
	}
	if err := unix.Fchown(dfd, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("chown %s: %v", dname, err)
	}
	// setuid/setgid у файлов не переносятся: копию создаёт root.
	if err := unix.Fchmod(dfd, st.Mode&0o777|st.Mode&unix.S_ISVTX); err != nil {
		return fmt.Errorf("chmod %s: %v", dname, err)
	}
	return dst.Sync()
}

func copyDir(ctx context.Context, sdir int, sname string, st *unix.Stat_t, ddir int, dname string, res *RestoreResult, depth int) error {
	sfd, err := unix.Openat(sdir, sname, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("открытие %s: %v", sname, err)
	}
	src := os.NewFile(uintptr(sfd), sname)
	defer src.Close()
	// 0700 до смены владельца и режима: в незаконченную копию не войти.
	if err := unix.Mkdirat(ddir, dname, 0o700); err != nil {
		return fmt.Errorf("создание %s: %v", dname, err)
	}
	dfd, err := unix.Openat(ddir, dname, dirFlags, 0)
	if err != nil {
		return fmt.Errorf("открытие %s: %v", dname, err)
	}
	defer unix.Close(dfd)
	names, err := src.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("чтение %s: %v", sname, err)
	}
	for _, n := range names {
		var cst unix.Stat_t
		if err := unix.Fstatat(sfd, n, &cst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("stat %s: %v", n, err)
		}
		if err := copyAt(ctx, sfd, n, &cst, dfd, n, res, depth+1); err != nil {
			return err
		}
	}
	if err := unix.Fchown(dfd, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("chown %s: %v", dname, err)
	}
	// После chown: смена владельца сбрасывает setgid. У каталогов setgid
	// (наследование группы gamayun-users) сохраняется.
	return unix.Fchmod(dfd, st.Mode&0o7777)
}

// removeAt рекурсивно удаляет name в каталоге dir без следования по ссылкам
// (только частичную копию, созданную restoreCopy).
func removeAt(dir int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(dir, name, 0)
	}
	fd, err := unix.Openat(dir, name, dirFlags, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	names, err := f.Readdirnames(-1)
	if err == nil {
		for _, n := range names {
			if err = removeAt(fd, n); err != nil {
				break
			}
		}
	}
	f.Close()
	if err != nil {
		return err
	}
	return unix.Unlinkat(dir, name, unix.AT_REMOVEDIR)
}
