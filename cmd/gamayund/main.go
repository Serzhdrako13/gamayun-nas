// Команда gamayund — демон управления gamayun-nas (без root): HTTP API, задачи,
// события. Привилегированные операции выполняет gamayun-helper через Unix-сокет.
//
// По умолчанию слушает 127.0.0.1 по HTTPS (самоподписанный сертификат) и
// требует входа администратора. Первый администратор:
// gamayund admin create -name <имя> или токен установки (см. README).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gamayun-nas/internal/config"
	"gamayun-nas/internal/daemon"
	"gamayun-nas/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		os.Exit(runAdmin(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "config" {
		os.Exit(runConfig(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	var (
		confPath  = flag.String("config", "", "файл конфигурации JSON (пусто — значения по умолчанию)")
		listen    = flag.String("listen", "", "адрес HTTP API, переопределяет listen")
		dbPath    = flag.String("db", "", "файл SQLite, переопределяет database")
		socket    = flag.String("helper-socket", "", "сокет gamayun-helper, переопределяет helper_socket")
		demoSleep = flag.Duration("demo-sleep", 0, "только для разработки: поставить задачу demo.sleep указанной длительности")
		showVer   = flag.Bool("version", false, "показать версию и выйти")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version.Version)
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := config.Default()
	if *confPath != "" {
		var err error
		if cfg, err = config.Load(*confPath); err != nil {
			logger.Error("gamayund", "err", err)
			os.Exit(2)
		}
	}
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "listen":
			cfg.Listen = *listen
		case "db":
			cfg.Database = *dbPath
		case "helper-socket":
			cfg.HelperSocket = *socket
		}
	})
	if *demoSleep < 0 || *demoSleep > 10*time.Minute {
		logger.Error("gamayund: -demo-sleep должен быть 0..10m")
		os.Exit(2)
	}
	if os.Geteuid() == 0 {
		logger.Warn("gamayund запущен от root; должен работать от отдельного пользователя (gamayun)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, cfg, daemon.Options{Logger: logger, DemoSleep: *demoSleep}); err != nil {
		logger.Error("gamayund", "err", err)
		os.Exit(1)
	}
}
