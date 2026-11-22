package compose

// Секреты приложения каталога — по файлу на сервис
// secrets-<сервис>.env в каталоге проекта (0600), только те секреты, на
// которые сервис ссылается; в контейнер через env_file. В нормализованный compose
// значения не попадают. Допустимая ссылка на секрет KEY — только
// environment KEY: ${KEY} (имя переменной контейнера = ключ секрета).

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Файлы секретов сервиса: SecretsPrefix + сервис + SecretsSuffix.
const (
	SecretsPrefix = "secrets-"
	SecretsSuffix = ".env"
)

// SecretsFile — файл секретов сервиса в каталоге проекта.
func SecretsFile(svc string) string { return SecretsPrefix + svc + SecretsSuffix }

// SecretsService — сервис по имени файла секретов ("" — не файл секретов).
func SecretsService(name string) string {
	if !strings.HasPrefix(name, SecretsPrefix) || !strings.HasSuffix(name, SecretsSuffix) {
		return ""
	}
	svc := strings.TrimSuffix(strings.TrimPrefix(name, SecretsPrefix), SecretsSuffix)
	if !ValidService(svc) {
		return ""
	}
	return svc
}

// ValidService — допустимое имя сервиса compose.
func ValidService(s string) bool { return svcRe.MatchString(s) }

// FSecretRef — секрет используется не как environment KEY: ${KEY}.
const FSecretRef = "secret_ref"

func secretRef(v any, k string) bool {
	s, ok := scalar(v)
	return ok && (s == "${"+k+"}" || s == "$"+k)
}

// stripSecrets убирает из environment сервисов допустимые ссылки на
// секреты (до подстановок) и возвращает ключи секретов каждого сервиса
// и ошибки — прочие ссылки на секреты в документе.
func stripSecrets(top map[string]any, keys map[string]bool) (map[string][]string, []string) {
	need := map[string][]string{}
	svcs, _ := asMap(top["services"])
	for name, v := range svcs {
		m, ok := asMap(v)
		if !ok {
			continue
		}
		switch t := m["environment"].(type) {
		case []any:
			var keep []any
			for _, x := range t {
				s, _ := scalar(x)
				k, val, _ := strings.Cut(s, "=")
				if keys[k] && (val == "${"+k+"}" || val == "$"+k) {
					need[name] = append(need[name], k)
					continue
				}
				keep = append(keep, x)
			}
			m["environment"] = keep
		default:
			if em, ok := asMap(t); ok {
				for k, x := range em {
					if keys[k] && secretRef(x, k) {
						delete(em, k)
						need[name] = append(need[name], k)
					}
				}
				m["environment"] = em
			}
		}
		svcs[name] = m
	}
	for _, l := range need {
		sort.Strings(l)
	}
	var errs []string
	var walk func(v any)
	refs := map[string]bool{}
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for k := range keys {
				if regexp.MustCompile(`\$\{?` + k + `([^A-Za-z0-9_]|$)`).MatchString(t) {
					refs[k] = true
				}
			}
		case map[string]any:
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		default:
			if m, ok := asMap(t); ok {
				walk(m)
			}
		}
	}
	for k, v := range top {
		if !strings.HasPrefix(k, "x-") {
			walk(v)
		}
	}
	for k := range refs {
		errs = append(errs, fmt.Sprintf("секрет %s можно передать только как environment %s: ${%s}", k, k, k))
	}
	sort.Strings(errs)
	return need, errs
}

// CheckSecretRefs — проверка compose каталога: секреты используются только
// как environment KEY: ${KEY}.
func CheckSecretRefs(src []byte, keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	var doc any
	if err := yaml.NewDecoder(bytes.NewReader(src)).Decode(&doc); err != nil {
		return []string{"ошибка YAML: " + yamlErr(err)}
	}
	top, ok := asMap(doc)
	if !ok {
		return []string{"корень — не словарь"}
	}
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	_, errs := stripSecrets(top, set)
	return errs
}
