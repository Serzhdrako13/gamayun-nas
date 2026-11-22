package helper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Cred — учётные данные процесса-клиента.
type Cred struct{ PID, UID, GID int }

// PeerCredFunc получает учётные данные клиента; в тестах подменяется.
type PeerCredFunc func(*net.UnixConn) (Cred, error)

// Server — приём соединений helper.
type Server struct {
	Registry   *Registry
	AllowedUID int          // uid gamayund; любой другой отклоняется
	PeerCred   PeerCredFunc // nil — SOPeerCred
	Logger     *slog.Logger
	// IdleTimeout — сколько ждать следующий запрос в соединении; 0 — 30 с.
	IdleTimeout time.Duration
	// MaxConns — одновременных соединений; 0 — 16. Лишние закрываются сразу.
	MaxConns int
}

// Listen создаёт Unix-сокет path с правами 0660 и группой gid (<0 — не
// менять). Устаревший сокет на месте path удаляется; любой другой файл —
// ошибка (не удаляем то, что не создавали).
func Listen(path string, gid int) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s существует и не является сокетом", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// До chmod/chown сокет может быть доступен по umask; это не даёт
	// доступа: каждое соединение проверяется по SO_PEERCRED. Каталог сокета
	// (RuntimeDirectory, 0750) дополнительно ограничивает доступ.
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			ln.Close()
			return nil, fmt.Errorf("chown %s: %w", path, err)
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return ln, nil
}

// Serve принимает соединения до отмены ctx, затем закрывает ln и ждёт
// завершения обработчиков.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	peer := s.PeerCred
	if peer == nil {
		peer = SOPeerCred
	}
	idle := s.IdleTimeout
	if idle <= 0 {
		idle = 30 * time.Second
	}
	maxConns := s.MaxConns
	if maxConns <= 0 {
		maxConns = 16
	}
	slots := make(chan struct{}, maxConns)

	var wg sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			logger.Warn("helper: превышен лимит соединений")
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.handle(ctx, conn, peer, idle, logger)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn, peer PeerCredFunc, idle time.Duration, logger *slog.Logger) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	write := func(r Response) bool {
		r.V = ProtocolVersion
		if r.file != nil {
			// Строка и дескриптор одним сообщением.
			uc, ok := conn.(*net.UnixConn)
			if !ok {
				r.file.Close()
				return false
			}
			line, err := json.Marshal(r)
			if err != nil {
				r.file.Close()
				return false
			}
			return sendWithFile(uc, append(line, '\n'), r.file) == nil
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return enc.Encode(r) == nil
	}

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		write(errResponse(CodeForbidden, "только Unix-сокет"))
		return
	}
	cred, err := peer(uc)
	if err != nil || cred.UID != s.AllowedUID {
		logger.Warn("helper: отклонено соединение", "uid", cred.UID, "pid", cred.PID, "err", err)
		write(errResponse(CodeForbidden, "клиент не допущен"))
		return
	}

	r := bufio.NewReaderSize(conn, 4096)
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		// Проверка после установки срока: если остановка уже сработала
		// (AfterFunc выставил срок «сейчас»), строкой выше он перезаписан.
		if ctx.Err() != nil {
			return
		}
		line, err := readLine(r, MaxRequestBytes)
		if errors.Is(err, errLineTooLong) {
			write(errResponse(CodeTooLarge, fmt.Sprintf("запрос длиннее %d байт", MaxRequestBytes)))
			return
		}
		if err != nil {
			return // EOF, таймаут простоя, остановка
		}
		resp, op := s.process(ctx, line)
		if s.Registry.IsMutating(op) {
			// Изменяющие операции — с параметрами и итогом (журнал аудита).
			msg := ""
			if resp.Error != nil {
				msg = resp.Error.Message
			}
			logger.Warn("helper: изменяющая операция", "op", op, "ok", resp.OK, "uid", cred.UID, "pid", cred.PID,
				"code", errCode(resp), "error", msg, "request", auditRequest(line))
		} else {
			logger.Info("helper: запрос", "op", op, "ok", resp.OK, "uid", cred.UID, "pid", cred.PID, "code", errCode(resp))
		}
		if !write(resp) || ctx.Err() != nil {
			return
		}
	}
}

// secretKey — поля, значения которых не пишутся в журнал аудита:
// пароли, токены, ключи. Сравнение по имени поля на любой глубине params,
// без учёта регистра; совпадение по подстроке ловит new_password и т. п.
func secretKey(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "env", "user_compose", "normalized": // apps.project.write: переменные и compose с секретами
		return true
	}
	for _, w := range []string{"password", "passwd", "secret", "token", "psk", "key"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if secretKey(k) {
				x[k] = "***"
			} else {
				x[k] = redact(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = redact(x[i])
		}
	}
	return v
}

// auditRequest — запрос для журнала аудита без секретов. Строка,
// которая не разбирается как JSON, в журнал не пишется (там может быть
// пароль): только её размер.
func auditRequest(line []byte) string {
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		return fmt.Sprintf("<не JSON, %d байт>", len(line))
	}
	b, err := json.Marshal(redact(v))
	if err != nil {
		return fmt.Sprintf("<%d байт>", len(line))
	}
	return string(truncate(b, 4096))
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func errCode(r Response) string {
	if r.Error == nil {
		return ""
	}
	return r.Error.Code
}

// process разбирает конверт строго и выполняет операцию.
func (s *Server) process(ctx context.Context, line []byte) (Response, string) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errResponse(CodeBadRequest, "неверный запрос: "+err.Error()), ""
	}
	if _, err := dec.Token(); err != io.EOF {
		return errResponse(CodeBadRequest, "лишние данные после запроса"), ""
	}
	if req.V != ProtocolVersion {
		resp := errResponse(CodeBadRequest, fmt.Sprintf("версия протокола %d не поддерживается", req.V))
		return resp, req.Op
	}
	if len(req.ID) > 64 {
		return errResponse(CodeBadRequest, "id длиннее 64 символов"), req.Op
	}
	resp := s.Registry.Execute(ctx, req.Op, req.Params)
	resp.ID = req.ID
	return resp, req.Op
}

var errLineTooLong = errors.New("строка слишком длинная")

// readLine читает строку до '\n' не длиннее max байт (без '\n').
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max+1 {
			return nil, errLineTooLong
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}
