package compose

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// VerifyContext — окружение повторной проверки в helper.
type VerifyContext struct {
	ID        string
	MountRoot string
	AppData   string
	// Resolve — разрешение символьных ссылок пути bind (helper:
	// filepath.EvalSymlinks); nil — путь как есть.
	Resolve func(string) (string, error)
}

// Verify — жёсткая проверка нормализованного файла (JSON) в helper перед
// запуском docker compose: helper root и не доверяет gamayund вслепую.
// Риски (privileged и т. п.) здесь не проверяются — их подтверждает
// администратор в gamayund; запрещённое не проходит ни при каком подтверждении.
func Verify(normalized []byte, vc VerifyContext) []Issue {
	var doc map[string]any
	if err := json.Unmarshal(normalized, &doc); err != nil {
		return []Issue{{Code: FInvalid, Detail: "нормализованный файл не JSON: " + err.Error()}}
	}
	return verifyDoc(unescape(doc).(map[string]any), vc)
}

// unescape — обратное escapeDollar: так строки увидит Compose.
func unescape(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "$$", "$")
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = unescape(x)
		}
		return m
	case []any:
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = unescape(x)
		}
		return l
	}
	return v
}

func verifyDoc(doc map[string]any, vc VerifyContext) []Issue {
	var iss []Issue
	bad := func(code, svc, f string, a ...any) {
		iss = append(iss, Issue{Code: code, Service: svc, Detail: fmt.Sprintf(f, a...)})
	}
	for k := range doc {
		switch k {
		case "name", "services", "volumes", "networks":
		default:
			bad(FUnsupported, "", "ключ верхнего уровня %s", k)
		}
	}
	if name, _ := doc["name"].(string); name != ProjectName(vc.ID) {
		bad(FProjectName, "", "имя проекта %q, ожидалось %s", name, ProjectName(vc.ID))
	}
	vols, _ := asMap(doc["volumes"])
	for name, v := range vols {
		m, _ := asMap(v)
		for k := range m {
			if !volKeys[k] {
				bad(FVolumeDriver, "", "том %s: ключ %s", name, k)
			}
		}
		if d, _ := scalar(m["driver"]); d != "" && d != "local" {
			bad(FVolumeDriver, "", "том %s: драйвер %s", name, d)
		}
	}
	nets, _ := asMap(doc["networks"])
	for name, v := range nets {
		m, _ := asMap(v)
		for k := range m {
			if !netKeys[k] {
				bad(FExternalNetwork, "", "сеть %s: ключ %s", name, k)
			}
		}
		if d, _ := scalar(m["driver"]); d != "" && d != "bridge" {
			bad(FNetworkDriver, "", "сеть %s: драйвер %s", name, d)
		}
	}
	svcs, ok := asMap(doc["services"])
	if !ok || len(svcs) == 0 || len(svcs) > MaxServices {
		bad(FNoServices, "", "сервисов: %d", len(svcs))
	}
	allowed := map[string]bool{"environment": true, "labels": true, "logging": true, "restart": true, "ports": true,
		"volumes": true, "deploy": true, "volumes_from": true, "privileged": true, "cap_add": true, "devices": true,
		"device_cgroup_rules": true, "gpus": true, "use_api_socket": true, "security_opt": true, "network_mode": true,
		"pid": true, "ipc": true, "uts": true, "userns_mode": true, "env_file": true}
	for svc, v := range svcs {
		m, ok := asMap(v)
		if !ok {
			bad(FInvalid, svc, "сервис — не словарь")
			continue
		}
		for k := range m {
			if !passKeys[k] && !allowed[k] {
				bad(FUnsupported, svc, "ключ %s", k)
			}
		}
		if img, _ := scalar(m["image"]); img == "" {
			bad(FNoImage, svc, "нет image")
		}
		// env_file — только файл секретов этого сервиса.
		if ef, ok := m["env_file"]; ok {
			l, _ := ef.([]any)
			if s, _ := scalar(firstOf(l)); len(l) != 1 || s != SecretsFile(svc) {
				bad(FEnvFile, svc, "env_file: допускается только %s проекта", SecretsFile(svc))
			}
		}
		for _, k := range []string{"network_mode", "pid", "ipc", "uts", "userns_mode"} {
			if s, _ := scalar(m[k]); strings.HasPrefix(s, "container:") {
				bad(FContainerNS, svc, "%s: %s", k, s)
			}
		}
		if l, ok := m["volumes_from"].([]any); ok {
			for _, x := range l {
				s, _ := scalar(x)
				if src, _, _ := strings.Cut(s, ":"); strings.HasPrefix(s, "container:") || svcs[src] == nil {
					bad(FContainerNS, svc, "volumes_from %s", s)
				}
			}
		}
		if lb, ok := asMap(m["labels"]); !ok || lb[AppLabel] != vc.ID {
			bad(FInvalid, svc, "нет метки %s=%s", AppLabel, vc.ID)
		}
		if ports, ok := m["ports"]; ok {
			l, _ := ports.([]any)
			if l == nil && ports != nil {
				bad(FInvalid, svc, "ports — не список")
			}
			for _, x := range l {
				p, ok := asMap(x)
				ip, _ := scalar(p["host_ip"])
				pub, _ := scalar(p["published"])
				if !ok || (ip != "127.0.0.1" && ip != "0.0.0.0") || pub == "" || strings.Contains(pub, "-") {
					bad(FPortHostIP, svc, "порт не в длинной форме с адресом и портом NAS")
				}
			}
		}
		if vl, ok := m["volumes"]; ok {
			l, _ := vl.([]any)
			if l == nil && vl != nil {
				bad(FInvalid, svc, "volumes — не список")
			}
			for _, x := range l {
				e, ok := asMap(x)
				if !ok {
					bad(FInvalid, svc, "том не в длинной форме")
					continue
				}
				typ, _ := scalar(e["type"])
				switch typ {
				case "volume", "tmpfs":
					if s, _ := scalar(e["source"]); strings.ContainsAny(s, "/.") {
						bad(FVolumeUndecl, svc, "том %q", s)
					}
				case "bind":
					src, _ := scalar(e["source"])
					b, _ := asMap(e["bind"])
					if b == nil || b["create_host_path"] != false {
						bad(FBindPath, svc, "bind %s без create_host_path: false", src)
					}
					if p, _ := scalar(b["propagation"]); p != "" && p != "rprivate" && p != "private" {
						bad(FPropagation, svc, "bind %s: propagation %s", src, p)
					}
					if c, d := vc.bindOK(src); c != "" {
						bad(c, svc, "%s", d)
					}
				default:
					bad(FMountType, svc, "тип тома %q", typ)
				}
			}
		}
	}
	return iss
}

// bindOK — путь bind допустим: данные приложения, общая папка на пуле
// (не корень пула, не .apps) или сокет Docker (риск подтверждён в gamayund).
func (vc VerifyContext) bindOK(src string) (code, detail string) {
	if src == "" || !filepath.IsAbs(src) || filepath.Clean(src) != src || strings.ContainsAny(src, "$\x00\n") {
		return FBindPath, fmt.Sprintf("недопустимый путь %q", src)
	}
	if src == "/var/run/docker.sock" || src == "/run/docker.sock" {
		return "", ""
	}
	p := src
	if vc.Resolve != nil {
		r, err := vc.Resolve(src)
		if err != nil {
			return FBindPath, fmt.Sprintf("%s: %v", src, err)
		}
		p = r
	}
	if _, ok := under(p, vc.AppData); ok && vc.AppData != "" {
		return "", ""
	}
	if classifyApps(p, vc.MountRoot) != "" {
		return FBindApps, fmt.Sprintf("%s (→ %s) — данные Docker или других приложений", src, p)
	}
	if rel, ok := under(p, vc.MountRoot); ok && strings.Contains(rel, "/") {
		return "", ""
	}
	return FBindOutside, fmt.Sprintf("%s (→ %s) — вне данных приложения и пулов", src, p)
}

func firstOf(l []any) any {
	if len(l) == 0 {
		return nil
	}
	return l[0]
}
