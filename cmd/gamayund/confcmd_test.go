package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamayun-nas/internal/confbackup"
	"gamayun-nas/internal/config"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/store"
)

func confCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runConfig(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// okHelper — helper, на всё отвечающий успехом.
type okHelper struct{ ops []string }

func (h *okHelper) Call(_ context.Context, op string, _, _ any) error {
	h.ops = append(h.ops, op)
	return nil
}

func TestConfigCLI(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	db, err := store.Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO nas_groups(name, created_at) VALUES ('administrators',1),('users',1)")
	db.Exec("INSERT INTO nas_users(name, enabled, ssh, created_at) VALUES ('owner',1,1,1)")
	db.Exec("INSERT INTO nas_group_members(grp, user) VALUES ('administrators','owner'),('users','owner')")
	db.Close()

	out := filepath.Join(dir, "copy.json.gz")
	if code, o, e := confCLI(t, "", "export", "-db", src, "-o", out); code != 0 || !strings.Contains(o, "записана") {
		t.Fatalf("export: %d %s %s", code, o, e)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("права копии: %v", fi.Mode())
	}
	if code, _, e := confCLI(t, "", "export", "-db", src, "-o", out); code == 0 || !strings.Contains(e, "exists") {
		t.Fatalf("перезапись файла: %d %s", code, e)
	}

	dst := filepath.Join(dir, "dst.db")
	if code, o, _ := confCLI(t, "", "plan", "-db", dst, "-file", out); code != 0 || !strings.Contains(o, "пользователь owner (администратор): создать") {
		t.Fatalf("plan: %d %s", code, o)
	}
	junk := filepath.Join(dir, "junk")
	os.WriteFile(junk, []byte("x"), 0o600)
	if code, _, e := confCLI(t, "", "plan", "-db", dst, "-file", junk); code == 0 || !strings.Contains(e, "не является") {
		t.Fatalf("мусор: %d %s", code, e)
	}

	h := &okHelper{}
	old := newRestoreTarget
	newRestoreTarget = func(_ config.Config, db *sql.DB) confbackup.Target { return nas.NewService(h, db) }
	defer func() { newRestoreTarget = old }()

	// Без терминала и без -answers — отказ.
	if code, _, e := confCLI(t, "", "restore", "-db", dst, "-file", out); code == 0 || !strings.Contains(e, "терминал") {
		t.Fatalf("без терминала: %d %s", code, e)
	}
	ans := filepath.Join(dir, "answers.json")
	os.WriteFile(ans, []byte(`{"passwords":{"owner":"correct horse battery"}}`), 0o644)
	if code, _, e := confCLI(t, "", "restore", "-db", dst, "-file", out, "-answers", ans); code == 0 || !strings.Contains(e, "0600") {
		t.Fatalf("права ответов: %d %s", code, e)
	}
	os.Chmod(ans, 0o600)
	if code, _, e := confCLI(t, "нет\n", "restore", "-db", dst, "-file", out, "-answers", ans); code == 0 || !strings.Contains(e, "отменено") {
		t.Fatalf("отмена: %d %s", code, e)
	}
	code, o, e := confCLI(t, "да\n", "restore", "-db", dst, "-file", out, "-answers", ans)
	if code != 0 || !strings.Contains(o, "настройки восстановлены") || strings.Contains(o+e, "horse") {
		t.Fatalf("restore: %d %s %s", code, o, e)
	}
	if !strings.Contains(strings.Join(h.ops, ","), nas.OpUserPassword) {
		t.Fatalf("helper: %v", h.ops)
	}
}
