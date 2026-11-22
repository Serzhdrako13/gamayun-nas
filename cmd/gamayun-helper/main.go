// Команда gamayun-helper — привилегированный исполнитель (root). Слушает
// Unix-сокет и выполняет только операции фиксированного реестра
// (internal/helper.DefaultRegistry) для одного разрешённого uid — gamayund.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/storage/home"
	"gamayun-nas/internal/version"
)

func main() {
	var (
		socket    = flag.String("socket", "/run/gamayun-helper/helper.sock", "путь Unix-сокета")
		group     = flag.String("group", "", "группа сокета (имя или gid); пусто — не менять")
		allowUser = flag.String("allow-user", "", "пользователь gamayund (имя или uid), единственный допущенный клиент")
		confPath  = flag.String("config", "", "файл конфигурации JSON (destructive_allow_ids, mount_root); пусто — без ограничения списком дисков")
		showVer   = flag.Bool("version", false, "показать версию и выйти")
		openInteg = flag.String("open-integrity", "", "открыть dm-integrity пулов по файлу gamayund и выйти (ранний юнит загрузки)")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version.Version)
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(err error) {
		logger.Error("gamayun-helper", "err", err)
		os.Exit(2)
	}

	if *openInteg != "" {
		cfg, err := helper.LoadConfig(*confPath)
		if err != nil {
			fail(err)
		}
		e := helper.DefaultDeps(cfg).Home
		err = home.OpenIntegrityFile(context.Background(), e, *openInteg, func(pool string, r home.AssembleResult, err error) {
			if err != nil {
				logger.Error("dm-integrity пула не открыт", "pool", pool, "err", err, "commands", r.Commands)
				return
			}
			logger.Info("dm-integrity пула открыт", "pool", pool, "commands", r.Commands, "missing_disks", r.MissingDisks, "failed", r.Failed)
		})
		if err != nil {
			fail(err)
		}
		return
	}
	uid, err := resolveUser(*allowUser)
	if err != nil {
		fail(err)
	}
	gid := -1
	if *group != "" {
		if gid, err = resolveGroup(*group); err != nil {
			fail(err)
		}
	}
	if os.Geteuid() != 0 {
		logger.Warn("gamayun-helper запущен не от root: часть сведений может быть недоступна")
	}

	cfg := helper.DefaultConfig()
	if *confPath != "" {
		if cfg, err = helper.LoadConfig(*confPath); err != nil {
			fail(err)
		}
	}
	if len(cfg.DestructiveAllowIDs) == 0 {
		logger.Info("destructive_allow_ids пуст: список дисков не ограничен, защита — проверками перед записью")
	} else {
		logger.Warn("разрешены разрушающие операции с дисками", "ids", cfg.DestructiveAllowIDs)
	}

	ln, err := helper.Listen(*socket, gid)
	if err != nil {
		fail(err)
	}
	reg := helper.DefaultRegistry(helper.DefaultDeps(cfg))
	srv := &helper.Server{Registry: reg, AllowedUID: uid, Logger: logger}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("gamayun-helper запущен", "socket", *socket, "allow_uid", uid, "gid", gid, "ops", reg.Names())
	if err := srv.Serve(ctx, ln); err != nil {
		logger.Error("gamayun-helper", "err", err)
		os.Exit(1)
	}
	logger.Info("gamayun-helper остановлен")
}

// resolveUser: имя или uid. root (0) запрещён: gamayund не должен работать от root.
func resolveUser(s string) (int, error) {
	if s == "" {
		return 0, errors.New("не задан -allow-user")
	}
	id, err := strconv.Atoi(s)
	if err != nil {
		u, lerr := user.Lookup(s)
		if lerr != nil {
			return 0, fmt.Errorf("-allow-user %q: %w", s, lerr)
		}
		if id, err = strconv.Atoi(u.Uid); err != nil {
			return 0, err
		}
	}
	if id <= 0 {
		return 0, fmt.Errorf("-allow-user: uid %d не допускается (gamayund работает без root)", id)
	}
	return id, nil
}

func resolveGroup(s string) (int, error) {
	if id, err := strconv.Atoi(s); err == nil {
		if id < 0 {
			return 0, fmt.Errorf("-group: неверный gid %d", id)
		}
		return id, nil
	}
	g, err := user.LookupGroup(s)
	if err != nil {
		return 0, fmt.Errorf("-group %q: %w", s, err)
	}
	return strconv.Atoi(g.Gid)
}
