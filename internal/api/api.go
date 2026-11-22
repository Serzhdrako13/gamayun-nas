// Package api — HTTP API gamayund (REST + SSE), описание — api/openapi.yaml.
//
// Аутентификация подключается снаружи: Options.Auth оборачивает
// всё, кроме /api/v1/health и маршрутов /api/v1/auth/* (Options.AuthRoutes,
// они сами проверяют сессию, где нужно). Без Auth API открыт — допустимо
// только на loopback (см. config).
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"gamayun-nas/internal/apps"
	"gamayun-nas/internal/catalog"
	"gamayun-nas/internal/confbackup"
	"gamayun-nas/internal/proxy"
	"gamayun-nas/internal/snapshots"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"gamayun-nas/internal/auth"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/files"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/maintenance"
	"gamayun-nas/internal/metrics"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/notify"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/storage/planner"
	"gamayun-nas/internal/tasks"
	"gamayun-nas/internal/version"
)

// HelperClient — вызов операции gamayun-helper (helper.Client; в тестах — подмена).
type HelperClient interface {
	Call(ctx context.Context, op string, params, result any) error
}

// Middleware — обёртка обработчика.
type Middleware func(http.Handler) http.Handler

// Options — зависимости API.
type Options struct {
	Helper HelperClient
	Tasks  *tasks.Manager
	Events *events.Log
	// Metrics — текущая нагрузка для виджетов; nil — 503.
	Metrics *metrics.Sampler
	DB      *sql.DB // проверка в /health
	// Storage — пулы профиля Home; nil — эндпоинты пулов отвечают 503.
	Storage *home.Service
	// NAS — пользователи, группы и общие папки; nil — 503.
	NAS *nas.Service
	// UI — веб-интерфейс; отдаётся без входа для всех путей вне /api/.
	// nil — только API.
	UI http.Handler
	// Auth — аутентификация и авторизация. nil — пропускать всё.
	// Вызывается для всех путей кроме /api/v1/health и /api/v1/auth/*.
	Auth Middleware
	// AuthRoutes регистрирует маршруты /api/v1/auth/* (вход, выход, сессия);
	// они не оборачиваются Auth. nil — таких маршрутов нет.
	AuthRoutes func(mux *http.ServeMux)
	// SessionCheck — повторная проверка сессии долгого потока SSE на каждом
	// пинге; false — поток закрывается (выход, отзыв, истечение). nil — нет.
	SessionCheck func(r *http.Request) bool
	Logger       *slog.Logger
	// Notify, Deliver — центр уведомлений и доставка; nil — 503.
	Notify  *notify.Service
	Deliver *notify.Deliverer
	// Maintenance — расписание проверок и SMART; nil — 503.
	Maintenance *maintenance.Service
	// TLS — действующий сертификат HTTPS; nil — TLS отключён, 503.
	TLS TLSStore
	// Config — резервные копии настроек; nil — 503.
	Config *confbackup.Service
	// Snapshots — снапшоты общих папок; nil — 503.
	Snapshots *snapshots.Service
	// Files — веб-файловый менеджер; nil — 503.
	Files *files.Service
	// Apps — подсистема приложений Docker; nil — 503.
	Apps *apps.Service
	// Proxy — публикация приложений через Caddy; nil — 503.
	Proxy *proxy.Service
	// Catalog — каталог приложений; nil — 503. CatalogURL — источник.
	Catalog    *catalog.Store
	CatalogURL string
	// Heartbeat — интервал комментария-пинга SSE; 0 — 15 с.
	Heartbeat time.Duration
}

type server struct {
	Options
}

// New возвращает обработчик API.
func New(o Options) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	auth := o.Auth
	if auth == nil {
		auth = func(h http.Handler) http.Handler { return h }
	}
	s := &server{o}

	open := http.NewServeMux()
	open.HandleFunc("GET /api/v1/health", s.health)
	if o.AuthRoutes != nil {
		o.AuthRoutes(open)
		open.HandleFunc("/api/v1/auth/", fallback(open))
	}

	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/v1/system", s.system)
	protected.HandleFunc("GET /api/v1/system/metrics", s.metrics)
	protected.HandleFunc("GET /api/v1/events/recent", s.recentEvents)
	protected.HandleFunc("GET /api/v1/disks", s.disks)
	protected.HandleFunc("POST /api/v1/storage/plan", s.plan)
	protected.HandleFunc("POST /api/v1/storage/pools/plans", s.poolPlan)
	protected.HandleFunc("POST /api/v1/storage/pools/plans/{id}/confirm", s.poolConfirm)
	protected.HandleFunc("GET /api/v1/storage/pools", s.listPools)
	protected.HandleFunc("GET /api/v1/storage/pools/{name}", s.getPool)
	protected.HandleFunc("DELETE /api/v1/storage/pools/{name}", s.deletePool)
	protected.HandleFunc("POST /api/v1/storage/pools/{name}/resume", s.resumePool)
	protected.HandleFunc("GET /api/v1/storage/found-pools", s.foundPools)
	protected.HandleFunc("POST /api/v1/storage/found-pools/{name}/import", s.importPool)
	protected.HandleFunc("POST /api/v1/disks/{id}/release", s.releaseDisk)
	protected.HandleFunc("PUT /api/v1/storage/pools/{name}/protection", s.setProtection)
	protected.HandleFunc("GET /api/v1/storage/pools/{name}/protection/log", s.protectionLog)
	protected.HandleFunc("POST /api/v1/storage/pools/{name}/write-override", s.setOverride)
	protected.HandleFunc("DELETE /api/v1/storage/pools/{name}/write-override", s.clearOverride)
	s.nasRoutes(protected)
	s.settingsRoutes(protected)
	s.notifyRoutes(protected)
	s.maintenanceRoutes(protected)
	s.tlsRoutes(protected)
	s.configRoutes(protected)
	s.snapshotRoutes(protected)
	s.appsRoutes(protected)
	s.retentionRoutes(protected)
	s.filesRoutes(protected)
	protected.HandleFunc("GET /api/v1/tasks", s.listTasks)
	protected.HandleFunc("GET /api/v1/tasks/{id}", s.getTask)
	protected.HandleFunc("GET /api/v1/events", s.events)
	protected.HandleFunc("/", fallback(protected, open))

	root := http.NewServeMux()
	root.HandleFunc("GET /api/v1/health", s.health)
	if o.AuthRoutes != nil {
		root.Handle("/api/v1/auth/", open)
	}
	if o.UI != nil {
		// Сам интерфейс — статика без данных; вход и CSRF — на стороне API.
		root.Handle("/api/", auth(protected))
		root.Handle("/", o.UI)
	} else {
		root.Handle("/", auth(protected))
	}

	return s.recoverer(s.logRequests(securityHeaders(root)))
}

// fallback отвечает JSON-ошибкой 404/405 вместо текстовых ответов mux.
func fallback(muxes ...*http.ServeMux) http.HandlerFunc {
	methods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	return func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, m := range methods {
			r2 := r.Clone(r.Context())
			r2.Method = m
			for _, mux := range muxes {
				// Настоящие маршруты — с методом ("GET /..."); шаблоны без
				// метода ("/", "/api/v1/auth/") — это сами fallback.
				if _, pat := mux.Handler(r2); strings.Contains(pat, " ") {
					allowed = append(allowed, m)
					break
				}
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "метод "+r.Method+" не поддерживается")
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "нет такого ресурса")
	}
}

// --- ответы ---

// ErrorBody — единый формат ошибки: {"error": {"code": "...", "message": "..."}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail — код (стабильный, для программ) и сообщение (для человека).
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{ErrorDetail{Code: code, Message: msg}})
}

func (s *server) helperError(w http.ResponseWriter, r *http.Request, err error) {
	var re *helper.RemoteError
	switch {
	case errors.Is(err, helper.ErrUnavailable):
		s.Logger.Warn("helper недоступен", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "helper_unavailable", "служба управления дисками (gamayun-helper) недоступна")
	case errors.As(err, &re):
		writeError(w, http.StatusBadGateway, "helper_error", re.Code+": "+re.Message)
	default:
		s.Logger.Error("ошибка helper", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusBadGateway, "helper_error", "сбой обращения к службе управления дисками (gamayun-helper)")
	}
}

func (s *server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.Logger.Error("внутренняя ошибка", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
}

// --- middleware ---

// contentSecurityPolicy — базовая политика для API и будущего SPA:
// всё только со своего источника, без встраивания в чужие фреймы, без
// плагинов; data: — для встроенных картинок/иконок.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", contentSecurityPolicy)
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.TLS != nil {
			// Браузер учитывает HSTS только при доверенном сертификате (RFC 6797
			// §8.1), поэтому с самоподписанным он безвреден. Без includeSubDomains:
			// имя NAS может быть поддоменом чужих сервисов.
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap — для http.ResponseController (Flush, SetWriteDeadline в SSE).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		s.Logger.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

func (s *server) recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Logger.Error("паника в обработчике", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// --- обработчики ---

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{"status": "ok", "version": version.Version, "database": "ok"}
	status := http.StatusOK
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.DB == nil || s.DB.PingContext(ctx) != nil {
		resp["status"], resp["database"] = "degraded", "error"
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

func (s *server) system(w http.ResponseWriter, r *http.Request) {
	var host json.RawMessage
	if err := s.Helper.Call(r.Context(), "system.info", nil, &host); err != nil {
		s.helperError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"daemon": map[string]string{"version": version.Version},
		"host":   host,
	})
}

// metrics — текущая нагрузка для виджетов; ЦП и сеть — средние
// с прошлого запроса любого клиента (первый ответ — без них).
func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.Metrics == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "метрики не настроены")
		return
	}
	writeJSON(w, http.StatusOK, s.Metrics.Sample())
}

// recentEvents — события журнала, новые первыми (?limit=1..200, по
// умолчанию 10; фильтры: type — тип или префикс "disk.", severity —
// не ниже info|warning|critical, subject — "disk:<id>" или префикс
// "disk:", before — ID для следующей страницы).
func (s *server) recentEvents(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	f := events.Filter{Limit: 10, Type: qv.Get("type"), Severity: qv.Get("severity"), Subject: qv.Get("subject")}
	if v := qv.Get("limit"); v != "" {
		x, err := strconv.Atoi(v)
		if err != nil || x < 1 || x > 200 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit: 1..200")
			return
		}
		f.Limit = x
	}
	if v := qv.Get("before"); v != "" {
		x, err := strconv.ParseInt(v, 10, 64)
		if err != nil || x < 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "before: ID события")
			return
		}
		f.Before = x
	}
	if f.Severity != "" && !events.ValidSeverity(f.Severity) {
		writeError(w, http.StatusBadRequest, "invalid_request", "severity: info|warning|critical")
		return
	}
	if len(f.Type) > 64 || len(f.Subject) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_request", "type/subject: слишком длинно")
		return
	}
	if s.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}})
		return
	}
	evs, err := s.Events.Query(r.Context(), f)
	if err != nil {
		s.Logger.Error("последние события", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

// disks — снимок дисков helper: у диска с ID — last_event
// (последнее событие журнала), absent — известные журналу диски, которых
// нет в системе и последнее событие которых — пропажа или отказ.
func (s *server) disks(w http.ResponseWriter, r *http.Request) {
	var snap map[string]any
	if err := s.Helper.Call(r.Context(), "disks.observe", nil, &snap); err != nil {
		s.helperError(w, r, err)
		return
	}
	s.diskSmartSummary(r, snap)
	if s.Events == nil {
		writeJSON(w, http.StatusOK, snap)
		return
	}
	last, err := s.Events.LastPerSubject(r.Context(), "disk:")
	if err != nil {
		s.Logger.Error("события дисков", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "внутренняя ошибка")
		return
	}
	present := map[string]bool{}
	list, _ := snap["disks"].([]any)
	for _, x := range list {
		d, ok := x.(map[string]any)
		if !ok {
			continue
		}
		id, _ := d["id"].(string)
		if id == "" {
			continue
		}
		present[id] = true
		if e, ok := last["disk:"+id]; ok {
			d["last_event"] = e
		}
	}
	absent := []events.Event{}
	for subj, e := range last {
		id := strings.TrimPrefix(subj, "disk:")
		if !present[id] && (e.Type == events.DiskRemoved || e.Type == events.DiskFailed) {
			absent = append(absent, e)
		}
	}
	sort.Slice(absent, func(i, j int) bool { return absent[i].ID > absent[j].ID })
	snap["absent"] = absent
	writeJSON(w, http.StatusOK, snap)
}

// poolEvent — последнее событие пула (nil — нет или журнал недоступен).
func (s *server) poolEvent(r *http.Request, name string) *events.Event {
	if s.Events == nil {
		return nil
	}
	e, ok, err := s.Events.LastBySubject(r.Context(), "pool:"+name)
	if err != nil {
		s.Logger.Warn("событие пула", "pool", name, "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	return &e
}

type poolWithEvent struct {
	home.PoolView
	LastEvent *events.Event `json:"last_event,omitempty"`
}

// decodeBody — строгий разбор тела JSON не длиннее max байт.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	// Только application/json: «простые» межсайтовые запросы (формы, fetch
	// без заголовков) идут без него и отсекаются. Не замена CSRF.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errors.New("ожидается Content-Type: application/json")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		return fmt.Errorf("тело запроса: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("тело запроса: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("тело запроса: лишние данные после объекта")
	}
	return nil
}

const defaultTaskLimit, maxTaskLimit = 100, 1000

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	limit := defaultTaskLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxTaskLimit {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("limit должен быть 1..%d", maxTaskLimit))
			return
		}
		limit = n
	}
	list, err := s.Tasks.List(r.Context(), limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": list})
}

func (s *server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Tasks.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, tasks.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "задача не найдена")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// --- план хранилища ---

// PlanRequest — тело POST /api/v1/storage/plan.
type PlanRequest struct {
	Type               string     `json:"type"`
	Disks              []PlanDisk `json:"disks"`
	SystemReserveBytes int64      `json:"system_reserve_bytes"`
}

// PlanDisk — диск-кандидат.
type PlanDisk struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"size_bytes"`
}

// PlanResponse — расчёт пула и возможные переходы.
type PlanResponse struct {
	Type        string           `json:"type"`
	Arrays      []PlanArray      `json:"arrays"`
	UsableBytes int64            `json:"usable_bytes"`
	RawBytes    int64            `json:"raw_bytes"`
	UnusedBytes int64            `json:"unused_bytes"`
	Tolerance   int              `json:"tolerance"`
	Warnings    []string         `json:"warnings"`
	Transitions []PlanTransition `json:"transitions"`
}

// PlanArray — md-массив (или linear/single).
type PlanArray struct {
	Level          string   `json:"level"`
	Members        []string `json:"members"`
	SliceSizeBytes int64    `json:"slice_size_bytes"`
	UsableBytes    int64    `json:"usable_bytes"`
	Tolerance      int      `json:"tolerance"`
}

// PlanTransition — допустимый переход из рассчитанного пула.
type PlanTransition struct {
	Op       string `json:"op"`
	Target   string `json:"target"`
	AddDisks int    `json:"add_disks"`
	Note     string `json:"note"`
	Verified bool   `json:"verified"`
}

const maxPlanDisks = 64

func (s *server) plan(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if err := decodeBody(w, r, 64<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(req.Disks) > maxPlanDisks {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("не более %d дисков", maxPlanDisks))
		return
	}
	if req.SystemReserveBytes < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "system_reserve_bytes не может быть отрицательным")
		return
	}
	pr := planner.Request{Type: planner.PoolType(req.Type), SystemReserve: req.SystemReserveBytes}
	for i, d := range req.Disks {
		if d.ID == "" || len(d.ID) > 128 {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("disks[%d].id: 1..128 символов", i))
			return
		}
		if d.SizeBytes <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("disks[%d].size_bytes должен быть > 0", i))
			return
		}
		pr.Disks = append(pr.Disks, planner.Disk{ID: d.ID, Size: d.SizeBytes})
	}
	p, err := planner.Compute(pr)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "plan_rejected", err.Error())
		return
	}
	resp := PlanResponse{
		Type: string(p.Type), UsableBytes: p.Usable, RawBytes: p.Raw, UnusedBytes: p.Unused,
		Tolerance: p.Tolerance, Warnings: p.Warnings,
		Arrays: []PlanArray{}, Transitions: []PlanTransition{},
	}
	if resp.Warnings == nil {
		resp.Warnings = []string{}
	}
	for _, a := range p.Arrays {
		resp.Arrays = append(resp.Arrays, PlanArray{
			Level: a.Level, Members: a.Members, SliceSizeBytes: a.SliceSize,
			UsableBytes: a.Usable, Tolerance: a.Tolerance,
		})
	}
	for _, t := range planner.Transitions(p.Type, len(req.Disks)) {
		resp.Transitions = append(resp.Transitions, PlanTransition{
			Op: string(t.Op), Target: string(t.Target), AddDisks: t.AddDisks, Note: t.Note, Verified: t.Verified,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- пулы Home ---

func (s *server) storageError(w http.ResponseWriter, r *http.Request, err error) {
	var re *helper.RemoteError
	switch {
	case errors.Is(err, home.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, home.ErrPlanNotFound):
		writeError(w, http.StatusNotFound, "plan_not_found", err.Error())
	case errors.Is(err, home.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, home.ErrPlanExpired):
		writeError(w, http.StatusGone, "plan_expired", err.Error())
	case errors.Is(err, home.ErrBadToken):
		writeError(w, http.StatusForbidden, "bad_token", err.Error())
	case errors.Is(err, home.ErrPlanStale):
		writeError(w, http.StatusConflict, "plan_stale", err.Error())
	case errors.Is(err, home.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, home.ErrUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "unsupported", err.Error())
	case errors.Is(err, home.ErrTooMany):
		writeError(w, http.StatusTooManyRequests, "too_many_plans", err.Error())
	case errors.Is(err, helper.ErrUnavailable), errors.As(err, &re):
		s.helperError(w, r, err)
	default:
		s.internalError(w, r, err)
	}
}

func (s *server) noStorage(w http.ResponseWriter) bool {
	if s.Storage == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "управление пулами не настроено")
		return true
	}
	return false
}

func (s *server) poolPlan(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req home.PlanRequest
	if err := decodeBody(w, r, 64<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	p, err := s.Storage.Plan(r.Context(), req)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ConfirmRequest — тело подтверждения плана.
type ConfirmRequest struct {
	ConfirmToken string `json:"confirm_token"`
}

func (s *server) poolConfirm(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req ConfirmRequest
	if err := decodeBody(w, r, 4<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ConfirmToken == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "confirm_token обязателен")
		return
	}
	t, err := s.Storage.Confirm(r.Context(), r.PathValue("id"), req.ConfirmToken)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t})
}

func (s *server) listPools(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	list, err := s.Storage.List(r.Context())
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	out := make([]poolWithEvent, 0, len(list))
	for _, v := range list {
		out = append(out, poolWithEvent{v, s.poolEvent(r, v.Name)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

func (s *server) getPool(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	v, err := s.Storage.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, poolWithEvent{v, s.poolEvent(r, v.Name)})
}

// actor — автор действия для журналов: имя пользователя сессии; при
// отключённой аутентификации — "auth-disabled".
func actor(r *http.Request) string {
	if u, ok := auth.UserFrom(r.Context()); ok {
		return u.Name
	}
	return "auth-disabled"
}

// DeletePoolRequest — подтверждение удаления пула: имя пула полностью.
type DeletePoolRequest struct {
	ConfirmName string `json:"confirm_name"`
}

func (s *server) deletePool(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req DeletePoolRequest
	if err := decodeBody(w, r, 4<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	t, err := s.Storage.Delete(r.Context(), r.PathValue("name"), req.ConfirmName)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	s.Logger.Warn("удаление пула", "pool", r.PathValue("name"), "actor", actor(r), "task", t.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t})
}

// resumePool — «продолжить замену»: подхватить
// прерванную операцию с диском без перезапуска gamayund. Тело не нужно.
func (s *server) resumePool(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	t, err := s.Storage.Resume(r.Context(), r.PathValue("name"))
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	s.Logger.Info("продолжение замены", "pool", r.PathValue("name"), "actor", actor(r), "task", t.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t})
}

// foundPools — пулы GamayunNas на дисках, которых нет в базе.
func (s *server) foundPools(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	ps, err := s.Storage.Found(r.Context())
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": ps})
}

// importPool — подхват найденного пула (задача). Тело {"confirm_name":
// "<имя>", "degraded": false}; degraded — подтверждение неполного пула.
func (s *server) importPool(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
		Degraded    bool   `json:"degraded"`
	}
	if err := decodeBody(w, r, 4<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := r.PathValue("name")
	if req.ConfirmName != name {
		writeError(w, http.StatusBadRequest, "invalid_request", "confirm_name должен совпадать с именем пула")
		return
	}
	t, err := s.Storage.Import(r.Context(), name, req.Degraded)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	s.Logger.Warn("подхват пула с дисков", "pool", name, "degraded", req.Degraded, "actor", actor(r), "task", t.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t})
}

// releaseDisk — «освободить диск» от остатков несуществующего пула;
// тело {"confirm_id": "<id диска>"} — подтверждение стирания.
func (s *server) releaseDisk(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req struct {
		ConfirmID string `json:"confirm_id"`
	}
	if err := decodeBody(w, r, 4<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	id := r.PathValue("id")
	if req.ConfirmID != id {
		writeError(w, http.StatusBadRequest, "invalid_request", "confirm_id должен совпадать с идентификатором диска")
		return
	}
	res, err := s.Storage.ReleaseDisk(r.Context(), id)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	s.Logger.Warn("диск освобождён", "disk", id, "actor", actor(r))
	writeJSON(w, http.StatusOK, res)
}

// ProtectionRequest — включение/отключение защиты read-only.
type ProtectionRequest struct {
	Enabled *bool  `json:"enabled"`
	Reason  string `json:"reason"`
}

func (s *server) setProtection(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req ProtectionRequest
	if err := decodeBody(w, r, 4<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "enabled обязателен")
		return
	}
	p, err := s.Storage.SetProtectionEnabled(r.Context(), r.PathValue("name"), *req.Enabled, actor(r), req.Reason)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) protectionLog(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.Storage.ProtectionLog(r.Context(), r.PathValue("name"), limit)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": list})
}

// OverrideRequest — временное ручное разрешение записи.
type OverrideRequest struct {
	Reason          string `json:"reason"`
	DurationMinutes int    `json:"duration_minutes"`
}

func (s *server) setOverride(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req OverrideRequest
	if err := decodeBody(w, r, 4<<10, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	p, err := s.Storage.SetWriteOverride(r.Context(), r.PathValue("name"), actor(r), req.Reason,
		time.Duration(req.DurationMinutes)*time.Minute)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	s.Logger.Warn("ручное разрешение записи", "pool", r.PathValue("name"), "actor", actor(r), "minutes", req.DurationMinutes, "reason", req.Reason)
	writeJSON(w, http.StatusOK, p)
}

func (s *server) clearOverride(w http.ResponseWriter, r *http.Request) {
	if s.noStorage(w) {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, 4<<10, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	p, err := s.Storage.ClearWriteOverride(r.Context(), r.PathValue("name"), actor(r), req.Reason)
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
