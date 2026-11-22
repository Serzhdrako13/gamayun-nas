//go:build linux

package apps

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// statT — stat объекта обхода.
type statT = unix.Stat_t

const (
	xattrAccess  = "system.posix_acl_access"
	xattrDefault = "system.posix_acl_default"
	maxWalkDepth = 256
)

// fdWalker — обход дерева по дескрипторам: каждый объект адресуется как
// /proc/self/fd/<каталог>/<имя> (одна компонента от закреплённого каталога,
// без перехода по ссылкам) — подмена каталога ссылкой во время обхода
// (приложение пишет в эту папку) не уводит за пределы дерева.
type fdWalker struct {
	dev uint64
	// visit — объект по закреплённому пути (/proc/self/fd/N[/.]): путь
	// указывает на уже открытый inode, операции по нему идут с переходом
	// по «магической» ссылке fd — только к проверенному объекту.
	visit func(path string, st *statT) error
	// Skipped — ссылки, устройства, другие ФС/субтомы.
	Skipped int64
	stop    func() bool
}

func (w *fdWalker) walk(root string) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", root, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	w.dev = st.Dev
	// Корень: /proc/self/fd/<fd> с именем "." относительно него самого.
	if err := w.visit(dirPath(fd), &st); err != nil {
		return err
	}
	return w.dir(fd, 0)
}

func (w *fdWalker) dir(fd, depth int) error {
	if depth > maxWalkDepth {
		return errors.New("слишком глубокая вложенность каталогов")
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	for {
		if w.stop != nil && w.stop() {
			return errors.New("прервано")
		}
		names, err := f.Readdirnames(1024)
		for _, name := range names {
			var st unix.Stat_t
			if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue // удалён во время обхода
				}
				return fmt.Errorf("%s: %w", name, err)
			}
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				ofd, err := unix.Openat(fd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if err != nil {
					if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
						w.Skipped++
						continue
					}
					return fmt.Errorf("%s: %w", name, err)
				}
				var ost unix.Stat_t
				if err := unix.Fstat(ofd, &ost); err != nil || ost.Ino != st.Ino || ost.Mode&unix.S_IFMT != unix.S_IFREG {
					unix.Close(ofd)
					w.Skipped++
					continue
				}
				err = w.visit("/proc/self/fd/"+strconv.Itoa(ofd), &ost)
				unix.Close(ofd)
				if err != nil {
					return err
				}
			case unix.S_IFDIR:
				if st.Dev != w.dev {
					w.Skipped++
					continue
				}
				cfd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if err != nil {
					if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
						w.Skipped++
						continue
					}
					return fmt.Errorf("%s: %w", name, err)
				}
				var cst unix.Stat_t
				if err := unix.Fstat(cfd, &cst); err != nil || cst.Dev != w.dev || cst.Ino != st.Ino {
					unix.Close(cfd)
					w.Skipped++
					continue
				}
				err = w.visit(dirPath(cfd), &cst)
				if err == nil {
					err = w.dir(cfd, depth+1)
				}
				unix.Close(cfd)
				if err != nil {
					return err
				}
			default:
				w.Skipped++
			}
		}
		if err != nil {
			if errors.Is(err, errEOF) {
				return nil
			}
			return err
		}
		if len(names) == 0 {
			return nil
		}
	}
}

var errEOF = io.EOF

// dirPath — закреплённый путь открытого каталога.
func dirPath(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) + "/." }

func getACL(path, attr string) (ACL, error) {
	buf := make([]byte, 512)
	for {
		n, err := unix.Getxattr(path, attr, buf)
		if errors.Is(err, unix.ENODATA) {
			return nil, nil
		}
		if errors.Is(err, unix.ERANGE) {
			buf = make([]byte, len(buf)*4)
			continue
		}
		if err != nil {
			return nil, err
		}
		return parseACL(buf[:n])
	}
}

func setACL(path, attr string, a ACL) error {
	if a == nil {
		err := unix.Removexattr(path, attr)
		if errors.Is(err, unix.ENODATA) {
			return nil
		}
		return err
	}
	return unix.Setxattr(path, attr, a.encode(), 0)
}

// applyACL — правка ACL одного объекта (access и, для каталога, default).
func (c aclChange) apply(p string, st *statT) (bool, error) {
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	cur, err := getACL(p, xattrAccess)
	if err != nil {
		return false, fmt.Errorf("чтение ACL: %w", err)
	}
	acc, ch := c.applyAccess(cur, st.Mode&0o7777, isDir)
	if ch {
		if acc.minimal() {
			// Эквивалент прав файла: ядро хранит ACL в mode; xattr снимаем,
			// права группы возвращаются из записи группы.
			if gp, ok := acc.perm(aclGroupObj, aclUndef); ok {
				if err := setACL(p, xattrAccess, nil); err != nil {
					return false, err
				}
				mode := st.Mode&^0o070 | uint32(gp)<<3
				if err := unix.Chmod(p, mode&0o7777); err != nil {
					return false, err
				}
			}
		} else if err := setACL(p, xattrAccess, acc); err != nil {
			return false, fmt.Errorf("запись ACL: %w", err)
		}
	}
	if !isDir {
		return ch, nil
	}
	curD, err := getACL(p, xattrDefault)
	if err != nil {
		return false, fmt.Errorf("чтение default ACL: %w", err)
	}
	def, chD := c.applyDefault(curD, acc)
	if chD {
		if err := setACL(p, xattrDefault, def); err != nil {
			return false, fmt.Errorf("запись default ACL: %w", err)
		}
	}
	return ch || chD, nil
}

// readACLs — access и default ACL корня папки (apps.acl.check).
func readACLs(path string) (acc, def ACL, err error) {
	if acc, err = getACL(path, xattrAccess); err != nil {
		return
	}
	def, err = getACL(path, xattrDefault)
	return
}

// chownTree — владелец всех каталогов и файлов дерева (ссылки не трогаются).
func chownTree(root string, uid, gid int, stop func() bool) (int64, error) {
	var n int64
	w := &fdWalker{stop: stop}
	w.visit = func(p string, st *statT) error {
		n++
		return unix.Chown(p, uid, gid)
	}
	err := w.walk(root)
	return n, err
}
