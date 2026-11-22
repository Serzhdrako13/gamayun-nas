//go:build linux

package nas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestValidDirName(t *testing.T) {
	for _, ok := range []string{"a", "Фото 2024", "a.b", ".hidden", "x y", strings.Repeat("я", 127), strings.Repeat("a", MaxDirName)} {
		if err := ValidDirName(ok); err != nil {
			t.Errorf("%q отклонено: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "/", "a\\b", "a\x00b", "a\nb", "a\tb", "a\x7fb", "a\u0085b",
		strings.Repeat("a", MaxDirName+1), strings.Repeat("я", 128), "\xff\xfe"} {
		if err := ValidDirName(bad); err == nil {
			t.Errorf("%q принято", bad)
		}
	}
}

func TestShareDirOpParamsValidate(t *testing.T) {
	mk := ShareMkdirParams{Pool: "p1", Share: "docs", Path: "a/b", Name: "new"}
	if err := mk.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ShareMkdirParams{Pool: "p1", Share: "docs", Name: "new"}).Validate(); err != nil {
		t.Fatalf("mkdir в корне: %v", err)
	}
	for i, f := range []func(*ShareMkdirParams){
		func(p *ShareMkdirParams) { p.Pool = "../x" },
		func(p *ShareMkdirParams) { p.Pool = "" },
		func(p *ShareMkdirParams) { p.Share = "a/b" },
		func(p *ShareMkdirParams) { p.Share = ".." },
		func(p *ShareMkdirParams) { p.Path = "../.." },
		func(p *ShareMkdirParams) { p.Path = "/etc" },
		func(p *ShareMkdirParams) { p.Path = "a/" },
		func(p *ShareMkdirParams) { p.Name = "" },
		func(p *ShareMkdirParams) { p.Name = ".." },
		func(p *ShareMkdirParams) { p.Name = "x/y" },
		func(p *ShareMkdirParams) { p.Name = "x\\y" },
		func(p *ShareMkdirParams) { p.Path = strings.Repeat("a/", 400) + "a"; p.Name = strings.Repeat("b", 250) },
	} {
		p := mk
		f(&p)
		if p.Validate() == nil {
			t.Errorf("mkdir %d принято: %+v", i, p)
		}
	}

	rn := ShareRenameParams{Pool: "p1", Share: "docs", Path: "a/b", Name: "c"}
	if err := rn.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, f := range []func(*ShareRenameParams){
		func(p *ShareRenameParams) { p.Pool = "P!" },
		func(p *ShareRenameParams) { p.Share = "" },
		func(p *ShareRenameParams) { p.Path = "" }, // корень шары
		func(p *ShareRenameParams) { p.Path = ".." },
		func(p *ShareRenameParams) { p.Path = "a/../b" },
		func(p *ShareRenameParams) { p.Path = "/a" },
		func(p *ShareRenameParams) { p.Name = "../c" }, // перенос в другой родитель
		func(p *ShareRenameParams) { p.Name = "x/c" },
		func(p *ShareRenameParams) { p.Name = "." },
		func(p *ShareRenameParams) { p.Name = "" },
		func(p *ShareRenameParams) { p.Name = "a\x01" },
	} {
		p := rn
		f(&p)
		if p.Validate() == nil {
			t.Errorf("rename %d принято: %+v", i, p)
		}
	}

	rm := ShareRmdirParams{Pool: "p1", Share: "docs", Path: "a"}
	if err := rm.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, f := range []func(*ShareRmdirParams){
		func(p *ShareRmdirParams) { p.Pool = "../p" },
		func(p *ShareRmdirParams) { p.Share = "a b" },
		func(p *ShareRmdirParams) { p.Path = "" }, // корень шары
		func(p *ShareRmdirParams) { p.Path = ".." },
		func(p *ShareRmdirParams) { p.Path = "/" },
		func(p *ShareRmdirParams) { p.Path = "a//b" },
	} {
		p := rm
		f(&p)
		if p.Validate() == nil {
			t.Errorf("rmdir %d принято: %+v", i, p)
		}
	}
}

// dirOpsFixture — шара p1/docs: a/b, a/full/f, file.txt, ссылки наружу и
// внутрь, каталог outside вне шары. chowned — inode каталогов, которым
// вызывался DirChown.
func dirOpsFixture(t *testing.T) (e *Executor, share, outside string, chowned *[]uint64) {
	t.Helper()
	mount := t.TempDir()
	share = ShareDir(mount, "p1", "docs")
	for _, d := range []string{"a/b", "a/full", "z"} {
		if err := os.MkdirAll(filepath.Join(share, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(share, "a/full/f"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(share, "file.txt"), []byte("x"), 0o644)
	outside = t.TempDir()
	os.MkdirAll(filepath.Join(outside, "secret"), 0o755)
	os.Symlink(outside, filepath.Join(share, "escape"))   // наружу
	os.Symlink("a", filepath.Join(share, "inner"))        // внутрь шары
	os.Symlink("z", filepath.Join(share, "a", "tolink"))  // внутрь, как цель операции
	os.Symlink("../../../..", filepath.Join(share, "up")) // «..» через ссылку
	var ids []uint64
	e = &Executor{
		MountRoot: mount,
		Mounted:   func(string) bool { return true },
		DirChown: func(fd int) error {
			var st syscall.Stat_t
			if err := syscall.Fstat(fd, &st); err != nil {
				return err
			}
			ids = append(ids, st.Ino)
			return nil
		},
	}
	return e, share, outside, &ids
}

func ino(t *testing.T, p string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

func assertNoEntries(t *testing.T, dir string, want int) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != want {
		t.Errorf("%s: %d записей, ожидалось %d", dir, len(ents), want)
	}
}

func TestShareMkdir(t *testing.T) {
	e, share, outside, chowned := dirOpsFixture(t)
	ctx := context.Background()
	old := syscall.Umask(0o077) // режим не должен зависеть от umask
	defer syscall.Umask(old)

	res, err := e.ShareMkdir(ctx, ShareMkdirParams{Pool: "p1", Share: "docs", Path: "a/b", Name: "Новая папка"})
	if err != nil || res.Path != "a/b/Новая папка" || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	created := filepath.Join(share, "a/b/Новая папка")
	st, err := os.Lstat(created)
	if err != nil {
		t.Fatal(err)
	}
	if want := os.ModeDir | ShareDirMode; st.Mode() != want {
		t.Errorf("режим %v, ожидался %v", st.Mode(), want)
	}
	if len(*chowned) != 1 || (*chowned)[0] != ino(t, created) {
		t.Errorf("DirChown не вызван для созданного каталога: %v", *chowned)
	}
	if res, err := e.ShareMkdir(ctx, ShareMkdirParams{Pool: "p1", Share: "docs", Name: "top"}); err != nil || res.Path != "top" {
		t.Fatalf("в корне: %+v %v", res, err)
	}

	cases := []struct {
		name string
		p    ShareMkdirParams
		want error
	}{
		{"занято каталогом", ShareMkdirParams{Path: "a", Name: "b"}, ErrConflict},
		{"занято файлом", ShareMkdirParams{Name: "file.txt"}, ErrConflict},
		{"занято ссылкой", ShareMkdirParams{Name: "escape"}, ErrConflict},
		{"родителя нет", ShareMkdirParams{Path: "nope", Name: "x"}, ErrNotFound},
		{"родитель — файл", ShareMkdirParams{Path: "file.txt", Name: "x"}, ErrRefused},
		{"ссылка наружу в пути", ShareMkdirParams{Path: "escape", Name: "x"}, ErrRefused},
		{"ссылка наружу в середине", ShareMkdirParams{Path: "escape/secret", Name: "x"}, ErrRefused},
		{"ссылка внутрь в пути", ShareMkdirParams{Path: "inner/b", Name: "x"}, ErrRefused},
		{"ссылка с .. в пути", ShareMkdirParams{Path: "up", Name: "x"}, ErrRefused},
		{"..", ShareMkdirParams{Path: "a/..", Name: "x"}, ErrInvalid},
		{".. в имени", ShareMkdirParams{Name: ".."}, ErrInvalid},
		{"абсолютный путь", ShareMkdirParams{Path: outside, Name: "x"}, ErrInvalid},
		{"/ в имени", ShareMkdirParams{Name: "a/x"}, ErrInvalid},
	}
	for _, c := range cases {
		c.p.Pool, c.p.Share = "p1", "docs"
		if _, err := e.ShareMkdir(ctx, c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, ожидалось %v", c.name, err, c.want)
		}
	}
	assertNoEntries(t, outside, 1) // только secret
	assertNoEntries(t, filepath.Join(outside, "secret"), 0)
	if len(*chowned) != 2 {
		t.Errorf("DirChown вызван %d раз, ожидалось 2", len(*chowned))
	}
}

func TestShareMkdirRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("пул не смонтирован", func(t *testing.T) {
		e, share, _, _ := dirOpsFixture(t)
		e.Mounted = func(string) bool { return false }
		if _, err := e.ShareMkdir(ctx, ShareMkdirParams{Pool: "p1", Share: "docs", Name: "x"}); !errors.Is(err, ErrRefused) {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(share, "x")); err == nil {
			t.Fatal("каталог создан на несмонтированном пуле")
		}
	})
	t.Run("каталог шары — ссылка", func(t *testing.T) {
		e, share, outside, _ := dirOpsFixture(t)
		os.Rename(share, share+".real")
		os.Symlink(outside, share)
		if _, err := e.ShareMkdir(ctx, ShareMkdirParams{Pool: "p1", Share: "docs", Name: "x"}); !errors.Is(err, ErrRefused) {
			t.Fatal(err)
		}
		assertNoEntries(t, outside, 1)
	})
	t.Run("шары нет", func(t *testing.T) {
		e, _, _, _ := dirOpsFixture(t)
		if _, err := e.ShareMkdir(ctx, ShareMkdirParams{Pool: "p1", Share: "other", Name: "x"}); !errors.Is(err, ErrRefused) {
			t.Fatal(err)
		}
	})
	t.Run("сбой chown — откат", func(t *testing.T) {
		e, share, _, _ := dirOpsFixture(t)
		e.DirChown = func(int) error { return errors.New("EPERM") }
		_, err := e.ShareMkdir(ctx, ShareMkdirParams{Pool: "p1", Share: "docs", Path: "a", Name: "x"})
		if err == nil || !strings.Contains(err.Error(), "созданный каталог удалён") {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(share, "a/x")); !os.IsNotExist(err) {
			t.Fatalf("каталог не удалён: %v", err)
		}
	})
}

// Владелец по умолчанию: root:gamayun-users через fchown. Без root
// fchown(uid 0) запрещён — проверяется откат; под root — итоговый владелец.
func TestShareMkdirDefaultChown(t *testing.T) {
	e, share, _, _ := dirOpsFixture(t)
	e.DirChown = nil
	gid := os.Getgid()
	gf := filepath.Join(t.TempDir(), "group")
	os.WriteFile(gf, []byte("root:x:0:\n"+UnixGroup+":x:"+strconv.Itoa(gid)+":\n"), 0o644)
	e.GroupFile = gf
	_, err := e.ShareMkdir(context.Background(), ShareMkdirParams{Pool: "p1", Share: "docs", Name: "x"})
	st, serr := os.Lstat(filepath.Join(share, "x"))
	if os.Geteuid() != 0 {
		if err == nil || !os.IsNotExist(serr) {
			t.Fatalf("без root ожидался отказ chown с откатом: %v, %v", err, serr)
		}
		t.Logf("без root: %v", err)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if sys.Uid != 0 || int(sys.Gid) != gid || st.Mode() != os.ModeDir|ShareDirMode {
		t.Fatalf("uid=%d gid=%d mode=%v", sys.Uid, sys.Gid, st.Mode())
	}
}

func TestGroupID(t *testing.T) {
	gf := filepath.Join(t.TempDir(), "group")
	os.WriteFile(gf, []byte("root:x:0:\nother-users-old:x:5:\ngamayun-users:x:1234:a,b\nbad:x:zz:\n"), 0o644)
	if gid, err := groupID(gf, UnixGroup); err != nil || gid != 1234 {
		t.Fatalf("%d %v", gid, err)
	}
	if _, err := groupID(gf, "missing"); err == nil {
		t.Fatal("нет группы — без ошибки")
	}
	if _, err := groupID(gf, "bad"); err == nil {
		t.Fatal("неверный gid — без ошибки")
	}
	if _, err := groupID(filepath.Join(t.TempDir(), "none"), UnixGroup); err == nil {
		t.Fatal("нет файла — без ошибки")
	}
}

func TestShareRename(t *testing.T) {
	e, share, outside, _ := dirOpsFixture(t)
	ctx := context.Background()
	before := ino(t, filepath.Join(share, "a/b"))
	res, err := e.ShareRename(ctx, ShareRenameParams{Pool: "p1", Share: "docs", Path: "a/b", Name: "c"})
	if err != nil || res.Path != "a/c" || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	if ino(t, filepath.Join(share, "a/c")) != before {
		t.Fatal("переименован не тот каталог")
	}
	if res, err := e.ShareRename(ctx, ShareRenameParams{Pool: "p1", Share: "docs", Path: "a/c", Name: "c"}); err != nil || res.Changed || res.Path != "a/c" {
		t.Fatalf("то же имя: %+v %v", res, err)
	}

	if err := os.Mkdir(filepath.Join(share, "a/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	emptyIno := ino(t, filepath.Join(share, "a/empty"))
	cases := []struct {
		name string
		p    ShareRenameParams
		want error
	}{
		{"в существующий каталог", ShareRenameParams{Path: "a/c", Name: "full"}, ErrConflict},
		{"в существующий пустой каталог", ShareRenameParams{Path: "a/c", Name: "empty"}, ErrConflict},
		{"в существующий файл", ShareRenameParams{Path: "z", Name: "file.txt"}, ErrConflict},
		{"в существующую ссылку", ShareRenameParams{Path: "z", Name: "escape"}, ErrConflict},
		{"источника нет", ShareRenameParams{Path: "a/nope", Name: "x"}, ErrNotFound},
		{"источник — ссылка", ShareRenameParams{Path: "a/tolink", Name: "x"}, ErrRefused},
		{"источник — ссылка наружу", ShareRenameParams{Path: "escape", Name: "x"}, ErrRefused},
		{"источник — файл", ShareRenameParams{Path: "file.txt", Name: "x"}, ErrRefused},
		{"ссылка наружу в пути", ShareRenameParams{Path: "escape/secret", Name: "x"}, ErrRefused},
		{"ссылка внутрь в пути", ShareRenameParams{Path: "inner/c", Name: "x"}, ErrRefused},
		{"корень шары", ShareRenameParams{Path: "", Name: "x"}, ErrInvalid},
		{"..", ShareRenameParams{Path: "a/../z", Name: "x"}, ErrInvalid},
		{"в другой родитель", ShareRenameParams{Path: "z", Name: "../x"}, ErrInvalid},
		{"абсолютный путь", ShareRenameParams{Path: outside + "/secret", Name: "x"}, ErrInvalid},
	}
	for _, c := range cases {
		c.p.Pool, c.p.Share = "p1", "docs"
		if _, err := e.ShareRename(ctx, c.p); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, ожидалось %v", c.name, err, c.want)
		}
	}
	// Ничего не перезаписано и не сдвинуто.
	for _, p := range []string{"a/c", "a/full/f", "z", "file.txt", "escape", "a/tolink"} {
		if _, err := os.Lstat(filepath.Join(share, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if ino(t, filepath.Join(share, "a/empty")) != emptyIno || ino(t, filepath.Join(share, "a/c")) != before {
		t.Error("пустой каталог перезаписан")
	}
	if fi, _ := os.Lstat(filepath.Join(share, "escape")); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("ссылка escape перезаписана")
	}
	if _, err := os.Lstat(filepath.Join(outside, "secret")); err != nil {
		t.Error("каталог вне шары затронут")
	}
	if _, err := os.Lstat(filepath.Join(outside, "x")); err == nil {
		t.Error("каталог создан вне шары")
	}
}

func TestShareRmdir(t *testing.T) {
	e, share, outside, _ := dirOpsFixture(t)
	ctx := context.Background()
	res, err := e.ShareRmdir(ctx, ShareRmdirParams{Pool: "p1", Share: "docs", Path: "a/b"})
	if err != nil || res.Path != "a/b" || !res.Changed {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(share, "a/b")); !os.IsNotExist(err) {
		t.Fatal("каталог не удалён")
	}
	cases := []struct {
		name string
		path string
		want error
	}{
		{"непустой", "a/full", ErrConflict},
		{"уже нет", "a/b", ErrNotFound},
		{"цель — ссылка внутрь", "a/tolink", ErrRefused},
		{"цель — ссылка наружу", "escape", ErrRefused},
		{"цель — файл", "file.txt", ErrRefused},
		{"ссылка наружу в пути", "escape/secret", ErrRefused},
		{"ссылка внутрь в пути", "inner/full", ErrRefused},
		{"корень шары", "", ErrInvalid},
		{"..", "a/..", ErrInvalid},
		{"абсолютный путь", outside + "/secret", ErrInvalid},
	}
	for _, c := range cases {
		if _, err := e.ShareRmdir(ctx, ShareRmdirParams{Pool: "p1", Share: "docs", Path: c.path}); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, ожидалось %v", c.name, err, c.want)
		}
	}
	for _, p := range []string{"a/full/f", "a/tolink", "z", "escape", "file.txt"} {
		if _, err := os.Lstat(filepath.Join(share, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(outside, "secret")); err != nil {
		t.Error("каталог вне шары удалён")
	}
	if _, err := os.Lstat(share); err != nil {
		t.Error("корень шары удалён")
	}
}
