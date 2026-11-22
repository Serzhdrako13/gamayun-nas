package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Передача открытого файла от helper к gamayund (SCM_RIGHTS).
// Операция возвращает WithFile: результат уходит обычной строкой JSON, а
// дескриптор — вспомогательными данными того же сообщения. gamayund читает
// и пишет файл сам (скачивание, загрузка, копирование) — содержимое не идёт
// через JSON-протокол, а права проверены helper при открытии.

// WithFile — результат операции с открытым файлом. helper закрывает File
// после отправки.
type WithFile struct {
	Result any
	File   *os.File
}

// sendWithFile пишет строку ответа и дескриптор одним сообщением.
func sendWithFile(conn *net.UnixConn, line []byte, f *os.File) error {
	defer f.Close()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	n, oobn, err := conn.WriteMsgUnix(line, unix.UnixRights(int(f.Fd())), nil)
	if err != nil {
		return err
	}
	if oobn == 0 {
		return errors.New("дескриптор не отправлен")
	}
	if n < len(line) {
		_, err = conn.Write(line[n:])
	}
	return err
}

// CallFile — Call для операции, возвращающей файл. Возвращённый файл
// закрывает вызывающий. Ошибка операции — без файла.
func (c *Client) CallFile(ctx context.Context, op string, params, result any) (*os.File, error) {
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
			return nil, err
		}
		req.Params = raw
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	conn := nc.(*net.UnixConn)
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	_, werr := conn.Write(append(line, '\n'))
	var buf []byte
	var file *os.File
	closeFile := func() {
		if file != nil {
			file.Close()
			file = nil
		}
	}
	chunk := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(4*4))
	for {
		n, oobn, _, _, err := conn.ReadMsgUnix(chunk, oob)
		if oobn > 0 {
			if f, ferr := parseRights(oob[:oobn]); ferr == nil && file == nil {
				file = f
			} else if f != nil {
				f.Close()
			}
		}
		buf = append(buf, chunk[:n]...)
		if i := indexNL(buf); i >= 0 {
			buf = buf[:i]
			break
		}
		if err != nil || n == 0 {
			closeFile()
			if werr != nil && err == nil {
				err = werr
			}
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if len(buf) > MaxResponseBytes {
			closeFile()
			return nil, errors.New("gamayun-helper: ответ слишком длинный")
		}
	}
	var resp Response
	if err := json.Unmarshal(buf, &resp); err != nil {
		closeFile()
		return nil, fmt.Errorf("gamayun-helper: неверный ответ: %w", err)
	}
	if !resp.OK {
		closeFile()
		if resp.Error == nil {
			return nil, &RemoteError{Code: CodeInternal, Message: "ошибка без описания"}
		}
		return nil, &RemoteError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if resp.ID != req.ID {
		closeFile()
		return nil, fmt.Errorf("gamayun-helper: id ответа %q не совпадает с %q", resp.ID, req.ID)
	}
	if file == nil {
		return nil, fmt.Errorf("gamayun-helper: %s: ответ без файла", op)
	}
	if result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			closeFile()
			return nil, fmt.Errorf("gamayun-helper: результат %s: %w", op, err)
		}
	}
	return file, nil
}

func indexNL(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// parseRights — первый дескриптор из SCM_RIGHTS; лишние закрываются.
func parseRights(oob []byte) (*os.File, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var f *os.File
	for _, m := range msgs {
		fds, err := unix.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if f == nil {
				unix.CloseOnExec(fd)
				f = os.NewFile(uintptr(fd), "helper-fd")
			} else {
				unix.Close(fd)
			}
		}
	}
	if f == nil {
		return nil, errors.New("нет дескриптора")
	}
	return f, nil
}
