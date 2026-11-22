package apps

// Операции helper над проектами Compose пользовательских приложений.
//
// Раскладка: <пул>/.apps/projects/<id>/ (каталог 0700 root):
//
//	compose.user.yaml  исходный текст пользователя (для правки в вебе)
//	user.env           переменные пользователя (секреты) — не .env, чтобы
//	                   Compose не подставлял их сам
//	compose.json       нормализованный файл — запускается только он
//	override.yaml      устаревший (прежние правки пользователя, убраны 2026-11-21): удаляется при записи
//	*.prev             предыдущая версия (откат неудачного изменения)
//
// helper повторно проверяет нормализованный файл (compose.Verify: жёсткие
// запреты, симлинки bind) и сверяет sha256 с переданным gamayund.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gamayun-nas/internal/apps/compose"
	"gamayun-nas/internal/nas"
)

// Операции helper над проектами Compose.
const (
	OpProjectWrite   = "apps.project.write"
	OpProjectRevert  = "apps.project.revert"
	OpProjectRead    = "apps.project.read"
	OpProjectCompose = "apps.project.compose"
	OpProjectDelete  = "apps.project.delete"
	OpProjectsPS     = "apps.projects.ps"
	OpProjectLogs    = "apps.project.logs"
)

// projectFiles — постоянные файлы проекта; к ним — secrets-<сервис>.env.
var projectFiles = []string{FileUser, FileEnv, FileCompose}

// secretFiles — необязательные файлы проекта: секреты сервисов и устаревший override.yaml
// (suffix "" или prevSuffix).
func secretFiles(dir, suffix string) ([]string, error) {
	l, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, d := range l {
		n := d.Name()
		if d.Type().IsRegular() && n == FileOverride+suffix {
			out = append(out, n)
			continue
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(n, compose.SecretsSuffix+suffix) {
			continue
		}
		if compose.SecretsService(strings.TrimSuffix(n, suffix)) != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// Файлы проекта.
const (
	ProjectsDir   = "projects"
	FileUser      = "compose.user.yaml"
	FileEnv       = "user.env"
	FileCompose   = "compose.json"
	FileOverride  = "override.yaml"
	prevSuffix    = ".prev"
	MaxLogBytes   = 1 << 20
	MaxLogTail    = 5000
	ComposeUpWait = 120 // с, ожидание running/healthy в up --wait
)

// Действия compose.
const (
	ActUp      = "up"
	ActPull    = "pull"
	ActStop    = "stop"
	ActStart   = "start"
	ActRestart = "restart"
	ActDown    = "down"
)

var composeActs = map[string]bool{ActUp: true, ActPull: true, ActStop: true, ActStart: true, ActRestart: true, ActDown: true}

var shaRe = func(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// SHA256 — контрольная сумма нормализованного файла.
func SHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (p Paths) dockerCLI() string {
	if p.DockerCLI != "" {
		return p.DockerCLI
	}
	return filepath.Join(filepath.Dir(p.Dockerd), "docker")
}

// ProjectWriteParams — apps.project.write.
type ProjectWriteParams struct {
	ID          string `json:"id"`
	UserCompose string `json:"user_compose"`
	Env         string `json:"env"`
	// Secrets — файлы секретов сервисов (приложения каталога): сервис → содержимое
	// secrets-<сервис>.env; прочие такие файлы удаляются.
	Secrets    map[string]string `json:"secrets,omitempty"`
	Normalized string            `json:"normalized"`
	SHA256     string            `json:"sha256"`
	Mkdirs     []string          `json:"mkdirs,omitempty"`
	// Backup — сохранить текущие файлы как *.prev (изменение приложения).
	Backup bool `json:"backup,omitempty"`
	// Owner — владелец создаваемых каталогов Mkdirs (учётка приложения); 0 — root.
	Owner int `json:"owner,omitempty"`
}

// Validate — пределы и формат.
func (p ProjectWriteParams) Validate() error {
	switch {
	case !ValidID(p.ID):
		return errors.New("недопустимый идентификатор приложения")
	case len(p.UserCompose) > compose.MaxSize, len(p.Env) > 64<<10, len(p.Normalized) > 4*compose.MaxSize:
		return errors.New("файл слишком большой")
	case !shaRe(p.SHA256):
		return errors.New("недопустимая контрольная сумма")
	case len(p.Mkdirs) > 64:
		return errors.New("слишком много каталогов")
	case p.Owner != 0 && !ValidAppUID(p.Owner):
		return errors.New("владелец вне диапазона учёток приложений")
	}
	if len(p.Secrets) > 64 {
		return errors.New("слишком много файлов секретов")
	}
	total := 0
	for svc, v := range p.Secrets {
		total += len(v)
		if !compose.ValidService(svc) {
			return fmt.Errorf("недопустимое имя сервиса %q", svc)
		}
	}
	if total > 64<<10 {
		return errors.New("файл слишком большой")
	}
	for _, d := range p.Mkdirs {
		if d == "" || filepath.IsAbs(d) || filepath.Clean(d) != d || d == ".." || strings.HasPrefix(d, "../") {
			return fmt.Errorf("недопустимый каталог %q", d)
		}
	}
	return nil
}

// ProjectParams — apps.project.revert/read/delete.
type ProjectParams struct {
	ID string `json:"id"`
}

// Validate проверяет идентификатор.
func (p ProjectParams) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("недопустимый идентификатор приложения")
	}
	return nil
}

// ComposeParams — apps.project.compose.
type ComposeParams struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	SHA256 string `json:"sha256"`
	// RemoveVolumes — down -v (удаление приложения вместе с данными).
	RemoveVolumes bool `json:"remove_volumes,omitempty"`
}

// Validate проверяет действие.
func (p ComposeParams) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("недопустимый идентификатор приложения")
	}
	if !composeActs[p.Action] {
		return fmt.Errorf("недопустимое действие %q", p.Action)
	}
	if p.Action != ActDown && !shaRe(p.SHA256) {
		return errors.New("недопустимая контрольная сумма")
	}
	if p.RemoveVolumes && p.Action != ActDown {
		return errors.New("remove_volumes — только для down")
	}
	return nil
}

// LogsParams — apps.project.logs.
type LogsParams struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	Tail    int    `json:"tail"`
	Since   int64  `json:"since,omitempty"` // unix, с
}

// Validate проверяет пределы.
func (p LogsParams) Validate() error {
	if !ValidID(p.ID) || p.Service == "" || len(p.Service) > 63 || strings.ContainsAny(p.Service, "/\x00 ") {
		return errors.New("недопустимые приложение или сервис")
	}
	if p.Tail < 1 || p.Tail > MaxLogTail || p.Since < 0 {
		return fmt.Errorf("tail в [1, %d]", MaxLogTail)
	}
	return nil
}

// projectDir — <пул>/.apps/projects/<id>; пул — из настройки helper, на запись.
func (e *Executor) projectDir(id string, write bool) (dir string, s state, err error) {
	s, err = e.loadState()
	if err != nil {
		return "", s, err
	}
	if s.Pool == "" {
		return "", s, refuse("приложения не настроены")
	}
	if write {
		if err := e.needPoolRW(s.Pool); err != nil {
			return "", s, err
		}
	} else if m, _ := e.poolMount(s.Pool); !m {
		return "", s, refuse("пул %s не смонтирован", s.Pool)
	}
	return filepath.Join(e.appsRoot(s.Pool), ProjectsDir, id), s, nil
}

func (e *Executor) verifyContext(pool, id string) compose.VerifyContext {
	return compose.VerifyContext{ID: id, MountRoot: e.MountRoot,
		AppData: filepath.Join(e.appsRoot(pool), AppdataDir, id), Resolve: filepath.EvalSymlinks}
}

// ProjectResult — итог операций с файлами проекта.
type ProjectResult struct {
	Dir string `json:"dir"`
}

// ProjectWrite записывает файлы проекта после повторной проверки.
func (e *Executor) ProjectWrite(ctx context.Context, p ProjectWriteParams) (ProjectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, s, err := e.projectDir(p.ID, true)
	if err != nil {
		return ProjectResult{}, err
	}
	norm := []byte(p.Normalized)
	if SHA256(norm) != p.SHA256 {
		return ProjectResult{}, refuse("контрольная сумма нормализованного файла не совпадает")
	}
	appdata := filepath.Join(e.appsRoot(s.Pool), AppdataDir, p.ID)
	if sv, err := e.isSubvol(appdata); err != nil || !sv {
		return ProjectResult{}, refuse("нет субтома данных приложения %s", appdata)
	}
	for _, d := range p.Mkdirs {
		if err := mkdirNoFollow(appdata, d, p.Owner); err != nil {
			return ProjectResult{}, err
		}
	}
	if iss := compose.Verify(norm, e.verifyContext(s.Pool, p.ID)); len(iss) > 0 {
		return ProjectResult{}, refuse("compose не прошёл проверку helper: %s: %s", iss[0].Code, iss[0].Detail)
	}
	if err := ensureDir0700(filepath.Join(e.appsRoot(s.Pool), ProjectsDir)); err != nil {
		return ProjectResult{}, err
	}
	if err := ensureDir0700(dir); err != nil {
		return ProjectResult{}, err
	}
	files := map[string][]byte{FileUser: []byte(p.UserCompose), FileEnv: []byte(p.Env), FileCompose: norm}
	oldSec, err := secretFiles(dir, "")
	if err != nil {
		return ProjectResult{}, err
	}
	if p.Backup {
		stale, err := secretFiles(dir, prevSuffix)
		if err != nil {
			return ProjectResult{}, err
		}
		for _, f := range stale {
			if err := os.Remove(filepath.Join(dir, f)); err != nil {
				return ProjectResult{}, err
			}
		}
		for _, f := range append(append([]string{}, projectFiles...), oldSec...) {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				return ProjectResult{}, fmt.Errorf("резервная копия %s: %w", f, err)
			}
			if err := writePrivate(filepath.Join(dir, f+prevSuffix), b); err != nil {
				return ProjectResult{}, err
			}
		}
	}
	for _, f := range projectFiles {
		if err := writePrivate(filepath.Join(dir, f), files[f]); err != nil {
			return ProjectResult{}, err
		}
	}
	keep := map[string]bool{}
	for svc, v := range p.Secrets {
		f := compose.SecretsFile(svc)
		keep[f] = true
		if err := writePrivate(filepath.Join(dir, f), []byte(v)); err != nil {
			return ProjectResult{}, err
		}
	}
	for _, f := range oldSec {
		if !keep[f] {
			if err := os.Remove(filepath.Join(dir, f)); err != nil {
				return ProjectResult{}, err
			}
		}
	}
	return ProjectResult{Dir: dir}, nil
}

// ProjectRevert возвращает предыдущую версию файлов (*.prev).
func (e *Executor) ProjectRevert(ctx context.Context, p ProjectParams) (ProjectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, _, err := e.projectDir(p.ID, true)
	if err != nil {
		return ProjectResult{}, err
	}
	for _, f := range []string{FileUser, FileEnv, FileCompose} {
		if _, err := os.Stat(filepath.Join(dir, f+prevSuffix)); err != nil {
			return ProjectResult{}, refuse("нет предыдущей версии %s", f)
		}
	}
	cur, err := secretFiles(dir, "")
	if err != nil {
		return ProjectResult{}, err
	}
	prev, err := secretFiles(dir, prevSuffix)
	if err != nil {
		return ProjectResult{}, err
	}
	for _, f := range cur {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			return ProjectResult{}, err
		}
	}
	for _, f := range prev {
		if err := os.Rename(filepath.Join(dir, f), filepath.Join(dir, strings.TrimSuffix(f, prevSuffix))); err != nil {
			return ProjectResult{}, err
		}
	}
	for _, f := range projectFiles {
		if err := os.Rename(filepath.Join(dir, f+prevSuffix), filepath.Join(dir, f)); err != nil {
			return ProjectResult{}, err
		}
	}
	return ProjectResult{Dir: dir}, nil
}

// ProjectFiles — apps.project.read: исходный compose и переменные
// (gamayund отдаёт в API только имена переменных).
type ProjectFiles struct {
	UserCompose string `json:"user_compose"`
	Env         string `json:"env"`
	// Secrets — файлы секретов: сервис → содержимое; в API не отдаётся.
	Secrets map[string]string `json:"secrets,omitempty"`
	SHA256  string            `json:"sha256"`
	Exists  bool              `json:"exists"`
}

// ProjectRead читает файлы проекта.
func (e *Executor) ProjectRead(ctx context.Context, p ProjectParams) (ProjectFiles, error) {
	dir, _, err := e.projectDir(p.ID, false)
	if err != nil {
		return ProjectFiles{}, err
	}
	var r ProjectFiles
	u, err := os.ReadFile(filepath.Join(dir, FileUser))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	env, _ := os.ReadFile(filepath.Join(dir, FileEnv))
	n, _ := os.ReadFile(filepath.Join(dir, FileCompose))
	r = ProjectFiles{UserCompose: string(u), Env: string(env), SHA256: SHA256(n), Exists: true}
	sec, err := secretFiles(dir, "")
	if err != nil {
		return r, err
	}
	for _, f := range sec {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return r, err
		}
		if f == FileOverride {
			continue
		}
		if r.Secrets == nil {
			r.Secrets = map[string]string{}
		}
		r.Secrets[compose.SecretsService(f)] = string(b)
	}
	return r, nil
}

// ProjectDelete удаляет каталог проекта (идемпотентна; данные — appdata.delete).
func (e *Executor) ProjectDelete(ctx context.Context, p ProjectParams) (ProjectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, _, err := e.projectDir(p.ID, true)
	if err != nil {
		return ProjectResult{}, err
	}
	if fi, err := os.Lstat(dir); err == nil && !fi.IsDir() {
		return ProjectResult{}, refuse("%s не каталог", dir)
	}
	return ProjectResult{Dir: dir}, os.RemoveAll(dir)
}

// ComposeResult — итог apps.project.compose (вывод — последние строки).
type ComposeResult struct {
	Output string `json:"output"`
}

// ProjectCompose — docker compose <действие> для проекта приложения.
func (e *Executor) ProjectCompose(ctx context.Context, p ComposeParams) (ComposeResult, error) {
	dir, s, err := e.projectDir(p.ID, p.Action != ActStop && p.Action != ActDown)
	if err != nil {
		return ComposeResult{}, err
	}
	file := filepath.Join(dir, FileCompose)
	b, err := os.ReadFile(file)
	if err != nil {
		if p.Action == ActDown && errors.Is(err, os.ErrNotExist) {
			return e.downByName(ctx, p)
		}
		return ComposeResult{}, err
	}
	if p.Action != ActDown {
		if SHA256(b) != p.SHA256 {
			return ComposeResult{}, refuse("файл проекта изменён: контрольная сумма не совпадает")
		}
		if iss := compose.Verify(b, e.verifyContext(s.Pool, p.ID)); len(iss) > 0 {
			return ComposeResult{}, refuse("compose не прошёл проверку helper: %s: %s", iss[0].Code, iss[0].Detail)
		}
	}
	args := []string{"-p", compose.ProjectName(p.ID), "-f", file, "--project-directory", dir}
	args = append(args, composeArgs(p)...)
	return e.dockerCompose(ctx, args)
}

func composeArgs(p ComposeParams) []string {
	switch p.Action {
	case ActUp:
		return []string{"up", "-d", "--remove-orphans", "--pull", "missing", "--wait", "--wait-timeout", strconv.Itoa(ComposeUpWait)}
	case ActPull:
		return []string{"pull", "--quiet"}
	case ActDown:
		a := []string{"down", "--remove-orphans"}
		if p.RemoveVolumes {
			a = append(a, "--volumes")
		}
		return a
	}
	return []string{p.Action}
}

// downByName — down без файла (файл удалён, контейнеры остались).
func (e *Executor) downByName(ctx context.Context, p ComposeParams) (ComposeResult, error) {
	return e.dockerCompose(ctx, append([]string{"-p", compose.ProjectName(p.ID)}, composeArgs(p)...))
}

func (e *Executor) dockerCompose(ctx context.Context, args []string) (ComposeResult, error) {
	cfg := filepath.Join(e.Paths.Run, "docker-config")
	if err := os.MkdirAll(e.Paths.Run, 0o755); err != nil {
		return ComposeResult{}, err
	}
	if err := ensureDir0700(cfg); err != nil {
		return ComposeResult{}, err
	}
	full := append([]string{"--config", cfg, "-H", "unix://" + e.Paths.DockerSock, "compose"}, args...)
	r, err := e.Runner.Run(ctx, 15*time.Minute, nil, e.Paths.dockerCLI(), full...)
	out := lastLines(string(r.Stdout)+string(r.Stderr), 20)
	if err != nil {
		return ComposeResult{Output: out}, err
	}
	if r.ExitCode != 0 {
		return ComposeResult{Output: out}, fmt.Errorf("docker compose %s: код %d: %s", composeTail(args)[0], r.ExitCode, lastLines(string(r.Stderr), 5))
	}
	return ComposeResult{Output: out}, nil
}

func composeTail(args []string) []string {
	for i, a := range args {
		if composeActs[a] {
			return args[i:]
		}
	}
	return args[len(args)-1:]
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// Container — контейнер приложения (apps.projects.ps).
type Container struct {
	App     string `json:"app"`
	Service string `json:"service"`
	Name    string `json:"name"`
	ID      string `json:"id"`
	Image   string `json:"image"`
	State   string `json:"state"`  // running, exited, restarting, created, paused, dead
	Health  string `json:"health"` // healthy, unhealthy, starting, ""
	Status  string `json:"status"` // строка Docker («Up 3 minutes (healthy)»)
}

// PSResult — контейнеры всех приложений и число посторонних.
type PSResult struct {
	Containers []Container `json:"containers"`
	Foreign    int         `json:"foreign"`
}

// ProjectsPS — контейнеры приложений (метка nas.gamayun.app) и посторонние.
func (e *Executor) ProjectsPS(ctx context.Context, _ NoParams) (PSResult, error) {
	res := PSResult{Containers: []Container{}}
	cs, err := e.docker().Containers(ctx)
	if err != nil {
		return res, err
	}
	for _, c := range cs {
		app := compose.AppOf(c.Labels)
		if app == "" || c.Labels["com.docker.compose.project"] != compose.ProjectName(app) {
			res.Foreign++
			continue
		}
		res.Containers = append(res.Containers, Container{App: app, Service: c.Labels["com.docker.compose.service"],
			Name: c.Name(), ID: shortID(c.ID), Image: c.Image, State: c.State, Health: healthOf(c.Status), Status: c.Status})
	}
	sort.Slice(res.Containers, func(i, j int) bool { return res.Containers[i].Name < res.Containers[j].Name })
	return res, nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func healthOf(status string) string {
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// LogsResult — журнал контейнера.
type LogsResult struct {
	Container string `json:"container"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// ProjectLogs — последние строки журнала контейнера сервиса.
func (e *Executor) ProjectLogs(ctx context.Context, p LogsParams) (LogsResult, error) {
	cs, err := e.docker().Containers(ctx)
	if err != nil {
		return LogsResult{}, err
	}
	for _, c := range cs {
		if compose.AppOf(c.Labels) != p.ID || c.Labels["com.docker.compose.project"] != compose.ProjectName(p.ID) ||
			c.Labels["com.docker.compose.service"] != p.Service {
			continue
		}
		q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {strconv.Itoa(p.Tail)}}
		if p.Since > 0 {
			q.Set("since", strconv.FormatInt(p.Since, 10))
		}
		resp, err := e.docker().get(ctx, "/containers/"+url.PathEscape(c.ID)+"/logs", q)
		if err != nil {
			return LogsResult{}, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxLogBytes+1))
		if err != nil {
			return LogsResult{}, err
		}
		r := LogsResult{Container: c.Name(), Truncated: len(raw) > MaxLogBytes}
		if r.Truncated {
			raw = raw[:MaxLogBytes]
		}
		r.Text = string(bytes.ToValidUTF8(demux(raw), []byte("?")))
		return r, nil
	}
	return LogsResult{}, fmt.Errorf("%w: контейнер сервиса %s приложения %s не найден", ErrNotFoundOp, p.Service, p.ID)
}

// ErrNotFoundOp — объект не найден (код not_found helper).
var ErrNotFoundOp = nas.ErrNotFound

// demux — поток журнала Docker без TTY: кадры [поток, 0, 0, 0, длина(4)].
func demux(b []byte) []byte {
	if len(b) < 8 || b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
		return b
	}
	var out bytes.Buffer
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if n > len(b) {
			n = len(b)
		}
		out.Write(b[:n])
		b = b[n:]
	}
	return out.Bytes()
}

// mkdirNoFollow создаёт root/rel по компонентам, не проходя по симлинкам.
func mkdirNoFollow(root, rel string, owner int) error {
	cur := root
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return refuse("недопустимый каталог %q", rel)
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(cur, 0o755); err != nil {
				return err
			}
			if owner > 0 {
				if err := os.Lchown(cur, owner, owner); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		case fi.Mode()&os.ModeSymlink != 0:
			return refuse("%s — символьная ссылка", cur)
		case !fi.IsDir():
			return nil // файл (bind файла) — оставить как есть
		}
	}
	return nil
}

func ensureDir0700(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		return os.Chmod(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return refuse("%s не каталог", dir)
	}
	return nil
}

func writePrivate(path string, data []byte) error {
	tmp := path + ".gamayun-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- Docker API: список контейнеров ----------

// APIContainer — элемент GET /containers/json.
type APIContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// Name — имя без '/'.
func (c APIContainer) Name() string {
	if len(c.Names) == 0 {
		return shortID(c.ID)
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Containers — все контейнеры (all=1).
func (c *DockerClient) Containers(ctx context.Context) ([]APIContainer, error) {
	resp, err := c.get(ctx, "/containers/json", url.Values{"all": {"1"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var cs []APIContainer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&cs); err != nil {
		return nil, err
	}
	return cs, nil
}
