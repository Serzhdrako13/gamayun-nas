package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/auth"
	"gamayun-nas/internal/config"
	"gamayun-nas/internal/store"
)

func admin(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runAdmin(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestAdminCLI(t *testing.T) {
	db := filepath.Join(t.TempDir(), "gamayun.db")

	if code, out, _ := admin(t, "", "list", "-db", db); code != 0 || !strings.Contains(out, "учётных записей нет") {
		t.Fatalf("list на пустой базе: %d %q", code, out)
	}
	if code, _, e := admin(t, "short\n", "create", "-db", db, "-name", "owner"); code == 0 || !strings.Contains(e, "пароль") {
		t.Fatalf("слабый пароль принят: %d %q", code, e)
	}
	if code, _, _ := admin(t, "correct horse battery\n", "create", "-db", db, "-name", "Bad Name"); code == 0 {
		t.Fatal("недопустимое имя принято")
	}
	if code, _, _ := admin(t, "", "create", "-db", db, "-name", "owner"); code == 0 {
		t.Fatal("пустой ввод пароля принят")
	}
	if code, out, e := admin(t, "correct horse battery\n", "create", "-db", db, "-name", "owner"); code != 0 {
		t.Fatalf("create: %d %q %q", code, out, e)
	}
	if code, _, _ := admin(t, "correct horse battery\n", "create", "-db", db, "-name", "owner"); code == 0 {
		t.Fatal("повторное создание")
	}

	ctx := context.Background()
	d, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := auth.Store{DB: d}
	u, err := st.UserByName(ctx, "owner")
	if err != nil || !u.IsAdmin {
		t.Fatalf("пользователь: %+v %v", u, err)
	}
	var hash string
	d.QueryRow("SELECT password FROM users WHERE name='owner'").Scan(&hash)
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(hash, "correct horse") {
		t.Fatalf("хэш: %q", hash)
	}
	if ok, _, _ := auth.VerifyPassword("correct horse battery", hash, auth.DefaultParams); !ok {
		t.Fatal("пароль не проверяется")
	}

	// passwd отзывает все сессии.
	for i := 0; i < 2; i++ {
		if _, err := st.CreateSession(ctx, u.ID, time.Now(), time.Hour, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	code, out, e := admin(t, "another long secret\n", "passwd", "-db", db, "-name", "owner")
	if code != 0 || !strings.Contains(out, "отозвано сессий: 2") {
		t.Fatalf("passwd: %d %q %q", code, out, e)
	}
	d.QueryRow("SELECT password FROM users WHERE name='owner'").Scan(&hash)
	if ok, _, _ := auth.VerifyPassword("another long secret", hash, auth.DefaultParams); !ok {
		t.Fatal("новый пароль не записан")
	}
	if code, _, _ := admin(t, "another long secret\n", "passwd", "-db", db, "-name", "nobody"); code == 0 {
		t.Fatal("passwd несуществующего")
	}
	if code, out, _ := admin(t, "", "list", "-db", db); code != 0 || !strings.Contains(out, "owner\tадминистратор") {
		t.Fatalf("list: %q", out)
	}
	if code, _, _ := admin(t, "", "frobnicate", "-db", db); code == 0 {
		t.Fatal("неизвестная команда")
	}
	evs := 0
	d.QueryRow("SELECT COUNT(*) FROM events WHERE type IN ('auth.user_created','auth.password_changed')").Scan(&evs)
	if evs != 2 {
		t.Errorf("событий CLI: %d", evs)
	}
}

type fakeOSPw struct {
	calls []string
	err   error
}

func (f *fakeOSPw) SyncAfterWeb(_ context.Context, name, pw string) error {
	f.calls = append(f.calls, name+":"+pw)
	return f.err
}

// Пароль из CLI доходит до SMB/ОС (единая учётка); сбой helper — предупреждение.
func TestAdminSyncsOSPassword(t *testing.T) {
	f := &fakeOSPw{}
	old := osPasswordSetter
	osPasswordSetter = func(config.Config, *sql.DB) interface {
		SyncAfterWeb(context.Context, string, string) error
	} {
		return f
	}
	defer func() { osPasswordSetter = old }()
	db := filepath.Join(t.TempDir(), "d.db")
	if code, out, _ := admin(t, "correct horse battery\n", "create", "-db", db, "-name", "owner"); code != 0 || !strings.Contains(out, "SMB/ОС") {
		t.Fatalf("create: %d %s", code, out)
	}
	f.err = errors.New("нет сокета")
	code, _, e := admin(t, "another long secret\n", "passwd", "-db", db, "-name", "owner")
	if code != 0 || !strings.Contains(e, "не обновлён") {
		t.Fatalf("passwd: %d %s", code, e)
	}
	if len(f.calls) != 2 || f.calls[0] != "owner:correct horse battery" || f.calls[1] != "owner:another long secret" {
		t.Fatalf("вызовы: %v", f.calls)
	}
}
