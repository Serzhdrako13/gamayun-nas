package nas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failWeb — веб-учётки, у которых SetPassword падает по флагу.
type failWeb struct{ fail bool }

func (f *failWeb) Validate(string, string) error { return nil }
func (f *failWeb) SetPassword(context.Context, string, string, bool, bool) error {
	if f.fail {
		return errors.New("база веба недоступна")
	}
	return nil
}
func (f *failWeb) SetFlags(context.Context, string, bool, bool) error { return nil }
func (f *failWeb) Delete(context.Context, string) error               { return nil }
func (f *failWeb) Admins(context.Context) ([]string, error)           { return nil, nil }

func syncErr(t *testing.T, s *Service, name string) string {
	t.Helper()
	us, err := s.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		if u.Name == name {
			return u.PasswordSyncError
		}
	}
	t.Fatalf("нет %s", name)
	return ""
}

// Сбой после helper (ОС и Samba новые, веб — нет) помечается,
// следующая успешная смена снимает пометку; сбой helper — без пометки.
func TestPasswordMismatchMarked(t *testing.T) {
	ctx := context.Background()
	s, h := newService(t)
	if _, err := s.CreateUser(ctx, "ann", "password-ann1", false); err != nil {
		t.Fatal(err)
	}
	w := &failWeb{}
	s.Web = w
	w.fail = true
	if err := s.SetPassword(ctx, "ann", "password-ann2"); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if e := syncErr(t, s, "ann"); !strings.Contains(e, "веба — прежний") {
		t.Fatalf("пометка: %q", e)
	}
	w.fail = false
	if err := s.SetPassword(ctx, "ann", "password-ann3"); err != nil || syncErr(t, s, "ann") != "" {
		t.Fatalf("пометка не снята: %v %q", err, syncErr(t, s, "ann"))
	}
	// helper отказал (ничего не изменено) — пометки нет
	h.fail = map[string]error{OpUserPassword: errors.New("smbpasswd: отказ")}
	if err := s.SetPassword(ctx, "ann", "password-ann4"); err == nil || syncErr(t, s, "ann") != "" {
		t.Fatalf("лишняя пометка: %v %q", err, syncErr(t, s, "ann"))
	}
	// CLI: веб уже новый, helper отказал — пометка
	if err := s.SyncAfterWeb(ctx, "ann", "password-ann5"); err == nil || !strings.Contains(syncErr(t, s, "ann"), "веба новый") {
		t.Fatalf("CLI: %v %q", err, syncErr(t, s, "ann"))
	}
	h.fail = nil
	if err := s.SyncAfterWeb(ctx, "ann", "password-ann6"); err != nil || syncErr(t, s, "ann") != "" {
		t.Fatalf("CLI успех: %v %q", err, syncErr(t, s, "ann"))
	}
}

// helper — при отказе smbpasswd прежний хэш ОС возвращается.
func TestUserPasswordRollsBackOS(t *testing.T) {
	r := &fakeRunner{reply: ours()}
	r.reply["/usr/bin/smbpasswd"] = CmdResult{ExitCode: 1, Stderr: []byte("tdb error")}
	e := newExec(t, r)
	e.ShadowFile = filepath.Join(t.TempDir(), "shadow")
	os.WriteFile(e.ShadowFile, []byte("root:!:1::::::\nann:$y$j9T$OLDHASH:19000:0:99999:7:::\n"), 0o600)
	_, err := e.UserPassword(context.Background(), PasswordParams{Name: "ann", Password: "new-pass-123"})
	if err == nil || !strings.Contains(err.Error(), "возвращён прежний") {
		t.Fatalf("ошибка: %v", err)
	}
	var rb *call
	for i, c := range r.calls {
		if c.path == "/usr/sbin/chpasswd" && len(c.args) == 1 && c.args[0] == "-e" {
			rb = &r.calls[i]
		}
	}
	if rb == nil || rb.stdin != "ann:$y$j9T$OLDHASH\n" {
		t.Fatalf("отката нет: %+v", r.calls)
	}
	if strings.Contains(err.Error(), "OLDHASH") || strings.Contains(err.Error(), "new-pass-123") {
		t.Fatal("секрет в тексте ошибки")
	}
	// хэша нет — честное сообщение о расхождении
	e.ShadowFile = filepath.Join(t.TempDir(), "нет")
	if _, err := e.UserPassword(context.Background(), PasswordParams{Name: "ann", Password: "x-pass-1234"}); err == nil || !strings.Contains(err.Error(), "Samba прежний") {
		t.Fatalf("без хэша: %v", err)
	}
}

// CLI не сообщает об успехе, если учётки NAS нет.
func TestSyncAfterWebMissingUser(t *testing.T) {
	s, _ := newService(t)
	if err := s.SyncAfterWeb(context.Background(), "ghost", "password-ghost1"); err == nil || !strings.Contains(err.Error(), "учётки NAS ghost нет") {
		t.Fatalf("%v", err)
	}
}
