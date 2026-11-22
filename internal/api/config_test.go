package api

import (
	"context"

	"bytes"
	"encoding/base64"
	"encoding/json"
	"golang.org/x/crypto/ssh"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/confbackup"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/store"
)

func confEnv(t *testing.T) *env {
	sh := &storageHelper{}
	return newEnv(t, func(o *Options) {
		o.NAS = nas.NewService(sh, o.DB)
		o.Config = &confbackup.Service{DB: o.DB, Helper: sh}
	})
}

func TestConfigBackupRestoreAPI(t *testing.T) {
	if r := newEnv(t, nil).get(t, "/api/v1/config/backup/download"); r.status != 503 {
		t.Fatalf("без сервиса: %d", r.status)
	}
	src := confEnv(t)
	src.db.Exec("INSERT INTO nas_groups(name, created_at) VALUES ('administrators',1),('users',1)")
	src.db.Exec("INSERT INTO nas_users(name, enabled, ssh, created_at) VALUES ('owner',1,1,1)")
	src.db.Exec("INSERT INTO nas_group_members(grp, user) VALUES ('administrators','owner'),('users','owner')")
	src.db.Exec(`INSERT INTO settings(key, value) VALUES ('notify.smtp', '{"enabled":true,"host":"h","port":587,"security":"starttls","username":"u","password":"PW-SECRET","from":"a@b.c","to":["d@e.f"]}')`)
	r := src.get(t, "/api/v1/config/backup/download")
	if r.status != 200 || r.header.Get("Content-Type") != "application/gzip" ||
		!strings.Contains(r.header.Get("Content-Disposition"), "gamayun-config-") || strings.Contains(r.raw, "PW-SECRET") {
		t.Fatalf("выгрузка: %d %v", r.status, r.header)
	}
	file := []byte(r.raw)
	if _, d, err := confbackup.DecodeBytes(file, store.LatestVersion()); err != nil || len(d.Users) != 1 || !d.Secrets.SMTPPassword {
		t.Fatalf("копия: %v %+v", err, d)
	}

	dst := confEnv(t)
	b64 := base64.StdEncoding.EncodeToString(file)
	if r := dst.do(t, "POST", "/api/v1/config/restore/plan", "application/json", `{"file":"`+base64.StdEncoding.EncodeToString([]byte("junk"))+`"}`); r.status != 400 || errCode(r) != "bad_backup" {
		t.Fatalf("мусор: %d %s", r.status, r.raw)
	}
	r = dst.do(t, "POST", "/api/v1/config/restore/plan", "application/json", `{"file":"`+b64+`"}`)
	if r.status != 200 || !strings.Contains(r.raw, `"name":"owner"`) || !strings.Contains(r.raw, `"key":"smtp_password"`) {
		t.Fatalf("план: %d %s", r.status, r.raw)
	}
	if r := dst.do(t, "POST", "/api/v1/config/restore", "application/json", `{"file":"`+b64+`","answers":{}}`); r.status != 400 || errCode(r) != "answers_required" {
		t.Fatalf("без паролей: %d %s", r.status, r.raw)
	}
	body, _ := json.Marshal(map[string]any{"file": file, "answers": map[string]any{
		"passwords": map[string]string{"owner": "correct horse battery"}, "smtp_password": "NEW-SMTP"}})
	r = dst.do(t, "POST", "/api/v1/config/restore", "application/json", string(body))
	if r.status != 200 || strings.Contains(r.raw, "correct horse") || strings.Contains(r.raw, "NEW-SMTP") {
		t.Fatalf("восстановление: %d %s", r.status, r.raw)
	}
	var n int
	dst.db.QueryRow("SELECT COUNT(*) FROM nas_users WHERE name = 'owner'").Scan(&n)
	if n != 1 {
		t.Fatal("пользователь не восстановлен")
	}
	// Мастер пройден — повтор из веба запрещён.
	r = dst.do(t, "POST", "/api/v1/config/restore", "application/json", string(body))
	if r.status != 409 || errCode(r) != "setup_done" {
		t.Fatalf("повтор: %d %s", r.status, r.raw)
	}
	// Несовместимая копия — 422 с понятной причиной.
	b, _, _ := confbackup.Collect(t.Context(), src.db, nil, time.Now())
	b.SchemaVersion = store.LatestVersion() + 5
	newer, _ := confbackup.Encode(b)
	r = dst.do(t, "POST", "/api/v1/config/restore/plan", "application/json", `{"file":"`+base64.StdEncoding.EncodeToString(newer)+`"}`)
	if r.status != 422 || errCode(r) != "incompatible" || !strings.Contains(r.raw, "обновите систему") {
		t.Fatalf("несовместимая: %d %s", r.status, r.raw)
	}
	if !bytes.HasPrefix(file, []byte{0x1f, 0x8b}) {
		t.Fatal("не gzip")
	}
}

func TestConfigSFTPAPI(t *testing.T) {
	var uploads []string
	sh := &storageHelper{}
	e := newEnv(t, func(o *Options) {
		o.NAS = nas.NewService(sh, o.DB)
		o.Config = &confbackup.Service{DB: o.DB, Helper: sh, SFTPKeyFile: t.TempDir() + "/k",
			Upload: func(_ context.Context, c confbackup.SFTPConfig, _ ssh.Signer, name string, _ []byte, keep int) (confbackup.SaveResult, error) {
				uploads = append(uploads, c.Host+"/"+name)
				return confbackup.SaveResult{Path: c.Host + ":" + name}, nil
			}}
	})
	r := e.get(t, "/api/v1/config/sftp")
	if r.status != 200 || !strings.Contains(r.raw, `"public_key":"ssh-ed25519 `) || !strings.Contains(r.raw, `"key_fingerprint":"SHA256:`) ||
		strings.Contains(r.raw, "PRIVATE") {
		t.Fatalf("sftp: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "PUT", "/api/v1/config/sftp", "application/json", `{"enabled":true,"host":"bk.lan","port":22,"user":"nas","dir":"","host_key":""}`); r.status != 400 {
		t.Fatalf("без ключа хоста: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/config/sftp/scan", "application/json", `{"host":"a b","port":22}`); r.status != 400 {
		t.Fatalf("скан: %d %s", r.status, r.raw)
	}
	hk := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	r = e.do(t, "PUT", "/api/v1/config/sftp", "application/json", `{"enabled":true,"host":"bk.lan","port":22,"user":"nas","dir":"bk","host_key":"`+hk+`"}`)
	if r.status != 200 || !strings.Contains(r.raw, `"host_fingerprint":"SHA256:`) {
		t.Fatalf("сохранение: %d %s", r.status, r.raw)
	}
	if r := e.do(t, "POST", "/api/v1/config/sftp/upload", "application/json", ``); r.status != 200 || len(uploads) != 1 {
		t.Fatalf("выгрузка: %d %s %v", r.status, r.raw, uploads)
	}
	// Без пулов, но с SFTP — автокопия идёт на SFTP.
	r = e.do(t, "POST", "/api/v1/config/backup", "application/json", ``)
	if r.status != 200 || !strings.Contains(r.raw, `"dest":"sftp"`) || len(uploads) != 2 {
		t.Fatalf("автокопия: %d %s", r.status, r.raw)
	}
}
