package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenFreshWALAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamayun.db")
	db := open(t, path)
	ctx := context.Background()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	v, err := SchemaVersion(ctx, db)
	if err != nil || v != LatestVersion() {
		t.Fatalf("версия схемы %d, %v; ожидалась %d", v, err, LatestVersion())
	}
	for _, table := range []string{"tasks", "events", "pools", "schema_migrations"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil || n != 1 {
			t.Errorf("таблица %s: %d, %v", table, n, err)
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("права базы %v, ожидалось 0600", st.Mode().Perm())
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamayun.db")
	db := open(t, path)
	if _, err := db.Exec("INSERT INTO tasks(id, kind, state, created_at, updated_at) VALUES ('a','k','queued',1,1)"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2 := open(t, path)
	var n int
	if err := db2.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil || n != len(migrations) {
		t.Fatalf("schema_migrations: %d, %v", n, err)
	}
	if err := db2.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n); err != nil || n != 1 {
		t.Fatalf("данные потеряны после повторного открытия: %d, %v", n, err)
	}
}

func TestSchemaTooNewRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gamayun.db")
	db := open(t, path)
	if _, err := db.Exec("INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, 'future', 0)", LatestVersion()+1); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err := Open(context.Background(), path)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("ожидалась ErrSchemaTooNew, получено %v", err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "gamayun.db"))
	ctx := context.Background()
	list := append([]Migration{}, migrations...)
	list = append(list, Migration{
		Version: len(list) + 1,
		Name:    "broken",
		SQL:     "CREATE TABLE half_done(x INTEGER); THIS IS NOT SQL;",
	})
	if err := migrate(ctx, db, list); err == nil {
		t.Fatal("ожидалась ошибка битой миграции")
	}
	v, _ := SchemaVersion(ctx, db)
	if v != LatestVersion() {
		t.Errorf("версия после сбоя %d, ожидалась %d", v, LatestVersion())
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='half_done'").Scan(&n)
	if n != 0 {
		t.Error("частично применённая миграция не откатилась")
	}
}

func TestMigrationOrderChecked(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "gamayun.db"))
	bad := []Migration{{Version: 2, Name: "gap", SQL: "SELECT 1"}}
	if err := migrate(context.Background(), db, bad); err == nil {
		t.Fatal("пропуск версии должен отклоняться")
	}
}

func TestOpenRejectsURIChars(t *testing.T) {
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "a?mode=ro")); err == nil {
		t.Fatal("путь с '?' должен отклоняться")
	}
}

// Коммиты долговечны — synchronous=FULL (2).
func TestSynchronousFull(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&v); err != nil || v != 2 {
		t.Fatalf("synchronous=%d %v", v, err)
	}
}
