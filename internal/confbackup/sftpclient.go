package confbackup

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Минимальный клиент SFTP v3 (draft-ietf-secsh-filexfer-02): ровно то, что
// нужно для выгрузки копии — запись файла, переименование, список каталога,
// удаление старых копий. Внешних зависимостей нет (только x/crypto/ssh).

const (
	fxpInit      = 1
	fxpVersion   = 2
	fxpOpen      = 3
	fxpClose     = 4
	fxpWrite     = 6
	fxpOpendir   = 11
	fxpReaddir   = 12
	fxpRemove    = 13
	fxpRename    = 18
	fxpStatus    = 101
	fxpHandle    = 102
	fxpName      = 104
	fxfWrite     = 0x02
	fxfCreat     = 0x08
	fxfExcl      = 0x20
	fxOK         = 0
	fxEOF        = 1
	fxNoSuchFile = 2
	maxPacket    = 256 << 10
	writeChunk   = 32 << 10
)

// errSFTPEOF — конец списка каталога.
var errSFTPEOF = errors.New("sftp: eof")

// SFTPStatusError — ответ сервера с ошибкой.
type SFTPStatusError struct {
	Code uint32
	Msg  string
}

func (e *SFTPStatusError) Error() string {
	switch e.Code {
	case fxNoSuchFile:
		return "sftp: нет такого файла или каталога"
	case 3:
		return "sftp: доступ запрещён"
	}
	return fmt.Sprintf("sftp: код %d: %s", e.Code, e.Msg)
}

type sftpClient struct {
	w  io.Writer
	r  io.Reader
	mu sync.Mutex
	id uint32
}

func newSFTP(w io.Writer, r io.Reader) (*sftpClient, error) {
	c := &sftpClient{w: w, r: r}
	if err := c.send(fxpInit, u32(3)); err != nil {
		return nil, err
	}
	t, _, err := c.recv()
	if err != nil {
		return nil, err
	}
	if t != fxpVersion {
		return nil, fmt.Errorf("sftp: неожиданный ответ %d на init", t)
	}
	return c, nil
}

func u32(v uint32) []byte  { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte  { return binary.BigEndian.AppendUint64(nil, v) }
func sstr(s []byte) []byte { return append(u32(uint32(len(s))), s...) }

func (c *sftpClient) send(t byte, payload ...[]byte) error {
	n := 1
	for _, p := range payload {
		n += len(p)
	}
	buf := make([]byte, 0, 4+n)
	buf = append(buf, u32(uint32(n))...)
	buf = append(buf, t)
	for _, p := range payload {
		buf = append(buf, p...)
	}
	_, err := c.w.Write(buf)
	return err
}

func (c *sftpClient) recv() (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < 1 || n > maxPacket {
		return 0, nil, fmt.Errorf("sftp: пакет длиной %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.r, b); err != nil {
		return 0, nil, err
	}
	return b[0], b[1:], nil
}

// call — запрос с id и ответ на него (запросы последовательные).
func (c *sftpClient) call(t byte, payload ...[]byte) (byte, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	id := c.id
	if err := c.send(t, append([][]byte{u32(id)}, payload...)...); err != nil {
		return 0, nil, err
	}
	rt, b, err := c.recv()
	if err != nil {
		return 0, nil, err
	}
	if len(b) < 4 || binary.BigEndian.Uint32(b) != id {
		return 0, nil, errors.New("sftp: ответ с чужим id")
	}
	return rt, b[4:], nil
}

func parseStatus(b []byte) error {
	if len(b) < 4 {
		return errors.New("sftp: короткий status")
	}
	code := binary.BigEndian.Uint32(b)
	if code == fxOK {
		return nil
	}
	if code == fxEOF {
		return errSFTPEOF
	}
	msg, _, _ := readStr(b[4:])
	return &SFTPStatusError{Code: code, Msg: string(msg)}
}

func readStr(b []byte) ([]byte, []byte, error) {
	if len(b) < 4 {
		return nil, nil, errors.New("sftp: короткая строка")
	}
	n := binary.BigEndian.Uint32(b)
	if uint32(len(b)-4) < n {
		return nil, nil, errors.New("sftp: короткая строка")
	}
	return b[4 : 4+n], b[4+n:], nil
}

func (c *sftpClient) status(t byte, b []byte, err error) error {
	if err != nil {
		return err
	}
	if t != fxpStatus {
		return fmt.Errorf("sftp: ожидался status, пришёл %d", t)
	}
	return parseStatus(b)
}

func (c *sftpClient) handle(t byte, b []byte, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	switch t {
	case fxpHandle:
		h, _, err := readStr(b)
		return h, err
	case fxpStatus:
		if err := parseStatus(b); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("sftp: ожидался handle, пришёл %d", t)
}

// writeFile создаёт новый файл (O_EXCL) с правами 0600 и пишет данные.
func (c *sftpClient) writeFile(path string, data []byte) error {
	attrs := append(u32(0x04), u32(0o600)...) // SSH_FILEXFER_ATTR_PERMISSIONS
	h, err := c.handle(c.call(fxpOpen, sstr([]byte(path)), u32(fxfWrite|fxfCreat|fxfExcl), attrs))
	if err != nil {
		return err
	}
	var werr error
	for off := 0; off < len(data) && werr == nil; off += writeChunk {
		end := min(off+writeChunk, len(data))
		werr = c.status(c.call(fxpWrite, sstr(h), u64(uint64(off)), sstr(data[off:end])))
	}
	cerr := c.status(c.call(fxpClose, sstr(h)))
	if werr != nil {
		return werr
	}
	return cerr
}

func (c *sftpClient) rename(from, to string) error {
	return c.status(c.call(fxpRename, sstr([]byte(from)), sstr([]byte(to))))
}

func (c *sftpClient) remove(path string) error {
	return c.status(c.call(fxpRemove, sstr([]byte(path))))
}

// readDir — имена в каталоге.
func (c *sftpClient) readDir(path string) ([]string, error) {
	h, err := c.handle(c.call(fxpOpendir, sstr([]byte(path))))
	if err != nil {
		return nil, err
	}
	defer func() { c.status(c.call(fxpClose, sstr(h))) }()
	var out []string
	for {
		t, b, err := c.call(fxpReaddir, sstr(h))
		if err != nil {
			return nil, err
		}
		if t == fxpStatus {
			if err := parseStatus(b); errors.Is(err, errSFTPEOF) {
				return out, nil
			} else if err != nil {
				return nil, err
			}
			continue
		}
		if t != fxpName || len(b) < 4 {
			return nil, fmt.Errorf("sftp: ожидался name, пришёл %d", t)
		}
		n := binary.BigEndian.Uint32(b)
		b = b[4:]
		for i := uint32(0); i < n; i++ {
			var name []byte
			if name, b, err = readStr(b); err != nil {
				return nil, err
			}
			if _, b, err = readStr(b); err != nil { // longname
				return nil, err
			}
			if b, err = skipAttrs(b); err != nil {
				return nil, err
			}
			out = append(out, string(name))
		}
	}
}

func skipAttrs(b []byte) ([]byte, error) {
	if len(b) < 4 {
		return nil, errors.New("sftp: короткие атрибуты")
	}
	flags := binary.BigEndian.Uint32(b)
	b = b[4:]
	need := func(n int) error {
		if len(b) < n {
			return errors.New("sftp: короткие атрибуты")
		}
		b = b[n:]
		return nil
	}
	if flags&0x1 != 0 && need(8) != nil { // size
		return nil, errors.New("sftp: атрибуты")
	}
	if flags&0x2 != 0 && need(8) != nil { // uid gid
		return nil, errors.New("sftp: атрибуты")
	}
	if flags&0x4 != 0 && need(4) != nil { // permissions
		return nil, errors.New("sftp: атрибуты")
	}
	if flags&0x8 != 0 && need(8) != nil { // atime mtime
		return nil, errors.New("sftp: атрибуты")
	}
	if flags&0x80000000 != 0 {
		if len(b) < 4 {
			return nil, errors.New("sftp: атрибуты")
		}
		n := binary.BigEndian.Uint32(b)
		b = b[4:]
		for i := uint32(0); i < n*2; i++ {
			var err error
			if _, b, err = readStr(b); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}
