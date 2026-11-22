package compose

import (
	"fmt"
	"strings"
)

// interpolate подставляет переменные в строку по правилам Compose:
// $$ → $, $VAR, ${VAR}, ${VAR:-d}, ${VAR-d}, ${VAR:?e}, ${VAR?e}, ${VAR:+r}, ${VAR+r}.
// Источник — только env проекта (.env), переменные среды хоста не
// используются. missing получает имена неопределённых переменных.
func interpolate(s string, env map[string]string, missing map[string]bool) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			continue
		}
		n := s[i+1]
		switch {
		case n == '$':
			b.WriteByte('$')
			i++
		case n == '{':
			end := matchBrace(s, i+1)
			if end < 0 {
				return "", fmt.Errorf("незакрытая подстановка в %q", short(s))
			}
			v, err := expr(s[i+2:end], env, missing)
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i = end
		case isNameStart(n):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			name := s[i+1 : j]
			v, ok := env[name]
			if !ok {
				missing[name] = true
			}
			b.WriteString(v)
			i = j - 1
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

// matchBrace — индекс '}' для '{' в позиции open (с учётом вложенности).
func matchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func expr(e string, env map[string]string, missing map[string]bool) (string, error) {
	j := 0
	for j < len(e) && isNameChar(e[j]) {
		j++
	}
	name := e[:j]
	if name == "" || !isNameStart(name[0]) {
		return "", fmt.Errorf("недопустимая подстановка ${%s}", short(e))
	}
	rest := e[j:]
	v, set := env[name]
	if rest == "" {
		if !set {
			missing[name] = true
		}
		return v, nil
	}
	var op string
	for _, o := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, o) {
			op = o
			break
		}
	}
	if op == "" {
		return "", fmt.Errorf("недопустимая подстановка ${%s}", short(e))
	}
	arg := rest[len(op):]
	nonEmpty := set && (v != "" || !strings.HasPrefix(op, ":"))
	switch op[len(op)-1] {
	case '-':
		if nonEmpty {
			return v, nil
		}
		return interpolate(arg, env, missing)
	case '?':
		if nonEmpty {
			return v, nil
		}
		return "", fmt.Errorf("переменная %s не задана: %s", name, short(arg))
	default: // '+'
		if nonEmpty {
			return interpolate(arg, env, missing)
		}
		return "", nil
	}
}

func isNameStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isNameChar(c byte) bool  { return isNameStart(c) || c >= '0' && c <= '9' }

func short(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// ParseEnv разбирает текст .env: KEY=VALUE, # комментарии, кавычки ' и ".
// Ошибка не содержит значений (секреты).
func ParseEnv(text string) (map[string]string, error) {
	env := map[string]string{}
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || !isNameStart(k[0]) || strings.IndexFunc(k, func(r rune) bool { return r > 127 || !isNameChar(byte(r)) }) >= 0 {
			return nil, fmt.Errorf(".env, строка %d: ожидается ИМЯ=значение", n+1)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		env[k] = v
	}
	return env, nil
}
