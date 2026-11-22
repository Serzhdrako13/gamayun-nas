package confbackup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
)

// Выгрузка копии по сети — SFTP. Ключ SSH
// создаёт NAS (внутренний секрет: файл 0600 в каталоге данных, не в базе и
// не в копии); пароль не хранится. Ключ хоста принимается только после
// сверки отпечатка оператором и затем проверяется при каждом входе.

const keySFTP = "config.sftp"

// SFTPKeyName — файл закрытого ключа в каталоге данных gamayund.
const SFTPKeyName = "sftp_ed25519"

// SFTPConfig — назначение.
type SFTPConfig struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Dir     string `json:"dir"`      // каталог на сервере (пусто — домашний)
	HostKey string `json:"host_key"` // ключ хоста (формат authorized_keys), принятый оператором
}

var (
	hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})$`)
	userRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,31}$`)
)

// ErrSFTP — неверные настройки SFTP.
var ErrSFTP = errors.New("настройки SFTP")

func validHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return true
	}
	return hostRe.MatchString(h)
}

// Validate проверяет настройки (выключенное назначение — тоже, если задано).
func (c SFTPConfig) Validate() error {
	if !c.Enabled && c.Host == "" {
		return nil
	}
	if !validHost(c.Host) {
		return fmt.Errorf("%w: недопустимый адрес сервера", ErrSFTP)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("%w: порт 1..65535", ErrSFTP)
	}
	if !userRe.MatchString(c.User) {
		return fmt.Errorf("%w: недопустимое имя пользователя", ErrSFTP)
	}
	if len(c.Dir) > 200 || strings.ContainsFunc(c.Dir, unicode.IsControl) {
		return fmt.Errorf("%w: каталог до 200 символов без управляющих", ErrSFTP)
	}
	for _, p := range strings.Split(c.Dir, "/") {
		if p == ".." {
			return fmt.Errorf("%w: «..» в каталоге", ErrSFTP)
		}
	}
	if c.HostKey == "" {
		return fmt.Errorf("%w: ключ хоста не подтверждён — проверьте отпечаток", ErrSFTP)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey)); err != nil {
		return fmt.Errorf("%w: ключ хоста: %v", ErrSFTP, err)
	}
	return nil
}

func (c SFTPConfig) addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// Fingerprint — SHA256-отпечаток ключа (как ssh-keygen -l).
func Fingerprint(keyLine string) string {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(keyLine))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

// LoadSFTP — настройки (нет — пустые, выключено).
func LoadSFTP(ctx context.Context, db *sql.DB) (SFTPConfig, error) {
	c := SFTPConfig{Port: 22}
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", keySFTP).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(v), &c)
}

// SaveSFTP проверяет и сохраняет настройки.
func SaveSFTP(ctx context.Context, db *sql.DB, c SFTPConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return setKV(ctx, db, keySFTP, c)
}

// EnsureKey читает или создаёт ключ ed25519 (файл 0600, O_EXCL).
// Возвращает подписанта и открытый ключ в формате authorized_keys.
func EnsureKey(file string) (ssh.Signer, string, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, "", gerr
		}
		blk, merr := ssh.MarshalPrivateKey(priv, "gamayun-nas")
		if merr != nil {
			return nil, "", merr
		}
		b = pem.EncodeToMemory(blk)
		f, oerr := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(oerr, os.ErrExist) {
			return EnsureKey(file) // создан параллельно
		}
		if oerr != nil {
			return nil, "", oerr
		}
		_, werr := f.Write(b)
		serr := f.Sync()
		if cerr := f.Close(); werr == nil && serr == nil && cerr != nil {
			werr = cerr
		}
		if werr != nil || serr != nil {
			os.Remove(file)
			return nil, "", errors.Join(werr, serr)
		}
	} else if err != nil {
		return nil, "", err
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, "", fmt.Errorf("ключ SFTP %s: %w", file, err)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " gamayun-nas"
	return s, pub, nil
}

const sftpTimeout = 30 * time.Second

// ScanHostKey — ключ хоста для сверки отпечатка оператором (без входа).
func ScanHostKey(ctx context.Context, host string, port int) (string, error) {
	if !validHost(host) || port < 1 || port > 65535 {
		return "", fmt.Errorf("%w: адрес или порт", ErrSFTP)
	}
	var got ssh.PublicKey
	cfg := &ssh.ClientConfig{User: "gamayun-scan", Timeout: sftpTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error { got = k; return errStopScan }}
	d := net.Dialer{Timeout: sftpTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(sftpTimeout))
	_, _, _, err = ssh.NewClientConn(conn, host, cfg)
	if got == nil {
		return "", fmt.Errorf("ключ хоста не получен: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(got))), nil
}

var errStopScan = errors.New("scan")

// dialSFTP — вход по ключу NAS с проверкой ключа хоста.
func dialSFTP(ctx context.Context, c SFTPConfig, signer ssh.Signer) (*ssh.Client, *sftpClient, func(), error) {
	want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if err != nil {
		return nil, nil, nil, err
	}
	cfg := &ssh.ClientConfig{User: c.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout: sftpTimeout,
		HostKeyAlgorithms: hostKeyAlgos(want.Type()),
		HostKeyCallback:   ssh.FixedHostKey(want)}
	d := net.Dialer{Timeout: sftpTimeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr())
	if err != nil {
		return nil, nil, nil, err
	}
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	cc, chans, reqs, err := ssh.NewClientConn(conn, c.addr(), cfg)
	if err != nil {
		conn.Close()
		if strings.Contains(err.Error(), "host key mismatch") {
			return nil, nil, nil, fmt.Errorf("ключ хоста %s изменился — выгрузка остановлена (возможна подмена сервера); проверьте и подтвердите отпечаток заново", c.Host)
		}
		return nil, nil, nil, err
	}
	cl := ssh.NewClient(cc, chans, reqs)
	sess, err := cl.NewSession()
	if err != nil {
		cl.Close()
		return nil, nil, nil, err
	}
	w, _ := sess.StdinPipe()
	r, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		sess.Close()
		cl.Close()
		return nil, nil, nil, fmt.Errorf("подсистема sftp: %w", err)
	}
	fc, err := newSFTP(w, r)
	if err != nil {
		sess.Close()
		cl.Close()
		return nil, nil, nil, err
	}
	return cl, fc, func() { sess.Close(); cl.Close() }, nil
}

// UploadSFTP пишет копию во временный файл, переименовывает в итоговое имя
// и удаляет старые копии сверх keep.
func UploadSFTP(ctx context.Context, c SFTPConfig, signer ssh.Signer, name string, data []byte, keep int) (SaveResult, error) {
	if err := c.Validate(); err != nil {
		return SaveResult{}, err
	}
	if err := validFile(name, data); err != nil {
		return SaveResult{}, err
	}
	_, fc, closeFn, err := dialSFTP(ctx, c, signer)
	if err != nil {
		return SaveResult{}, err
	}
	defer closeFn()
	dir := c.Dir
	if dir == "" {
		dir = "."
	}
	tmp := path.Join(dir, ".tmp-"+name)
	final := path.Join(dir, name)
	if err := fc.writeFile(tmp, data); err != nil {
		return SaveResult{}, fmt.Errorf("запись %s: %w", tmp, err)
	}
	if err := fc.rename(tmp, final); err != nil {
		fc.remove(tmp)
		return SaveResult{}, fmt.Errorf("переименование в %s: %w", final, err)
	}
	res := SaveResult{Path: c.User + "@" + c.Host + ":" + final}
	names, err := fc.readDir(dir)
	if err != nil {
		return res, nil // копия записана; ротация — в следующий раз
	}
	var ours []string
	for _, n := range names {
		if nameRe.MatchString(n) {
			ours = append(ours, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ours)))
	for i := keep; i < len(ours); i++ {
		if fc.remove(path.Join(dir, ours[i])) == nil {
			res.Removed = append(res.Removed, ours[i])
		}
	}
	return res, nil
}

// hostKeyAlgos — алгоритмы подписи для типа ключа хоста (RSA — только SHA-2).
func hostKeyAlgos(t string) []string {
	if t == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{t}
}
