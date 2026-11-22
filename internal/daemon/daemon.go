// Package daemon собирает gamayund: база состояния, журнал событий, менеджер
// задач, клиент gamayun-helper и HTTP API. Работает без root.
package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"gamayun-nas/internal/catalog"
	"gamayun-nas/internal/confbackup"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gamayun-nas/internal/api"
	"gamayun-nas/internal/apps"
	"gamayun-nas/internal/auth"
	"gamayun-nas/internal/collector"
	"gamayun-nas/internal/config"
	"gamayun-nas/internal/disks"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/files"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/maintenance"
	"gamayun-nas/internal/metrics"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/notify"
	"gamayun-nas/internal/proxy"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/store"
	"gamayun-nas/internal/tasks"
	"gamayun-nas/internal/tlscert"
	"gamayun-nas/internal/web"
)

// SetupTokenName — файл одноразового токена установки в каталоге данных.
const SetupTokenName = "setup-token"

// Options — параметры запуска сверх конфигурации.
type Options struct {
	Logger *slog.Logger
	// Listener — готовый слушатель (тесты); nil — net.Listen по cfg.Listen.
	Listener net.Listener
	// DemoSleep > 0 — зарегистрировать demo.sleep и поставить одну такую
	// задачу при старте (только для ручной проверки задач и SSE).
	DemoSleep time.Duration
	// Ready вызывается с адресом, когда API начал принимать соединения.
	Ready func(addr net.Addr)
}

// NotificationsKeep — предел числа уведомлений в центре (страховка сверх
// срока хранения).
const NotificationsKeep = 5000

// EventsKeep — предел числа событий в журнале (страховка сверх срока
// хранения).
const EventsKeep = 10000

// RetentionInterval — период обрезки журнала и центра уведомлений.
const RetentionInterval = 24 * time.Hour

// Run запускает демон и блокируется до отмены ctx, затем корректно
// останавливается: закрывает HTTP (в т.ч. SSE), прерывает задачи
// (они становятся interrupted), закрывает базу.
// ProtectionInterval — период проверки запаса избыточности пулов.
const ProtectionInterval = 30 * time.Second

// CollectorInterval — период сверки коллектора событий.
const CollectorInterval = 60 * time.Second

func Run(ctx context.Context, cfg config.Config, opt Options) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}

	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	evlog := events.NewLog(db)

	notifyDel := notify.NewDeliverer(db, notify.DeliverOptions{})
	notifySvc := notify.New(db, evlog, notify.Options{Channels: notifyDel.Channels, OnQueued: notifyDel.Kick, Logger: logger})

	mgr := tasks.New(db, evlog, tasks.Options{Logger: logger})
	if opt.DemoSleep > 0 {
		if err := mgr.Register(tasks.DemoSleepKind, tasks.DemoSleep); err != nil {
			return err
		}
	}
	// Пулы Home: изменяющие операции helper (разметка, dm-integrity format,
	// mdadm, mkfs) могут идти минуты — отдельный клиент с большим таймаутом.
	storage, err := home.NewService(&helper.Client{Socket: cfg.HelperSocket, Timeout: 35 * time.Minute}, db, mgr)
	if err != nil {
		return err
	}
	nasSvc := nas.NewService(&helper.Client{Socket: cfg.HelperSocket, Timeout: 2 * time.Minute}, db)
	storage.Shares = nasSvc
	// Состав пулов для раннего юнита gamayun-integrity.service.
	storage.BootFile = filepath.Join(filepath.Dir(cfg.Database), home.BootFileName)
	// Участника не вернуть ни re-add, ни add — событие администратору
	// (одно на пул, пока участник не вернётся: проход защиты — раз в 30 с).
	var readdMu sync.Mutex
	readdWarned := map[string]bool{}
	storage.OnReassemble = func(pool string, r home.AssembleResult, err error) {
		logger.Warn("повторная сборка деградированного пула", "pool", pool, "err", err, "readded", r.Readded, "full_recovery", r.FullRecovery, "readd_failed", r.ReaddFailed, "commands", r.Commands)
		readdMu.Lock()
		defer readdMu.Unlock()
		if len(r.ReaddFailed) == 0 {
			if len(r.Readded) > 0 {
				delete(readdWarned, pool)
			}
			return
		}
		if readdWarned[pool] {
			return
		}
		readdWarned[pool] = true
		data, _ := json.Marshal(map[string]any{"pool": pool, "members": r.ReaddFailed})
		if _, err := evlog.Emit(ctx, events.Event{Type: events.ArrayReaddFailed, Severity: events.SevWarning, Subject: "pool:" + pool, Data: data}, true); err != nil {
			logger.Warn("событие не записано", "type", events.ArrayReaddFailed, "err", err)
		}
	}
	storage.OnWatch = func(name, observation string) {
		logger.Info("наблюдение за процессом", "process", name, "state", observation)
	}
	storage.Unavailable = func(err error) bool { return errors.Is(err, helper.ErrUnavailable) }
	// Recover — до AssembleAll: сборка может поставить задачу роста после
	// прерванного reshape, её нельзя пометить interrupted.
	if n, err := mgr.Recover(ctx); err != nil {
		return fmt.Errorf("восстановление задач: %w", err)
	} else if n > 0 {
		logger.Warn("незавершённые задачи прошлого запуска помечены interrupted", "count", n)
	}
	// Сборка пулов после загрузки (dm-integrity в режиме пула → md →
	// LVM → Btrfs) — до применения конфигурации Samba и защиты.
	// Идемпотентна: при обычном перезапуске gamayund ничего не меняет.
	if err := storage.AssembleAll(ctx, func(pool string, r home.AssembleResult, err error) {
		if err != nil {
			logger.Error("пул не собран", "pool", pool, "err", err, "commands", r.Commands)
			return
		}
		logger.Info("пул собран", "pool", pool, "mount", r.Mount, "commands", r.Commands, "missing_disks", r.MissingDisks, "failed", r.Failed)
	}); err != nil {
		logger.Warn("сборка пулов при старте завершилась с ошибками", "err", err)
	}
	if err := nasSvc.Resync(ctx); err != nil {
		logger.Warn("общие папки: не удалось применить конфигурацию Samba при старте", "err", err)
	}

	// Каталог данных — каталог базы: там же самоподписанный сертификат и
	// токен установки.
	stateDir := filepath.Dir(cfg.Database)
	// Проверка пулов и SMART по расписанию.
	maint := maintenance.New(db, &helper.Client{Socket: cfg.HelperSocket, Timeout: 2 * time.Minute}, storage, evlog,
		maintenance.Options{Logger: logger})
	storage.OnScrub = maint.OnScrub
	// Резервная копия настроек (выгрузка, автокопии в пулы, USB).
	confSvc := &confbackup.Service{DB: db, Helper: &helper.Client{Socket: cfg.HelperSocket, Timeout: 4 * time.Minute},
		Events: evlog, Logger: logger, SFTPKeyFile: filepath.Join(stateDir, confbackup.SFTPKeyName),
		Identity: func(ctx context.Context) (*confbackup.Identity, error) {
			id, err := nasSvc.Identity(ctx)
			if err != nil {
				return nil, err
			}
			return &confbackup.Identity{Timezone: id.Timezone, NTP: id.NTP}, nil
		}}
	// Снапшоты общих папок и домашних папок по расписанию.
	snapSvc, err := newSnapshots(cfg.HelperSocket, db, evlog, logger, mgr, nasSvc)
	if err != nil {
		return err
	}
	// Веб-файловый менеджер.
	filesSvc := &files.Service{
		Helper: &helper.Client{Socket: cfg.HelperSocket, Timeout: time.Minute},
		Long:   &helper.Client{Socket: cfg.HelperSocket, Timeout: 31 * time.Minute},
		Roots:  nasSvc, Tasks: mgr, Logger: logger,
	}
	if err := filesSvc.Init(); err != nil {
		return err
	}
	// Приложения Docker (пакет gamayun-nas-apps); без пакета —
	// состояние «не установлено», NAS работает как обычно.
	appsSvc := &apps.Service{DB: db, Events: evlog, Tasks: mgr, Pools: storage, Logger: logger, Shares: nasSvc,
		Helper:       &helper.Client{Socket: cfg.HelperSocket, Timeout: 20 * time.Minute},
		EventsHelper: &helper.Client{Socket: cfg.HelperSocket, Timeout: 2 * time.Minute}}
	if err := appsSvc.Init(); err != nil {
		return err
	}
	storage.BeforeReadOnly = appsSvc.BeforeReadOnly
	storage.InUse = appsSvc.PoolInUse
	// Публикация приложений через Caddy (служба gamayun-caddy пакета gamayun-nas-apps).
	_, portalPort, _ := net.SplitHostPort(cfg.Listen)
	proxySvc := proxy.New(proxy.Options{Logger: logger, AdminSocket: cfg.Proxy.AdminSocket,
		AuthSocket: cfg.Proxy.AuthSocket, PortalPort: portalPort, Routes: appsSvc.Routes})
	appsSvc.OnRoutes, appsSvc.WebName = proxySvc.Trigger, proxySvc.CheckName
	// Каталог приложений — встроенные ключи + catalog.keys_dir.
	catKeys, err := catalog.BuiltinKeys()
	if err != nil {
		return err
	}
	if cfg.Catalog.KeysDir != "" {
		extra, err := catalog.LoadKeys(cfg.Catalog.KeysDir)
		if err != nil {
			return err
		}
		catKeys = append(catKeys, extra...)
	}
	catDir := cfg.Catalog.Dir
	if catDir == "" {
		catDir = filepath.Join(filepath.Dir(cfg.Database), "catalog")
	}
	catStore := &catalog.Store{DB: db, Dir: catDir, Trusted: catKeys, AllowHTTP: cfg.Catalog.AllowHTTP}
	appsSvc.Catalog = catStore
	apiOpts := api.Options{
		Catalog:     catStore,
		CatalogURL:  cfg.Catalog.URL,
		Apps:        appsSvc,
		Proxy:       proxySvc,
		Files:       filesSvc,
		Snapshots:   snapSvc,
		Config:      confSvc,
		Helper:      &helper.Client{Socket: cfg.HelperSocket},
		Tasks:       mgr,
		Events:      evlog,
		Metrics:     &metrics.Sampler{},
		DB:          db,
		Storage:     storage,
		NAS:         nasSvc,
		Notify:      notifySvc,
		Deliver:     notifyDel,
		Logger:      logger,
		Maintenance: maint,
	}
	var appAuth http.Handler // forward_auth Caddy (nil — аутентификация отключена)
	if cfg.Auth.Disable {
		logger.Warn("аутентификация ОТКЛЮЧЕНА (auth.disable): API доступен без входа")
	} else {
		as := auth.New(db, auth.Options{
			Logger: logger, Events: evlog,
			IdleTimeout:        time.Duration(cfg.Auth.SessionIdle),
			AbsoluteTimeout:    time.Duration(cfg.Auth.SessionAbsolute),
			SetupTokenFile:     filepath.Join(stateDir, SetupTokenName),
			OnPassword:         nasSvc.SetSambaPassword,
			OnPasswordMismatch: nasSvc.MarkPasswordMismatch,
		})
		nasSvc.Web = auth.Accounts{S: as}
		if err := as.Init(ctx); err != nil {
			return fmt.Errorf("аутентификация: %w", err)
		}
		apiOpts.Auth, apiOpts.AuthRoutes, apiOpts.SessionCheck = as.Middleware, as.Routes, as.StillValid
		as.SetAppGate(auth.AppGateOptions{Allowed: proxySvc.Allowed, Portal: proxySvc.Portal})
		appAuth = as.AppVerify()
	}
	// Учётки ОС, SSH и sudo по модели NAS (идемпотентно).
	if changed, err := nasSvc.MigrateOS(ctx); err != nil {
		logger.Warn("учётки ОС: приведение к модели при старте не выполнено", "err", err, "changed", changed)
	} else if len(changed) > 0 {
		logger.Info("учётки ОС приведены к модели", "changed", changed)
	}
	apiOpts.UI = web.Handler()

	var tlsConf *tls.Config
	if cfg.TLS.Disable {
		logger.Warn("TLS отключён (tls.disable): API по обычному HTTP")
	} else {
		var st *tlscert.Store
		if st, tlsConf, err = loadTLS(cfg, stateDir, logger); err != nil {
			return err
		}
		apiOpts.TLS = st
	}

	handler := api.New(apiOpts)

	ln := opt.Listener
	if ln == nil {
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			return err
		}
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if !config.IsLoopback(host) && (cfg.TLS.Disable || cfg.Auth.Disable) {
		logger.Warn("API доступен не только с этой машины без TLS или без аутентификации (insecure_allow_remote)",
			"addr", ln.Addr().String())
	}

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	// События дисков и пулов — сверка снимков (при старте, по таймеру).
	diskHelper := &helper.Client{Socket: cfg.HelperSocket, Timeout: time.Minute}
	coll := collector.New(db, evlog, func(ctx context.Context) collector.Observation {
		var o collector.Observation
		var snap disks.Snapshot
		if err := diskHelper.Call(ctx, "disks.observe", nil, &snap); err != nil {
			logger.Warn("коллектор: диски не прочитаны", "err", err)
		} else {
			o.Disks = &snap
		}
		o.Pools, o.PoolsErr = storage.List(ctx)
		if o.PoolsErr != nil {
			logger.Warn("коллектор: пулы не прочитаны", "err", o.PoolsErr)
		}
		return o
	})
	coll.OnEvent = func(e events.Event) {
		lvl := slog.LevelInfo
		if e.Severity != events.SevInfo {
			lvl = slog.LevelWarn
		}
		logger.Log(baseCtx, lvl, "событие", "type", e.Type, "severity", e.Severity, "subject", e.Subject, "data", string(e.Data))
	}
	storage.OnMdstatChange = coll.Trigger // отказ участника md не даёт uevent
	// Защита read-only при исчерпании запаса избыточности.
	go storage.RunProtection(baseCtx, ProtectionInterval, func(err error) {
		logger.Warn("защита пулов (read-only)", "err", err)
	})
	if err := collector.ListenUevents(baseCtx, coll.Trigger); err != nil {
		logger.Warn("коллектор: uevent недоступны, только сверка по таймеру", "err", err)
	}
	go coll.Run(baseCtx, CollectorInterval, func(err error) {
		logger.Warn("коллектор событий", "err", err)
	})
	// Центр уведомлений и доставка по внешним каналам (каналы
	// включаются настройками — шаги 3–5).
	// Обрезка журнала и центра по сроку и числу записей —
	// при старте и раз в сутки.
	pruneJournals(ctx, db, evlog, notifySvc, logger, time.Now())
	go runRetention(baseCtx, RetentionInterval, func() {
		pruneJournals(baseCtx, db, evlog, notifySvc, logger, time.Now())
	})
	if c, err := notify.LoadSMTP(ctx, db); err != nil {
		logger.Warn("уведомления: настройки SMTP не прочитаны", "err", err)
	} else {
		notifyDel.ApplySMTP(c)
	}
	if c, err := notify.LoadWebhook(ctx, db); err != nil {
		logger.Warn("уведомления: настройки webhook не прочитаны", "err", err)
	} else {
		notifyDel.ApplyWebhook(c)
	}
	go maint.Run(baseCtx)
	go confSvc.Run(baseCtx)
	go snapSvc.Run(baseCtx)
	go appsSvc.Run(baseCtx)
	go proxySvc.Run(baseCtx)
	if appAuth != nil {
		if stop, err := serveAuthSocket(baseCtx, cfg.Proxy.AuthSocket, appAuth, logger); err != nil {
			logger.Warn("прокси: сокет входа в приложения не открыт — вход через прокси недоступен", "err", err)
		} else {
			defer stop()
		}
	} else {
		logger.Warn("прокси: аутентификация отключена — вход в приложения через прокси недоступен")
	}
	go notifySvc.Run(baseCtx)
	go notifyDel.Run(baseCtx, func(err error) {
		logger.Warn("доставка уведомлений", "err", err)
	})
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // SSE снимает его для своего потока
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	// Shutdown ждёт активные запросы; SSE-потоки завершаются отменой baseCtx.
	srv.RegisterOnShutdown(cancelBase)

	serveErr := make(chan error, 2)
	scheme := "http"
	if tlsConf != nil {
		scheme = "https"
		srv.TLSConfig = tlsConf
		go func() { serveErr <- srv.ServeTLS(ln, "", "") }()
	} else {
		go func() { serveErr <- srv.Serve(ln) }()
	}
	var redir *http.Server
	if cfg.TLS.RedirectListen != "" && tlsConf != nil {
		rln, err := net.Listen("tcp", cfg.TLS.RedirectListen)
		if err != nil {
			srv.Close()
			return fmt.Errorf("tls.redirect_listen: %w", err)
		}
		redir = &http.Server{
			Handler:           RedirectHandler(port),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		go func() { serveErr <- redir.Serve(rln) }()
		logger.Info("перенаправление HTTP → HTTPS", "addr", rln.Addr().String())
	}
	logger.Info("gamayund запущен", "addr", ln.Addr().String(), "scheme", scheme, "db", cfg.Database, "helper", cfg.HelperSocket)
	if opt.Ready != nil {
		opt.Ready(ln.Addr())
	}

	if opt.DemoSleep > 0 {
		params, _ := json.Marshal(tasks.DemoSleepParams{DurationMS: int(opt.DemoSleep / time.Millisecond)})
		if t, err := mgr.Submit(ctx, tasks.DemoSleepKind, params); err != nil {
			logger.Error("demo.sleep", "err", err)
		} else {
			logger.Info("поставлена тестовая задача demo.sleep", "task", t.ID)
		}
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErr:
	}

	logger.Info("gamayund останавливается")
	// До отмены задач — новые изменяющие действия не начинаются,
	// откат прерванного создания выполнит следующий запуск (один владелец пула).
	storage.Stop()
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if redir != nil {
		redir.Shutdown(shCtx)
	}
	if err := srv.Shutdown(shCtx); err != nil {
		logger.Error("остановка HTTP", "err", err)
	}
	if err := mgr.Shutdown(shCtx); err != nil {
		logger.Error("остановка задач", "err", err)
	}
	if errors.Is(runErr, http.ErrServerClosed) {
		runErr = nil
	}
	return runErr
}

// loadTLS — сертификат по приоритету: из конфигурации, загруженный через
// веб, самоподписанный в <каталог данных>/tls. Отпечаток пишется
// в журнал для сверки.
func loadTLS(cfg config.Config, stateDir string, logger *slog.Logger) (*tlscert.Store, *tls.Config, error) {
	st, warn, err := tlscert.NewStore(filepath.Join(stateDir, "tls"), cfg.TLS.CertFile, cfg.TLS.KeyFile, tlscert.LocalNames, time.Now)
	if warn != nil {
		logger.Warn("TLS", "err", warn)
	}
	if err != nil {
		return nil, nil, err
	}
	res, src := st.Current()
	if res.Generated {
		logger.Warn("создан новый самоподписанный сертификат: браузер покажет предупреждение, сверьте отпечаток",
			"fingerprint_sha256", res.Fingerprint)
	}
	logger.Info("TLS", "source", src, "fingerprint_sha256", res.Fingerprint, "subject", res.Leaf.Subject.CommonName,
		"dns", res.Leaf.DNSNames, "ip", res.Leaf.IPAddresses, "not_after", res.Leaf.NotAfter.Format(time.RFC3339))
	if left := time.Until(res.Leaf.NotAfter); left < tlscert.RenewBefore {
		logger.Warn("сертификат истекает или истёк, замените его", "not_after", res.Leaf.NotAfter.Format(time.RFC3339))
	}
	return st, tlscert.ServerConfig(st.GetCertificate), nil
}

// RedirectHandler перенаправляет HTTP-запросы на HTTPS-порт httpsPort
// (308 сохраняет метод и тело). Хост берётся из запроса без порта.
func RedirectHandler(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.TrimSuffix(strings.TrimPrefix(r.Host, "["), "]")
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if host == "" {
			http.Error(w, "нет Host", http.StatusBadRequest)
			return
		}
		target := "https://" + net.JoinHostPort(host, httpsPort)
		if httpsPort == "443" {
			target = strings.TrimSuffix(target, ":443")
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}
