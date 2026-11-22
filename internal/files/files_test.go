//go:build linux

package files

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/events"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
)

type fakeRoots map[string][]nas.FileRoot

func (f fakeRoots) FileRoots(_ context.Context, user string) ([]nas.FileRoot, error) {
	return f[user], nil
}

type fixture struct {
	svc   *Service
	mount string
	roots fakeRoots
}

func newFixture(t *testing.T, withTasks bool) *fixture {
	t.Helper()
	mount := t.TempDir()
	for _, sh := range []string{"docs", "media", "ro"} {
		os.MkdirAll(nas.ShareDir(mount, "p1", sh), 0o755)
	}
	d := nas.ShareDir(mount, "p1", "docs")
	os.MkdirAll(filepath.Join(d, "a/b"), 0o755)
	os.WriteFile(filepath.Join(d, "a/one.txt"), []byte("один"), 0o644)
	os.WriteFile(filepath.Join(d, "a/b/two.txt"), []byte("два-два"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("секрет"), 0o644)
	os.Symlink(outside, filepath.Join(d, "a", "out"))
	etc := t.TempDir()
	os.WriteFile(filepath.Join(etc, "passwd"), []byte("ann:x:1001:1001::/h:/bin/sh\nbob:x:1002:1002::/h:/bin/sh\n"), 0o644)
	os.WriteFile(filepath.Join(etc, "group"), []byte("gamayun-users:x:990:\n"), 0o644)
	ex := &nas.Executor{MountRoot: mount, Mounted: func(string) bool { return true },
		PasswdFile: filepath.Join(etc, "passwd"), GroupFile: filepath.Join(etc, "group"),
		DirChown: func(int) error { return nil }, FileChown: func(int, int, int) error { return nil }}
	roots := fakeRoots{
		"ann": {{Name: "docs", Pool: "p1", Level: nas.LevelRW}, {Name: "media", Pool: "p1", Level: nas.LevelRW}, {Name: "ro", Pool: "p1", Level: nas.LevelRead}},
		"bob": {{Name: "ro", Pool: "p1", Level: nas.LevelRead}},
	}
	s := &Service{Helper: &ExecHelper{Exec: ex}, Roots: roots, Logger: slog.New(slog.DiscardHandler)}
	if withTasks {
		db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		s.Tasks = tasks.New(db, events.NewLog(db), tasks.Options{Logger: slog.New(slog.DiscardHandler)})
		if err := s.Init(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.Tasks.Shutdown(ctx)
			db.Close()
		})
	}
	return &fixture{svc: s, mount: mount, roots: roots}
}

func (f *fixture) path(share, p string) string {
	return filepath.Join(nas.ShareDir(f.mount, "p1", share), p)
}

func TestAccess(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	if _, err := f.svc.ListDir(ctx, "bob", "docs", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("чужая папка: %v", err)
	}
	if _, err := f.svc.Upload(ctx, "bob", "ro", "", "x.txt", "", 1, strings.NewReader("x")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("запись в read: %v", err)
	}
	if _, err := f.svc.Mkdir(ctx, "ann", "ro", "", "d"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mkdir в read: %v", err)
	}
	if _, err := f.svc.Transfer(ctx, "bob", TransferRequest{Op: OpDelete, Root: "ro", Paths: []string{"x"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("удаление в read: %v", err)
	}
	if _, err := f.svc.ListDir(ctx, "ann", "docs", "../x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("путь с ..: %v", err)
	}
	if d, err := f.svc.ListDir(ctx, "bob", "ro", ""); err != nil || d.Level != nas.LevelRead {
		t.Fatalf("чтение: %+v %v", d, err)
	}
	var re *helper.RemoteError
	if _, err := f.svc.ListDir(ctx, "ann", "docs", "a/out"); !errors.As(err, &re) || re.Code != helper.CodeRefused {
		t.Fatalf("ссылка наружу: %v", err)
	}
}

func TestUploadDownloadZip(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	p, err := f.svc.Upload(ctx, "ann", "docs", "a", "новый файл.txt", "", 5, strings.NewReader("hello"))
	if err != nil || p != "a/новый файл.txt" {
		t.Fatalf("загрузка: %q %v", p, err)
	}
	if _, err := f.svc.Upload(ctx, "ann", "docs", "a", "новый файл.txt", "", 1, strings.NewReader("x")); err == nil {
		t.Fatal("перезапись при error")
	}
	if p, err := f.svc.Upload(ctx, "ann", "docs", "a", "новый файл.txt", nas.ConflictRename, 1, strings.NewReader("x")); err != nil || p != "a/новый файл (2).txt" {
		t.Fatalf("rename: %q %v", p, err)
	}
	// Короткое тело — ошибка, временный файл удалён.
	if _, err := f.svc.Upload(ctx, "ann", "docs", "a", "short.txt", "", 10, strings.NewReader("abc")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("короткое тело: %v", err)
	}
	ents, _ := os.ReadDir(f.path("docs", "a"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), nas.TempPrefix) || e.Name() == "short.txt" {
			t.Fatalf("остался %s", e.Name())
		}
	}
	file, fi, err := f.svc.Open(ctx, "ann", "docs", "a/новый файл.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(file)
	file.Close()
	if string(b) != "hello" || fi.Size != 5 {
		t.Fatalf("скачивание: %q %+v", b, fi)
	}
	var buf bytes.Buffer
	if err := f.svc.Zip(ctx, "ann", "docs", "a", &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
		if zf.Name == "b/two.txt" {
			rc, _ := zf.Open()
			c, _ := io.ReadAll(rc)
			rc.Close()
			if string(c) != "два-два" {
				t.Fatalf("содержимое в архиве %q", c)
			}
		}
	}
	sort.Strings(names)
	want := "b/ b/two.txt one.txt новый файл (2).txt новый файл.txt"
	if strings.Join(names, " ") != want {
		t.Fatalf("архив: %v (ссылка out не должна попасть)", names)
	}
}

func waitTask(t *testing.T, m *tasks.Manager, id string) tasks.Task {
	t.Helper()
	for i := 0; i < 200; i++ {
		tk, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.State != tasks.Queued && tk.State != tasks.Running {
			return tk
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("задача не завершилась")
	return tasks.Task{}
}

func TestTransfer(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	run := func(req TransferRequest) tasks.Task {
		t.Helper()
		tk, err := f.svc.Transfer(ctx, "ann", req)
		if err != nil {
			t.Fatal(err)
		}
		return waitTask(t, f.svc.Tasks, tk.ID)
	}
	// Копия папки в другую общую папку; ссылка внутри не копируется.
	tk := run(TransferRequest{Op: OpCopy, Root: "docs", Paths: []string{"a"}, ToRoot: "media"})
	if tk.State != tasks.Succeeded {
		t.Fatalf("копия: %+v", tk)
	}
	if b, _ := os.ReadFile(f.path("media", "a/b/two.txt")); string(b) != "два-два" {
		t.Fatalf("копия содержимого %q", b)
	}
	if _, err := os.Lstat(f.path("media", "a/out")); !os.IsNotExist(err) {
		t.Fatal("ссылка скопирована")
	}
	// Повтор: error — сбой; rename — «a (2)».
	if tk := run(TransferRequest{Op: OpCopy, Root: "docs", Paths: []string{"a"}, ToRoot: "media"}); tk.State != tasks.Failed {
		t.Fatalf("конфликт: %+v", tk)
	}
	if tk := run(TransferRequest{Op: OpCopy, Root: "docs", Paths: []string{"a/one.txt", "a"}, ToRoot: "media", OnConflict: nas.ConflictRename}); tk.State != tasks.Succeeded {
		t.Fatalf("rename: %+v", tk)
	}
	if _, err := os.Stat(f.path("media", "a (2)/b/two.txt")); err != nil {
		t.Fatal("нет a (2)")
	}
	if _, err := os.Stat(f.path("media", "one.txt")); err != nil {
		t.Fatal("нет one.txt")
	}
	// Перенос внутри папки — переименованием (тот же inode).
	st0, _ := os.Stat(f.path("docs", "a/one.txt"))
	if tk := run(TransferRequest{Op: OpMove, Root: "docs", Paths: []string{"a/one.txt"}, ToRoot: "docs", ToPath: "a/b"}); tk.State != tasks.Succeeded {
		t.Fatalf("перенос: %+v", tk)
	}
	st1, err := os.Stat(f.path("docs", "a/b/one.txt"))
	if err != nil || !os.SameFile(st0, st1) {
		t.Fatal("перенос внутри папки не переименованием")
	}
	// Перенос между папками — копия и удаление источника.
	if tk := run(TransferRequest{Op: OpMove, Root: "docs", Paths: []string{"a/b"}, ToRoot: "media"}); tk.State != tasks.Succeeded {
		t.Fatalf("перенос между папками: %+v", tk)
	}
	if _, err := os.Stat(f.path("media", "b/one.txt")); err != nil {
		t.Fatal("нет в цели")
	}
	if _, err := os.Stat(f.path("docs", "a/b")); !os.IsNotExist(err) {
		t.Fatal("источник остался")
	}
	// Удаление: ссылка удаляется сама, цель цела.
	if tk := run(TransferRequest{Op: OpDelete, Root: "docs", Paths: []string{"a"}}); tk.State != tasks.Succeeded {
		t.Fatalf("удаление: %+v", tk)
	}
	if _, err := os.Stat(f.path("docs", "a")); !os.IsNotExist(err) {
		t.Fatal("не удалено")
	}
	// Отказы при постановке.
	bad := []TransferRequest{
		{Op: OpCopy, Root: "media", Paths: []string{"a"}, ToRoot: "media", ToPath: "a/b"},
		{Op: OpCopy, Root: "media", Paths: []string{""}, ToRoot: "docs"},
		{Op: OpCopy, Root: "media", Paths: []string{"a", "a"}, ToRoot: "docs"},
		{Op: "chmod", Root: "media", Paths: []string{"a"}},
		{Op: OpCopy, Root: "media", Paths: []string{"a"}, ToRoot: "ro"},
		{Op: OpCopy, Root: "media", Paths: nil, ToRoot: "docs"},
	}
	for i, req := range bad {
		if _, err := f.svc.Transfer(ctx, "ann", req); err == nil {
			t.Errorf("отказ %d: принято", i)
		}
	}
	// Права сняты между постановкой и запуском — задача не выполняется.
	raw := []byte(`{"op":"copy","root":"media","paths":["a"],"to_root":"docs","user":"ann","src":{"pool":"p1","share":"media","user":"ann","path":""},"dst":{"pool":"p1","share":"docs","user":"ann","path":""}}`)
	f.roots["ann"] = f.roots["ann"][:1]
	if _, err := f.svc.runTransfer(ctx, raw, func(int) {}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("без прав при запуске: %v", err)
	}
	if _, err := os.Stat(f.path("docs", "a/b")); !os.IsNotExist(err) {
		t.Fatal("скопировано без прав")
	}
}

// Отмена во время копирования: недописанный файл (временный) удалён.
func TestTransferCancelled(t *testing.T) {
	f := newFixture(t, false)
	big := make([]byte, 8<<20)
	os.WriteFile(f.path("docs", "big.bin"), big, 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	tr := &transfer{s: f.svc, ctx: ctx, progress: func(int) {}, p: transferParams{
		TransferRequest: TransferRequest{Op: OpCopy, Paths: []string{"big.bin"}, OnConflict: nas.ConflictError},
		User:            "ann",
		Src:             nas.FileLoc{Pool: "p1", Share: "docs", User: "ann"},
		Dst:             nas.FileLoc{Pool: "p1", Share: "media", User: "ann"},
	}}
	tr.progress = func(p int) {
		if p > 0 {
			cancel()
		}
	}
	if _, err := tr.run(); !errors.Is(err, context.Canceled) {
		t.Fatalf("ждали отмену: %v", err)
	}
	ents, _ := os.ReadDir(nas.ShareDir(f.mount, "p1", "media"))
	if len(ents) != 0 {
		t.Fatalf("в цели осталось: %v", ents)
	}
}
