package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Пределы архива каталога.
const (
	MaxArchive = 64 << 20  // сжатый
	MaxFiles   = 2000      // файлов в архиве
	MaxFile    = 8 << 20   // один файл
	MaxTotal   = 128 << 20 // распакованный
)

// stateKey — состояние каталога в settings.
const stateKey = "catalog.state"

// State — установленный каталог.
type State struct {
	Serial   int64     `json:"serial"`
	KeyID    string    `json:"key_id"`
	Source   string    `json:"source"` // URL или "file"
	LoadedAt time.Time `json:"loaded_at"`
	Apps     int       `json:"apps"`
}

// Store — каталог на NAS: <Dir>/current (проверенная распаковка) и номер в БД.
type Store struct {
	DB      *sql.DB
	Dir     string
	Trusted []ed25519.PublicKey
	// AllowHTTP — разрешить http:// (только стенд).
	AllowHTTP bool
	Client    *http.Client
	Now       func() time.Time

	mu sync.Mutex
}

// Extract распаковывает tar.gz в память: только обычные файлы с
// безопасными путями, без повторов, в пределах лимитов.
func Extract(r io.Reader) (map[string][]byte, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, MaxArchive+1))
	if err != nil {
		return nil, fmt.Errorf("архив: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("архив: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("архив: %s — допустимы только обычные файлы", h.Name)
		}
		if !SafePath(name) {
			return nil, fmt.Errorf("архив: недопустимый путь %q", h.Name)
		}
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("архив: повтор %s", name)
		}
		if len(files) >= MaxFiles || h.Size > MaxFile || total+h.Size > MaxTotal {
			return nil, errors.New("архив: превышены пределы размера")
		}
		b, err := io.ReadAll(io.LimitReader(tr, MaxFile+1))
		if err != nil {
			return nil, fmt.Errorf("архив: %w", err)
		}
		if int64(len(b)) != h.Size {
			return nil, fmt.Errorf("архив: %s: размер не совпадает", name)
		}
		total += h.Size
		files[name] = b
	}
	return files, nil
}

// Pack — tar.gz из файлов (для сборщика и тестов), порядок детерминирован.
func Pack(files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Check — полная проверка распакованного каталога: подпись, хэши и
// отсутствие лишних файлов, манифесты совпадают с индексом.
func Check(files map[string][]byte, trusted []ed25519.PublicKey) (Index, string, error) {
	idx, sig := files[IndexFile], files[SigFile]
	if idx == nil || sig == nil {
		return Index{}, "", errors.New("каталог: нет index.json или index.json.sig")
	}
	ix, err := Verify(idx, sig, trusted)
	if err != nil {
		return Index{}, "", err
	}
	s, _ := ParseSignature(sig)
	known := map[string]bool{IndexFile: true, SigFile: true}
	for _, a := range ix.Apps {
		for p := range a.Files {
			full := "apps/" + a.ID + "/" + p
			if err := a.CheckFile(p, files[full]); err != nil {
				return Index{}, "", err
			}
			known[full] = true
		}
		app, errs := Parse(files["apps/"+a.ID+"/compose.yaml"])
		if len(errs) > 0 {
			return Index{}, "", fmt.Errorf("%s: %v", a.ID, errs[0])
		}
		if app.Manifest.ID != a.ID || app.Manifest.Version != a.Version {
			return Index{}, "", fmt.Errorf("%s: манифест не совпадает с индексом", a.ID)
		}
	}
	for p := range files {
		if !known[p] {
			return Index{}, "", fmt.Errorf("каталог: лишний файл %s вне индекса", p)
		}
	}
	return ix, s.KeyID, nil
}

// State — текущее состояние (нулевое — каталог не загружался).
func (s *Store) State(ctx context.Context) (State, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", stateKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	return st, json.Unmarshal([]byte(v), &st)
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Load проверяет архив и атомарно ставит его текущим каталогом.
// allowRollback — только импорт файлом с подтверждением пользователя.
func (s *Store) Load(ctx context.Context, archive []byte, source string, allowRollback bool) (State, error) {
	files, err := Extract(bytes.NewReader(archive))
	if err != nil {
		return State{}, err
	}
	ix, keyID, err := Check(files, s.Trusted)
	if err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.State(ctx)
	if err != nil {
		return State{}, err
	}
	if err := CheckSerial(cur.Serial, ix.Serial, allowRollback); err != nil {
		return State{}, err
	}
	if err := s.install(files); err != nil {
		return State{}, err
	}
	st := State{Serial: ix.Serial, KeyID: keyID, Source: source, LoadedAt: s.now().UTC(), Apps: len(ix.Apps)}
	b, _ := json.Marshal(st)
	_, err = s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, stateKey, string(b))
	return st, err
}

// install — запись во временный каталог и замена current переименованием;
// предыдущий сохраняется как previous.
func (s *Store) install(files map[string][]byte) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.Dir, ".new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for p, b := range files {
		full := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			return err
		}
	}
	cur, prev := filepath.Join(s.Dir, "current"), filepath.Join(s.Dir, "previous")
	if _, err := os.Stat(cur); err == nil {
		if err := os.RemoveAll(prev); err != nil {
			return err
		}
		if err := os.Rename(cur, prev); err != nil {
			return err
		}
	}
	return os.Rename(tmp, cur)
}

// Fetch скачивает архив по URL (HTTPS; http — только AllowHTTP) и загружает.
func (s *Store) Fetch(ctx context.Context, raw string) (State, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && s.AllowHTTP)) {
		return State{}, fmt.Errorf("адрес каталога %q: нужен https://", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return State{}, err
	}
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return State{}, fmt.Errorf("загрузка каталога: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return State{}, fmt.Errorf("загрузка каталога: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxArchive+1))
	if err != nil {
		return State{}, fmt.Errorf("загрузка каталога: %w", err)
	}
	if len(b) > MaxArchive {
		return State{}, errors.New("загрузка каталога: архив больше предела")
	}
	return s.Load(ctx, b, u.Redacted(), false)
}

// Entry — приложение текущего каталога.
type Entry struct {
	Manifest Manifest          `json:"manifest"`
	Images   map[string]string `json:"images"`
}

// current — проверенный индекс текущего каталога (nil — каталог не загружен).
func (s *Store) current() (*Index, error) {
	cur := filepath.Join(s.Dir, "current")
	idx, err := os.ReadFile(filepath.Join(cur, IndexFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sig, err := os.ReadFile(filepath.Join(cur, SigFile))
	if err != nil {
		return nil, err
	}
	ix, err := Verify(idx, sig, s.Trusted)
	if err != nil {
		return nil, err
	}
	return &ix, nil
}

func (s *Store) parse(a IndexApp) (App, error) {
	b, err := s.File(a, "compose.yaml")
	if err != nil {
		return App{}, err
	}
	app, errs := Parse(b)
	if len(errs) > 0 {
		return App{}, fmt.Errorf("%s: %v", a.ID, errs[0])
	}
	return app, nil
}

// Apps — приложения текущего каталога (перепроверяются хэши файлов на диске).
func (s *Store) Apps() ([]Entry, error) {
	ix, err := s.current()
	if ix == nil || err != nil {
		return nil, err
	}
	var out []Entry
	for _, a := range ix.Apps {
		app, err := s.parse(a)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{Manifest: app.Manifest, Images: app.Images})
	}
	return out, nil
}

// ErrNoApp — приложения нет в текущем каталоге.
var ErrNoApp = errors.New("приложения нет в каталоге")

// App — приложение текущего каталога с перепроверкой подписи и хэшей;
// serial — номер каталога, из которого оно взято.
func (s *Store) App(id string) (App, int64, error) {
	ix, err := s.current()
	if err != nil {
		return App{}, 0, err
	}
	if ix == nil {
		return App{}, 0, ErrNoApp
	}
	for _, a := range ix.Apps {
		if a.ID == id {
			app, err := s.parse(a)
			return app, ix.Serial, err
		}
	}
	return App{}, 0, fmt.Errorf("%w: %s", ErrNoApp, id)
}

// File — файл приложения текущего каталога с проверкой хэша.
func (s *Store) File(a IndexApp, p string) ([]byte, error) {
	if !SafePath(p) {
		return nil, fmt.Errorf("недопустимый путь %q", p)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "current", "apps", a.ID, filepath.FromSlash(p)))
	if err != nil {
		return nil, err
	}
	return b, a.CheckFile(p, b)
}

// LoadKeys — открытые ключи (base64 по одному на файл *.pub) из каталога;
// нет каталога — пустой список.
func LoadKeys(dir string) ([]ed25519.PublicKey, error) {
	l, err := filepath.Glob(filepath.Join(dir, "*.pub"))
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for _, f := range l {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s: неверный открытый ключ", f)
		}
		keys = append(keys, ed25519.PublicKey(k))
	}
	return keys, nil
}
