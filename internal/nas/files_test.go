//go:build linux

package nas

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Операции helper файлового менеджера.
func filesFixture(t *testing.T) (*Executor, string, string, *[][2]int) {
	t.Helper()
	e, share, outside, _ := dirOpsFixture(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "passwd"), []byte("root:x:0:0::/root:/bin/sh\nann:x:1001:1001::/home/ann:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "group"), []byte("gamayun-users:x:990:ann\n"), 0o644)
	e.PasswdFile, e.GroupFile = filepath.Join(dir, "passwd"), filepath.Join(dir, "group")
	var owners [][2]int
	e.FileChown = func(fd, uid, gid int) error { owners = append(owners, [2]int{uid, gid}); return nil }
	os.WriteFile(filepath.Join(outside, "secret", "s.txt"), []byte("секрет"), 0o644)
	return e, share, outside, &owners
}

func loc(p string) FileLoc { return FileLoc{Pool: "p1", Share: "docs", User: "ann", Path: p} }

func TestFilesListOpen(t *testing.T) {
	e, share, _, _ := filesFixture(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(share, TempPrefix+"0123456789abcdef"), nil, 0o600)
	r, err := e.FilesList(ctx, FilesLocParams{loc("")})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, x := range r.Entries {
		types[x.Name] = x.Type
	}
	if types["a"] != "dir" || types["file.txt"] != "file" || types["escape"] != "link" {
		t.Fatalf("типы: %v", types)
	}
	if _, ok := types[TempPrefix+"0123456789abcdef"]; ok {
		t.Fatal("временный файл в списке")
	}
	for _, p := range []string{"escape", "escape/secret", "inner", "up/etc"} {
		if _, err := e.FilesList(ctx, FilesLocParams{loc(p)}); !errors.Is(err, ErrRefused) && !errors.Is(err, ErrNotFound) {
			t.Errorf("список через ссылку %s: %v", p, err)
		}
	}
	h, err := e.FilesOpen(ctx, FilesLocParams{loc("file.txt")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(h.File)
	h.File.Close()
	if string(b) != "x" || h.Info.(FileInfo).Size != 1 {
		t.Fatalf("open: %q %+v", b, h.Info)
	}
	for _, p := range []string{"escape", "escape/secret/s.txt", "inner/full/f", "a", "nope"} {
		if h, err := e.FilesOpen(ctx, FilesLocParams{loc(p)}); err == nil {
			h.File.Close()
			t.Errorf("open %s: должен быть отказ", p)
		}
	}
	if _, err := e.FilesOpen(ctx, FilesLocParams{loc("")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("open корня: %v", err)
	}
}

func TestFilesUploadCommit(t *testing.T) {
	e, share, _, owners := filesFixture(t)
	ctx := context.Background()
	up := func(dir, name, conflict, data string) (FilesNameResult, error) {
		h, err := e.FilesCreate(ctx, FilesCreateParams{FileLoc: loc(dir), Size: int64(len(data))})
		if err != nil {
			return FilesNameResult{}, err
		}
		h.File.WriteString(data)
		h.File.Close()
		temp := h.Info.(FilesCreateResult).Temp
		st, _ := os.Stat(filepath.Join(share, dir, temp))
		if st.Mode().Perm() != 0o660 {
			t.Errorf("режим временного файла %v", st.Mode())
		}
		r, err := e.FilesCommit(ctx, FilesCommitParams{FileLoc: loc(dir), Temp: temp, Name: name, OnConflict: conflict})
		if err != nil {
			e.FilesDiscard(ctx, FilesDiscardParams{FileLoc: loc(dir), Temp: temp})
		}
		return r, err
	}
	if r, err := up("a", "new.txt", ConflictError, "один"); err != nil || r.Path != "a/new.txt" {
		t.Fatalf("загрузка: %+v %v", r, err)
	}
	if (*owners)[0] != [2]int{1001, 990} {
		t.Fatalf("владелец %v", *owners)
	}
	if _, err := up("a", "new.txt", ConflictError, "два"); !errors.Is(err, ErrConflict) {
		t.Fatalf("конфликт: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(share, "a/new.txt")); string(b) != "один" {
		t.Fatal("перезаписан существующий файл")
	}
	if r, err := up("a", "new.txt", ConflictRename, "два"); err != nil || r.Path != "a/new (2).txt" {
		t.Fatalf("rename: %+v %v", r, err)
	}
	if r, err := up("a", "new.txt", ConflictRename, "три"); err != nil || r.Path != "a/new (3).txt" {
		t.Fatalf("rename 3: %+v %v", r, err)
	}
	ents, _ := os.ReadDir(filepath.Join(share, "a"))
	for _, x := range ents {
		if len(x.Name()) > len(TempPrefix) && x.Name()[:len(TempPrefix)] == TempPrefix {
			t.Fatal("остался временный файл")
		}
	}
	for _, d := range []string{"escape", "inner", "escape/secret"} {
		if _, err := up(d, "x.txt", ConflictError, "x"); err == nil {
			t.Errorf("загрузка через ссылку %s прошла", d)
		}
	}
	if _, err := e.FilesCreate(ctx, FilesCreateParams{FileLoc: loc("a"), Size: 1 << 62}); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("нет места: %v", err)
	}
	// Подменённый временный файл (каталог с именем-шаблоном) — отказ.
	os.Mkdir(filepath.Join(share, TempPrefix+"aaaaaaaaaaaaaaaa"), 0o700)
	if _, err := e.FilesCommit(ctx, FilesCommitParams{FileLoc: loc(""), Temp: TempPrefix + "aaaaaaaaaaaaaaaa", Name: "q", OnConflict: ConflictError}); !errors.Is(err, ErrRefused) {
		t.Fatalf("подмена временного: %v", err)
	}
	bad := []FilesCommitParams{
		{FileLoc: loc(""), Temp: "x", Name: "q", OnConflict: ConflictError},
		{FileLoc: loc(""), Temp: TempPrefix + "aaaaaaaaaaaaaaaa", Name: "a/b", OnConflict: ConflictError},
		{FileLoc: loc(""), Temp: TempPrefix + "aaaaaaaaaaaaaaaa", Name: TempPrefix + "x", OnConflict: ConflictError},
		{FileLoc: loc(""), Temp: TempPrefix + "aaaaaaaaaaaaaaaa", Name: "CON", OnConflict: ConflictError},
		{FileLoc: loc(""), Temp: TempPrefix + "aaaaaaaaaaaaaaaa", Name: "q", OnConflict: "replace"},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("неверные параметры %d приняты", i)
		}
	}
	if (FileLoc{Pool: "p1", Share: "docs", User: "ann", Path: "../x"}).validate() == nil {
		t.Error("путь с .. принят")
	}
}

func TestFilesRenameMoveRemove(t *testing.T) {
	e, share, outside, _ := filesFixture(t)
	ctx := context.Background()
	if r, err := e.FilesRename(ctx, FilesNameParams{FileLoc: loc("file.txt"), Name: "f2.txt"}); err != nil || r.Path != "f2.txt" {
		t.Fatalf("rename файла: %+v %v", r, err)
	}
	if _, err := e.FilesRename(ctx, FilesNameParams{FileLoc: loc("f2.txt"), Name: "z"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("rename в занятое: %v", err)
	}
	// Ссылка переименовывается сама, цель не меняется.
	if _, err := e.FilesRename(ctx, FilesNameParams{FileLoc: loc("escape"), Name: "esc2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatal("цель ссылки пострадала")
	}
	if r, err := e.FilesMove(ctx, FilesMoveParams{FileLoc: loc("f2.txt"), To: "a/b"}); err != nil || r.Path != "a/b/f2.txt" {
		t.Fatalf("move: %+v %v", r, err)
	}
	if _, err := e.FilesMove(ctx, FilesMoveParams{FileLoc: loc("a/b/f2.txt"), To: "esc2"}); err == nil {
		t.Fatal("перенос в ссылку прошёл")
	}
	if r, err := e.FilesMkdir(ctx, FilesNameParams{FileLoc: loc("a"), Name: "new"}); err != nil || r.Path != "a/new" {
		t.Fatalf("mkdir: %+v %v", r, err)
	}
	// Рекурсивное удаление: ссылка внутри удаляется сама, цель цела.
	os.Symlink(outside, filepath.Join(share, "a", "b", "out"))
	r, err := e.FilesRemove(ctx, FilesLocParams{loc("a")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Files < 3 || r.Dirs < 4 {
		t.Fatalf("удалено %+v", r)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret", "s.txt")); err != nil {
		t.Fatal("удаление прошло по ссылке наружу")
	}
	if _, err := os.Lstat(filepath.Join(share, "a")); !os.IsNotExist(err) {
		t.Fatal("каталог не удалён")
	}
	if _, err := e.FilesRemove(ctx, FilesLocParams{loc("")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("удаление корня: %v", err)
	}
	if _, err := e.FilesRemove(ctx, FilesLocParams{loc("esc2/secret")}); err == nil {
		t.Fatal("удаление через ссылку прошло")
	}
}

func TestFilesHomes(t *testing.T) {
	e, _, _, owners := filesFixture(t)
	ctx := context.Background()
	home := filepath.Join(HomesDir(e.MountRoot, "p1"), "ann")
	os.MkdirAll(home, 0o700)
	l := FileLoc{Pool: "p1", Share: HomesShare, User: "ann"}
	h, err := e.FilesCreate(ctx, FilesCreateParams{FileLoc: l})
	if err != nil {
		t.Fatal(err)
	}
	h.File.Close()
	if (*owners)[0] != [2]int{1001, 1001} {
		t.Fatalf("владелец в домашней %v", *owners)
	}
	st, _ := os.Stat(filepath.Join(home, h.Info.(FilesCreateResult).Temp))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("режим %v", st.Mode())
	}
	l.Path = ""
	if _, err := e.FilesMkdir(ctx, FilesNameParams{FileLoc: l, Name: "d"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(home, "d")); st.Mode().Perm() != 0o700 {
		t.Fatalf("режим каталога %v", st.Mode())
	}
	if _, err := e.FilesCreate(ctx, FilesCreateParams{FileLoc: FileLoc{Pool: "p1", Share: HomesShare, User: "bob"}}); err == nil {
		t.Fatal("нет пользователя ОС — должен быть отказ")
	}
}

func TestConflictName(t *testing.T) {
	for in, want := range map[string]string{"a.txt": "a (2).txt", "a": "a (2)", ".bashrc": ".bashrc (2)", "a.tar.gz": "a.tar (2).gz"} {
		if got := ConflictName(in, 2); got != want {
			t.Errorf("%s → %s, ждали %s", in, got, want)
		}
	}
}
