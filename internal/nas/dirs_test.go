package nas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanRel(t *testing.T) {
	for _, ok := range []string{"", "a", "a/b", "Фото 2024/лето", "a.b/c-d"} {
		if _, err := CleanRel(ok); err != nil {
			t.Errorf("%q отклонён: %v", ok, err)
		}
	}
	for _, bad := range []string{"/", "/etc", "a/", "..", "../x", "a/../b", "a/..", ".", "a/./b", "a//b", "a\\b", "a\x00b", "a\nb", strings.Repeat("a/", 600)} {
		if _, err := CleanRel(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
}

func TestShareDirsParamsValidate(t *testing.T) {
	good := ShareDirsParams{Pool: "p1", Share: "docs", Path: "a", Depth: 2}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func(*ShareDirsParams){
		func(p *ShareDirsParams) { p.Pool = "../x" },
		func(p *ShareDirsParams) { p.Share = "a/b" },
		func(p *ShareDirsParams) { p.Share = ".." },
		func(p *ShareDirsParams) { p.Depth = 0 },
		func(p *ShareDirsParams) { p.Depth = MaxDirDepth + 1 },
		func(p *ShareDirsParams) { p.Path = "../.." },
	} {
		p := good
		f(&p)
		if p.Validate() == nil {
			t.Errorf("принято: %+v", p)
		}
	}
}

func dirsFixture(t *testing.T) (*Executor, string) {
	t.Helper()
	mount := t.TempDir()
	share := ShareDir(mount, "p1", "docs")
	for _, d := range []string{"a/b/c/d", "a/b2", "z", "unreadable"} {
		if err := os.MkdirAll(filepath.Join(share, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(share, "file.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(share, "a", "f2"), []byte("x"), 0o644)
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(outside, "secret"), 0o755)
	os.Symlink(outside, filepath.Join(share, "escape"))            // наружу
	os.Symlink("a", filepath.Join(share, "inner"))                 // внутрь шары
	os.Symlink(outside, filepath.Join(share, "a", "b", "escape2")) // наружу, глубже
	return &Executor{MountRoot: mount}, outside
}

func names(r ShareDirsResult) string {
	var n []string
	for _, d := range r.Dirs {
		n = append(n, d.Name)
	}
	return strings.Join(n, ",")
}

func TestShareDirsListsOnlyDirs(t *testing.T) {
	e, _ := dirsFixture(t)
	ctx := context.Background()
	r, err := e.ShareDirs(ctx, ShareDirsParams{Pool: "p1", Share: "docs", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	// файлы и ссылки (в т. ч. наружу и внутрь) не показываются
	if got := names(r); got != "a,unreadable,z" {
		t.Fatalf("корень: %s", got)
	}
	if r.Dirs[0].Children != nil {
		t.Errorf("глубина 1: потомки не запрашивались")
	}
	r, err = e.ShareDirs(ctx, ShareDirsParams{Pool: "p1", Share: "docs", Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	a := r.Dirs[0]
	if len(a.Children) != 2 || a.Children[0].Path != "a/b" || a.Children[0].Children[0].Path != "a/b/c" {
		t.Errorf("дерево: %+v", a)
	}
	if len(a.Children[0].Children) != 1 { // escape2 не показан
		t.Errorf("ссылка наружу показана: %v", a.Children[0].Children)
	}
	if a.Children[0].Children[0].Children != nil {
		t.Errorf("глубина 3 превышена")
	}
	r, err = e.ShareDirs(ctx, ShareDirsParams{Pool: "p1", Share: "docs", Path: "a/b", Depth: 1})
	if err != nil || names(r) != "c" || r.Path != "a/b" {
		t.Errorf("подкаталог: %v %+v", err, r)
	}
}

func TestShareDirsPathProtection(t *testing.T) {
	e, outside := dirsFixture(t)
	ctx := context.Background()
	for _, p := range []string{"../", "..", "a/../..", "/etc", outside, "escape", "escape/secret", "inner", "inner/b", "a/b/escape2", "a/b/escape2/secret"} {
		r, err := e.ShareDirs(ctx, ShareDirsParams{Pool: "p1", Share: "docs", Path: p, Depth: 1})
		if err == nil && (!r.Missing || len(r.Dirs) != 0) {
			t.Errorf("путь %q дал результат: %+v", p, r)
		}
	}
	// отсутствующий каталог и файл вместо каталога
	for _, p := range []string{"nope", "file.txt", "a/f2"} {
		r, err := e.ShareDirs(ctx, ShareDirsParams{Pool: "p1", Share: "docs", Path: p, Depth: 1})
		if err != nil || !r.Missing {
			t.Errorf("%q: %v %+v", p, err, r)
		}
	}
	// шара-ссылка наружу: само дерево шары — симлинк на чужой каталог
	e2 := &Executor{MountRoot: t.TempDir()}
	os.MkdirAll(filepath.Join(e2.MountRoot, "p1", "shares"), 0o755)
	os.Symlink(outside, ShareDir(e2.MountRoot, "p1", "docs"))
	r, err := e2.ShareDirs(ctx, ShareDirsParams{Pool: "p1", Share: "docs", Depth: 1})
	if err == nil && len(r.Dirs) != 0 {
		// OpenRoot следует ссылке в самом корне — это путь шары, создаётся helper'ом (install -d), не пользователем
		t.Logf("корень шары-ссылка: %s (создаётся только helper)", names(r))
	}
	// нет шары
	if _, err := e.ShareDirs(ctx, ShareDirsParams{Pool: "p1", Share: "none", Depth: 1}); !errors.Is(err, ErrRefused) {
		t.Errorf("нет каталога шары: %v", err)
	}
}

func TestShareDirsLimits(t *testing.T) {
	e := &Executor{MountRoot: t.TempDir()}
	share := ShareDir(e.MountRoot, "p1", "big")
	os.MkdirAll(share, 0o755)
	for i := 0; i < MaxDirEntries+20; i++ {
		os.Mkdir(filepath.Join(share, "d"+strings.Repeat("0", 3)+string(rune('a'+i/26))+string(rune('a'+i%26))), 0o755)
	}
	r, err := e.ShareDirs(context.Background(), ShareDirsParams{Pool: "p1", Share: "big", Depth: 1})
	if err != nil || len(r.Dirs) != MaxDirEntries || !r.Partial {
		t.Fatalf("%v %d %v", err, len(r.Dirs), r.Partial)
	}
}

func TestServiceShareDirs(t *testing.T) {
	s, h := newService(t)
	ctx := context.Background()
	if _, err := s.CreateShare(ctx, "docs", "p1", ""); err != nil {
		t.Fatal(err)
	}
	h.reply = map[string]string{OpShareDirs: `{"path":"a","dirs":[{"name":"b","path":"a/b","children":null}]}`}
	r, err := s.ShareDirs(ctx, "docs", "a", 0)
	if err != nil || len(r.Dirs) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	if !strings.Contains(h.raw[OpShareDirs][0], `"pool":"p1"`) || !strings.Contains(h.raw[OpShareDirs][0], `"depth":1`) {
		t.Errorf("параметры: %s", h.raw[OpShareDirs][0])
	}
	n := h.count(OpShareDirs)
	for _, c := range []struct {
		share, path string
		depth       int
	}{{"docs", "../x", 1}, {"docs", "/etc", 1}, {"docs", "a", 9}, {"docs", "a", -1}, {"a/b", "", 1}} {
		if _, err := s.ShareDirs(ctx, c.share, c.path, c.depth); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if h.count(OpShareDirs) != n {
		t.Error("невалидный запрос дошёл до helper")
	}
	if _, err := s.ShareDirs(ctx, "nope", "", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("нет папки: %v", err)
	}
	h.reply = map[string]string{OpShareDirs: `{"path":"x","dirs":[],"missing":true}`}
	if _, err := s.ShareDirs(ctx, "docs", "x", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("нет каталога: %v", err)
	}
}

// Методы сервиса — проверка до helper, пул из БД, ошибки helper
// возвращаются как есть.
func TestServiceShareDirOps(t *testing.T) {
	s, h := newService(t)
	ctx := context.Background()
	if _, err := s.CreateShare(ctx, "docs", "p1", ""); err != nil {
		t.Fatal(err)
	}
	h.reply = map[string]string{
		OpShareMkdir:  `{"path":"a/new","changed":true}`,
		OpShareRename: `{"path":"a/b2","changed":true}`,
		OpShareRmdir:  `{"path":"a/b2","changed":true}`,
		OpShareMove:   `{"path":"c/b2","changed":true}`,
	}
	if r, err := s.ShareMove(ctx, "docs", "a/b2", "c"); err != nil || r.Path != "c/b2" || !r.Changed {
		t.Fatalf("move: %v %+v", err, r)
	}
	if got := h.raw[OpShareMove][0]; got != `{"pool":"p1","share":"docs","path":"a/b2","to":"c"}` {
		t.Errorf("параметры move: %s", got)
	}
	if r, err := s.ShareMkdir(ctx, "docs", "a", "new"); err != nil || r.Path != "a/new" || !r.Changed {
		t.Fatalf("mkdir: %v %+v", err, r)
	}
	if got := h.raw[OpShareMkdir][0]; got != `{"pool":"p1","share":"docs","path":"a","name":"new"}` {
		t.Errorf("параметры mkdir: %s", got)
	}
	if r, err := s.ShareRename(ctx, "docs", "a/b", "b2"); err != nil || r.Path != "a/b2" {
		t.Fatalf("rename: %v %+v", err, r)
	}
	if got := h.raw[OpShareRename][0]; got != `{"pool":"p1","share":"docs","path":"a/b","name":"b2"}` {
		t.Errorf("параметры rename: %s", got)
	}
	if _, err := s.ShareRmdir(ctx, "docs", "a/b2"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if got := h.raw[OpShareRmdir][0]; got != `{"pool":"p1","share":"docs","path":"a/b2"}` {
		t.Errorf("параметры rmdir: %s", got)
	}
	n := len(h.ops)
	bad := []func() error{
		func() error { _, err := s.ShareMkdir(ctx, "docs", "../x", "a"); return err },
		func() error { _, err := s.ShareMkdir(ctx, "docs", "/etc", "a"); return err },
		func() error { _, err := s.ShareMkdir(ctx, "docs", "", ".."); return err },
		func() error { _, err := s.ShareMkdir(ctx, "docs", "", "a/b"); return err },
		func() error { _, err := s.ShareMkdir(ctx, "docs", "", ""); return err },
		func() error { _, err := s.ShareMkdir(ctx, "a/b", "", "x"); return err },
		func() error { _, err := s.ShareRename(ctx, "docs", "", "x"); return err },
		func() error { _, err := s.ShareRename(ctx, "docs", "a", "x\n"); return err },
		func() error { _, err := s.ShareRmdir(ctx, "docs", ""); return err },
		func() error { _, err := s.ShareMove(ctx, "docs", "", "c"); return err },
		func() error { _, err := s.ShareMove(ctx, "docs", "a", "a/b"); return err },
		func() error { _, err := s.ShareMove(ctx, "docs", "a", "../x"); return err },
		func() error { _, err := s.ShareRmdir(ctx, "docs", "a/../.."); return err },
	}
	for i, f := range bad {
		if err := f(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%d: %v", i, err)
		}
	}
	if len(h.ops) != n {
		t.Errorf("невалидный запрос дошёл до helper: %v", h.ops[n:])
	}
	if _, err := s.ShareMkdir(ctx, "nope", "", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("нет папки: %v", err)
	}
	boom := errors.New("helper: conflict")
	h.fail = map[string]error{OpShareRmdir: boom}
	if _, err := s.ShareRmdir(ctx, "docs", "a"); !errors.Is(err, boom) {
		t.Errorf("ошибка helper: %v", err)
	}
}
