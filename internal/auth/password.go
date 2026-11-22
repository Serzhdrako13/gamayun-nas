package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params — параметры argon2id. Хранятся в самой строке хэша (формат PHC),
// поэтому их можно менять: старые хэши проверяются со своими параметрами
// и пересчитываются с новыми при следующем успешном входе.
type Params struct {
	Memory  uint32 // КиБ
	Time    uint32 // проходы
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams — argon2id m=19 МиБ, t=2, p=1 (минимум OWASP Password
// Storage Cheat Sheet). Выбор под SBC: 64 МиБ из RFC 9106 при нескольких
// одновременных входах заметны на плате с 512 МиБ–1 ГиБ ОЗУ, а
// одновременных вычислений не больше MaxConcurrentHashes (≈ 38 МиБ пика).
// Время (замер 2026-11-05, i5-13500): 14 мс, 18 мс без ассемблера
// (-tags purego — так работает arm64, где у x/crypto/argon2 его нет);
// на Cortex-A72/A53 оценочно 0,1–0,4 с — не измерялось.
var DefaultParams = Params{Memory: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// MaxConcurrentHashes — сколько вычислений argon2id идёт одновременно в
// демоне (ограничение памяти и CPU при потоке попыток входа).
const MaxConcurrentHashes = 2

// Границы пароля: длина в символах и в байтах.
const (
	MinPasswordLen   = 10 // 8 было временно для стенда (2026-11-05…06)
	MaxPasswordBytes = 1024
)

var (
	// ErrWeakPassword — пароль не проходит требования.
	ErrWeakPassword = errors.New("пароль не подходит")
	// ErrBadName — недопустимое имя пользователя.
	ErrBadName = errors.New("недопустимое имя пользователя")
	errBadHash = errors.New("неверный формат хэша пароля")
)

// nameRe — имя совместимо с именами пользователей Linux/Samba:
// строчные латинские буквы, цифры, '_', '-', начинается с буквы или '_'.
var nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidateName проверяет имя пользователя.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: 1–32 символа a-z, 0-9, '_', '-', начинается с буквы или '_'", ErrBadName)
	}
	return nil
}

// ValidatePassword проверяет новый пароль.
func ValidatePassword(pw, name string) error {
	switch {
	case !utf8.ValidString(pw) || strings.ContainsRune(pw, 0):
		return fmt.Errorf("%w: недопустимые символы", ErrWeakPassword)
	case utf8.RuneCountInString(pw) < MinPasswordLen:
		return fmt.Errorf("%w: не короче %d символов", ErrWeakPassword, MinPasswordLen)
	case len(pw) > MaxPasswordBytes:
		return fmt.Errorf("%w: не длиннее %d байт", ErrWeakPassword, MaxPasswordBytes)
	case strings.EqualFold(pw, name):
		return fmt.Errorf("%w: совпадает с именем", ErrWeakPassword)
	}
	return nil
}

var b64 = base64.RawStdEncoding

// HashPassword возвращает строку PHC: $argon2id$v=19$m=..,t=..,p=..$соль$хэш.
func HashPassword(pw string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword сравнивает пароль с хэшем за постоянное время.
// needsRehash — хэш сделан с параметрами, отличными от want.
func VerifyPassword(pw, encoded string, want Params) (ok, needsRehash bool, err error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, uint32(len(key)))
	ok = subtle.ConstantTimeCompare(got, key) == 1
	needsRehash = p.Memory != want.Memory || p.Time != want.Time || p.Threads != want.Threads ||
		uint32(len(salt)) != want.SaltLen || uint32(len(key)) != want.KeyLen
	return ok, needsRehash, nil
}

func decodeHash(s string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errBadHash
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return p, nil, nil, errBadHash
	}
	var threads uint32
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &threads); err != nil || n != 3 {
		return p, nil, nil, errBadHash
	}
	// Границы защищают от испорченной записи в базе (1 ГиБ, 64 прохода).
	if p.Memory < 8 || p.Memory > 1<<20 || p.Time < 1 || p.Time > 64 || threads < 1 || threads > 64 {
		return p, nil, nil, errBadHash
	}
	p.Threads = uint8(threads)
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) < 8 {
		return p, nil, nil, errBadHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, errBadHash
	}
	return p, salt, key, nil
}
