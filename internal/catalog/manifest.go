// Package catalog — каталог приложений gamayun-nas: формат
// манифеста x-gamayun (schema 1), индекс каталога, подпись ed25519, сверка
// декларации security с фактическим compose. Чистые функции без побочных
// эффектов: сборщик (cmd/gamayun-catalog) и gamayund используют одни правила.
//
// Подпись ed25519 (закрытый ключ хранится офлайн у владельца каталога,
// открытый — встроен в gamayund или лежит в catalog.keys_dir), доставка подписанным архивом по HTTPS +
// импорт файлом, обновление только manual|notify.
package catalog

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"gamayun-nas/internal/apps/compose"
)

// Schema — поддерживаемая версия формата x-gamayun.
const Schema = 1

// Пределы.
const (
	MaxForm       = 40
	MaxHealthWait = 1800
)

// Text — строка на двух языках.
type Text struct {
	RU string `yaml:"ru" json:"ru"`
	EN string `yaml:"en" json:"en"`
}

// Web — веб-интерфейс приложения (публикация через прокси).
type Web struct {
	Service string `yaml:"service" json:"service"`
	Port    int    `yaml:"port" json:"port"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	Scheme  string `yaml:"scheme,omitempty" json:"scheme,omitempty"`
	// Disabled — не публиковать по умолчанию (по умолчанию публикация включена).
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// PortDecl — порт, который можно открыть в сеть.
type PortDecl struct {
	Service     string `yaml:"service" json:"service"`
	Container   int    `yaml:"container" json:"container"`
	DefaultHost int    `yaml:"default_host" json:"default_host"`
	Protocol    string `yaml:"protocol,omitempty" json:"protocol,omitempty"`
	Title       Text   `yaml:"title" json:"title"`
	Exposable   bool   `yaml:"exposable,omitempty" json:"exposable,omitempty"`
}

// StorageDecl — точка хранения сервиса.
type StorageDecl struct {
	Service  string `yaml:"service" json:"service"`
	Target   string `yaml:"target" json:"target"`
	Kind     string `yaml:"kind" json:"kind"` // appdata|share|cache
	Title    Text   `yaml:"title" json:"title"`
	ReadOnly bool   `yaml:"readonly,omitempty" json:"readonly,omitempty"`
	// Key — поле формы типа share, выбирающее папку (для kind: share).
	Key string `yaml:"key,omitempty" json:"key,omitempty"`
}

// Field — поле формы установки.
type Field struct {
	Key      string   `yaml:"key" json:"key"`
	Type     string   `yaml:"type" json:"type"`
	Title    Text     `yaml:"title" json:"title"`
	Default  string   `yaml:"default,omitempty" json:"default,omitempty"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Regex    string   `yaml:"regex,omitempty" json:"regex,omitempty"`
	Options  []string `yaml:"options,omitempty" json:"options,omitempty"`
	ShowIf   string   `yaml:"show_if,omitempty" json:"show_if,omitempty"`
	Secret   bool     `yaml:"secret,omitempty" json:"secret,omitempty"`
	Group    string   `yaml:"group,omitempty" json:"group,omitempty"`
	// Length — длина для random (по умолчанию 32).
	Length int `yaml:"length,omitempty" json:"length,omitempty"`
}

// Health — ожидание готовности после up.
type Health struct {
	Service  string `yaml:"service" json:"service"`
	TimeoutS int    `yaml:"timeout_s" json:"timeout_s"`
	GraceS   int    `yaml:"grace_s,omitempty" json:"grace_s,omitempty"`
}

// Update — сведения об обновлении до этой версии.
type Update struct {
	Policy                string   `yaml:"policy,omitempty" json:"policy,omitempty"`
	MigrationIrreversible bool     `yaml:"migration_irreversible,omitempty" json:"migration_irreversible,omitempty"`
	ReleaseNotes          Text     `yaml:"release_notes,omitempty" json:"release_notes,omitempty"`
	Security              bool     `yaml:"security,omitempty" json:"security,omitempty"`
	CVEsFixed             []string `yaml:"cves_fixed,omitempty" json:"cves_fixed,omitempty"`
}

// Security — декларация рисков; сверяется с фактическим compose.
type Security struct {
	Privileged bool `yaml:"privileged,omitempty" json:"privileged,omitempty"`
	DockerSock bool `yaml:"docker_sock,omitempty" json:"docker_sock,omitempty"`
	// HostNamespaces — host-пространства: network, pid, ipc, uts, userns.
	HostNamespaces []string `yaml:"host_namespaces,omitempty" json:"host_namespaces,omitempty"`
	CapAdd         []string `yaml:"cap_add,omitempty" json:"cap_add,omitempty"`
	Devices        bool     `yaml:"devices,omitempty" json:"devices,omitempty"`
	Unconfined     bool     `yaml:"unconfined,omitempty" json:"unconfined,omitempty"`
}

// Integration — связь с NAS.
type Integration struct {
	BackupPaths []string `yaml:"backup_paths,omitempty" json:"backup_paths,omitempty"`
}

// Manifest — блок x-gamayun.
type Manifest struct {
	Schema        int           `yaml:"schema" json:"schema"`
	ID            string        `yaml:"id" json:"id"`
	Name          Text          `yaml:"name" json:"name"`
	Version       string        `yaml:"version" json:"version"`
	AppVersion    string        `yaml:"app_version" json:"app_version"`
	Description   Text          `yaml:"description" json:"description"`
	Icon          string        `yaml:"icon,omitempty" json:"icon,omitempty"`
	Screenshots   []string      `yaml:"screenshots,omitempty" json:"screenshots,omitempty"`
	Category      []string      `yaml:"category,omitempty" json:"category,omitempty"`
	Homepage      string        `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	Source        string        `yaml:"source,omitempty" json:"source,omitempty"`
	License       string        `yaml:"license,omitempty" json:"license,omitempty"`
	Maintainer    string        `yaml:"maintainer,omitempty" json:"maintainer,omitempty"`
	OSMin         string        `yaml:"os_min,omitempty" json:"os_min,omitempty"`
	Architectures []string      `yaml:"architectures" json:"architectures"`
	MinRAMMB      int           `yaml:"min_ram_mb,omitempty" json:"min_ram_mb,omitempty"`
	MinDiskMB     int           `yaml:"min_disk_mb,omitempty" json:"min_disk_mb,omitempty"`
	Web           *Web          `yaml:"web,omitempty" json:"web,omitempty"`
	Ports         []PortDecl    `yaml:"ports,omitempty" json:"ports,omitempty"`
	Storage       []StorageDecl `yaml:"storage,omitempty" json:"storage,omitempty"`
	Form          []Field       `yaml:"form,omitempty" json:"form,omitempty"`
	Conflicts     []string      `yaml:"conflicts,omitempty" json:"conflicts,omitempty"`
	Health        *Health       `yaml:"health,omitempty" json:"health,omitempty"`
	RestartLimit  int           `yaml:"restart_limit,omitempty" json:"restart_limit,omitempty"`
	Update        Update        `yaml:"update,omitempty" json:"update,omitempty"`
	Security      Security      `yaml:"security,omitempty" json:"security,omitempty"`
	Integration   Integration   `yaml:"integration,omitempty" json:"integration,omitempty"`
}

// App — приложение каталога: манифест и исходный compose.
type App struct {
	Manifest Manifest
	Compose  []byte
	// Images — образы сервисов: сервис → ссылка с digest.
	Images map[string]string
}

var (
	idRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)
	verRe     = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)
	keyRe     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	digestRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	archs     = map[string]bool{"amd64": true, "arm64": true}
	fieldType = map[string]bool{"text": true, "password": true, "random": true, "bool": true, "int": true, "select": true, "share": true}
	nsNames   = map[string]string{"network": compose.RHostNetwork, "pid": compose.RHostPID, "ipc": compose.RHostIPC, "uts": compose.RHostUTS, "userns": compose.RHostUserns}
	// Ключи, пока не поддерживаемые: их наличие — ошибка, а не молчаливый пропуск.
	deferredKeys = []string{"depends_on_apps", "run_as"}
)

// Parse извлекает x-gamayun из compose приложения и проверяет манифест.
// Возвращает все найденные ошибки одной пачкой.
func Parse(src []byte) (App, []error) {
	var doc map[string]any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return App{}, []error{fmt.Errorf("compose: %w", err)}
	}
	raw, ok := doc["x-gamayun"]
	if !ok {
		return App{}, []error{fmt.Errorf("нет блока x-gamayun")}
	}
	if m, ok := raw.(map[string]any); ok {
		for _, k := range deferredKeys {
			if _, has := m[k]; has {
				return App{}, []error{fmt.Errorf("x-gamayun.%s не поддерживается в schema %d", k, Schema)}
			}
		}
		if in, ok := m["integration"].(map[string]any); ok {
			for _, k := range []string{"users", "shares"} {
				if _, has := in[k]; has {
					return App{}, []error{fmt.Errorf("x-gamayun.integration.%s не поддерживается в schema %d", k, Schema)}
				}
			}
		}
	}
	b, err := yaml.Marshal(raw)
	if err != nil {
		return App{}, []error{err}
	}
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return App{}, []error{fmt.Errorf("x-gamayun: %w", err)}
	}
	app := App{Manifest: m, Compose: src, Images: map[string]string{}}
	errs := m.Validate()
	services, _ := doc["services"].(map[string]any)
	if len(services) == 0 {
		errs = append(errs, fmt.Errorf("services: пусто"))
	}
	for name, v := range services {
		svc, _ := v.(map[string]any)
		img, _ := svc["image"].(string)
		if !digestRe.MatchString(img) {
			errs = append(errs, fmt.Errorf("services.%s.image: нужен образ с digest (repo:tag@sha256:…), получено %q", name, img))
			continue
		}
		app.Images[name] = img
	}
	errs = append(errs, m.checkServices(services)...)
	var secrets []string
	for _, f := range m.Form {
		if f.Secret {
			secrets = append(secrets, f.Key)
		}
	}
	for _, e := range compose.CheckSecretRefs(src, secrets) {
		errs = append(errs, fmt.Errorf("compose: %s", e))
	}
	return app, errs
}

// Validate — проверки манифеста без compose.
func (m Manifest) Validate() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if m.Schema != Schema {
		bad("schema: поддерживается %d, получено %d", Schema, m.Schema)
	}
	if !idRe.MatchString(m.ID) {
		bad("id: %q — латиница a-z, цифры, дефис, 3–32 символа", m.ID)
	}
	if m.Name.RU == "" || m.Name.EN == "" {
		bad("name: нужны ru и en")
	}
	if m.Description.RU == "" || m.Description.EN == "" {
		bad("description: нужны ru и en")
	}
	if !verRe.MatchString(m.Version) {
		bad("version: %q — нужен вид 1.2.3", m.Version)
	}
	if m.AppVersion == "" {
		bad("app_version: пусто")
	}
	if m.OSMin != "" && !verRe.MatchString(strings.SplitN(m.OSMin, "~", 2)[0]) {
		bad("os_min: %q", m.OSMin)
	}
	if len(m.Architectures) == 0 {
		bad("architectures: пусто")
	}
	for _, a := range m.Architectures {
		if !archs[a] {
			bad("architectures: неизвестная %q", a)
		}
	}
	if m.MinRAMMB < 0 || m.MinDiskMB < 0 || m.RestartLimit < 0 {
		bad("min_ram_mb/min_disk_mb/restart_limit: отрицательные значения")
	}
	if p := m.Update.Policy; p != "" && p != "manual" && p != "notify" {
		bad("update.policy: %q — только manual|notify", p)
	}
	if h := m.Health; h != nil && (h.TimeoutS <= 0 || h.TimeoutS > MaxHealthWait || h.GraceS < 0) {
		bad("health.timeout_s: 1..%d", MaxHealthWait)
	}
	if w := m.Web; w != nil {
		if w.Port < 1 || w.Port > 65535 {
			bad("web.port: %d", w.Port)
		}
		if w.Scheme != "" && w.Scheme != "http" && w.Scheme != "https" {
			bad("web.scheme: %q", w.Scheme)
		}
		if w.Path != "" && !strings.HasPrefix(w.Path, "/") {
			bad("web.path: должен начинаться с /")
		}
	}
	for _, ns := range m.Security.HostNamespaces {
		if _, ok := nsNames[ns]; !ok {
			bad("security.host_namespaces: неизвестное %q", ns)
		}
	}
	if len(m.Form) > MaxForm {
		bad("form: больше %d полей", MaxForm)
	}
	keys := map[string]string{}
	for i, f := range m.Form {
		if !keyRe.MatchString(f.Key) {
			bad("form[%d].key: %q — A-Z, цифры, _", i, f.Key)
		}
		if _, dup := keys[f.Key]; dup {
			bad("form: повтор ключа %s", f.Key)
		}
		keys[f.Key] = f.Type
		if !fieldType[f.Type] {
			bad("form.%s.type: %q", f.Key, f.Type)
		}
		if f.Type == "select" && len(f.Options) == 0 {
			bad("form.%s: select без options", f.Key)
		}
		if f.Regex != "" {
			if _, err := regexp.Compile(f.Regex); err != nil {
				bad("form.%s.regex: %v", f.Key, err)
			}
		}
		if f.Type == "random" && (f.Length < 0 || f.Length > 128) {
			bad("form.%s.length: 0..128", f.Key)
		}
		if (f.Type == "password" || f.Type == "random") && !f.Secret {
			bad("form.%s: %s должен быть secret: true", f.Key, f.Type)
		}
	}
	for _, f := range m.Form {
		if f.ShowIf != "" {
			if _, ok := keys[strings.SplitN(f.ShowIf, "=", 2)[0]]; !ok {
				bad("form.%s.show_if: неизвестный ключ", f.Key)
			}
		}
	}
	for i, s := range m.Storage {
		switch s.Kind {
		case "appdata", "cache":
		case "share":
			if keys[s.Key] != "share" {
				bad("storage[%d]: kind share требует key поля формы типа share", i)
			}
		default:
			bad("storage[%d].kind: %q", i, s.Kind)
		}
		if !strings.HasPrefix(s.Target, "/") {
			bad("storage[%d].target: нужен абсолютный путь", i)
		}
	}
	for i, p := range m.Ports {
		if p.Container < 1 || p.Container > 65535 || p.DefaultHost < 1 || p.DefaultHost > 65535 {
			bad("ports[%d]: номер вне 1..65535", i)
		}
		if p.Protocol != "" && p.Protocol != "tcp" && p.Protocol != "udp" {
			bad("ports[%d].protocol: %q", i, p.Protocol)
		}
	}
	return errs
}

// checkServices — ссылки манифеста на сервисы compose существуют.
func (m Manifest) checkServices(services map[string]any) []error {
	var errs []error
	has := func(where, s string) {
		if _, ok := services[s]; !ok {
			errs = append(errs, fmt.Errorf("%s: нет сервиса %q", where, s))
		}
	}
	if m.Web != nil {
		has("web.service", m.Web.Service)
	}
	if m.Health != nil {
		has("health.service", m.Health.Service)
	}
	for i, p := range m.Ports {
		has(fmt.Sprintf("ports[%d].service", i), p.Service)
	}
	for i, s := range m.Storage {
		where := fmt.Sprintf("storage[%d]", i)
		has(where+".service", s.Service)
		svc, ok := services[s.Service].(map[string]any)
		if ok && !mounted(svc["volumes"], s.Target) {
			// storage — декларация; монтирование задаёт compose (volumes).
			errs = append(errs, fmt.Errorf("%s.target %s: не смонтирован в volumes сервиса %q", where, s.Target, s.Service))
		}
	}
	return errs
}

// mounted — точка target есть в volumes сервиса (краткая "src:target[:mode]"
// или полная {target: …} запись).
func mounted(vols any, target string) bool {
	l, _ := vols.([]any)
	want := path.Clean(target)
	for _, v := range l {
		var t string
		switch x := v.(type) {
		case string:
			parts := strings.Split(x, ":")
			if len(parts) >= 2 {
				t = parts[1]
			} else {
				t = parts[0] // анонимный том: только путь в контейнере
			}
		case map[string]any:
			t, _ = x["target"].(string)
		}
		if t != "" && path.Clean(t) == want {
			return true
		}
	}
	return false
}

// declared — коды рисков compose, разрешённые декларацией.
func (s Security) declared() map[string]bool {
	d := map[string]bool{}
	d[compose.RPrivileged] = s.Privileged
	d[compose.RDockerSock] = s.DockerSock
	d[compose.RCapAdd] = len(s.CapAdd) > 0
	d[compose.RDevices] = s.Devices
	d[compose.RUnconfined] = s.Unconfined
	for _, ns := range s.HostNamespaces {
		d[nsNames[ns]] = true
	}
	return d
}

// securityCodes — коды рисков, которые обязана покрывать декларация.
// Порты в сеть и доступ к общим папкам задаются формой, не security.
var securityCodes = map[string]bool{
	compose.RPrivileged: true, compose.RDockerSock: true, compose.RCapAdd: true, compose.RDevices: true,
	compose.RUnconfined: true, compose.RHostNetwork: true, compose.RHostPID: true, compose.RHostIPC: true,
	compose.RHostUTS: true, compose.RHostUserns: true,
}

// CheckSecurity сверяет риски нормализации с декларацией: возвращает
// недекларированные риски (отказ) и декларированные (подтверждение
// пользователем, как у пользовательских приложений). Лишняя декларация (риска нет) — не ошибка.
func CheckSecurity(decl Security, risks []compose.Issue) (undeclared, confirm []compose.Issue) {
	d := decl.declared()
	for _, r := range risks {
		if !securityCodes[r.Code] {
			continue
		}
		if d[r.Code] && (r.Code != compose.RCapAdd || capsCovered(decl.CapAdd, r.Detail)) {
			confirm = append(confirm, r)
		} else {
			undeclared = append(undeclared, r)
		}
	}
	sort.Slice(undeclared, func(i, j int) bool { return undeclared[i].Key < undeclared[j].Key })
	return undeclared, confirm
}

// capsCovered — все права из текста риска cap_add ("…: A, B") есть в декларации.
func capsCovered(decl []string, detail string) bool {
	have := map[string]bool{}
	for _, c := range decl {
		have[strings.ToUpper(strings.TrimPrefix(strings.ToUpper(c), "CAP_"))] = true
	}
	i := strings.LastIndex(detail, ": ")
	if i < 0 {
		return false
	}
	for _, c := range strings.Split(detail[i+2:], ",") {
		if !have[strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")] {
			return false
		}
	}
	return true
}
