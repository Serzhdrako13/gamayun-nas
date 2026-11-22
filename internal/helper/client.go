package helper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrUnavailable — helper недоступен (нет сокета, отказ в подключении, обрыв).
var ErrUnavailable = errors.New("gamayun-helper недоступен")

// RemoteError — helper ответил ошибкой.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string { return fmt.Sprintf("gamayun-helper: %s: %s", e.Code, e.Message) }

// RefusedBeforeWrite — отказ проверок или неверные параметры: helper не
// выполнял изменяющих команд.
func (e *RemoteError) RefusedBeforeWrite() bool {
	return e.Code == CodeRefused || e.Code == CodeInvalidParams
}

// Client — клиент gamayund к helper. Одно соединение на вызов: вызовы редкие,
// а так не нужно восстанавливать состояние после обрыва.
type Client struct {
	Socket  string
	Timeout time.Duration // на весь вызов; 0 — 60 с
}

// Call выполняет операцию op. params сериализуется в JSON (nil — без
// параметров); результат разбирается в result, если он не nil
// (*json.RawMessage — сохранить как есть).
func (c *Client) Call(ctx context.Context, op string, params, result any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := Request{V: ProtocolVersion, Op: op, ID: newReqID()}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(line) > MaxRequestBytes {
		return fmt.Errorf("запрос к helper длиннее %d байт", MaxRequestBytes)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	// Ошибку записи не возвращаем сразу: helper мог отклонить соединение
	// (forbidden) и закрыть его до чтения запроса — его ответ уже в буфере.
	_, werr := conn.Write(append(line, '\n'))
	respLine, err := readLine(bufio.NewReader(conn), MaxResponseBytes)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		}
		if werr != nil {
			err = werr
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var resp Response
	if err := json.NewDecoder(bytes.NewReader(respLine)).Decode(&resp); err != nil {
		return fmt.Errorf("gamayun-helper: неверный ответ: %w", err)
	}
	if !resp.OK {
		if resp.Error == nil {
			return &RemoteError{Code: CodeInternal, Message: "ошибка без описания"}
		}
		return &RemoteError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if resp.ID != req.ID {
		return fmt.Errorf("gamayun-helper: id ответа %q не совпадает с %q", resp.ID, req.ID)
	}
	if result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("gamayun-helper: результат %s: %w", op, err)
		}
	}
	return nil
}

func newReqID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
