package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gamayun-nas/internal/proxy"
)

// serveAuthSocket — unix-сокет forward_auth для Caddy. Только
// проверка входа в приложения (proxy.VerifyPath), не API: сетевого доступа
// к нему нет, права 0660 (группа gamayun; служба gamayun-caddy входит в неё).
func serveAuthSocket(ctx context.Context, path string, h http.Handler, logger *slog.Logger) (func(), error) {
	if path == "" {
		path = proxy.DefaultAuthSocket
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path) // остаток прошлого запуска
	} else if err == nil {
		return nil, errors.New(path + ": существует и не является сокетом")
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+proxy.VerifyPath, h)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn)}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("прокси: сокет входа в приложения", "err", err)
		}
	}()
	logger.Info("прокси: сокет входа в приложения", "path", path)
	return func() { srv.Close(); os.Remove(path) }, nil
}
