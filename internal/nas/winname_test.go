package nas

import (
	"encoding/json"
	"os"
	"testing"
)

// winNameCases — общие примеры (их же читает web/src/lib/names.test.js).
type winNameCases struct {
	Valid   []string `json:"valid"`
	Invalid []struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	} `json:"invalid"`
}

func loadWinNames(t *testing.T) winNameCases {
	t.Helper()
	b, err := os.ReadFile("testdata/winnames.json")
	if err != nil {
		t.Fatal(err)
	}
	var c winNameCases
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Valid) == 0 || len(c.Invalid) == 0 {
		t.Fatal("пустой набор примеров")
	}
	return c
}

func TestValidDirNameWindows(t *testing.T) {
	c := loadWinNames(t)
	for _, n := range c.Valid {
		if err := ValidDirName(n); err != nil {
			t.Errorf("%q отклонено: %v", n, err)
		}
	}
	for _, x := range c.Invalid {
		err := ValidDirName(x.Name)
		if err == nil {
			t.Errorf("%q принято", x.Name)
			continue
		}
		if err.Error() != x.Error {
			t.Errorf("%q: ошибка %q, ждали %q", x.Name, err.Error(), x.Error)
		}
	}
	if err := ValidDirName("\xff\xfe"); err == nil || err.Error() != "имя папки не в UTF-8" {
		t.Errorf("не UTF-8: %v", err)
	}
}

func TestWinReservedName(t *testing.T) {
	for n, want := range map[string]string{
		"con": "CON", "CON.txt": "CON", "con .x": "CON", "Com3": "COM3", "lpt²": "LPT²",
		"console": "", "com10": "", "COM0": "", "": "", ".con": "", "конь": "", "docs": "",
	} {
		if got := WinReservedName(n); got != want {
			t.Errorf("%q: %q, ждали %q", n, got, want)
		}
	}
}

// Новые правила касаются только нового имени: существующий каталог с
// «Windows-несовместимым» именем остаётся доступен как путь (Path) — его
// можно открыть, переименовать в допустимое имя и удалить.
func TestDirOpsExistingBadNameInPath(t *testing.T) {
	for _, bad := range []string{"a:b", "CON", "x.", "y ", "q?", "nul.txt"} {
		if _, err := CleanRel(bad + "/sub"); err != nil {
			t.Errorf("путь %q: %v", bad, err)
		}
		if err := (ShareRenameParams{Pool: "p1", Share: "docs", Path: bad, Name: "Хорошее"}).Validate(); err != nil {
			t.Errorf("rename %q → допустимое: %v", bad, err)
		}
		if err := (ShareRmdirParams{Pool: "p1", Share: "docs", Path: bad}).Validate(); err != nil {
			t.Errorf("rmdir %q: %v", bad, err)
		}
		if err := (ShareMkdirParams{Pool: "p1", Share: "docs", Path: bad, Name: "new"}).Validate(); err != nil {
			t.Errorf("mkdir в %q: %v", bad, err)
		}
		if err := (ShareRenameParams{Pool: "p1", Share: "docs", Path: "ok", Name: bad}).Validate(); err == nil {
			t.Errorf("rename в %q принят", bad)
		}
		if err := (ShareMkdirParams{Pool: "p1", Share: "docs", Name: bad}).Validate(); err == nil {
			t.Errorf("mkdir %q принят", bad)
		}
	}
}
