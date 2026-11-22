package nas

import (
	"fmt"
	"strings"
)

// Правила имён папок, совместимые с Windows-клиентами SMB. Те же
// правила и тексты ошибок — в web/src/lib/names.js; общие примеры для обеих
// реализаций — internal/nas/testdata/winnames.json.

// winForbidden — символы, недопустимые в именах файлов Windows (вместе с
// управляющими U+0000…U+001F); '/' и '\\' — ещё и разделители пути.
const winForbidden = `<>:"/\|?*`

// winReserved — имена устройств Windows: недопустимы в любом регистре и с
// любым расширением (CON.txt, nul.tar.gz). COM¹²³/LPT¹²³ — тоже устройства.
var winReserved = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
	for _, d := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
		m["COM"+d] = true
		m["LPT"+d] = true
	}
	return m
}()

// WinReservedName возвращает имя устройства Windows (CON, COM1, …), если
// name — зарезервированное: часть до первой точки без пробелов в конце,
// без учёта регистра латиницы. Иначе "".
func WinReservedName(name string) string {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = asciiUpper(strings.TrimRight(base, " "))
	if winReserved[base] {
		return base
	}
	return ""
}

// asciiUpper — верхний регистр только для латиницы (как в Windows для имён
// устройств и одинаково с JS-реализацией).
func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// winNameError — проверка имени по правилам Windows: символы, точка или
// пробел в конце, имена устройств. Длина, UTF-8, «.»/«..» проверяются в
// ValidDirName.
func winNameError(name string) error {
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Errorf("управляющий символ U+%04X в имени папки недопустим", r)
		}
		if strings.ContainsRune(winForbidden, r) {
			return fmt.Errorf(`символ «%c» в имени папки недопустим (Windows не допускает < > : " / \ | ? *)`, r)
		}
	}
	switch {
	case strings.HasSuffix(name, "."):
		return fmt.Errorf("имя папки не может оканчиваться точкой (Windows её отбрасывает)")
	case strings.HasSuffix(name, " "):
		return fmt.Errorf("имя папки не может оканчиваться пробелом (Windows его отбрасывает)")
	}
	if dev := WinReservedName(name); dev != "" {
		return fmt.Errorf("имя «%s» недопустимо: %s — зарезервированное имя устройства Windows (в любом регистре и с расширением)", name, dev)
	}
	return nil
}
