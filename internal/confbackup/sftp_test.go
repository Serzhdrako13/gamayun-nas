package confbackup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const sftpServer = "/usr/lib/ssh/sftp-server"

// testSSHServer — SSH-сервер в процессе: вход только по ключу allowed,
// подсистема sftp — системный sftp-server в каталоге root.
func testSSHServer(t *testing.T, root string, allowed ssh.PublicKey) (host string, port int, hostKey ssh.Signer) {
	t.Helper()
	if _, err := os.Stat(sftpServer); err != nil {
		t.Skip("нет " + sftpServer)
	}
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, _ = ssh.NewSignerFromKey(hk)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if allowed != nil && string(k.Marshal()) == string(allowed.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("ключ не разрешён")
	}}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, _ := nc.Accept()
					go func() {
						for r := range creqs {
							if r.Type != "subsystem" || !strings.HasSuffix(string(r.Payload), "sftp") {
								r.Reply(false, nil)
								continue
							}
							r.Reply(true, nil)
							cmd := osexec.Command(sftpServer)
							cmd.Dir = root
							cmd.Stdin, cmd.Stdout = ch, ch
							cmd.Run()
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
							ch.Close()
						}
					}()
				}
			}()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port, hostKey
}

func TestSFTPUpload(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), SFTPKeyName)
	signer, pub, err := EnsureKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(keyFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("права ключа %v", fi.Mode())
	}
	if s2, pub2, _ := EnsureKey(keyFile); pub2 != pub || s2 == nil {
		t.Fatal("ключ пересоздан")
	}
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "bk"), 0o700)
	host, port, hk := testSSHServer(t, root, signer.PublicKey())
	ctx := context.Background()

	got, err := ScanHostKey(ctx, host, port)
	if err != nil || Fingerprint(got) != ssh.FingerprintSHA256(hk.PublicKey()) {
		t.Fatalf("скан: %v %q", err, got)
	}
	c := SFTPConfig{Enabled: true, Host: host, Port: port, User: "nas", Dir: filepath.Join(root, "bk"), HostKey: got}
	data := gz(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < Keep+2; i++ {
		if _, err := UploadSFTP(ctx, c, signer, FileName(base.Add(time.Duration(i)*time.Hour), "nas1"), data, Keep); err != nil {
			t.Fatalf("выгрузка %d: %v", i, err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(root, "bk"))
	if len(ents) != Keep || ents[len(ents)-1].Name() != FileName(base.Add(time.Duration(Keep+1)*time.Hour), "nas1") {
		t.Fatalf("на сервере %d файлов", len(ents))
	}
	b, _ := os.ReadFile(filepath.Join(root, "bk", ents[0].Name()))
	if string(b) != string(data) {
		t.Fatal("содержимое не совпало")
	}
	// Тот же файл повторно — отказ (без перезаписи).
	if _, err := UploadSFTP(ctx, c, signer, FileName(base.Add(time.Duration(Keep+1)*time.Hour), "nas1"), data, Keep); err == nil {
		t.Fatal("перезапись принята")
	}
	// Подмена ключа хоста — отказ с понятной причиной.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os2, _ := ssh.NewSignerFromKey(other)
	bad := c
	bad.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(os2.PublicKey())))
	if _, err := UploadSFTP(ctx, bad, signer, FileName(base.Add(99*time.Hour), ""), data, Keep); err == nil || !strings.Contains(err.Error(), "ключ хоста") {
		t.Fatalf("подмена хоста: %v", err)
	}
	// Чужой ключ NAS — вход не проходит.
	_, k3, _ := ed25519.GenerateKey(rand.Reader)
	s3, _ := ssh.NewSignerFromKey(k3)
	if _, err := UploadSFTP(ctx, c, s3, FileName(base.Add(98*time.Hour), ""), data, Keep); err == nil {
		t.Fatal("вход чужим ключом")
	}
	// Нет каталога — ошибка SFTP.
	nd := c
	nd.Dir = filepath.Join(root, "none")
	if _, err := UploadSFTP(ctx, nd, signer, FileName(base.Add(97*time.Hour), ""), data, Keep); err == nil {
		t.Fatal("нет каталога — без ошибки")
	}
}

func TestSFTPValidate(t *testing.T) {
	ok := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	bad := []SFTPConfig{
		{Enabled: true, Host: "a b", Port: 22, User: "u", HostKey: ok},
		{Enabled: true, Host: "h", Port: 0, User: "u", HostKey: ok},
		{Enabled: true, Host: "h", Port: 22, User: "-u", HostKey: ok},
		{Enabled: true, Host: "h", Port: 22, User: "u", Dir: "../etc", HostKey: ok},
		{Enabled: true, Host: "h", Port: 22, User: "u"},
		{Enabled: true, Host: "h", Port: 22, User: "u", HostKey: "junk"},
	}
	for i, c := range bad {
		if c.Validate() == nil {
			t.Errorf("%d: принято %+v", i, c)
		}
	}
	if err := (SFTPConfig{Enabled: true, Host: "backup.lan", Port: 2222, User: "nas", Dir: "/srv/bk", HostKey: ok}).Validate(); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(ok) == "" || !strings.HasPrefix(Fingerprint(ok), "SHA256:") {
		t.Fatal(Fingerprint(ok))
	}
}
