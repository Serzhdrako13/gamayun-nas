package snapshots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gamayun-nas/internal/nas"
)

type runFunc func(path string, args []string) nas.CmdResult

func (f runFunc) Run(_ context.Context, _ time.Duration, _ []byte, path string, args ...string) (nas.CmdResult, error) {
	return f(path, args), nil
}

type tenv struct {
	e     *Executor
	share string // каталог папки docs на пуле p1
	fs    nas.FSInfo
	cmds  []string
	now   time.Time
}

// cloneTree — копия дерева с режимами, временем и ссылками (как снапшот).
func cloneTree(src, dst string) error {
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		d := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, d)
		case info.IsDir():
			return os.Mkdir(d, 0o700)
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(d, b, 0o600); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Режимы и время — после содержимого (запись меняет время каталогов).
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		d := filepath.Join(dst, rel)
		if err := os.Chmod(d, info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
			return err
		}
		return os.Chtimes(d, info.ModTime(), info.ModTime())
	})
}

// newEnv — пул p1 с папкой docs; «btrfs subvolume snapshot -r» в подмене
// копирует дерево (cloneTree), «delete» — удаляет каталог.
func newEnv(t *testing.T) *tenv {
	t.Helper()
	root := t.TempDir()
	v := &tenv{fs: nas.FSInfo{Btrfs: true, Subvol: true, Total: 1000, Avail: 500}, now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	v.share = nas.ShareDir(root, "p1", "docs")
	if err := os.MkdirAll(v.share, 0o770); err != nil {
		t.Fatal(err)
	}
	v.e = &Executor{
		MountRoot: root,
		Mounted:   func(dir string) bool { st, err := os.Stat(dir); return err == nil && st.IsDir() },
		FS:        func(string) (nas.FSInfo, error) { return v.fs, nil },
		Now:       func() time.Time { return v.now },
		Local:     time.UTC,
		Runner: runFunc(func(path string, args []string) nas.CmdResult {
			v.cmds = append(v.cmds, path+" "+strings.Join(args, " "))
			switch {
			case len(args) == 5 && args[0] == "subvolume" && args[1] == "snapshot" && args[2] == "-r":
				if err := cloneTree(args[3], args[4]); err != nil {
					return nas.CmdResult{ExitCode: 1, Stderr: []byte(err.Error())}
				}
			case len(args) == 3 && args[0] == "subvolume" && args[1] == "delete":
				if err := os.RemoveAll(args[2]); err != nil {
					return nas.CmdResult{ExitCode: 1, Stderr: []byte(err.Error())}
				}
			}
			return nas.CmdResult{}
		}),
	}
	return v
}

func write(t *testing.T, p, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func (v *tenv) snap(t *testing.T, auto bool) string {
	t.Helper()
	r, err := v.e.Create(context.Background(), CreateParams{Pool: "p1", Share: "docs", Auto: auto, MinFreePct: 10})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name == "" {
		t.Fatalf("снапшот не создан: %+v", r)
	}
	v.now = v.now.Add(time.Hour)
	return r.Name
}

func TestCreateListDelete(t *testing.T) {
	v := newEnv(t)
	write(t, filepath.Join(v.share, "a.txt"), "раз", 0o660)
	manual := v.snap(t, false)
	auto := v.snap(t, true)
	if manual != "@GMT-2026.11.18-10.00.00" {
		t.Fatalf("имя: %s", manual)
	}
	want := "/usr/bin/btrfs subvolume snapshot -r " + v.share + " " + filepath.Join(v.e.MountRoot, "p1/.snapshots/docs", manual)
	if v.cmds[0] != want {
		t.Fatalf("команда: %s\nожидалась: %s", v.cmds[0], want)
	}
	l, err := v.e.List(context.Background(), ShareParams{Pool: "p1", Share: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Subvolume || len(l.Snapshots) != 2 || l.Snapshots[0].Name != auto || !l.Snapshots[0].Auto || l.Snapshots[1].Auto {
		t.Fatalf("список: %+v", l)
	}
	if !l.Snapshots[1].Time.Equal(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("время: %v", l.Snapshots[1].Time)
	}
	// Каталоги снапшотов: .snapshots — только проход, папки — чтение, запись — никому, кроме владельца.
	for p, mode := range map[string]os.FileMode{".snapshots": SnapRootMode, ".snapshots/docs": SnapShareMode, ".snapshots/docs/.auto": autoMode} {
		st, err := os.Stat(filepath.Join(v.e.MountRoot, "p1", p))
		if err != nil || st.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", p, st.Mode(), err)
		}
	}
	if _, err := v.e.Delete(context.Background(), SnapParams{Pool: "p1", Share: "docs", Name: auto}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(v.e.MountRoot, "p1/.snapshots/docs/.auto", auto)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("отметка не удалена: %v", err)
	}
	if _, err := v.e.Delete(context.Background(), SnapParams{Pool: "p1", Share: "docs", Name: auto}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("повторное удаление: %v", err)
	}
	l, _ = v.e.List(context.Background(), ShareParams{Pool: "p1", Share: "docs"})
	if len(l.Snapshots) != 1 || l.Snapshots[0].Name != manual {
		t.Fatalf("после удаления: %+v", l)
	}
}

func TestCreateRefusals(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	// Обычный каталог (папка, созданная до появления снимков).
	v.fs.Subvol = false
	if _, err := v.e.Create(ctx, CreateParams{Pool: "p1", Share: "docs"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не подтом: %v", err)
	}
	if l, _ := v.e.List(ctx, ShareParams{Pool: "p1", Share: "docs"}); l.Subvolume {
		t.Fatal("List: Subvolume должно быть false")
	}
	v.fs.Subvol = true
	// Мало места: по расписанию — пропуск без снапшота, вручную — снапшот.
	v.fs.Avail = 50 // 5 %
	r, err := v.e.Create(ctx, CreateParams{Pool: "p1", Share: "docs", Auto: true, MinFreePct: 10})
	if err != nil || r.Skipped != SkipLowSpace || r.Name != "" || len(v.cmds) != 0 {
		t.Fatalf("мало места: %+v %v %v", r, err, v.cmds)
	}
	if r, err := v.e.Create(ctx, CreateParams{Pool: "p1", Share: "docs"}); err != nil || r.Name == "" {
		t.Fatalf("вручную при нехватке места: %+v %v", r, err)
	}
	// Только чтение: по расписанию — пропуск, вручную — отказ.
	v.fs.Avail, v.fs.ReadOnly = 500, true
	v.now = v.now.Add(time.Second)
	if r, err := v.e.Create(ctx, CreateParams{Pool: "p1", Share: "docs", Auto: true}); err != nil || r.Skipped != SkipReadOnly {
		t.Fatalf("ro по расписанию: %+v %v", r, err)
	}
	if _, err := v.e.Create(ctx, CreateParams{Pool: "p1", Share: "docs"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("ro вручную: %v", err)
	}
	// Тот же момент — конфликт имени.
	v.fs.ReadOnly = false
	v.now = v.now.Add(-time.Second)
	if _, err := v.e.Create(ctx, CreateParams{Pool: "p1", Share: "docs"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("повтор в ту же секунду: %v", err)
	}
	// Пул не смонтирован.
	v.e.Mounted = func(string) bool { return false }
	if _, err := v.e.Create(ctx, CreateParams{Pool: "p1", Share: "docs"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не смонтирован: %v", err)
	}
}

func TestParamsValidate(t *testing.T) {
	bad := []PathParams{
		{Pool: "p1", Share: "docs", Name: "x", Path: ""},
		{Pool: "p1", Share: "docs", Name: "@GMT-2026.13.18-10.00.00"}, // месяц 13
		{Pool: "p1", Share: "docs", Name: "@GMT-2026.11.18-10.00.00/..", Path: ""},
		{Pool: "p1", Share: "../x", Name: "@GMT-2026.11.18-10.00.00"},
		{Pool: "P1", Share: "docs", Name: "@GMT-2026.11.18-10.00.00"},
		{Pool: "p1", Share: "docs", Name: "@GMT-2026.11.18-10.00.00", Path: "../etc"},
		{Pool: "p1", Share: "docs", Name: "@GMT-2026.11.18-10.00.00", Path: "/etc"},
		{Pool: "p1", Share: "docs", Name: "@GMT-2026.11.18-10.00.00", Path: "a/./b"},
	}
	for _, p := range bad {
		if p.Validate() == nil {
			t.Errorf("должно быть отвергнуто: %+v", p)
		}
	}
	if err := (PathParams{Pool: "p1", Share: "homes", Name: "@GMT-2026.11.18-10.00.00", Path: "ann/Документы"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (CreateParams{Pool: "p1", Share: "docs", MinFreePct: 51}).Validate() == nil {
		t.Fatal("min_free_pct > 50")
	}
}

func TestBrowse(t *testing.T) {
	v := newEnv(t)
	write(t, filepath.Join(v.share, "b.txt"), "12345", 0o660)
	write(t, filepath.Join(v.share, "Отчёты/q1.txt"), "x", 0o660)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "s", 0o600)
	if err := os.Symlink(outside, filepath.Join(v.share, "lnk")); err != nil {
		t.Fatal(err)
	}
	name := v.snap(t, false)
	ctx := context.Background()
	r, err := v.e.Browse(ctx, PathParams{Pool: "p1", Share: "docs", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range r.Entries {
		got = append(got, e.Type+":"+e.Name)
	}
	if strings.Join(got, ",") != "dir:Отчёты,file:b.txt,link:lnk" || r.Entries[1].Size != 5 {
		t.Fatalf("содержимое: %v %+v", got, r.Entries)
	}
	if r, err := v.e.Browse(ctx, PathParams{Pool: "p1", Share: "docs", Name: name, Path: "Отчёты"}); err != nil || len(r.Entries) != 1 {
		t.Fatalf("вложенный: %+v %v", r, err)
	}
	if _, err := v.e.Browse(ctx, PathParams{Pool: "p1", Share: "docs", Name: name, Path: "lnk"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("через ссылку наружу: %v", err)
	}
	if _, err := v.e.Browse(ctx, PathParams{Pool: "p1", Share: "docs", Name: "@GMT-2020.01.01-00.00.00"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет снапшота: %v", err)
	}
}

func TestRestoreFileAndDir(t *testing.T) {
	v := newEnv(t)
	write(t, filepath.Join(v.share, "Отчёты/q1.docx"), "версия 1", 0o640)
	write(t, filepath.Join(v.share, "Отчёты/sub/deep.txt"), "глубоко", 0o600)
	if err := os.Symlink("../q1.docx", filepath.Join(v.share, "Отчёты/sub/ln")); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(v.share, "Отчёты/q1.docx"), mt, mt)
	name := v.snap(t, false)
	// После снапшота файл изменён — восстановление не трогает текущий.
	write(t, filepath.Join(v.share, "Отчёты/q1.docx"), "версия 2", 0o640)
	ctx := context.Background()
	p := PathParams{Pool: "p1", Share: "docs", Name: name, Path: "Отчёты/q1.docx"}
	r, err := v.e.Restore(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != "Отчёты/q1 (снимок 2026-11-18 10-00).docx" || r.Files != 1 || r.Bytes != int64(len("версия 1")) || r.ToRoot {
		t.Fatalf("итог: %+v", r)
	}
	got := filepath.Join(v.share, r.Path)
	if b, _ := os.ReadFile(got); string(b) != "версия 1" {
		t.Fatalf("содержимое копии: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(v.share, "Отчёты/q1.docx")); string(b) != "версия 2" {
		t.Fatalf("текущий файл изменён: %q", b)
	}
	if st, _ := os.Stat(got); st.Mode().Perm() != 0o640 || !st.ModTime().Equal(mt) {
		t.Fatalf("режим/время: %v %v", st.Mode(), st.ModTime())
	}
	// Повтор — новое имя, ничего не перезаписывается.
	if r2, err := v.e.Restore(ctx, p); err != nil || r2.Path != "Отчёты/q1 (снимок 2026-11-18 10-00 2).docx" {
		t.Fatalf("повтор: %+v %v", r2, err)
	}
	// Папка целиком: файлы, подкаталог, ссылка копируется как ссылка.
	p.Path = "Отчёты"
	rd, err := v.e.Restore(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Path != "Отчёты (снимок 2026-11-18 10-00)" || rd.Files != 2 || rd.Dirs != 2 || rd.Links != 1 {
		t.Fatalf("папка: %+v", rd)
	}
	base := filepath.Join(v.share, rd.Path)
	if b, _ := os.ReadFile(filepath.Join(base, "sub/deep.txt")); string(b) != "глубоко" {
		t.Fatalf("вложенный файл: %q", b)
	}
	if l, err := os.Readlink(filepath.Join(base, "sub/ln")); err != nil || l != "../q1.docx" {
		t.Fatalf("ссылка: %q %v", l, err)
	}
	if st, _ := os.Stat(filepath.Join(base, "sub/deep.txt")); st.Mode().Perm() != 0o600 {
		t.Fatalf("режим вложенного: %v", st.Mode())
	}
}

func TestRestoreParentGoneAndLinkParent(t *testing.T) {
	v := newEnv(t)
	write(t, filepath.Join(v.share, "old/a.txt"), "a", 0o660)
	write(t, filepath.Join(v.share, "keep/b.txt"), "b", 0o660)
	name := v.snap(t, false)
	ctx := context.Background()
	// Каталога old больше нет — копия в корне папки.
	if err := os.RemoveAll(filepath.Join(v.share, "old")); err != nil {
		t.Fatal(err)
	}
	r, err := v.e.Restore(ctx, PathParams{Pool: "p1", Share: "docs", Name: name, Path: "old/a.txt"})
	if err != nil || !r.ToRoot || r.Path != "a (снимок 2026-11-18 10-00).txt" {
		t.Fatalf("в корень: %+v %v", r, err)
	}
	// Каталог keep подменён ссылкой наружу — отказ, снаружи ничего.
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(v.share, "keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(v.share, "keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Restore(ctx, PathParams{Pool: "p1", Share: "docs", Name: name, Path: "keep/b.txt"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("ссылка в пути назначения: %v", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("запись за пределы папки: %v", ents)
	}
	// Нет такого в снапшоте.
	if _, err := v.e.Restore(ctx, PathParams{Pool: "p1", Share: "docs", Name: name, Path: "nope.txt"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет файла: %v", err)
	}
}

func TestRestoreStripsSetuidAndCleansUpOnError(t *testing.T) {
	v := newEnv(t)
	write(t, filepath.Join(v.share, "bin/tool"), "x", 0o755)
	if err := os.Chmod(filepath.Join(v.share, "bin/tool"), 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(v.share, "bad/1.txt"), "1", 0o660)
	write(t, filepath.Join(v.share, "bad/2.txt"), "2", 0o660)
	name := v.snap(t, false)
	ctx := context.Background()
	r, err := v.e.Restore(ctx, PathParams{Pool: "p1", Share: "docs", Name: name, Path: "bin/tool"})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(v.share, r.Path)); st.Mode()&os.ModeSetuid != 0 || st.Mode().Perm() != 0o755 {
		t.Fatalf("setuid перенесён: %v", st.Mode())
	}
	if os.Geteuid() == 0 {
		t.Skip("от root нечитаемый файл не вызвать")
	}
	// Нечитаемый файл в снапшоте — ошибка посреди копирования папки: частичная копия удаляется.
	if err := os.Chmod(filepath.Join(v.e.MountRoot, "p1/.snapshots/docs", name, "bad/2.txt"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Restore(ctx, PathParams{Pool: "p1", Share: "docs", Name: name, Path: "bad"}); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	ents, _ := os.ReadDir(filepath.Join(v.share))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "bad (") {
			t.Fatalf("частичная копия осталась: %s", e.Name())
		}
	}
}

func TestRestoredName(t *testing.T) {
	tm := time.Date(2026, 11, 18, 9, 5, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		dir  bool
		n    int
		want string
	}{
		{"отчёт.docx", false, 1, "отчёт (снимок 2026-11-18 09-05).docx"},
		{"отчёт.docx", false, 3, "отчёт (снимок 2026-11-18 09-05 3).docx"},
		{"папка.v2", true, 1, "папка.v2 (снимок 2026-11-18 09-05)"},
		{".bashrc", false, 1, ".bashrc (снимок 2026-11-18 09-05)"},
		{"archive.tar.gz", false, 1, "archive.tar (снимок 2026-11-18 09-05).gz"},
		{"noext", false, 1, "noext (снимок 2026-11-18 09-05)"},
	} {
		if got := RestoredName(c.name, tm, c.dir, c.n); got != c.want {
			t.Errorf("%s: %q, ожидалось %q", c.name, got, c.want)
		}
	}
	long := strings.Repeat("я", 120) + ".txt" // 244 байта
	got := RestoredName(long, tm, false, 1)
	if len(got) > MaxNameBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "09-05).txt") {
		t.Fatalf("длинное имя: %d байт %q", len(got), got)
	}
}
