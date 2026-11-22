package confbackup

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/maintenance"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/notify"
	"gamayun-nas/internal/store"
)

const (
	smtpPW   = "smtp-SECRET-pw-1"
	hookKey  = "hook-SECRET-key-2"
	urlToken = "tok-SECRET-3"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// populated — настроенная система: администратор, пользователь, группа,
// папка, настройки и каналы уведомлений с секретами; хэш пароля в users.
func populated(t *testing.T) *sql.DB {
	db := openDB(t)
	ctx := context.Background()
	exec(t, db, "INSERT INTO users(name, password, is_admin, created_at, updated_at) VALUES ('owner', '$argon2id$HASH-SECRET', 1, 1, 1)")
	for _, g := range []string{"administrators", "users", "media"} {
		exec(t, db, "INSERT INTO nas_groups(name, created_at) VALUES (?, 1)", g)
	}
	exec(t, db, "INSERT INTO nas_users(name, enabled, ssh, created_at) VALUES ('owner', 1, 1, 1), ('ann', 0, 0, 1)")
	exec(t, db, "INSERT INTO nas_group_members(grp, user) VALUES ('administrators','owner'),('users','owner'),('users','ann'),('media','ann')")
	exec(t, db, "INSERT INTO pools(name, type, integrity, layout, created_at, updated_at) VALUES ('tank', 'shr', 1, '{}', 1, 1)")
	exec(t, db, "INSERT INTO shares(name, pool, comment, created_at) VALUES ('docs', 'tank', 'Документы', 1), ('old', 'gone', '', 1)")
	exec(t, db, "INSERT INTO share_access(share, kind, subject, level) VALUES ('docs','group','media','rw'),('docs','user','owner','read')")
	exec(t, db, "INSERT INTO settings(key, value) VALUES ('setup.done','1'),('server.name','nas1'),('homes.enabled','1'),('homes.pool','tank')")
	exec(t, db, `INSERT INTO settings(key, value) VALUES ('sshguard', '{"enabled":true,"maxretry":5,"findtime":600,"bantime":600,"ignore":[]}')`)
	sc, _ := json.Marshal(maintenance.DefaultSchedule())
	exec(t, db, "INSERT INTO settings(key, value) VALUES ('maintenance.schedule', ?)", string(sc))
	if err := notify.SaveSMTP(ctx, db, notify.SMTPConfig{Enabled: true, Host: "smtp.example.org", Port: 587, Security: "starttls",
		Username: "u", Password: smtpPW, From: "nas@example.org", To: []string{"me@example.org"}}); err != nil {
		t.Fatal(err)
	}
	if err := notify.SaveWebhook(ctx, db, notify.WebhookConfig{Enabled: true, URL: "https://hook.example.org/x?token=" + urlToken, Secret: hookKey}); err != nil {
		t.Fatal(err)
	}
	return db
}

func unzip(t *testing.T, b []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.ReadFrom(zr)
	return out.Bytes()
}

func TestCollectNoSecretsRoundTrip(t *testing.T) {
	db := populated(t)
	ctx := context.Background()
	b, d, err := Collect(ctx, db, &Identity{Timezone: "Europe/Moscow", NTP: true}, time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	plain := unzip(t, data)
	for _, s := range []string{smtpPW, hookKey, urlToken, "HASH-SECRET", "argon2"} {
		if bytes.Contains(plain, []byte(s)) {
			t.Fatalf("секрет %q попал в копию", s)
		}
	}
	if !d.Secrets.SMTPPassword || !d.Secrets.WebhookSecret || !d.Secrets.WebhookURL {
		t.Fatalf("признаки секретов: %+v", d.Secrets)
	}
	if d.Webhook.URL != "https://hook.example.org/x" {
		t.Fatalf("адрес webhook: %q", d.Webhook.URL)
	}
	b2, d2, err := DecodeBytes(data, store.LatestVersion())
	if err != nil {
		t.Fatal(err)
	}
	if b2.SchemaVersion != store.LatestVersion() || b2.FormatVersion != FormatVersion || b2.Hostname != "nas1" {
		t.Fatalf("заголовок: %+v", b2)
	}
	if len(d2.Users) != 2 || !d2.Users[1].Admin || d2.Users[0].Enabled || len(d2.Shares) != 2 || len(d2.Groups) != 3 {
		t.Fatalf("данные: %+v", d2)
	}
	if d2.Shares[0].Name != "docs" || len(d2.Shares[0].Access) != 2 || d2.Settings.Timezone != "Europe/Moscow" || d2.Maintenance == nil {
		t.Fatalf("папки/настройки: %+v", d2)
	}
	if FileName(b.CreatedAt, b.Hostname) != "gamayun-config-20261118-100000-nas1.json.gz" {
		t.Fatal(FileName(b.CreatedAt, b.Hostname))
	}
}

// reencode — подменить заголовок/данные и упаковать заново.
func reencode(t *testing.T, data []byte, f func(m map[string]any)) []byte {
	var m map[string]any
	if err := json.Unmarshal(unzip(t, data), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	j, _ := json.Marshal(m)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(j)
	zw.Close()
	return buf.Bytes()
}

func TestDecodeCompatibility(t *testing.T) {
	db := populated(t)
	b, _, err := Collect(context.Background(), db, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := Encode(b)
	cases := []struct {
		name string
		in   []byte
		want error
		msg  string
	}{
		{"не gzip", []byte("hello"), ErrFormat, ""},
		{"чужой json", reencode(t, data, func(m map[string]any) { m["format"] = "other" }), ErrFormat, ""},
		{"формат новее", reencode(t, data, func(m map[string]any) { m["format_version"] = FormatVersion + 1; m["app_version"] = "9.9" }), ErrIncompatible, "обновите систему до версии не ниже 9.9"},
		{"формат 0", reencode(t, data, func(m map[string]any) { m["format_version"] = 0 }), ErrIncompatible, "слишком старый"},
		{"схема новее", reencode(t, data, func(m map[string]any) { m["schema_version"] = store.LatestVersion() + 1 }), ErrIncompatible, "схема базы"},
		{"порча данных", reencode(t, data, func(m map[string]any) {
			m["data"].(map[string]any)["users"].([]any)[0].(map[string]any)["admin"] = true
		}), ErrFormat, "контрольная сумма"},
	}
	for _, c := range cases {
		_, _, err := DecodeBytes(c.in, store.LatestVersion())
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// Старая схема базы (копия от предыдущей версии) — принимается.
	ob := b
	ob.SchemaVersion = 7
	old, _ := Encode(ob)
	if _, _, err := DecodeBytes(old, store.LatestVersion()); err != nil {
		t.Fatalf("старая совместимая схема: %v", err)
	}
	// Код знает схему 7 (откат версии) — копия со схемой новее отклоняется.
	if _, _, err := DecodeBytes(data, 7); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("откат версии: %v", err)
	}
}

// fakeTarget записывает вызовы и пишет модель в базу, как nas.Service.
type fakeTarget struct {
	db    *sql.DB
	calls []string
	fail  string
}

func (f *fakeTarget) rec(s string) error {
	f.calls = append(f.calls, s)
	if f.fail != "" && strings.HasPrefix(s, f.fail) {
		return errors.New("сбой helper")
	}
	return nil
}
func (f *fakeTarget) CreateUserSSH(_ context.Context, name, pw string, admin, ssh bool) (nas.User, error) {
	if err := f.rec("user " + name); err != nil {
		return nas.User{}, err
	}
	_, err := f.db.Exec("INSERT INTO nas_users(name, enabled, ssh, created_at) VALUES (?, 1, ?, 1)", name, ssh)
	return nas.User{}, err
}
func (f *fakeTarget) SetPassword(_ context.Context, name, _ string) error {
	return f.rec("passwd " + name)
}
func (f *fakeTarget) SetEnabled(_ context.Context, name string, on bool) error {
	return f.rec("disable " + name)
}
func (f *fakeTarget) CreateGroup(_ context.Context, name string) (nas.Group, error) {
	return nas.Group{}, f.rec("group " + name)
}
func (f *fakeTarget) SetMembers(_ context.Context, g string, m []string) error {
	return f.rec("members " + g + "=" + strings.Join(m, ","))
}
func (f *fakeTarget) CreateShare(_ context.Context, name, pool, _ string) (nas.ShareInfo, error) {
	return nas.ShareInfo{}, f.rec("share " + name + "@" + pool)
}
func (f *fakeTarget) SetAccess(_ context.Context, s string, e []nas.Access) error {
	return f.rec("access " + s)
}
func (f *fakeTarget) SetIdentity(_ context.Context, h, tz string, ntp bool) ([]string, error) {
	return nil, f.rec("identity " + h + " " + tz)
}
func (f *fakeTarget) SetHomes(_ context.Context, on bool, pool string) (nas.HomesInfo, error) {
	return nas.HomesInfo{}, f.rec("homes " + pool)
}
func (f *fakeTarget) SetGuard(_ context.Context, g nas.GuardSettings, ip string) (nas.GuardSettings, error) {
	return g, f.rec("guard")
}
func (f *fakeTarget) CompleteSetup(context.Context) error { return f.rec("setup") }

func backupData(t *testing.T) Data {
	db := populated(t)
	b, _, err := Collect(context.Background(), db, &Identity{Timezone: "UTC", NTP: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := Encode(b)
	_, d, err := DecodeBytes(data, store.LatestVersion())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRestoreCleanSystem(t *testing.T) {
	d := backupData(t)
	ctx := context.Background()
	db := openDB(t) // чистая система: мастер не пройден
	exec(t, db, "INSERT INTO pools(name, type, integrity, layout, created_at, updated_at) VALUES ('tank', 'shr', 1, '{}', 1, 1)")
	plan, err := MakePlan(ctx, db, Backup{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SetupDone || len(plan.Secrets) != 3 || plan.Users[0].CanKeep {
		t.Fatalf("план: %+v", plan)
	}
	// Без паролей — отказ до изменений.
	tg := &fakeTarget{db: db}
	if _, err := Apply(ctx, db, tg, d, Answers{}, ""); !errors.Is(err, ErrAnswers) || len(tg.calls) != 0 {
		t.Fatalf("без паролей: %v %v", err, tg.calls)
	}
	// «Оставить прежний» на чистой системе — нельзя (его нет).
	a := Answers{Passwords: map[string]string{"owner": "correct horse battery"}, Keep: map[string]bool{"ann": true}, AcceptInsecure: true}
	if _, err := Apply(ctx, db, tg, d, a, ""); !errors.Is(err, ErrAnswers) {
		t.Fatalf("keep без прежнего: %v", err)
	}
	a = Answers{Passwords: map[string]string{"owner": "correct horse battery", "ann": "another long pass"}, SMTPPassword: "new-smtp"}
	res, err := Apply(ctx, db, tg, d, a, "")
	if err != nil || res.Error != "" {
		t.Fatalf("восстановление: %v %+v", err, res)
	}
	want := []string{"user ann", "disable ann", "user owner", "group media", "members administrators=owner",
		"members media=ann", "members users=ann,owner", "share docs@tank", "access docs", "identity nas1 UTC", "homes tank", "guard", "setup"}
	if strings.Join(tg.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("шаги:\n%v\nожидалось\n%v", tg.calls, want)
	}
	// Папка на отсутствующем пуле пропущена с предупреждением.
	if !strings.Contains(strings.Join(res.Warnings, ";"), "old пропущена") {
		t.Fatalf("предупреждения: %v", res.Warnings)
	}
	smtp, _ := notify.LoadSMTP(ctx, db)
	if smtp.Password != "new-smtp" || !smtp.Enabled {
		t.Fatalf("smtp: %+v", smtp)
	}
	// Ключ и адрес webhook не заданы — канал выключен, без секрета.
	hook, _ := notify.LoadWebhook(ctx, db)
	if hook.Enabled || hook.Secret != "" || hook.URL != "https://hook.example.org/x" {
		t.Fatalf("webhook: %+v", hook)
	}
	var sc string
	db.QueryRow("SELECT value FROM settings WHERE key = 'maintenance.schedule'").Scan(&sc)
	if sc == "" {
		t.Fatal("расписание не восстановлено")
	}
}

func TestRestoreKeepAndResume(t *testing.T) {
	d := backupData(t)
	ctx := context.Background()
	db := populated(t) // настроенная система: учётки и секреты уже есть
	plan, _ := MakePlan(ctx, db, Backup{}, d)
	if !plan.SetupDone || !plan.Users[0].CanKeep || !plan.Secrets[0].CanKeep {
		t.Fatalf("план: %+v", plan)
	}
	keep := Answers{Keep: map[string]bool{"owner": true, "ann": true, SecretSMTP: true}}
	if _, err := Apply(ctx, db, &fakeTarget{db: db}, d, keep, ""); !errors.Is(err, ErrAnswers) {
		t.Fatalf("keep без подтверждения: %v", err)
	}
	keep.AcceptInsecure = true
	tg := &fakeTarget{db: db}
	res, err := Apply(ctx, db, tg, d, keep, "")
	if err != nil || res.Error != "" {
		t.Fatalf("%v %+v", err, res)
	}
	for _, c := range tg.calls {
		if strings.HasPrefix(c, "user ") || strings.HasPrefix(c, "passwd ") || c == "setup" || strings.HasPrefix(c, "share ") {
			t.Fatalf("лишний шаг %q: %v", c, tg.calls)
		}
	}
	if smtp, _ := notify.LoadSMTP(ctx, db); smtp.Password != smtpPW {
		t.Fatal("прежний пароль SMTP не оставлен")
	}
	if !strings.Contains(strings.Join(res.Warnings, ";"), "небезопасно") {
		t.Fatalf("нет предупреждения: %v", res.Warnings)
	}

	// Сбой на шаге — прерывание с причиной; повтор продолжает.
	db2 := openDB(t)
	exec(t, db2, "INSERT INTO pools(name, type, integrity, layout, created_at, updated_at) VALUES ('tank', 'shr', 1, '{}', 1, 1)")
	a := Answers{Passwords: map[string]string{"owner": "correct horse battery", "ann": "another long pass"}}
	tg = &fakeTarget{db: db2, fail: "user owner"}
	res, err = Apply(ctx, db2, tg, d, a, "")
	if err != nil || !strings.Contains(res.Error, "пользователь owner") {
		t.Fatalf("сбой: %v %+v", err, res)
	}
	tg.fail = ""
	tg.calls = nil
	res, err = Apply(ctx, db2, tg, d, a, "")
	if err != nil || res.Error != "" || tg.calls[0] != "passwd ann" || tg.calls[2] != "user owner" {
		t.Fatalf("повтор: %v %+v %v", err, res, tg.calls)
	}
	if a.String() != "Answers{passwords:2 keep:0}" || strings.Contains(a.String(), "horse") {
		t.Fatal(a.String())
	}
}

func TestAdminsNotRemoved(t *testing.T) {
	d := backupData(t)
	ctx := context.Background()
	db := openDB(t)
	// Мастер создал администратора boss, которого нет в копии.
	exec(t, db, "INSERT INTO nas_groups(name, created_at) VALUES ('administrators', 1), ('users', 1)")
	exec(t, db, "INSERT INTO nas_users(name, enabled, ssh, created_at) VALUES ('boss', 1, 1, 1)")
	exec(t, db, "INSERT INTO nas_group_members(grp, user) VALUES ('administrators','boss')")
	tg := &fakeTarget{db: db}
	a := Answers{Passwords: map[string]string{"owner": "correct horse battery", "ann": "another long pass"}}
	if res, err := Apply(ctx, db, tg, d, a, ""); err != nil || res.Error != "" {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(strings.Join(tg.calls, "|"), "members administrators=boss,owner") {
		t.Fatalf("администратор мастера убран: %v", tg.calls)
	}
}

func gz(t *testing.T) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("{}"))
	zw.Close()
	return buf.Bytes()
}

func TestPoolFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "tank"), 0o755)
	mounted := true
	e := &Executor{MountRoot: root, Mounted: func(string) bool { return mounted }}
	ctx := context.Background()
	data := gz(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < Keep+3; i++ {
		if _, err := e.Save(ctx, SaveParams{Pool: "tank", Name: FileName(base.Add(time.Duration(i)*time.Hour), "nas1"), Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	files, err := e.List(ctx, PoolParams{Pool: "tank"})
	if err != nil || len(files) != Keep || files[0].Name != FileName(base.Add(time.Duration(Keep+2)*time.Hour), "nas1") {
		t.Fatalf("ротация: %v %v", err, files)
	}
	fi, _ := os.Stat(filepath.Join(root, "tank", PoolDir, files[0].Name))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("права файла %v", fi.Mode())
	}
	if _, err := e.Save(ctx, SaveParams{Pool: "tank", Name: files[0].Name, Data: data}); !errors.Is(err, ErrExists) {
		t.Fatalf("перезапись: %v", err)
	}
	r, err := e.Read(ctx, ReadParams{Pool: "tank", Name: files[0].Name})
	if err != nil || !bytes.Equal(r.Data, data) {
		t.Fatalf("чтение: %v", err)
	}
	if _, err := e.Read(ctx, ReadParams{Pool: "tank", Name: FileName(base.Add(-time.Hour), "")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("нет файла: %v", err)
	}
	if (SaveParams{Pool: "tank", Name: "../x.json.gz", Data: data}).Validate() == nil ||
		(SaveParams{Pool: "tank", Name: FileName(base, ""), Data: []byte("plain")}).Validate() == nil ||
		(ReadParams{Pool: "../etc", Name: FileName(base, "")}).Validate() == nil {
		t.Fatal("недопустимые параметры приняты")
	}
	// Ссылка вместо каталога — отказ.
	os.MkdirAll(filepath.Join(root, "p2"), 0o755)
	os.Symlink(t.TempDir(), filepath.Join(root, "p2", ".gamayun"))
	if _, err := e.Save(ctx, SaveParams{Pool: "p2", Name: FileName(base, ""), Data: data}); !errors.Is(err, ErrRefused) {
		t.Fatalf("ссылка: %v", err)
	}
	mounted = false
	if _, err := e.Save(ctx, SaveParams{Pool: "tank", Name: FileName(base.Add(99*time.Hour), ""), Data: data}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не смонтирован: %v", err)
	}
}

type fakeRunner struct {
	calls []string
	fs    string
}

func (f *fakeRunner) Run(_ context.Context, _ time.Duration, path string, args ...string) (CmdResult, error) {
	f.calls = append(f.calls, filepath.Base(path)+" "+strings.Join(args, " "))
	if strings.HasSuffix(path, "blkid") {
		return CmdResult{Stdout: []byte("DEVNAME=x\nTYPE=" + f.fs + "\nLABEL=STICK\n")}, nil
	}
	return CmdResult{}, nil
}

func TestUSB(t *testing.T) {
	sys := t.TempDir()
	// sdb — USB (путь устройства содержит /usb), sdb1 — его раздел; sda — SATA.
	usbDisk := filepath.Join(sys, "devices", "pci0", "usb1", "1-1", "host6", "block", "sdb")
	sataDisk := filepath.Join(sys, "devices", "pci0", "ata1", "host0", "block", "sda")
	os.MkdirAll(filepath.Join(usbDisk, "sdb1"), 0o755)
	os.MkdirAll(filepath.Join(usbDisk, "device"), 0o755)
	os.MkdirAll(sataDisk, 0o755)
	os.WriteFile(filepath.Join(usbDisk, "sdb1", "partition"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(usbDisk, "sdb1", "size"), []byte("2048\n"), 0o644)
	os.WriteFile(filepath.Join(usbDisk, "device", "model"), []byte("Flash  \n"), 0o644)
	cb := filepath.Join(sys, "class", "block")
	os.MkdirAll(cb, 0o755)
	os.Symlink(usbDisk, filepath.Join(cb, "sdb"))
	os.Symlink(filepath.Join(usbDisk, "sdb1"), filepath.Join(cb, "sdb1"))
	os.Symlink(sataDisk, filepath.Join(cb, "sda"))
	mi := filepath.Join(t.TempDir(), "mountinfo")
	os.WriteFile(mi, []byte("22 1 8:1 / / rw - ext4 /dev/sda rw\n"), 0o644)
	run := t.TempDir()
	r := &fakeRunner{fs: "vfat"}
	e := &Executor{Sys: sys, MountInfo: mi, RunDir: run, Runner: r}
	ctx := context.Background()
	devs, err := e.USBList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// sdb (целиком) тоже vfat по фейковому blkid — оба USB; sda нет (не USB и смонтирован).
	if len(devs) != 2 || devs[1].Device != "sdb1" || devs[1].Size != 2048*512 || devs[1].Model != "Flash" || devs[1].Label != "STICK" {
		t.Fatalf("носители: %+v", devs)
	}
	data := gz(t)
	name := FileName(time.Now(), "nas1")
	if _, err := e.USBSave(ctx, USBSaveParams{Device: "sda", Name: name, Data: data}); !errors.Is(err, ErrRefused) {
		t.Fatalf("не USB: %v", err)
	}
	res, err := e.USBSave(ctx, USBSaveParams{Device: "sdb1", Name: name, Data: data})
	if err != nil || res.Path != "sdb1:/"+USBDir+"/"+name && res.Path != filepath.Join("sdb1:", USBDir, name) {
		t.Fatalf("запись: %v %+v", err, res)
	}
	joined := strings.Join(r.calls, "|")
	if !strings.Contains(joined, "mount -t vfat -o nosuid,nodev,noexec -- /dev/sdb1 ") || !strings.Contains(joined, "umount -- ") {
		t.Fatalf("команды: %v", r.calls)
	}
	// Временная точка удалена (фейковый mount не монтирует — файл остался бы в
	// каталоге; каталог не пуст — проверяем, что запись шла в /run/gamayun-usb-*).
	m, _ := filepath.Glob(filepath.Join(run, "gamayun-usb-*", USBDir, name))
	if len(m) != 1 {
		t.Fatalf("файл не записан в точку монтирования: %v", m)
	}
	r.fs = "ntfs"
	if _, err := e.USBSave(ctx, USBSaveParams{Device: "sdb1", Name: FileName(time.Now().Add(time.Hour), ""), Data: data}); !errors.Is(err, ErrRefused) {
		t.Fatalf("ntfs: %v", err)
	}
}

type fakeHelper struct {
	saves int
	err   error
}

func (f *fakeHelper) Call(_ context.Context, op string, params, result any) error {
	if op == OpSave {
		f.saves++
		if f.err != nil {
			return f.err
		}
	}
	return nil
}

func TestAutoSave(t *testing.T) {
	db := populated(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	h := &fakeHelper{}
	s := &Service{DB: db, Helper: h, Now: func() time.Time { return now }}
	if _, saved, err := s.SaveToPools(ctx, false); err != nil || !saved || h.saves != 1 {
		t.Fatalf("первая: %v %v %d", saved, err, h.saves)
	}
	now = now.Add(time.Hour)
	if _, saved, _ := s.SaveToPools(ctx, false); saved || h.saves != 1 {
		t.Fatal("без изменений сохранено повторно")
	}
	exec(t, db, "INSERT INTO nas_groups(name, created_at) VALUES ('new', 1)")
	if _, saved, _ := s.SaveToPools(ctx, false); !saved || h.saves != 2 {
		t.Fatal("изменение не сохранено")
	}
	now = now.Add(DailyInterval)
	if _, saved, _ := s.SaveToPools(ctx, false); !saved {
		t.Fatal("суточная копия не сделана")
	}
	h.err = errors.New("пул не смонтирован")
	if _, _, err := s.SaveToPools(ctx, true); err == nil {
		t.Fatal("сбой всех пулов не ошибка")
	}
	exec(t, db, "DELETE FROM shares")
	exec(t, db, "DELETE FROM pools")
	if _, _, err := s.SaveToPools(ctx, true); !errors.Is(err, ErrNoPools) {
		t.Fatal(err)
	}
}

// До конца мастера автокопия не пишется (пустая система после подхвата
// пула не вытесняет настоящие копии); ручная — пишется.
func TestAutoSaveWaitsForSetup(t *testing.T) {
	db := populated(t)
	exec(t, db, "DELETE FROM settings WHERE key = 'setup.done'")
	ctx := context.Background()
	h := &fakeHelper{}
	s := &Service{DB: db, Helper: h, Now: time.Now}
	if _, saved, err := s.SaveToPools(ctx, false); err != nil || saved || h.saves != 0 {
		t.Fatalf("автокопия до мастера: %v %v %d", saved, err, h.saves)
	}
	if _, saved, err := s.SaveToPools(ctx, true); err != nil || !saved || h.saves != 1 {
		t.Fatalf("ручная копия до мастера: %v %v %d", saved, err, h.saves)
	}
}
