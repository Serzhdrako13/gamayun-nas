//go:build linux

package apps

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Реальные xattr ACL во временном каталоге (tmpfs/ext4/btrfs с ACL).
func TestACLApplyTree(t *testing.T) {
	root := t.TempDir()
	if err := unix.Setxattr(root, xattrAccess, aclFromMode(0o700).set(aclUser, 59001, 5).recalcMask().encode(), 0); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("ФС без POSIX ACL")
		}
		t.Fatal(err)
	}
	os.Chmod(root, 0o2770)
	unix.Removexattr(root, xattrAccess)
	os.Chmod(root, 0o2770)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "a/b"), 0o770))
	must(os.WriteFile(filepath.Join(root, "a/b/f.txt"), []byte("x"), 0o660))
	must(os.Chmod(filepath.Join(root, "a/b/f.txt"), 0o660))
	must(os.WriteFile(filepath.Join(root, "outside"), []byte("x"), 0o600))
	other := t.TempDir()
	must(os.WriteFile(filepath.Join(other, "secret"), []byte("x"), 0o600))
	must(os.Symlink(other, filepath.Join(root, "a/link")))
	must(os.Symlink(filepath.Join(other, "secret"), filepath.Join(root, "a/flink")))

	run := func(ch aclChange) (n int64, skipped int64) {
		w := &fdWalker{}
		w.visit = func(p string, st *statT) error {
			_, err := ch.apply(p, st)
			n++
			return err
		}
		must(w.walk(root))
		return n, w.Skipped
	}
	n, sk := run(aclChange{Mode: GrantModeRW, UID: 59001, GID: uint32(os.Getgid())})
	if n != 5 || sk != 2 {
		t.Fatalf("объектов %d, пропущено %d", n, sk)
	}
	acc, def, err := readACLs(filepath.Join(root, "a/b"))
	must(err)
	if p, _ := acc.perm(aclUser, 59001); p != 7 {
		t.Fatalf("каталог: %+v", acc)
	}
	if p, _ := def.perm(aclUser, 59001); p != 7 {
		t.Fatalf("default: %+v", def)
	}
	fa, _, _ := readACLs(filepath.Join(root, "a/b/f.txt"))
	if p, _ := fa.perm(aclUser, 59001); p != permR|permW {
		t.Fatalf("файл: %+v", fa)
	}
	// по ссылкам не ходили
	if a, _, _ := readACLs(other); a != nil {
		t.Fatalf("ACL за ссылкой на каталог: %+v", a)
	}
	if a, _, _ := readACLs(filepath.Join(other, "secret")); a != nil {
		t.Fatalf("ACL за ссылкой на файл: %+v", a)
	}
	// новый файл наследует default ACL
	must(os.WriteFile(filepath.Join(root, "a/b/new"), []byte("x"), 0o666))
	na, _, _ := readACLs(filepath.Join(root, "a/b/new"))
	p, _ := na.perm(aclUser, 59001)
	m, _ := na.perm(aclMask, aclUndef)
	if p&m != permR|permW {
		t.Fatalf("наследование (действующие права): %+v", na)
	}
	// отзыв — ACL сняты полностью, права прежние
	run(aclChange{Mode: GrantModeRevoke, UID: 59001, GID: uint32(os.Getgid()), DropGroupDefault: true})
	for _, p := range []string{root, "a", "a/b", "a/b/f.txt", "a/b/new"} {
		if p != root {
			p = filepath.Join(root, p)
		}
		a, d, err := readACLs(p)
		must(err)
		if a != nil || d != nil {
			t.Fatalf("%s: после отзыва ACL остались: %+v %+v", p, a, d)
		}
	}
	fi, _ := os.Stat(filepath.Join(root, "a/b/f.txt"))
	if fi.Mode().Perm() != 0o660 {
		t.Fatalf("права файла после отзыва: %o", fi.Mode().Perm())
	}
}
