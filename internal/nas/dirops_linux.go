//go:build linux

package nas

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// dirOpenFlags — открытие каталога без следования по ссылке в последнем
// компоненте: ссылка даёт ELOOP, файл — ENOTDIR.
const dirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// openDirChain открывает каталог шары base (сам он не может быть ссылкой) и
// затем по одному компоненту rel через openat(O_NOFOLLOW). Ни ссылка (внутрь
// или наружу), ни «..» (отвергнуто CleanRel) не проходят; подмена компонента
// ссылкой между проверкой и действием невозможна — проверки нет отдельно от
// открытия.
func openDirChain(base, rel string) (int, error) {
	fd, err := unix.Open(base, dirOpenFlags, 0)
	if err != nil {
		return -1, fmt.Errorf("%w: каталог папки недоступен", ErrRefused)
	}
	if rel == "" {
		return fd, nil
	}
	cur := ""
	for _, c := range strings.Split(rel, "/") {
		cur = path.Join(cur, c)
		nfd, err := unix.Openat(fd, c, dirOpenFlags, 0)
		unix.Close(fd)
		if err != nil {
			return -1, pathErr(cur, err)
		}
		fd = nfd
	}
	return fd, nil
}

// pathErr — ошибка открытия компонента пути.
func pathErr(rel string, err error) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("%w: каталог %s", ErrNotFound, rel)
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return fmt.Errorf("%w: %s — ссылка или не каталог", ErrRefused, rel)
	default:
		return fmt.Errorf("каталог %s: %v", rel, err)
	}
}

// checkDir — name в каталоге dirfd существует и это каталог (не ссылка).
func checkDir(dirfd int, name, rel string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return pathErr(rel, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%w: %s — ссылка или не каталог", ErrRefused, rel)
	}
	return nil
}

func dirMkdir(base, parent, name string, chown func(fd int) error) error {
	rel := path.Join(parent, name)
	pfd, err := openDirChain(base, parent)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	// 0700 до смены владельца и режима: никто, кроме root, не войдёт в
	// каталог, пока он не стал root:gamayun-users 2770. umask на итог не
	// влияет — режим задаётся fchmod.
	if err := unix.Mkdirat(pfd, name, 0o700); err != nil {
		switch {
		case errors.Is(err, unix.EEXIST):
			return fmt.Errorf("%w: %s уже существует", ErrConflict, rel)
		case errors.Is(err, unix.ENOENT):
			return fmt.Errorf("%w: каталог %s", ErrNotFound, parent)
		}
		return fmt.Errorf("mkdir %s: %v", rel, err)
	}
	undo := func(cause error) error {
		// Только пустой каталог и не по ссылке (unlinkat AT_REMOVEDIR).
		if rerr := unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR); rerr != nil {
			return fmt.Errorf("%w; созданный каталог %s не удалён: %v", cause, rel, rerr)
		}
		return fmt.Errorf("%w; созданный каталог удалён", cause)
	}
	fd, err := unix.Openat(pfd, name, dirOpenFlags, 0)
	if err != nil {
		return undo(fmt.Errorf("открытие %s: %v", rel, err))
	}
	defer unix.Close(fd)
	if err := chown(fd); err != nil {
		return undo(fmt.Errorf("chown %s: %v", rel, err))
	}
	// После chown: смена владельца может сбросить setgid.
	if err := unix.Fchmod(fd, 0o2770); err != nil {
		return undo(fmt.Errorf("chmod %s: %v", rel, err))
	}
	return nil
}

func dirRename(base, parent, old, name string) (bool, error) {
	rel := path.Join(parent, old)
	pfd, err := openDirChain(base, parent)
	if err != nil {
		return false, err
	}
	defer unix.Close(pfd)
	if err := checkDir(pfd, old, rel); err != nil {
		return false, err
	}
	if old == name {
		return false, nil
	}
	// RENAME_NOREPLACE: проверка занятости и переименование — одна операция
	// ядра, без гонки «проверили — успели создать».
	err = unix.Renameat2(pfd, old, pfd, name, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EEXIST):
		return false, fmt.Errorf("%w: %s уже существует", ErrConflict, path.Join(parent, name))
	case errors.Is(err, unix.ENOENT):
		return false, fmt.Errorf("%w: каталог %s", ErrNotFound, rel)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return false, fmt.Errorf("%w: ФС не поддерживает переименование без перезаписи", ErrRefused)
	}
	return false, fmt.Errorf("rename %s: %v", rel, err)
}

// dirMove переносит каталог parent/name в каталог to. Оба родителя
// открываются цепочкой openat(O_NOFOLLOW) от каталога шары — ни ссылка, ни
// «..» не выводят за её пределы; renameat2 работает по дескрипторам, так что
// подмена компонентов пути после открытия не меняет, куда идёт перенос.
// Перенос в собственный подкаталог отвергает Validate, а при гонке (каталоги
// переставили по сети) — ядро (EINVAL).
func dirMove(base, parent, name, to string) (bool, error) {
	rel := path.Join(parent, name)
	if to == parent {
		spfd, err := openDirChain(base, parent)
		if err != nil {
			return false, err
		}
		defer unix.Close(spfd)
		return false, checkDir(spfd, name, rel)
	}
	spfd, err := openDirChain(base, parent)
	if err != nil {
		return false, err
	}
	defer unix.Close(spfd)
	if err := checkDir(spfd, name, rel); err != nil {
		return false, err
	}
	dfd, err := openDirChain(base, to)
	if err != nil {
		return false, err
	}
	defer unix.Close(dfd)
	dst := path.Join(to, name)
	err = unix.Renameat2(spfd, name, dfd, name, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return false, fmt.Errorf("%w: %s уже существует", ErrConflict, dst)
	case errors.Is(err, unix.ENOENT):
		return false, fmt.Errorf("%w: каталог %s", ErrNotFound, rel)
	case errors.Is(err, unix.EXDEV):
		return false, fmt.Errorf("%w: %s и %s на разных файловых системах (подтом/точка монтирования)", ErrRefused, rel, to)
	case errors.Is(err, unix.EBUSY):
		return false, fmt.Errorf("%w: %s занят (точка монтирования)", ErrRefused, rel)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return false, fmt.Errorf("%w: перенос %s в %s отвергнут (в собственный подкаталог или ФС без переименования без перезаписи)", ErrRefused, rel, to)
	}
	return false, fmt.Errorf("перенос %s в %s: %v", rel, to, err)
}

func dirRmdir(base, parent, name string) error {
	rel := path.Join(parent, name)
	pfd, err := openDirChain(base, parent)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	if err := checkDir(pfd, name, rel); err != nil {
		return err
	}
	// AT_REMOVEDIR удаляет только пустой каталог и не следует по ссылке.
	err = unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTEMPTY), errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%w: каталог %s не пуст", ErrConflict, rel)
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("%w: каталог %s", ErrNotFound, rel)
	case errors.Is(err, unix.ENOTDIR):
		return fmt.Errorf("%w: %s — ссылка или не каталог", ErrRefused, rel)
	case errors.Is(err, unix.EBUSY):
		return fmt.Errorf("%w: %s занят (точка монтирования)", ErrRefused, rel)
	}
	return fmt.Errorf("rmdir %s: %v", rel, err)
}

func fchown(fd, uid, gid int) error { return unix.Fchown(fd, uid, gid) }
