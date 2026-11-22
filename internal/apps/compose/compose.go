// Package compose — разбор, нормализация и политика пользовательского
// compose-файла. Чистые функции без побочных эффектов: gamayund
// вызывает Normalize до запуска, helper (root) — Verify перед каждым
// вызовом docker compose (повторная проверка жёстких запретов).
//
// Нормализованный файл — JSON (подмножество YAML, Compose его читает):
// все подстановки ${VAR} уже выполнены по .env проекта, каждый '$' в
// строках удвоен, поэтому Compose ничего не подставляет повторно и
// запускается ровно то, что проверено.
//
// Политика:
//   - bind — только в субтом приложения и общие папки NAS (общая папка —
//     с подтверждением, если у приложения нет своей учётки), всегда create_host_path: false;
//   - запрещено: системные пути, данные Docker и других приложений, build,
//     include/extends, внешние сети и тома, драйверы томов с опциями;
//   - с подтверждением риска: privileged, docker.sock, host-сеть/pid/ipc/uts/
//     userns, cap_add, devices, *unconfined, прямой порт в сеть;
//   - порты — 127.0.0.1, если приложению явно не разрешено «доступно в сети».
package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Пределы.
const (
	MaxSize     = 256 << 10
	MaxServices = 20
	// RestartPolicy — вместо always/unless-stopped: падения в цикле видны.
	RestartPolicy = "on-failure:5"
	// AppLabel — метка контейнеров приложения.
	AppLabel = "nas.gamayun.app"
)

// Коды запретов (отказ без подтверждения).
const (
	FInvalid         = "invalid"
	FTooLarge        = "too_large"
	FTooMany         = "too_many_services"
	FNoServices      = "no_services"
	FUnsupported     = "unsupported_key"
	FBuild           = "build"
	FInclude         = "include"
	FExtends         = "extends"
	FEnvFile         = "env_file"
	FNoImage         = "no_image"
	FExternalNetwork = "external_network"
	FExternalVolume  = "external_volume"
	FVolumeDriver    = "volume_driver"
	FNetworkDriver   = "network_driver"
	FVolumeUndecl    = "volume_undeclared"
	FBindSystem      = "bind_system"
	FBindApps        = "bind_apps"
	FBindOutside     = "bind_outside"
	FBindPath        = "bind_path"
	FBindShareRoot   = "bind_share_root"
	FPropagation     = "bind_propagation"
	// FShareNotGranted — bind в общую папку без выдачи доступа приложению.
	FShareNotGranted = "share_not_granted"
	FRunAs           = "run_as"
	FMountType       = "mount_type"
	FPortRange       = "port_range"
	FPortNoHost      = "port_no_host"
	FPortHostIP      = "port_host_ip"
	FPortInvalid     = "port_invalid"
	FPortDuplicate   = "port_duplicate"
	FContainerNS     = "container_namespace"
	FSecurityOpt     = "security_opt"
	FProjectName     = "project_name"
)

// Коды рисков (нужно подтверждение администратора).
const (
	RPrivileged  = "privileged"
	RDockerSock  = "docker_sock"
	RHostNetwork = "host_network"
	RHostPID     = "host_pid"
	RHostIPC     = "host_ipc"
	RHostUTS     = "host_uts"
	RHostUserns  = "host_userns"
	RCapAdd      = "cap_add"
	RDevices     = "devices"
	RUnconfined  = "unconfined"
	// RShareBind — доступ к общей папке как root: только приложения
	// без своей учётки (Context.UID == 0).
	RShareBind = "share_bind"
	// RRootShareWrite — режим root и запись в общую папку.
	RRootShareWrite = "root_share_write"
	RNetworkPort    = "network_port"
)

// Коды изменений нормализации.
const (
	CShortVolume   = "volume_long_form"
	CRelativeBind  = "relative_bind"
	CRestart       = "restart"
	CLogging       = "logging"
	CContainerName = "container_name"
	CLabel         = "label_removed"
	CPortLoopback  = "port_loopback"
	CRunAs         = "run_as"
	CShareRO       = "share_read_only"
	CEnvFile       = "env_file_inlined"
	CEnvMissing    = "env_missing"
	CEnvHost       = "env_host_dropped"
	CDropped       = "key_dropped"
	CName          = "project_name"
)

// Issue — запрет, риск или изменение. Detail не содержит значений
// переменных (секреты): только имена, пути, номера портов.
type Issue struct {
	Code    string `json:"code"`
	Service string `json:"service,omitempty"`
	// Key — ключ подтверждения риска (код[:объект]); одинаков для повторов.
	Key    string `json:"key,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Port — опубликованный порт.
type Port struct {
	Service   string `json:"service"`
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Proto     string `json:"proto"`
	Network   bool   `json:"network"` // доступен в сети (0.0.0.0), иначе 127.0.0.1
}

// PortKey — "8080/tcp".
func PortKey(host int, proto string) string { return strconv.Itoa(host) + "/" + proto }

// Context — окружение нормализации.
type Context struct {
	ID string
	// MountRoot — каталог точек монтирования пулов (/mnt/gamayun).
	MountRoot string
	// AppData — субтом приложения (<MountRoot>/<пул>/.apps/appdata/<id>).
	AppData string
	// Shares — общие папки: имя → абсолютный путь.
	Shares map[string]string
	// Env — переменные .env проекта (источник подстановок).
	Env map[string]string
	// Network — порты, разрешённые «в сеть»: PortKey → true.
	Network map[string]bool
	// UID, GID — учётка приложения app-<id>. 0 — приложение без
	// своей учётки: запуск как в образе, bind в общие папки — риск share_bind.
	UID, GID int
	// RunAs — режим запуска сервиса (RunUID/RunPUID/RunRoot); пусто —
	// подсказка по compose (SuggestRunAs).
	RunAs map[string]string
	// Grants — выдачи доступа к общим папкам: имя папки → GrantRO/GrantRW.
	Grants map[string]string
	// Secrets — ключи секретов: значения в secrets-<сервис>.env, не в compose;
	// допустимо только environment KEY: ${KEY}.
	Secrets map[string]bool
}

// Режимы запуска сервиса.
const (
	RunUID  = "uid"
	RunPUID = "puid"
	RunRoot = "root"
)

// Выдачи доступа к общей папке.
const (
	GrantRO = "ro"
	GrantRW = "rw"
)

// ShareBind — bind сервиса в общую папку.
type ShareBind struct {
	Service string `json:"service"`
	Share   string `json:"share"`
	Target  string `json:"target"`
	// ReadOnly — в compose пользователя указано :ro.
	ReadOnly bool `json:"read_only"`
	// Grant — выданный доступ (GrantRO/GrantRW), пусто — не выдан.
	Grant string `json:"grant"`
}

// Result — итог нормализации. Normalized — только без запретов.
type Result struct {
	Normalized []byte   `json:"-"`
	Changes    []Issue  `json:"changes"`
	Forbidden  []Issue  `json:"forbidden"`
	Risks      []Issue  `json:"risks"`
	Ports      []Port   `json:"ports"`
	Services   []string `json:"services"`
	// NoHealth — сервисы без healthcheck в compose (у образа он может быть).
	NoHealth []string `json:"no_health"`
	// Mkdirs — каталоги внутри субтома приложения для bind (создаёт helper).
	Mkdirs []string `json:"mkdirs"`
	// RunAs — режим запуска по сервисам (при UID > 0).
	RunAs map[string]string `json:"run_as"`
	// ShareBinds — bind в общие папки (для выдач доступа).
	ShareBinds []ShareBind `json:"share_binds"`
	// SecretUse — ключи секретов каждого сервиса: содержимое файла
	// secrets-<сервис>.env.
	SecretUse map[string][]string `json:"secret_use,omitempty"`
}

// OK — запретов нет.
func (r Result) OK() bool { return len(r.Forbidden) == 0 }

// ProjectName — имя проекта Compose приложения.
func ProjectName(id string) string { return "app-" + id }

var svcRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

type norm struct {
	ctx     Context
	res     Result
	missing map[string]bool
	risks   map[string]bool
	mkdirs  map[string]bool
	ports   map[string]bool
	vols    map[string]any
	runAs   string // режим текущего сервиса
}

func (n *norm) forbid(code, svc, format string, a ...any) {
	n.res.Forbidden = append(n.res.Forbidden, Issue{Code: code, Service: svc, Detail: fmt.Sprintf(format, a...)})
}

func (n *norm) change(code, svc, format string, a ...any) {
	n.res.Changes = append(n.res.Changes, Issue{Code: code, Service: svc, Detail: fmt.Sprintf(format, a...)})
}

func (n *norm) risk(code, svc, obj, format string, a ...any) {
	key := code
	if obj != "" {
		key += ":" + obj
	} else if svc != "" {
		key += ":" + svc
	}
	if n.risks[key] {
		return
	}
	n.risks[key] = true
	n.res.Risks = append(n.res.Risks, Issue{Code: code, Service: svc, Key: key, Detail: fmt.Sprintf(format, a...)})
}

// Normalize разбирает compose пользователя, применяет политику и
// возвращает нормализованный файл (если запретов нет).
func Normalize(src []byte, ctx Context) (Result, error) {
	n := &norm{ctx: ctx, missing: map[string]bool{}, risks: map[string]bool{}, mkdirs: map[string]bool{}, ports: map[string]bool{}}
	n.res = Result{Changes: []Issue{}, Forbidden: []Issue{}, Risks: []Issue{}, Ports: []Port{}, Services: []string{}, NoHealth: []string{}, Mkdirs: []string{},
		RunAs: map[string]string{}, ShareBinds: []ShareBind{}}
	if ctx.Env == nil {
		n.ctx.Env = map[string]string{}
	}
	if len(src) > MaxSize {
		n.forbid(FTooLarge, "", "больше %d КиБ", MaxSize>>10)
		return n.res, nil
	}
	var doc any
	dec := yaml.NewDecoder(bytes.NewReader(src))
	if err := dec.Decode(&doc); err != nil {
		n.forbid(FInvalid, "", "ошибка YAML: %s", yamlErr(err))
		return n.res, nil
	}
	var extra any
	if dec.Decode(&extra) == nil {
		n.forbid(FInvalid, "", "в файле несколько документов YAML")
		return n.res, nil
	}
	top, ok := asMap(doc)
	if !ok {
		n.forbid(FInvalid, "", "корень — не словарь")
		return n.res, nil
	}
	var secretSvcs map[string][]string
	if len(ctx.Secrets) > 0 {
		var errs []string
		secretSvcs, errs = stripSecrets(top, ctx.Secrets)
		n.res.SecretUse = secretSvcs
		for _, e := range errs {
			n.forbid(FSecretRef, "", "%s", e)
		}
		if len(errs) > 0 {
			return n.res, nil
		}
	}
	// Подстановки — до анализа: проверяется уже то, что будет запущено.
	iv, err := n.interp(top)
	if err != nil {
		n.forbid(FInvalid, "", "%v", err)
		return n.res, nil
	}
	top = iv.(map[string]any)
	out := map[string]any{"name": ProjectName(ctx.ID)}
	for _, k := range sortedKeys(top) {
		switch {
		case k == "services", k == "volumes", k == "networks":
		case k == "name":
			n.change(CName, "", "имя проекта заменено на %s", ProjectName(ctx.ID))
		case k == "version" || strings.HasPrefix(k, "x-"):
			n.change(CDropped, "", "ключ %s убран", k)
		case k == "include":
			n.forbid(FInclude, "", "include (подключение других файлов) не поддерживается")
		default:
			n.forbid(FUnsupported, "", "ключ верхнего уровня %s не поддерживается", k)
		}
	}
	if v, ok := n.topVolumes(top["volumes"]); ok {
		out["volumes"] = v
	}
	if v, ok := n.topNetworks(top["networks"]); ok {
		out["networks"] = v
	}
	svcs, ok := asMap(top["services"])
	switch {
	case !ok || len(svcs) == 0:
		n.forbid(FNoServices, "", "нет сервисов (services)")
	case len(svcs) > MaxServices:
		n.forbid(FTooMany, "", "сервисов %d, предел %d", len(svcs), MaxServices)
	default:
		os := map[string]any{}
		for _, name := range sortedKeys(svcs) {
			if !svcRe.MatchString(name) {
				n.forbid(FInvalid, name, "недопустимое имя сервиса")
				continue
			}
			n.res.Services = append(n.res.Services, name)
			o := n.service(name, svcs[name], svcs)
			if len(secretSvcs[name]) > 0 && o != nil {
				o["env_file"] = []any{SecretsFile(name)}
			}
			os[name] = o
		}
		out["services"] = os
	}
	if len(n.missing) > 0 {
		n.change(CEnvMissing, "", "не заданы переменные (подставлена пустая строка): %s", strings.Join(sortedSet(n.missing), ", "))
	}
	n.res.Mkdirs = sortedSet(n.mkdirs)
	if !n.res.OK() {
		return n.res, nil
	}
	b, err := json.MarshalIndent(escapeDollar(out), "", "  ")
	if err != nil {
		return n.res, err
	}
	// Самопроверка: результат проходит жёсткую проверку helper.
	if iss := verifyDoc(out, VerifyContext{ID: ctx.ID, MountRoot: ctx.MountRoot, AppData: ctx.AppData}); len(iss) > 0 {
		return n.res, fmt.Errorf("нормализация дала недопустимый файл: %s: %s", iss[0].Code, iss[0].Detail)
	}
	n.res.Normalized = append(b, '\n')
	return n.res, nil
}

func yamlErr(err error) string {
	s := err.Error()
	s = strings.TrimPrefix(s, "yaml: ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// interp — подстановки во всех строковых значениях (ключи — как есть).
func (n *norm) interp(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return interpolate(t, n.ctx.Env, n.missing)
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			y, err := n.interp(x)
			if err != nil {
				return nil, err
			}
			m[k] = y
		}
		return m, nil
	case map[any]any:
		mm, ok := asMap(t)
		if !ok {
			return nil, fmt.Errorf("ключи словаря должны быть строками")
		}
		return n.interp(mm)
	case []any:
		l := make([]any, len(t))
		for i, x := range t {
			y, err := n.interp(x)
			if err != nil {
				return nil, err
			}
			l[i] = y
		}
		return l, nil
	}
	return v, nil
}

func escapeDollar(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "$", "$$")
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = escapeDollar(x)
		}
		return m
	case []any:
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = escapeDollar(x)
		}
		return l
	}
	return v
}

func asMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			s, ok := k.(string)
			if !ok {
				return nil, false
			}
			m[s] = x
		}
		return m, true
	}
	return nil, false
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedSet(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func scalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case uint64:
		return strconv.FormatUint(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	case nil:
		return "", true
	}
	return "", false
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "yes" || t == "1"
	}
	return false
}

func strList(v any) ([]string, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case string:
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			s, ok := scalar(x)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// ---------- верхний уровень ----------

var volKeys = map[string]bool{"driver": true, "labels": true}
var netKeys = map[string]bool{"driver": true, "labels": true, "ipam": true, "internal": true, "attachable": true, "enable_ipv4": true, "enable_ipv6": true}

func (n *norm) topVolumes(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	m, ok := asMap(v)
	if !ok {
		n.forbid(FInvalid, "", "volumes: ожидается словарь")
		return nil, false
	}
	n.vols = m
	out := map[string]any{}
	for _, name := range sortedKeys(m) {
		if !svcRe.MatchString(name) {
			n.forbid(FInvalid, "", "недопустимое имя тома %q", name)
			continue
		}
		def, _ := asMap(m[name])
		o := map[string]any{}
		for _, k := range sortedKeys(def) {
			switch {
			case k == "external":
				if truthy(def[k]) {
					n.forbid(FExternalVolume, "", "том %s: внешний том (external) — данные чужого проекта", name)
				}
			case k == "driver":
				if d, _ := scalar(def[k]); d != "" && d != "local" {
					n.forbid(FVolumeDriver, "", "том %s: драйвер %s не поддерживается, только local", name, d)
				}
			case k == "driver_opts":
				n.forbid(FVolumeDriver, "", "том %s: driver_opts (монтирование в обход проверки путей) запрещён", name)
			case k == "name":
				n.change(CDropped, "", "том %s: имя (name) убрано — тома только внутри проекта", name)
			case volKeys[k]:
				o[k] = def[k]
			case strings.HasPrefix(k, "x-"):
			default:
				n.forbid(FUnsupported, "", "том %s: ключ %s не поддерживается", name, k)
			}
		}
		out[name] = o
	}
	return out, true
}

func (n *norm) topNetworks(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	m, ok := asMap(v)
	if !ok {
		n.forbid(FInvalid, "", "networks: ожидается словарь")
		return nil, false
	}
	out := map[string]any{}
	for _, name := range sortedKeys(m) {
		if !svcRe.MatchString(name) {
			n.forbid(FInvalid, "", "недопустимое имя сети %q", name)
			continue
		}
		def, _ := asMap(m[name])
		o := map[string]any{}
		for _, k := range sortedKeys(def) {
			switch {
			case k == "external":
				if truthy(def[k]) {
					n.forbid(FExternalNetwork, "", "сеть %s: внешняя сеть (external) не поддерживается", name)
				}
			case k == "driver":
				if d, _ := scalar(def[k]); d != "" && d != "bridge" {
					n.forbid(FNetworkDriver, "", "сеть %s: драйвер %s не поддерживается, только bridge", name, d)
				}
				o[k] = def[k]
			case k == "driver_opts":
				n.forbid(FNetworkDriver, "", "сеть %s: driver_opts запрещён", name)
			case k == "name":
				n.change(CDropped, "", "сеть %s: имя (name) убрано — сети только внутри проекта", name)
			case netKeys[k]:
				o[k] = def[k]
			case strings.HasPrefix(k, "x-"):
			default:
				n.forbid(FUnsupported, "", "сеть %s: ключ %s не поддерживается", name, k)
			}
		}
		out[name] = o
	}
	return out, true
}

// ---------- сервис ----------

// passKeys — ключи сервиса, которые переносятся без изменений.
var passKeys = map[string]bool{
	"image": true, "command": true, "entrypoint": true, "expose": true, "healthcheck": true, "depends_on": true,
	"user": true, "working_dir": true, "hostname": true, "domainname": true, "stop_signal": true,
	"stop_grace_period": true, "tty": true, "stdin_open": true, "init": true, "mem_limit": true,
	"mem_reservation": true, "memswap_limit": true, "mem_swappiness": true, "cpus": true, "cpu_shares": true,
	"cpu_quota": true, "cpu_period": true, "cpuset": true, "cpu_count": true, "cpu_percent": true,
	"shm_size": true, "ulimits": true, "sysctls": true, "tmpfs": true, "networks": true, "read_only": true,
	"extra_hosts": true, "dns": true, "dns_search": true, "dns_opt": true, "platform": true,
	"profiles": true, "cap_drop": true, "group_add": true, "links": true, "mac_address": true,
	"oom_score_adj": true, "oom_kill_disable": true, "pids_limit": true, "scale": true, "annotations": true,
	"attach": true, "pull_policy": true,
}

// forbidKeys — ключи сервиса с отказом и причиной.
var forbidKeys = map[string][2]string{
	"build":           {FBuild, "сборка образа на NAS (build) не поддерживается — укажите готовый образ (image)"},
	"extends":         {FExtends, "extends не поддерживается"},
	"configs":         {FUnsupported, "configs (файлы хоста) не поддерживается — используйте том в данных приложения"},
	"secrets":         {FUnsupported, "secrets (файлы хоста) не поддерживается — используйте .env"},
	"cgroup_parent":   {FUnsupported, "cgroup_parent не поддерживается"},
	"cgroup":          {FUnsupported, "cgroup не поддерживается"},
	"runtime":         {FUnsupported, "runtime не поддерживается"},
	"external_links":  {FUnsupported, "external_links (контейнеры вне проекта) не поддерживается"},
	"storage_opt":     {FUnsupported, "storage_opt не поддерживается"},
	"credential_spec": {FUnsupported, "credential_spec не поддерживается"},
	"isolation":       {FUnsupported, "isolation не поддерживается"},
	"post_start":      {FUnsupported, "post_start (команды с правами) не поддерживается"},
	"pre_stop":        {FUnsupported, "pre_stop (команды с правами) не поддерживается"},
	"label_file":      {FUnsupported, "label_file (файл хоста) не поддерживается"},
	"models":          {FUnsupported, "models не поддерживается"},
	"provider":        {FUnsupported, "provider не поддерживается"},
	"blkio_config":    {FUnsupported, "blkio_config не поддерживается"},
}

func (n *norm) service(name string, v any, all map[string]any) map[string]any {
	in, ok := asMap(v)
	out := map[string]any{}
	if !ok {
		n.forbid(FInvalid, name, "описание сервиса — не словарь")
		return out
	}
	for _, k := range sortedKeys(in) {
		val := in[k]
		if f, ok := forbidKeys[k]; ok {
			n.forbid(f[0], name, "%s", f[1])
			continue
		}
		switch {
		case passKeys[k]:
			out[k] = val
		case strings.HasPrefix(k, "x-"), k == "develop":
			n.change(CDropped, name, "ключ %s убран", k)
		case k == "container_name":
			n.change(CContainerName, name, "container_name убран: имена контейнеров задаёт проект")
		case k == "environment", k == "env_file", k == "labels", k == "logging", k == "restart",
			k == "ports", k == "volumes", k == "deploy", k == "volumes_from":
			// ниже
		case k == "privileged":
			if truthy(val) {
				n.risk(RPrivileged, name, "", "полный доступ к системе NAS (privileged)")
			}
			out[k] = val
		case k == "cap_add":
			l, ok := strList(val)
			if !ok {
				n.forbid(FInvalid, name, "cap_add: ожидается список")
			} else if len(l) > 0 {
				n.risk(RCapAdd, name, "", "дополнительные права ядра: %s", strings.Join(l, ", "))
			}
			out[k] = val
		case k == "devices", k == "device_cgroup_rules", k == "gpus":
			n.risk(RDevices, name, "", "прямой доступ к устройствам NAS (%s)", k)
			out[k] = val
		case k == "use_api_socket":
			if truthy(val) {
				n.risk(RDockerSock, name, "", "доступ к управлению Docker (use_api_socket) = права root на NAS")
			}
			out[k] = val
		case k == "security_opt":
			n.securityOpt(name, val)
			out[k] = val
		case k == "network_mode", k == "pid", k == "ipc", k == "uts", k == "userns_mode":
			n.namespace(name, k, val)
			out[k] = val
		default:
			n.forbid(FUnsupported, name, "ключ %s не поддерживается", k)
		}
	}
	if img, _ := scalar(in["image"]); strings.TrimSpace(img) == "" {
		if _, b := in["build"]; !b {
			n.forbid(FNoImage, name, "не указан образ (image)")
		}
	}
	if pp, _ := scalar(in["pull_policy"]); pp == "build" {
		n.forbid(FBuild, name, "pull_policy: build не поддерживается")
	}
	if hc, ok := asMap(in["healthcheck"]); !ok || truthy(hc["disable"]) {
		n.res.NoHealth = append(n.res.NoHealth, name)
	}
	if vf, ok := in["volumes_from"]; ok {
		l, ok := strList(vf)
		if !ok {
			n.forbid(FInvalid, name, "volumes_from: ожидается список")
		}
		for _, x := range l {
			src, _, _ := strings.Cut(x, ":")
			if strings.HasPrefix(x, "container:") || all[src] == nil {
				n.forbid(FContainerNS, name, "volumes_from %s: только сервисы этого приложения", x)
			}
		}
		out["volumes_from"] = vf
	}
	n.environment(name, in, out)
	n.runAs = ""
	if n.ctx.UID > 0 {
		n.applyRunAs(name, in, out)
	}
	n.labels(name, in["labels"], out)
	n.restart(name, in, out)
	if lg, ok := in["logging"]; ok && lg != nil {
		n.change(CLogging, name, "журнал контейнера: драйвер local, 10 МБ × 3 (на пуле)")
	}
	out["logging"] = map[string]any{"driver": "local", "options": map[string]any{"max-size": "10m", "max-file": "3"}}
	if v, ok := in["ports"]; ok {
		out["ports"] = n.portsOf(name, v)
	}
	if v, ok := in["volumes"]; ok {
		out["volumes"] = n.volumesOf(name, v)
	}
	if v, ok := in["deploy"]; ok {
		if d := n.deploy(name, v); d != nil {
			out["deploy"] = d
		}
	}
	return out
}

func (n *norm) securityOpt(svc string, v any) {
	l, ok := strList(v)
	if !ok {
		n.forbid(FInvalid, svc, "security_opt: ожидается список")
		return
	}
	for _, o := range l {
		lo := strings.ToLower(strings.ReplaceAll(o, ":", "="))
		switch {
		case strings.Contains(lo, "unconfined"), lo == "label=disable":
			n.risk(RUnconfined, svc, "", "отключена защита ядра (%s)", o)
		case strings.HasPrefix(lo, "no-new-privileges"), strings.HasPrefix(lo, "apparmor="), strings.HasPrefix(lo, "label="):
		default:
			n.forbid(FSecurityOpt, svc, "security_opt %s не поддерживается (файл профиля с хоста)", o)
		}
	}
}

var nsRisk = map[string]string{"network_mode": RHostNetwork, "pid": RHostPID, "ipc": RHostIPC, "uts": RHostUTS, "userns_mode": RHostUserns}
var nsText = map[string]string{
	"network_mode": "сеть NAS напрямую (network_mode: host): приложение видит все сетевые службы NAS",
	"pid":          "видит все процессы NAS (pid: host)",
	"ipc":          "общая память с NAS (ipc: host)",
	"uts":          "имя узла NAS (uts: host)",
	"userns_mode":  "без изоляции пользователей (userns_mode: host)",
}

func (n *norm) namespace(svc, key string, v any) {
	s, _ := scalar(v)
	switch {
	case s == "host":
		n.risk(nsRisk[key], svc, "", "%s", nsText[key])
	case strings.HasPrefix(s, "container:"):
		n.forbid(FContainerNS, svc, "%s: %s — контейнер вне приложения", key, s)
	}
}

func (n *norm) environment(svc string, in, out map[string]any) {
	env := map[string]any{}
	if ef, ok := in["env_file"]; ok {
		var paths []string
		switch t := ef.(type) {
		case string:
			paths = []string{t}
		case []any:
			for _, x := range t {
				if s, ok := x.(string); ok {
					paths = append(paths, s)
				} else if m, ok := asMap(x); ok {
					p, _ := scalar(m["path"])
					paths = append(paths, p)
				}
			}
		}
		for _, p := range paths {
			if filepath.Clean(p) != ".env" {
				n.forbid(FEnvFile, svc, "env_file %s: поддерживается только .env проекта", p)
			}
		}
		if len(paths) > 0 {
			for k, v := range n.ctx.Env {
				env[k] = v
			}
			n.change(CEnvFile, svc, "переменные .env перенесены в environment")
		}
	}
	switch t := in["environment"].(type) {
	case nil:
	case []any:
		for _, x := range t {
			s, _ := scalar(x)
			k, v, ok := strings.Cut(s, "=")
			if !ok {
				// "VAR" без значения — Compose взял бы из среды хоста.
				if ev, ok := n.ctx.Env[k]; ok {
					env[k] = ev
				} else {
					n.change(CEnvHost, svc, "переменная %s без значения убрана (среда NAS не передаётся)", k)
				}
				continue
			}
			env[k] = v
		}
	default:
		m, ok := asMap(t)
		if !ok {
			n.forbid(FInvalid, svc, "environment: ожидается словарь или список")
			break
		}
		for k, v := range m {
			if v == nil {
				if ev, ok := n.ctx.Env[k]; ok {
					env[k] = ev
				} else {
					n.change(CEnvHost, svc, "переменная %s без значения убрана (среда NAS не передаётся)", k)
				}
				continue
			}
			s, ok := scalar(v)
			if !ok {
				n.forbid(FInvalid, svc, "environment %s: ожидается строка", k)
				continue
			}
			env[k] = s
		}
	}
	for k := range env {
		if k == "" || strings.ContainsAny(k, "= \t\n") {
			n.forbid(FInvalid, svc, "environment: недопустимое имя переменной")
		}
	}
	if len(env) > 0 {
		out["environment"] = env
	}
}

func (n *norm) labels(svc string, v any, out map[string]any) {
	lb := map[string]any{}
	switch t := v.(type) {
	case nil:
	case []any:
		for _, x := range t {
			s, _ := scalar(x)
			k, val, _ := strings.Cut(s, "=")
			lb[k] = val
		}
	default:
		m, ok := asMap(t)
		if !ok {
			n.forbid(FInvalid, svc, "labels: ожидается словарь или список")
		}
		for k, x := range m {
			s, _ := scalar(x)
			lb[k] = s
		}
	}
	for k := range lb {
		if strings.HasPrefix(k, "com.docker.") || strings.HasPrefix(k, "nas.gamayun.") {
			delete(lb, k)
			n.change(CLabel, svc, "служебная метка %s убрана", k)
		}
	}
	lb[AppLabel] = n.ctx.ID
	out["labels"] = lb
}

func (n *norm) restart(svc string, in, out map[string]any) {
	r, _ := scalar(in["restart"])
	if r == "no" || r == "\"no\"" {
		out["restart"] = "no"
		return
	}
	if r != RestartPolicy {
		was := r
		if was == "" {
			was = "не задано"
		}
		n.change(CRestart, svc, "restart: %s → %s (падения в цикле видны и останавливаются)", was, RestartPolicy)
	}
	out["restart"] = RestartPolicy
}

func (n *norm) deploy(svc string, v any) map[string]any {
	m, ok := asMap(v)
	if !ok {
		n.forbid(FInvalid, svc, "deploy: ожидается словарь")
		return nil
	}
	for _, k := range sortedKeys(m) {
		if k != "resources" {
			n.change(CDropped, svc, "deploy.%s убран (Swarm)", k)
		}
	}
	res, ok := asMap(m["resources"])
	if !ok {
		return nil
	}
	if rv, ok := asMap(res["reservations"]); ok {
		if _, ok := rv["devices"]; ok {
			n.risk(RDevices, svc, "", "прямой доступ к устройствам NAS (deploy.resources.reservations.devices)")
		}
	}
	return map[string]any{"resources": res}
}

// ---------- порты ----------

func (n *norm) portsOf(svc string, v any) []any {
	l, ok := v.([]any)
	if !ok {
		n.forbid(FInvalid, svc, "ports: ожидается список")
		return nil
	}
	out := []any{}
	for _, x := range l {
		var hostIP, pub, tgt, proto string
		switch t := x.(type) {
		case map[string]any, map[any]any:
			m, _ := asMap(t)
			tgt, _ = scalar(m["target"])
			pub, _ = scalar(m["published"])
			hostIP, _ = scalar(m["host_ip"])
			proto, _ = scalar(m["protocol"])
		default:
			s, ok := scalar(t)
			if !ok {
				n.forbid(FInvalid, svc, "ports: недопустимая запись")
				continue
			}
			hostIP, pub, tgt, proto = splitPort(s)
		}
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			n.forbid(FPortInvalid, svc, "порт %s: протокол %s не поддерживается", tgt, proto)
			continue
		}
		if strings.Contains(pub, "-") || strings.Contains(tgt, "-") {
			n.forbid(FPortRange, svc, "диапазоны портов (%s) не поддерживаются — перечислите порты", pub+":"+tgt)
			continue
		}
		c, err := strconv.Atoi(tgt)
		if err != nil || c < 1 || c > 65535 {
			n.forbid(FPortInvalid, svc, "недопустимый порт контейнера %q", tgt)
			continue
		}
		if pub == "" {
			n.forbid(FPortNoHost, svc, "порт %d без порта NAS: укажите «порт_NAS:%d»", c, c)
			continue
		}
		h, err := strconv.Atoi(pub)
		if err != nil || h < 1 || h > 65535 {
			n.forbid(FPortInvalid, svc, "недопустимый порт NAS %q", pub)
			continue
		}
		key := PortKey(h, proto)
		if n.ports[key] {
			n.forbid(FPortDuplicate, svc, "порт NAS %s указан дважды", key)
			continue
		}
		n.ports[key] = true
		wantNet := false
		switch strings.Trim(hostIP, "[]") {
		case "", "0.0.0.0", "::":
			wantNet = hostIP != ""
		case "127.0.0.1":
		default:
			n.forbid(FPortHostIP, svc, "порт %s: адрес %s не поддерживается (только 127.0.0.1 или вся сеть)", key, hostIP)
			continue
		}
		p := Port{Service: svc, Host: h, Container: c, Proto: proto, Network: n.ctx.Network[key]}
		ip := "127.0.0.1"
		if p.Network {
			ip = "0.0.0.0"
			n.risk(RNetworkPort, svc, key, "порт %s открыт для всей сети: опубликованный порт Docker проходит мимо фаервола NAS", key)
		} else if hostIP == "" || wantNet {
			n.change(CPortLoopback, svc, "порт %s доступен только на NAS (127.0.0.1); для доступа из сети включите «доступно в сети»", key)
		}
		n.res.Ports = append(n.res.Ports, p)
		out = append(out, map[string]any{"target": c, "published": strconv.Itoa(h), "host_ip": ip, "protocol": proto})
	}
	return out
}

// splitPort разбирает [ip:][host:]container[/proto].
func splitPort(s string) (ip, host, ctr, proto string) {
	s, proto, _ = strings.Cut(s, "/")
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "]:"); i > 0 {
			ip, s = s[:i+1], s[i+2:]
		}
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		ctr = parts[0]
	case 2:
		host, ctr = parts[0], parts[1]
	default:
		ip, host, ctr = strings.Join(parts[:len(parts)-2], ":"), parts[len(parts)-2], parts[len(parts)-1]
	}
	return
}

// ---------- тома ----------

func (n *norm) volumesOf(svc string, v any) []any {
	l, ok := v.([]any)
	if !ok {
		n.forbid(FInvalid, svc, "volumes: ожидается список")
		return nil
	}
	out := []any{}
	for _, x := range l {
		var typ, src, tgt string
		ro := false
		var bindOpt, volOpt, tmpfsOpt map[string]any
		switch t := x.(type) {
		case map[string]any, map[any]any:
			m, _ := asMap(t)
			for _, k := range sortedKeys(m) {
				switch k {
				case "type", "source", "target", "read_only", "bind", "volume", "tmpfs", "consistency":
				default:
					n.forbid(FUnsupported, svc, "volumes: ключ %s не поддерживается", k)
				}
			}
			typ, _ = scalar(m["type"])
			src, _ = scalar(m["source"])
			tgt, _ = scalar(m["target"])
			ro = truthy(m["read_only"])
			bindOpt, _ = asMap(m["bind"])
			volOpt, _ = asMap(m["volume"])
			tmpfsOpt, _ = asMap(m["tmpfs"])
		default:
			s, ok := scalar(t)
			if !ok {
				n.forbid(FInvalid, svc, "volumes: недопустимая запись")
				continue
			}
			parts := strings.Split(s, ":")
			if len(parts) > 3 {
				n.forbid(FInvalid, svc, "volumes: недопустимая запись %q", s)
				continue
			}
			if len(parts) == 1 {
				tgt = parts[0]
			} else {
				src, tgt = parts[0], parts[1]
			}
			if len(parts) == 3 {
				for _, o := range strings.Split(parts[2], ",") {
					switch o {
					case "ro":
						ro = true
					case "rw", "z", "Z", "nocopy", "rprivate", "private", "":
					default:
						n.forbid(FPropagation, svc, "volumes %s: параметр %s не поддерживается", tgt, o)
					}
				}
			}
			switch {
			case src == "":
				typ = "volume"
			case strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~"):
				typ = "bind"
			default:
				typ = "volume"
			}
			n.change(CShortVolume, svc, "том %s записан в длинной форме", tgt)
		}
		if !strings.HasPrefix(tgt, "/") {
			n.forbid(FInvalid, svc, "volumes: путь в контейнере должен быть абсолютным (%q)", tgt)
			continue
		}
		e := map[string]any{"type": typ, "target": tgt}
		if ro {
			e["read_only"] = true
		}
		switch typ {
		case "bind":
			path, share, ok := n.bindSource(svc, src)
			if !ok {
				continue
			}
			if share != "" && !n.shareBind(svc, share, tgt, ro, e) {
				continue
			}
			if p, _ := scalar(bindOpt["propagation"]); p != "" && p != "rprivate" && p != "private" {
				n.forbid(FPropagation, svc, "bind %s: propagation %s не поддерживается", tgt, p)
				continue
			}
			e["source"] = path
			e["bind"] = map[string]any{"create_host_path": false}
		case "volume":
			if src != "" {
				if _, ok := n.vols[src]; !ok {
					n.forbid(FVolumeUndecl, svc, "том %s не объявлен в volumes верхнего уровня", src)
					continue
				}
				e["source"] = src
			}
			if volOpt != nil {
				e["volume"] = volOpt
			}
		case "tmpfs":
			if tmpfsOpt != nil {
				e["tmpfs"] = tmpfsOpt
			}
		default:
			n.forbid(FMountType, svc, "тип тома %s не поддерживается", typ)
			continue
		}
		out = append(out, e)
	}
	return out
}

// bindSource — проверка и приведение пути bind к абсолютному.
func (n *norm) bindSource(svc, src string) (string, string, bool) {
	if src == "" || strings.ContainsAny(src, "$\x00\n") || strings.HasPrefix(src, "~") {
		n.forbid(FBindPath, svc, "недопустимый путь bind %q", src)
		return "", "", false
	}
	p := src
	if !strings.HasPrefix(p, "/") {
		p = filepath.Join(n.ctx.AppData, p)
		n.change(CRelativeBind, svc, "относительный путь %s → данные приложения", src)
	}
	p = filepath.Clean(p)
	if rel, ok := under(p, n.ctx.AppData); ok {
		if rel != "." {
			n.mkdirs[rel] = true
		}
		return p, "", true
	}
	if p == "/var/run/docker.sock" || p == "/run/docker.sock" {
		n.risk(RDockerSock, svc, "", "доступ к управлению Docker (docker.sock) = права root на NAS")
		return p, "", true
	}
	for _, name := range sortedKeys(anyMap(n.ctx.Shares)) {
		sp := filepath.Clean(n.ctx.Shares[name])
		if _, ok := under(p, sp); !ok {
			continue
		}
		if c := classifyApps(sp, n.ctx.MountRoot); c != "" || isPoolRoot(sp, n.ctx.MountRoot) {
			n.forbid(FBindShareRoot, svc, "общая папка %s совпадает с корнем пула — bind запрещён (там данные приложений)", name)
			return "", "", false
		}
		if c := classifyApps(p, n.ctx.MountRoot); c != "" {
			n.forbid(FBindApps, svc, "%s — данные Docker или других приложений", p)
			return "", "", false
		}
		if n.ctx.UID == 0 {
			n.risk(RShareBind, svc, name, "доступ к общей папке «%s» с правами root (права по пользователю — позже)", name)
		}
		return p, name, true
	}
	switch {
	case classifyApps(p, n.ctx.MountRoot) != "":
		n.forbid(FBindApps, svc, "%s — данные Docker или других приложений", p)
	case systemPath(p):
		n.forbid(FBindSystem, svc, "%s — системный каталог NAS", p)
	default:
		n.forbid(FBindOutside, svc, "%s — вне данных приложения и общих папок", p)
	}
	return "", "", false
}

// shareBind — bind в общую папку при своей учётке приложения:
// нужна выдача; «только чтение» — монтирование read_only; root с записью — риск.
func (n *norm) shareBind(svc, share, tgt string, ro bool, e map[string]any) bool {
	if n.ctx.UID == 0 {
		return true
	}
	g := n.ctx.Grants[share]
	n.res.ShareBinds = append(n.res.ShareBinds, ShareBind{Service: svc, Share: share, Target: tgt, ReadOnly: ro, Grant: g})
	switch g {
	case GrantRO:
		if !ro {
			n.change(CShareRO, svc, "общая папка «%s» (%s): доступ только для чтения — монтирование только для чтения", share, tgt)
		}
		e["read_only"] = true
	case GrantRW:
		if n.runAs == RunRoot && !ro {
			n.risk(RRootShareWrite, svc, share, "сервис работает как root и может менять в общей папке «%s» любые файлы", share)
		}
	default:
		n.forbid(FShareNotGranted, svc, "нет доступа к общей папке «%s»: выберите «только чтение» или «чтение и запись»", share)
		return false
	}
	return true
}

// SuggestRunAs — режим запуска по описанию сервиса: user root → root;
// PUID/PGID в окружении или образ linuxserver → puid; иначе uid.
func SuggestRunAs(in map[string]any, env map[string]any) string {
	if u, ok := scalar(in["user"]); ok && in["user"] != nil {
		name, _, _ := strings.Cut(strings.TrimSpace(u), ":")
		if name == "0" || name == "root" {
			return RunRoot
		}
	}
	if _, ok := env["PUID"]; ok {
		return RunPUID
	}
	if _, ok := env["PGID"]; ok {
		return RunPUID
	}
	img, _ := scalar(in["image"])
	img = strings.ToLower(img)
	if strings.HasPrefix(img, "lscr.io/linuxserver/") || strings.HasPrefix(img, "linuxserver/") ||
		strings.HasPrefix(img, "ghcr.io/linuxserver/") || strings.HasPrefix(img, "docker.io/linuxserver/") {
		return RunPUID
	}
	return RunUID
}

// applyRunAs — запуск сервиса от учётки приложения.
func (n *norm) applyRunAs(svc string, in, out map[string]any) {
	env, _ := out["environment"].(map[string]any)
	mode := n.ctx.RunAs[svc]
	if mode == "" {
		mode = SuggestRunAs(in, env)
	}
	ids := fmt.Sprintf("%d:%d", n.ctx.UID, n.ctx.GID)
	switch mode {
	case RunUID:
		if u, ok := scalar(in["user"]); ok && in["user"] != nil && u != ids {
			n.change(CRunAs, svc, "user %s заменён учёткой приложения (%s)", u, ids)
		}
		out["user"] = ids
	case RunPUID:
		if env == nil {
			env = map[string]any{}
		}
		for k, v := range map[string]int{"PUID": n.ctx.UID, "PGID": n.ctx.GID} {
			if old, ok := env[k]; ok && old != strconv.Itoa(v) {
				n.change(CRunAs, svc, "%s заменён учёткой приложения (%d)", k, v)
			}
			env[k] = strconv.Itoa(v)
		}
		out["environment"] = env
		if _, ok := in["user"]; ok {
			n.change(CRunAs, svc, "user убран: образ сам сбрасывает права до PUID/PGID")
			delete(out, "user")
		}
	case RunRoot:
	default:
		n.forbid(FRunAs, svc, "неизвестный режим запуска %q", mode)
		return
	}
	n.runAs = mode
	n.res.RunAs[svc] = mode
}

func anyMap(m map[string]string) map[string]any {
	o := make(map[string]any, len(m))
	for k := range m {
		o[k] = nil
	}
	return o
}

// under — p внутри root (или равен); rel — путь относительно root.
func under(p, root string) (string, bool) {
	if root == "" || !filepath.IsAbs(root) {
		return "", false
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// classifyApps — p внутри <MountRoot>/<пул>/.apps (или содержит его).
func classifyApps(p, mountRoot string) string {
	rel, ok := under(p, mountRoot)
	if !ok || rel == "." {
		if ok {
			return "mount_root"
		}
		return ""
	}
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[1] == ".apps" {
		return "apps"
	}
	return ""
}

func isPoolRoot(p, mountRoot string) bool {
	rel, ok := under(p, mountRoot)
	return ok && (rel == "." || !strings.Contains(rel, "/"))
}

var systemPaths = []string{"/etc", "/boot", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/var/lib", "/run", "/var/run", "/proc", "/sys", "/dev", "/root", "/home", "/srv", "/opt", "/var"}

func systemPath(p string) bool {
	if p == "/" {
		return true
	}
	for _, s := range systemPaths {
		if _, ok := under(p, s); ok {
			return true
		}
	}
	return false
}

// AppOf — приложение контейнера по метке.
func AppOf(labels map[string]string) string {
	return labels[AppLabel]
}
