package compose

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const mr = "/mnt/gamayun"

func ctx() Context {
	return Context{
		ID: "web", MountRoot: mr, AppData: mr + "/p1/.apps/appdata/web",
		Shares:  map[string]string{"Photos": mr + "/p1/Photos", "Root": mr + "/p2"},
		Env:     map[string]string{"PASS": "s3cr3t-value", "PORT": "8080", "DATA": "./data", "EMPTY": ""},
		Network: map[string]bool{},
	}
}

func codes(l []Issue) []string {
	var o []string
	for _, i := range l {
		o = append(o, i.Code)
	}
	return o
}

func has(l []Issue, code string) bool {
	for _, i := range l {
		if i.Code == code {
			return true
		}
	}
	return false
}

func normT(t *testing.T, src string, c Context) Result {
	t.Helper()
	r, err := Normalize([]byte(src), c)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return r
}

func TestForbidden(t *testing.T) {
	cases := []struct{ name, src, code string }{
		{"yaml", "services: [", FInvalid},
		{"multi-doc", "services: {a: {image: x}}\n---\nservices: {}\n", FInvalid},
		{"no services", "volumes: {}\n", FNoServices},
		{"no image", "services: {a: {command: x}}\n", FNoImage},
		{"build", "services: {a: {build: .}}\n", FBuild},
		{"pull_policy build", "services: {a: {image: x, pull_policy: build}}\n", FBuild},
		{"include", "include: [other.yml]\nservices: {a: {image: x}}\n", FInclude},
		{"extends", "services: {a: {image: x, extends: {file: /etc/x.yml, service: b}}}\n", FExtends},
		{"unknown top", "secrets: {s: {file: /etc/shadow}}\nservices: {a: {image: x}}\n", FUnsupported},
		{"svc secrets", "services: {a: {image: x, secrets: [s]}}\n", FUnsupported},
		{"svc configs", "services: {a: {image: x, configs: [c]}}\n", FUnsupported},
		{"unknown key", "services: {a: {image: x, frobnicate: 1}}\n", FUnsupported},
		{"post_start", "services: {a: {image: x, post_start: [{command: id, privileged: true}]}}\n", FUnsupported},
		{"env_file host", "services: {a: {image: x, env_file: /etc/shadow}}\n", FEnvFile},
		{"env_file traversal", "services: {a: {image: x, env_file: [../../etc/passwd]}}\n", FEnvFile},
		{"bind root", "services: {a: {image: x, volumes: ['/:/host']}}\n", FBindSystem},
		{"bind etc", "services: {a: {image: x, volumes: ['/etc:/e:ro']}}\n", FBindSystem},
		{"bind var lib docker", "services: {a: {image: x, volumes: ['/var/lib/docker:/d']}}\n", FBindSystem},
		{"bind gamayun state", "services: {a: {image: x, volumes: ['/var/lib/gamayun-nas:/d']}}\n", FBindSystem},
		{"bind helper socket", "services: {a: {image: x, volumes: ['/run/gamayun-nas/helper.sock:/s']}}\n", FBindSystem},
		{"bind dotdot escape", "services: {a: {image: x, volumes: ['./../../../../etc:/e']}}\n", FBindOutside},
		{"bind dotdot etc", "services: {a: {image: x, volumes: ['./../../../../../../etc:/e']}}\n", FBindSystem},
		{"bind dotdot apps", "services: {a: {image: x, volumes: ['./../other:/e']}}\n", FBindApps},
		{"bind other app", "services: {a: {image: x, volumes: ['/mnt/gamayun/p1/.apps/appdata/other:/d']}}\n", FBindApps},
		{"bind docker data", "services: {a: {image: x, volumes: ['/mnt/gamayun/p1/.apps/docker:/d']}}\n", FBindApps},
		{"bind apps via share", "services: {a: {image: x, volumes: ['/mnt/gamayun/p1/Photos/../.apps:/d']}}\n", FBindApps},
		{"bind pool root share", "services: {a: {image: x, volumes: ['/mnt/gamayun/p2:/d']}}\n", FBindShareRoot},
		{"bind pool not share", "services: {a: {image: x, volumes: ['/mnt/gamayun/p1/Secret:/d']}}\n", FBindOutside},
		{"bind tmp", "services: {a: {image: x, volumes: ['/tmp:/t']}}\n", FBindOutside},
		{"bind home tilde", "services: {a: {image: x, volumes: ['~/x:/t']}}\n", FBindPath},
		{"bind long create", "services: {a: {image: x, volumes: [{type: bind, source: /, target: /h, bind: {create_host_path: true}}]}}\n", FBindSystem},
		{"bind propagation", "services: {a: {image: x, volumes: [{type: bind, source: ./d, target: /h, bind: {propagation: rshared}}]}}\n", FPropagation},
		{"bind via env", "services: {a: {image: x, volumes: ['${ROOT:-/}:/h']}}\n", FBindSystem},
		{"npipe", "services: {a: {image: x, volumes: [{type: npipe, source: x, target: /x}]}}\n", FMountType},
		{"undeclared volume", "services: {a: {image: x, volumes: ['data:/d']}}\n", FVolumeUndecl},
		{"external volume", "volumes: {d: {external: true}}\nservices: {a: {image: x}}\n", FExternalVolume},
		{"volume driver_opts bind", "volumes: {d: {driver: local, driver_opts: {type: none, o: bind, device: /etc}}}\nservices: {a: {image: x}}\n", FVolumeDriver},
		{"volume driver", "volumes: {d: {driver: nfs}}\nservices: {a: {image: x}}\n", FVolumeDriver},
		{"external network", "networks: {n: {external: true}}\nservices: {a: {image: x}}\n", FExternalNetwork},
		{"network driver", "networks: {n: {driver: macvlan}}\nservices: {a: {image: x}}\n", FNetworkDriver},
		{"container ns", "services: {a: {image: x, network_mode: 'container:other'}}\n", FContainerNS},
		{"volumes_from container", "services: {a: {image: x, volumes_from: ['container:gamayund']}}\n", FContainerNS},
		{"port range", "services: {a: {image: x, ports: ['8000-8010:8000-8010']}}\n", FPortRange},
		{"port no host", "services: {a: {image: x, ports: ['80']}}\n", FPortNoHost},
		{"port int", "services: {a: {image: x, ports: [80]}}\n", FPortNoHost},
		{"port ip", "services: {a: {image: x, ports: ['10.0.0.5:80:80']}}\n", FPortHostIP},
		{"port dup", "services: {a: {image: x, ports: ['80:80', '80:81']}}\n", FPortDuplicate},
		{"port bad", "services: {a: {image: x, ports: ['99999:80']}}\n", FPortInvalid},
		{"seccomp file", "services: {a: {image: x, security_opt: ['seccomp=/etc/x.json']}}\n", FSecurityOpt},
		{"bad svc name", "services: {'../x': {image: x}}\n", FInvalid},
		{"interp required", "services: {a: {image: '${NOPE:?нужно}'}}\n", FInvalid},
		{"too large", "services: {a: {image: x, command: '" + strings.Repeat("a", MaxSize) + "'}}\n", FTooLarge},
		{"non-string keys", "services: {a: {image: x, labels: {1: x}}}\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := normT(t, c.src, ctx())
			if c.code == "" {
				return
			}
			if !has(r.Forbidden, c.code) {
				t.Errorf("ожидался запрет %s, есть %v (риски %v)", c.code, codes(r.Forbidden), codes(r.Risks))
			}
			if r.Normalized != nil {
				t.Error("при запрете файл не должен создаваться")
			}
		})
	}
}

func TestTooManyServices(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n")
	for i := 0; i <= MaxServices; i++ {
		b.WriteString("  s" + strings.Repeat("x", i) + ": {image: x}\n")
	}
	if r := normT(t, b.String(), ctx()); !has(r.Forbidden, FTooMany) {
		t.Fatal(codes(r.Forbidden))
	}
}

func TestBillionLaughs(t *testing.T) {
	src := `a: &a ["x","x","x","x","x","x","x","x","x"]
b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]
e: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]
f: &f [*e,*e,*e,*e,*e,*e,*e,*e,*e]
g: &g [*f,*f,*f,*f,*f,*f,*f,*f,*f]
services: {a: {image: x, command: *g}}
`
	r := normT(t, src, ctx())
	if r.OK() {
		t.Fatal("ожидался отказ")
	}
}

func TestRisks(t *testing.T) {
	cases := []struct{ name, src, key string }{
		{"privileged", "services: {a: {image: x, privileged: true}}\n", "privileged:a"},
		{"docker sock", "services: {a: {image: x, volumes: ['/var/run/docker.sock:/var/run/docker.sock']}}\n", "docker_sock:a"},
		{"api socket", "services: {a: {image: x, use_api_socket: true}}\n", "docker_sock:a"},
		{"host net", "services: {a: {image: x, network_mode: host}}\n", "host_network:a"},
		{"host pid", "services: {a: {image: x, pid: host}}\n", "host_pid:a"},
		{"host ipc", "services: {a: {image: x, ipc: host}}\n", "host_ipc:a"},
		{"host uts", "services: {a: {image: x, uts: host}}\n", "host_uts:a"},
		{"userns", "services: {a: {image: x, userns_mode: host}}\n", "host_userns:a"},
		{"cap_add", "services: {a: {image: x, cap_add: [SYS_ADMIN]}}\n", "cap_add:a"},
		{"devices", "services: {a: {image: x, devices: ['/dev/sda:/dev/sda']}}\n", "devices:a"},
		{"gpus", "services: {a: {image: x, gpus: all}}\n", "devices:a"},
		{"deploy devices", "services: {a: {image: x, deploy: {resources: {reservations: {devices: [{capabilities: [gpu]}]}}}}}\n", "devices:a"},
		{"unconfined", "services: {a: {image: x, security_opt: ['seccomp:unconfined']}}\n", "unconfined:a"},
		{"apparmor unconfined", "services: {a: {image: x, security_opt: ['apparmor=unconfined']}}\n", "unconfined:a"},
		{"share", "services: {a: {image: x, volumes: ['/mnt/gamayun/p1/Photos/2024:/photos:ro']}}\n", "share_bind:Photos"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := normT(t, c.src, ctx())
			if !r.OK() {
				t.Fatalf("запреты: %v", r.Forbidden)
			}
			found := false
			for _, i := range r.Risks {
				found = found || i.Key == c.key
			}
			if !found {
				t.Errorf("нет риска %s: %+v", c.key, r.Risks)
			}
		})
	}
}

func TestPortsAndNetwork(t *testing.T) {
	src := `services:
  a:
    image: nginx
    ports: ["${PORT}:80", "0.0.0.0:8443:443/tcp", "[::]:5353:53/udp", {target: 9000, published: 9000, host_ip: 127.0.0.1}]
`
	c := ctx()
	r := normT(t, src, c)
	if !r.OK() || len(r.Risks) != 0 || len(r.Ports) != 4 {
		t.Fatalf("%+v", r)
	}
	for _, p := range r.Ports {
		if p.Network {
			t.Errorf("без разрешения порт не в сеть: %+v", p)
		}
	}
	if !strings.Contains(string(r.Normalized), `"host_ip": "127.0.0.1"`) || strings.Contains(string(r.Normalized), "0.0.0.0") {
		t.Errorf("%s", r.Normalized)
	}
	c.Network = map[string]bool{"8080/tcp": true}
	r = normT(t, src, c)
	if len(r.Risks) != 1 || r.Risks[0].Key != "network_port:8080/tcp" || !strings.Contains(r.Risks[0].Detail, "мимо фаервола") {
		t.Fatalf("%+v", r.Risks)
	}
	if !strings.Contains(string(r.Normalized), `"0.0.0.0"`) {
		t.Errorf("%s", r.Normalized)
	}
}

func TestNormalizedContent(t *testing.T) {
	src := `version: "3.8"
name: hack
x-common: &env
  TZ: Europe/Moscow
services:
  db:
    image: postgres:16
    container_name: db
    restart: unless-stopped
    environment:
      <<: *env
      POSTGRES_PASSWORD: ${PASS}
      HOSTVAR:
    env_file: .env
    labels: ["com.docker.compose.project=other", "my=1"]
    logging: {driver: json-file}
    command: ["sh", "-c", "echo $$HOME ${MISSING}"]
    volumes:
      - ${DATA}/pg:/var/lib/postgresql/data
      - ./:/cfg:ro
      - cache:/cache
    deploy: {replicas: 2, resources: {limits: {memory: 512m}}}
volumes:
  cache: {name: someone-elses}
`
	r := normT(t, src, ctx())
	if !r.OK() {
		t.Fatalf("%+v", r.Forbidden)
	}
	var doc map[string]any
	if err := json.Unmarshal(r.Normalized, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["name"] != "app-web" {
		t.Error(doc["name"])
	}
	db := doc["services"].(map[string]any)["db"].(map[string]any)
	if _, ok := db["container_name"]; ok {
		t.Error("container_name")
	}
	if db["restart"] != RestartPolicy {
		t.Error(db["restart"])
	}
	lg := db["logging"].(map[string]any)
	if lg["driver"] != "local" {
		t.Error(lg)
	}
	lb := db["labels"].(map[string]any)
	if lb[AppLabel] != "web" || lb["my"] != "1" || lb["com.docker.compose.project"] != nil {
		t.Error(lb)
	}
	env := db["environment"].(map[string]any)
	if env["POSTGRES_PASSWORD"] != "s3cr3t-value" || env["TZ"] != "Europe/Moscow" || env["HOSTVAR"] != nil || env["PORT"] != "8080" {
		t.Error(env)
	}
	cmd := db["command"].([]any)
	if cmd[2] != "echo $$HOME " {
		t.Errorf("экранирование $: %q", cmd[2])
	}
	vols := db["volumes"].([]any)
	v0 := vols[0].(map[string]any)
	if v0["source"] != mr+"/p1/.apps/appdata/web/data/pg" || v0["bind"].(map[string]any)["create_host_path"] != false {
		t.Error(v0)
	}
	if vols[1].(map[string]any)["source"] != mr+"/p1/.apps/appdata/web" || vols[1].(map[string]any)["read_only"] != true {
		t.Error(vols[1])
	}
	if strings.Join(r.Mkdirs, ",") != "data/pg" {
		t.Error(r.Mkdirs)
	}
	if v := doc["volumes"].(map[string]any)["cache"].(map[string]any); v["name"] != nil {
		t.Error(v)
	}
	if d := db["deploy"].(map[string]any); d["replicas"] != nil {
		t.Error(d)
	}
	for _, c := range []string{CRestart, CContainerName, CLogging, CLabel, CEnvFile, CEnvMissing, CEnvHost, CShortVolume, CName, CDropped} {
		if !has(r.Changes, c) {
			t.Errorf("нет изменения %s: %v", c, codes(r.Changes))
		}
	}
	// Секреты — не в описаниях изменений/рисков.
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "s3cr3t") {
		t.Error("значение секрета в итоге проверки")
	}
	if len(r.NoHealth) != 1 {
		t.Error(r.NoHealth)
	}
	// Повторная проверка helper проходит.
	if iss := Verify(r.Normalized, VerifyContext{ID: "web", MountRoot: mr, AppData: mr + "/p1/.apps/appdata/web"}); len(iss) > 0 {
		t.Error(iss)
	}
}

func TestRestartNo(t *testing.T) {
	r := normT(t, "services: {a: {image: x, restart: 'no', healthcheck: {test: [CMD, true]}}}\n", ctx())
	if has(r.Changes, CRestart) || len(r.NoHealth) != 0 {
		t.Error(r.Changes, r.NoHealth)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	vc := VerifyContext{ID: "web", MountRoot: mr, AppData: mr + "/p1/.apps/appdata/web"}
	ok := `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},
	  "volumes":[{"type":"bind","source":"/mnt/gamayun/p1/.apps/appdata/web/d","target":"/d","bind":{"create_host_path":false}}]}}}`
	if iss := Verify([]byte(ok), vc); len(iss) > 0 {
		t.Fatal(iss)
	}
	bad := map[string]string{
		"name":      `{"name":"other","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"}}}}`,
		"build":     `{"name":"app-web","services":{"a":{"image":"x","build":".","labels":{"nas.gamayun.app":"web"}}}}`,
		"short vol": `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"volumes":["/:/h"]}}}`,
		"create":    `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"volumes":[{"type":"bind","source":"/mnt/gamayun/p1/.apps/appdata/web","target":"/d"}]}}}`,
		"etc":       `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"volumes":[{"type":"bind","source":"/etc","target":"/d","bind":{"create_host_path":false}}]}}}`,
		"other app": `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"volumes":[{"type":"bind","source":"/mnt/gamayun/p1/.apps/appdata/x","target":"/d","bind":{"create_host_path":false}}]}}}`,
		"pool root": `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"volumes":[{"type":"bind","source":"/mnt/gamayun/p1","target":"/d","bind":{"create_host_path":false}}]}}}`,
		"dollar":    `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"volumes":[{"type":"bind","source":"$${HOME}","target":"/d","bind":{"create_host_path":false}}]}}}`,
		"port ip":   `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"ports":["80:80"]}}}`,
		"driver":    `{"name":"app-web","volumes":{"v":{"driver_opts":{"device":"/etc"}}},"services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"}}}}`,
		"ext net":   `{"name":"app-web","networks":{"n":{"external":true}},"services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"}}}}`,
		"container": `{"name":"app-web","services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"},"pid":"container:x"}}}`,
		"include":   `{"name":"app-web","include":["x"],"services":{"a":{"image":"x","labels":{"nas.gamayun.app":"web"}}}}`,
		"not json":  `services: {}`,
		"no label":  `{"name":"app-web","services":{"a":{"image":"x"}}}`,
	}
	for name, src := range bad {
		if iss := Verify([]byte(src), vc); len(iss) == 0 {
			t.Errorf("%s: прошло", name)
		}
	}
	// Символьная ссылка из данных приложения наружу.
	vc.Resolve = func(string) (string, error) { return "/etc", nil }
	if iss := Verify([]byte(ok), vc); len(iss) == 0 {
		t.Error("symlink наружу прошёл")
	}
}

func TestInterpolate(t *testing.T) {
	env := map[string]string{"A": "1", "E": ""}
	cases := map[string]string{
		"$A": "1", "${A}": "1", "$$A": "$A", "${B:-x}": "x", "${E:-x}": "x", "${E-x}": "",
		"${A:+y}": "y", "${B:+y}": "", "${B:-${A}}": "1", "a$": "a$", "$1": "$1",
	}
	for in, want := range cases {
		got, err := interpolate(in, env, map[string]bool{})
		if err != nil || got != want {
			t.Errorf("%q → %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := interpolate("${A", env, map[string]bool{}); err == nil {
		t.Error("незакрытая")
	}
	if _, err := interpolate("${B:?no}", env, map[string]bool{}); err == nil {
		t.Error(":?")
	}
}

func TestParseEnv(t *testing.T) {
	e, err := ParseEnv("# c\nA=1\nexport B=\"x y\"\nC='z' \nD=v # comment\n\n")
	if err != nil || e["A"] != "1" || e["B"] != "x y" || e["C"] != "z" || e["D"] != "v" {
		t.Fatal(e, err)
	}
	_, err = ParseEnv("bad line secretvalue")
	if err == nil || strings.Contains(err.Error(), "secretvalue") {
		t.Fatal(err)
	}
}

func ctxP3() Context {
	c := ctx()
	c.UID, c.GID = 59001, 59001
	c.Grants = map[string]string{}
	c.RunAs = map[string]string{}
	return c
}

func svcOut(t *testing.T, r Result, svc string) map[string]any {
	t.Helper()
	if !r.OK() {
		t.Fatalf("запреты: %v", r.Forbidden)
	}
	var doc map[string]any
	if err := json.Unmarshal(r.Normalized, &doc); err != nil {
		t.Fatal(err)
	}
	return doc["services"].(map[string]any)[svc].(map[string]any)
}

func TestP3RunAs(t *testing.T) {
	cases := []struct{ name, src, mode, user string }{
		{"uid default", "services: {a: {image: nginx}}\n", RunUID, "59001:59001"},
		{"uid replaces user", "services: {a: {image: nginx, user: '1000:1000'}}\n", RunUID, "59001:59001"},
		{"root by user", "services: {a: {image: nginx, user: root}}\n", RunRoot, "root"},
		{"root by 0", "services: {a: {image: nginx, user: '0:0'}}\n", RunRoot, "0:0"},
		{"puid env", "services: {a: {image: x, environment: {PUID: '1000', PGID: '1000'}}}\n", RunPUID, ""},
		{"puid image", "services: {a: {image: lscr.io/linuxserver/syncthing:latest}}\n", RunPUID, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := normT(t, c.src, ctxP3())
			if r.RunAs["a"] != c.mode {
				t.Fatalf("режим %q, ждали %q", r.RunAs["a"], c.mode)
			}
			o := svcOut(t, r, "a")
			u, _ := o["user"].(string)
			if u != c.user {
				t.Fatalf("user %q, ждали %q", u, c.user)
			}
			if c.mode == RunPUID {
				env := o["environment"].(map[string]any)
				if env["PUID"] != "59001" || env["PGID"] != "59001" {
					t.Fatalf("PUID/PGID: %v", env)
				}
			}
		})
	}
	// явный режим важнее подсказки
	c := ctxP3()
	c.RunAs["a"] = RunUID
	r := normT(t, "services: {a: {image: lscr.io/linuxserver/x}}\n", c)
	if svcOut(t, r, "a")["user"] != "59001:59001" {
		t.Fatal("явный uid не применён")
	}
	c.RunAs["a"] = "bogus"
	if r := normT(t, "services: {a: {image: x}}\n", c); !has(r.Forbidden, FRunAs) {
		t.Fatal("неизвестный режим не запрещён")
	}
	// без учётки user не трогается
	r = normT(t, "services: {a: {image: x, user: '1000'}}\n", ctx())
	if svcOut(t, r, "a")["user"] != "1000" || len(r.RunAs) != 0 {
		t.Fatal("П2-приложение изменено")
	}
}

func TestP3ShareGrants(t *testing.T) {
	src := "services: {a: {image: x, volumes: ['/mnt/gamayun/p1/Photos/2024:/photos']}}\n"
	c := ctxP3()
	r := normT(t, src, c)
	if !has(r.Forbidden, FShareNotGranted) || has(r.Risks, RShareBind) {
		t.Fatalf("без выдачи: %v %v", r.Forbidden, r.Risks)
	}
	if len(r.ShareBinds) != 1 || r.ShareBinds[0].Share != "Photos" || r.ShareBinds[0].Grant != "" {
		t.Fatalf("share_binds: %+v", r.ShareBinds)
	}
	c.Grants["Photos"] = GrantRO
	r = normT(t, src, c)
	v := svcOut(t, r, "a")["volumes"].([]any)[0].(map[string]any)
	if v["read_only"] != true || !has(r.Changes, CShareRO) || len(r.Risks) != 0 {
		t.Fatalf("ro: %v %v", v, r.Changes)
	}
	c.Grants["Photos"] = GrantRW
	r = normT(t, src, c)
	v = svcOut(t, r, "a")["volumes"].([]any)[0].(map[string]any)
	if v["read_only"] == true || len(r.Risks) != 0 {
		t.Fatalf("rw: %v %v", v, r.Risks)
	}
	// rw, но в compose :ro — остаётся ro
	r = normT(t, "services: {a: {image: x, volumes: ['/mnt/gamayun/p1/Photos:/p:ro']}}\n", c)
	if svcOut(t, r, "a")["volumes"].([]any)[0].(map[string]any)["read_only"] != true {
		t.Fatal(":ro потерян")
	}
	// root + rw → риск; root + ro → без риска
	rs := "services: {a: {image: x, user: root, volumes: ['/mnt/gamayun/p1/Photos:/p']}}\n"
	r = normT(t, rs, c)
	if !has(r.Risks, RRootShareWrite) || r.Risks[0].Key != RRootShareWrite+":Photos" {
		t.Fatalf("root rw: %v", r.Risks)
	}
	c.Grants["Photos"] = GrantRO
	if r = normT(t, rs, c); len(r.Risks) != 0 {
		t.Fatalf("root ro: %v", r.Risks)
	}
	// запреты прежние и при учётке
	if r = normT(t, "services: {a: {image: x, volumes: ['/mnt/gamayun/p2:/d']}}\n", c); !has(r.Forbidden, FBindShareRoot) {
		t.Fatal("корень пула")
	}
	// appdata без выдач
	if r = normT(t, "services: {a: {image: x, volumes: ['./d:/d']}}\n", ctxP3()); !r.OK() || len(r.ShareBinds) != 0 {
		t.Fatal("appdata")
	}
}

func TestSecretsEnvFile(t *testing.T) {
	src := "services:\n  a:\n    image: x\n    environment: {P: \"${P}\", Q: \"${Q}\", N: \"1\"}\n  b:\n    image: y\n    environment: [\"P=$P\"]\n  c: {image: z}\n"
	res, err := Normalize([]byte(src), Context{ID: "t", MountRoot: "/mnt", AppData: "/mnt/p/.apps/appdata/t",
		Env: map[string]string{"P": "leak"}, Secrets: map[string]bool{"P": true, "Q": true}})
	if err != nil || !res.OK() {
		t.Fatalf("%+v %v", res.Forbidden, err)
	}
	n := string(res.Normalized)
	if strings.Contains(n, "leak") || !strings.Contains(n, SecretsFile("a")) || !strings.Contains(n, SecretsFile("b")) ||
		strings.Contains(n, SecretsFile("c")) || !strings.Contains(n, `"N": "1"`) {
		t.Fatal(n)
	}
	if fmt.Sprint(res.SecretUse) != "map[a:[P Q] b:[P]]" {
		t.Fatal(res.SecretUse)
	}
	for _, bad := range []string{"    command: [\"$P\"]\n", "    environment: {X: \"${P}\"}\n", "    labels: {l: \"a${Q:-b}\"}\n"} {
		res, _ := Normalize([]byte("services:\n  a:\n    image: x\n"+bad), Context{ID: "t", MountRoot: "/mnt", AppData: "/mnt/p/.apps/appdata/t",
			Secrets: map[string]bool{"P": true, "Q": true}})
		if res.OK() || res.Forbidden[0].Code != FSecretRef {
			t.Fatalf("%q: %+v", bad, res.Forbidden)
		}
	}
	if e := CheckSecretRefs([]byte(src), []string{"P", "Q"}); len(e) != 0 {
		t.Fatal(e)
	}
	// helper: env_file — только secrets.env.
	vc := VerifyContext{ID: "t", MountRoot: "/mnt", AppData: "/mnt/p/.apps/appdata/t"}
	ok, _ := Normalize([]byte(src), Context{ID: "t", MountRoot: "/mnt", AppData: "/mnt/p/.apps/appdata/t", Secrets: map[string]bool{"P": true, "Q": true}})
	if iss := Verify(ok.Normalized, vc); len(iss) != 0 {
		t.Fatal(iss)
	}
	for _, to := range []string{`"/etc/shadow"`, `"` + SecretsFile("b") + `"`} {
		evil := strings.Replace(string(ok.Normalized), `"`+SecretsFile("a")+`"`, to, 1)
		if iss := Verify([]byte(evil), vc); len(iss) == 0 || iss[0].Code != FEnvFile {
			t.Fatal(to, iss)
		}
	}
	if SecretsService("secrets-web.env") != "web" || SecretsService("secrets-../x.env") != "" || SecretsService("user.env") != "" {
		t.Fatal("SecretsService")
	}
}
