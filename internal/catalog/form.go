package catalog

// Форма установки → переменные .env; совместимость с NAS.

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// randomAlphabet — символы random-секретов: без кавычек, $, # и пробелов
// (значение попадает в .env и интерполяцию compose без экранирования).
const randomAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// DefaultRandomLength — длина random-поля по умолчанию.
const DefaultRandomLength = 32

// Random — криптостойкая строка из randomAlphabet.
func Random(n int) string {
	if n <= 0 {
		n = DefaultRandomLength
	}
	b := make([]byte, n)
	max := big.NewInt(int64(len(randomAlphabet)))
	for i := range b {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err) // crypto/rand не отказывает на Linux
		}
		b[i] = randomAlphabet[k.Int64()]
	}
	return string(b)
}

// FormResult — итог заполнения формы.
type FormResult struct {
	// Env — переменные для .env (поле share — имя папки, путь подставляет вызывающий).
	Env map[string]string
	// Secrets — ключи секретных полей (значения не показывать и не журналировать).
	Secrets []string
	// Shares — поля типа share: ключ → имя общей папки.
	Shares map[string]string
}

// visible — поле показано: show_if "KEY" (bool true) или "KEY=значение".
func visible(f Field, env map[string]string) bool {
	if f.ShowIf == "" {
		return true
	}
	k, v, eq := strings.Cut(f.ShowIf, "=")
	if !eq {
		return env[k] == "true"
	}
	return env[k] == v
}

// RenderForm проверяет значения пользователя по полям формы и строит
// переменные. Скрытые (show_if) поля не попадают в .env; random без
// значения генерируется (gen; nil — Random). Ошибки — все одной пачкой,
// без значений секретов.
func RenderForm(fields []Field, values map[string]string, gen func(int) string) (FormResult, []error) {
	if gen == nil {
		gen = Random
	}
	res := FormResult{Env: map[string]string{}, Secrets: []string{}, Shares: map[string]string{}}
	var errs []error
	bad := func(f Field, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", f.Key, fmt.Sprintf(format, a...)))
	}
	known := map[string]bool{}
	for _, f := range fields {
		known[f.Key] = true
	}
	for k := range values {
		if !known[k] {
			errs = append(errs, fmt.Errorf("%s: нет такого поля формы", k))
		}
	}
	// show_if ссылается на поля выше по форме: проходим по порядку.
	for _, f := range fields {
		if !visible(f, res.Env) {
			continue
		}
		v, given := values[f.Key]
		if !given || v == "" {
			v = f.Default
		}
		if strings.ContainsAny(v, "\x00\n\r") {
			bad(f, "недопустимые символы (перевод строки)")
			continue
		}
		switch f.Type {
		case "random":
			if v == "" {
				v = gen(f.Length)
			}
		case "bool":
			if v == "" {
				v = "false"
			}
			if v != "true" && v != "false" {
				bad(f, "ожидается true или false")
				continue
			}
		case "int":
			if v != "" {
				if _, err := strconv.ParseInt(v, 10, 64); err != nil {
					bad(f, "ожидается целое число")
					continue
				}
			}
		case "select":
			if v != "" && !contains(f.Options, v) {
				bad(f, "значение не из списка")
				continue
			}
		}
		if f.Required && v == "" {
			bad(f, "обязательное поле")
			continue
		}
		if f.Regex != "" && v != "" {
			re, err := regexp.Compile(f.Regex)
			if err != nil || !re.MatchString(v) {
				bad(f, "значение не соответствует шаблону")
				continue
			}
		}
		if f.Type == "share" && v != "" {
			res.Shares[f.Key] = v
		}
		if f.Secret {
			res.Secrets = append(res.Secrets, f.Key)
		}
		res.Env[f.Key] = v
	}
	sort.Strings(res.Secrets)
	return res, errs
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// EnvText — переменные в формате .env: значения в одинарных кавычках
// (без интерпретации # и пробелов); значение с ' не поддерживается.
func EnvText(env map[string]string) (string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := env[k]
		if strings.ContainsAny(v, "'\x00\n\r") {
			return "", fmt.Errorf("%s: символ ' и переводы строк не поддерживаются", k)
		}
		b.WriteString(k + "='" + v + "'\n")
	}
	return b.String(), nil
}

// System — сведения о NAS для проверки совместимости.
type System struct {
	OSVersion  string // версия gamayun-nas
	Arch       string // amd64|arm64
	RAMMB      int64
	DiskFreeMB int64 // свободно на пуле приложений
}

// Compat — несовместимость приложения с NAS (пусто — совместимо).
// Версия "dev" (сборка разработчика) проходит проверку os_min.
func Compat(m Manifest, sys System) []error {
	var errs []error
	if m.OSMin != "" && sys.OSVersion != "dev" && CompareVersion(sys.OSVersion, m.OSMin) < 0 {
		errs = append(errs, fmt.Errorf("нужна версия gamayun-nas не ниже %s (установлена %s)", m.OSMin, sys.OSVersion))
	}
	if !contains(m.Architectures, sys.Arch) {
		errs = append(errs, fmt.Errorf("архитектура %s не поддерживается (есть: %s)", sys.Arch, strings.Join(m.Architectures, ", ")))
	}
	if m.MinRAMMB > 0 && sys.RAMMB < int64(m.MinRAMMB) {
		errs = append(errs, fmt.Errorf("нужно памяти не меньше %d МиБ (есть %d)", m.MinRAMMB, sys.RAMMB))
	}
	if m.MinDiskMB > 0 && sys.DiskFreeMB < int64(m.MinDiskMB) {
		errs = append(errs, fmt.Errorf("нужно свободного места не меньше %d МиБ (свободно %d)", m.MinDiskMB, sys.DiskFreeMB))
	}
	return errs
}
