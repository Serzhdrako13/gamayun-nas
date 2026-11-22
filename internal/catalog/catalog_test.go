package catalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"gamayun-nas/internal/apps/compose"
)

const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func sample(extra string) string {
	return `services:
  web:
    image: nginx:1.27` + digest + `
    volumes: ["./data:/data", {type: bind, source: "${MEDIA}", target: /media/}]
x-gamayun:
  schema: 1
  id: demo-web
  name: {ru: Демо, en: Demo}
  version: 1.0.0
  app_version: "1.27"
  description: {ru: Тест, en: Test}
  architectures: [amd64, arm64]
  web: {service: web, port: 80}
  health: {service: web, timeout_s: 60}
  form:
    - {key: ADMIN_PASS, type: random, secret: true, title: {ru: Пароль, en: Password}}
    - {key: MEDIA, type: share, title: {ru: Папка, en: Folder}}
  storage:
    - {service: web, target: /data, kind: appdata, title: {ru: Данные, en: Data}}
    - {service: web, target: /media, kind: share, key: MEDIA, title: {ru: Медиа, en: Media}}
` + extra
}

func TestParseOK(t *testing.T) {
	app, errs := Parse([]byte(sample("")))
	if len(errs) != 0 {
		t.Fatalf("ошибки: %v", errs)
	}
	if app.Manifest.ID != "demo-web" || app.Images["web"] == "" {
		t.Fatalf("разбор: %+v", app)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"без digest":             strings.Replace(sample(""), digest, "", 1),
		"неизвестный ключ":       sample("  bogus: 1\n"),
		"depends_on_apps":        sample("  depends_on_apps: [x]\n"),
		"run_as":                 sample("  run_as: {mode: user}\n"),
		"policy auto":            sample("  update: {policy: auto}\n"),
		"нет сервиса":            strings.Replace(sample(""), "health: {service: web", "health: {service: db", 1),
		"schema 2":               strings.Replace(sample(""), "schema: 1", "schema: 2", 1),
		"storage не смонтирован": strings.Replace(sample(""), "target: /data, kind", "target: /other, kind", 1),
		"random без secret":      strings.Replace(sample(""), "type: random, secret: true", "type: random", 1),
		"share без key":          strings.Replace(sample(""), "kind: share, key: MEDIA", "kind: share", 1),
		"нет x-gamayun":          "services: {web: {image: a" + digest + "}}\n",
	}
	for name, src := range cases {
		if _, errs := Parse([]byte(src)); len(errs) == 0 {
			t.Errorf("%s: ожидалась ошибка", name)
		}
	}
}

func TestCheckSecurity(t *testing.T) {
	risks := []compose.Issue{
		{Code: compose.RPrivileged, Key: "privileged:web"},
		{Code: compose.RCapAdd, Key: "cap_add:web", Detail: "дополнительные права ядра: NET_ADMIN, SYS_TIME"},
		{Code: compose.RHostNetwork, Key: "host_network:web"},
		{Code: compose.RNetworkPort, Key: "network_port:8080/tcp"},
	}
	und, conf := CheckSecurity(Security{Privileged: true, CapAdd: []string{"CAP_NET_ADMIN"}}, risks)
	if len(und) != 2 || len(conf) != 1 {
		t.Fatalf("und=%v conf=%v", und, conf)
	}
	und, conf = CheckSecurity(Security{Privileged: true, CapAdd: []string{"NET_ADMIN", "sys_time"}, HostNamespaces: []string{"network"}}, risks)
	if len(und) != 0 || len(conf) != 3 {
		t.Fatalf("полная декларация: und=%v conf=%v", und, conf)
	}
}

func signed(t *testing.T, serial int64) (pub ed25519.PublicKey, priv ed25519.PrivateKey, idx, sig []byte) {
	pub, priv, _ = ed25519.GenerateKey(rand.Reader)
	ix := Index{Schema: 1, Serial: serial, Created: time.Unix(0, 0).UTC(), Apps: []IndexApp{
		{ID: "demo-web", Version: "1.0.0", Files: map[string]string{"compose.yaml": SHA256([]byte("x"))}},
	}}
	idx, err := ix.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv, idx, Sign(priv, idx)
}

func TestVerify(t *testing.T) {
	pub, _, idx, sig := signed(t, 5)
	ix, err := Verify(idx, sig, []ed25519.PublicKey{pub})
	if err != nil || ix.Serial != 5 {
		t.Fatalf("verify: %v", err)
	}
	if err := ix.Apps[0].CheckFile("compose.yaml", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ix.Apps[0].CheckFile("compose.yaml", []byte("y")); err == nil {
		t.Fatal("подменённый файл принят")
	}
	bad := append([]byte{}, idx...)
	bad[10] ^= 1
	if _, err := Verify(bad, sig, []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("изменённый индекс: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(idx, sig, []ed25519.PublicKey{other}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("чужой ключ: %v", err)
	}
	if _, err := Verify(idx, []byte("garbage"), []ed25519.PublicKey{pub}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("мусор: %v", err)
	}
}

func TestSerial(t *testing.T) {
	if CheckSerial(5, 4, false) == nil || CheckSerial(5, 4, true) != nil || CheckSerial(5, 5, false) != nil {
		t.Fatal("CheckSerial")
	}
}

func TestSafePath(t *testing.T) {
	for _, p := range []string{"compose.yaml", "icons/a.png"} {
		if !SafePath(p) {
			t.Errorf("%s отклонён", p)
		}
	}
	for _, p := range []string{"", "/etc/x", "../x", "a/../b", ".hidden", "a//b", `a\b`} {
		if SafePath(p) {
			t.Errorf("%s принят", p)
		}
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0}, {"1.0.10", "1.0.9", 1}, {"1.0.0-rc1", "1.0.0", -1},
		{"0.1.0~alpha.160+g62ea9d2.dirty", "0.1.0~alpha.159", 1},
		{"0.1.0~alpha.160", "0.1.0", -1}, {"0.2.0", "0.1.0~alpha.999", 1},
	}
	for _, c := range cases {
		if got := CompareVersion(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
