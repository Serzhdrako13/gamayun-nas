package catalog

import (
	"strings"
	"testing"
)

func TestRenderForm(t *testing.T) {
	fields := []Field{
		{Key: "PASS", Type: "random", Secret: true, Length: 12},
		{Key: "USER", Type: "text", Required: true, Regex: `^[a-z]+$`},
		{Key: "TLS", Type: "bool"},
		{Key: "CERT", Type: "text", ShowIf: "TLS", Required: true},
		{Key: "MODE", Type: "select", Options: []string{"a", "b"}, Default: "a"},
		{Key: "MEDIA", Type: "share"},
		{Key: "PORT", Type: "int", ShowIf: "MODE=b"},
	}
	r, errs := RenderForm(fields, map[string]string{"USER": "bob", "MEDIA": "Photos"}, func(n int) string { return strings.Repeat("x", n) })
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if r.Env["PASS"] != "xxxxxxxxxxxx" || r.Env["TLS"] != "false" || r.Env["MODE"] != "a" || r.Shares["MEDIA"] != "Photos" {
		t.Fatalf("%+v", r)
	}
	if _, ok := r.Env["CERT"]; ok {
		t.Fatal("скрытое поле в env")
	}
	if strings.Join(r.Secrets, ",") != "PASS" {
		t.Fatal(r.Secrets)
	}
	// Ошибки — пачкой, без значений.
	_, errs = RenderForm(fields, map[string]string{"USER": "Bob1", "TLS": "true", "MODE": "c", "X": "1", "PASS": "a\nb"}, nil)
	msg := joinE(errs)
	for _, w := range []string{"X: нет такого", "USER: значение не соответствует", "CERT: обязательное", "MODE: значение не из списка", "PASS: недопустимые"} {
		if !strings.Contains(msg, w) {
			t.Fatalf("нет %q в %s", w, msg)
		}
	}
	if strings.Contains(msg, "Bob1") {
		t.Fatal("значение в ошибке")
	}
	if r, _ := RenderForm(fields, map[string]string{"USER": "a"}, nil); len(r.Env["PASS"]) != 12 {
		t.Fatal(r.Env["PASS"])
	}
}

func joinE(errs []error) string {
	var l []string
	for _, e := range errs {
		l = append(l, e.Error())
	}
	return strings.Join(l, "; ")
}

func TestEnvText(t *testing.T) {
	s, err := EnvText(map[string]string{"B": "a b #c $x", "A": "1"})
	if err != nil || s != "A='1'\nB='a b #c $x'\n" {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := EnvText(map[string]string{"A": "it's"}); err == nil {
		t.Fatal("кавычка принята")
	}
	if r := Random(0); len(r) != DefaultRandomLength || strings.ContainsAny(r, "'\"$# ") {
		t.Fatal(r)
	}
}

func TestCompat(t *testing.T) {
	m := Manifest{OSMin: "0.1.0~alpha.150", Architectures: []string{"amd64"}, MinRAMMB: 512, MinDiskMB: 100}
	ok := System{OSVersion: "0.1.0~alpha.160+g62ea9d2.dirty", Arch: "amd64", RAMMB: 1024, DiskFreeMB: 1000}
	if errs := Compat(m, ok); len(errs) != 0 {
		t.Fatal(errs)
	}
	if errs := Compat(m, System{OSVersion: "dev", Arch: "amd64", RAMMB: 1024, DiskFreeMB: 1000}); len(errs) != 0 {
		t.Fatal(errs)
	}
	bad := System{OSVersion: "0.1.0~alpha.149", Arch: "arm64", RAMMB: 100, DiskFreeMB: 10}
	if errs := Compat(m, bad); len(errs) != 4 {
		t.Fatal(errs)
	}
}
