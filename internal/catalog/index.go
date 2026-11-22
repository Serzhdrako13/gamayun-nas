package catalog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// IndexSchema — версия формата индекса.
const IndexSchema = 1

// Имена файлов каталога.
const (
	IndexFile = "index.json"
	SigFile   = "index.json.sig"
	// sigMagic — первая строка файла подписи: формат версионируется
	// (позже можно добавить cosign отдельным форматом).
	sigMagic = "gamayun-catalog-sig v1 ed25519"
)

// IndexApp — приложение в индексе.
type IndexApp struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	AppVersion string `json:"app_version"`
	Name       Text   `json:"name"`
	// Files — путь внутри каталога → sha256 (hex). Обязателен compose.yaml.
	Files map[string]string `json:"files"`
}

// Index — подписываемый список приложений. Serial только растёт.
type Index struct {
	Schema  int        `json:"schema"`
	Serial  int64      `json:"serial"`
	Created time.Time  `json:"created"`
	Apps    []IndexApp `json:"apps"`
}

// Signature — разобранный файл подписи.
type Signature struct {
	KeyID string
	Sig   []byte
}

// KeyID — короткий отпечаток открытого ключа (первые 8 байт sha256, hex).
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// SHA256 — hex sha256.
func SHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Marshal — каноничный JSON индекса (приложения по id, отступы).
func (ix Index) Marshal() ([]byte, error) {
	sort.Slice(ix.Apps, func(i, j int) bool { return ix.Apps[i].ID < ix.Apps[j].ID })
	b, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Validate — структура индекса.
func (ix Index) Validate() error {
	if ix.Schema != IndexSchema {
		return fmt.Errorf("индекс: schema %d не поддерживается", ix.Schema)
	}
	if ix.Serial <= 0 {
		return errors.New("индекс: serial должен быть > 0")
	}
	seen := map[string]bool{}
	for _, a := range ix.Apps {
		if !idRe.MatchString(a.ID) || seen[a.ID] {
			return fmt.Errorf("индекс: неверный или повторный id %q", a.ID)
		}
		seen[a.ID] = true
		if _, ok := a.Files["compose.yaml"]; !ok {
			return fmt.Errorf("индекс: %s без compose.yaml", a.ID)
		}
		for p, h := range a.Files {
			if !SafePath(p) {
				return fmt.Errorf("индекс: %s: недопустимый путь %q", a.ID, p)
			}
			if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
				return fmt.Errorf("индекс: %s: неверный sha256 у %s", a.ID, p)
			}
		}
	}
	return nil
}

// SafePath — относительный путь без выхода наружу и скрытых частей.
func SafePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// Sign — файл подписи для байт индекса.
func Sign(priv ed25519.PrivateKey, index []byte) []byte {
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, index)
	return []byte(fmt.Sprintf("%s\n%s %s\n", sigMagic, KeyID(pub), base64.StdEncoding.EncodeToString(sig)))
}

// ParseSignature разбирает файл подписи.
func ParseSignature(b []byte) (Signature, error) {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 || lines[0] != sigMagic {
		return Signature{}, errors.New("подпись: неизвестный формат")
	}
	f := strings.Fields(lines[1])
	if len(f) != 2 {
		return Signature{}, errors.New("подпись: неверная строка")
	}
	sig, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Signature{}, errors.New("подпись: неверное значение")
	}
	return Signature{KeyID: f[0], Sig: sig}, nil
}

// ErrBadSignature — подпись не сходится или ключ не доверенный.
var ErrBadSignature = errors.New("подпись каталога не прошла проверку")

// Verify проверяет подпись индекса доверенными ключами и разбирает индекс.
func Verify(index, sigFile []byte, trusted []ed25519.PublicKey) (Index, error) {
	s, err := ParseSignature(sigFile)
	if err != nil {
		return Index{}, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	ok := false
	for _, k := range trusted {
		if KeyID(k) == s.KeyID && ed25519.Verify(k, index, s.Sig) {
			ok = true
			break
		}
	}
	if !ok {
		return Index{}, ErrBadSignature
	}
	var ix Index
	if err := json.Unmarshal(index, &ix); err != nil {
		return Index{}, fmt.Errorf("индекс: %w", err)
	}
	return ix, ix.Validate()
}

// CheckFile — файл приложения совпадает с хэшем индекса.
func (a IndexApp) CheckFile(p string, b []byte) error {
	want, ok := a.Files[p]
	if !ok {
		return fmt.Errorf("%s: файл %s не в индексе", a.ID, p)
	}
	if SHA256(b) != want {
		return fmt.Errorf("%s: файл %s подменён (sha256 не совпадает)", a.ID, p)
	}
	return nil
}

// ErrSerialRollback — каталог старее установленного.
var ErrSerialRollback = errors.New("версия каталога старее текущей")

// CheckSerial — откат номера каталога запрещён; разрешён только явно
// (импорт файлом с подтверждением пользователя).
func CheckSerial(current, next int64, allowRollback bool) error {
	if next < current && !allowRollback {
		return fmt.Errorf("%w: %d < %d", ErrSerialRollback, next, current)
	}
	return nil
}
