package home

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLabel(t *testing.T) {
	for _, c := range []struct {
		in   string
		pool string
		i    int
		ok   bool
	}{
		{"dn-pool1-0", "pool1", 0, true},
		{"dn-tank-12", "tank", 12, true},
		{"dn", "", 0, false},
		{"dn-pool1-x", "", 0, false},
		{"dn-Pool-0", "", 0, false},
		{"dn--0", "", 0, false},
		{"other-pool-0", "", 0, false},
		{"dn-pool-32", "", 0, false},
	} {
		p, i, ok := parseLabel(c.in)
		if ok != c.ok || p != c.pool || i != c.i {
			t.Errorf("%q → %q %d %v", c.in, p, i, ok)
		}
	}
}

func TestGuessType(t *testing.T) {
	a := func(ls ...string) []ArraySpec {
		var out []ArraySpec
		for _, l := range ls {
			out = append(out, ArraySpec{Level: l})
		}
		return out
	}
	for want, arrs := range map[string][]ArraySpec{
		"basic": a(LevelSingle), "raid1": a(LevelRAID1), "raid5": a(LevelRAID5), "jbod": a(LevelSingle, LevelSingle),
		"shr": a(LevelRAID5, LevelRAID1), "shr2": a(LevelRAID6, LevelRAID1),
	} {
		if g := guessType(arrs); g != want {
			t.Errorf("%v → %s, ожидалось %s", arrs, g, want)
		}
	}
}

func TestWriteManifest(t *testing.T) {
	mp := t.TempDir()
	e := &Executor{MountRoot: filepath.Dir(mp)}
	p := AssembleParams{Pool: filepath.Base(mp), Type: "raid1", Arrays: []ArraySpec{{Level: LevelRAID1, Members: []string{"a", "b"}}}}
	if err := e.writeManifest(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(mp, ManifestPath))
	if err != nil || len(b) == 0 {
		t.Fatalf("манифест: %v", err)
	}
	st1, _ := os.Stat(filepath.Join(mp, ManifestPath))
	if err := e.writeManifest(p); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(filepath.Join(mp, ManifestPath))
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("тот же манифест переписан")
	}
}

func TestFullArrays(t *testing.T) {
	m := []ArraySpec{{Level: LevelRAID5, Members: []string{"a", "b", "c"}}}
	if !fullArrays(m, []ArraySpec{{Level: LevelRAID5, Members: []string{"a", "c"}}}) {
		t.Fatal("подмножество не принято")
	}
	if fullArrays(m, []ArraySpec{{Level: LevelRAID5, Members: []string{"a", "x"}}}) {
		t.Fatal("чужой диск принят")
	}
	if fullArrays(m, []ArraySpec{{Level: LevelRAID1, Members: []string{"a"}}}) {
		t.Fatal("другой уровень принят")
	}
}
