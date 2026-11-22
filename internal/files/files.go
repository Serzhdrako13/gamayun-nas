// Package files — веб-файловый менеджер, сторона gamayund.
//
// Права пользователя на общую папку — те же, что в Samba (nas.FileRoots):
// чтение — список и скачивание, запись — загрузка, папки, переименование,
// копирование и перемещение сюда, удаление. Безопасность пути и владельцы
// файлов — в helper (nas.files.*): каждый компонент пути открывается
// openat(O_NOFOLLOW). Содержимое файлов не идёт через протокол helper:
// nas.files.open/create отдают открытый дескриптор, читает и пишет его gamayund.
package files

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"syscall"

	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/tasks"
)

// TaskTransfer — вид задачи копирования, перемещения и удаления.
const TaskTransfer = "files.transfer"

// Ошибки: ErrForbidden — у пользователя нет нужного доступа к папке (или
// папки нет среди доступных); ErrInvalid — неверный запрос.
var (
	ErrForbidden = errors.New("нет доступа")
	ErrInvalid   = nas.ErrInvalid
)

// Helper — вызовы gamayun-helper (helper.Client).
type Helper interface {
	Call(ctx context.Context, op string, params, result any) error
	CallFile(ctx context.Context, op string, params, result any) (*os.File, error)
}

// Roots — доступные пользователю корни (nas.Service.FileRoots).
type Roots interface {
	FileRoots(ctx context.Context, user string) ([]nas.FileRoot, error)
}

// Service — файловый менеджер.
type Service struct {
	Helper Helper
	// Long — helper для долгих операций (рекурсивное удаление); nil — Helper.
	Long   Helper
	Roots  Roots
	Tasks  *tasks.Manager
	Logger *slog.Logger
}

// Init регистрирует вид задачи.
func (s *Service) Init() error {
	if s.Tasks == nil {
		return nil
	}
	return s.Tasks.Register(TaskTransfer, s.runTransfer)
}

func (s *Service) long() Helper {
	if s.Long != nil {
		return s.Long
	}
	return s.Helper
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// root — корень name пользователя user с уровнем не ниже need (read/rw).
func (s *Service) root(ctx context.Context, user, name, need string) (nas.FileRoot, error) {
	roots, err := s.Roots.FileRoots(ctx, user)
	if err != nil {
		return nas.FileRoot{}, err
	}
	for _, r := range roots {
		if r.Name == name {
			if need == nas.LevelRW && r.Level != nas.LevelRW {
				return nas.FileRoot{}, fmt.Errorf("%w: папка %s доступна только для чтения", ErrForbidden, name)
			}
			return r, nil
		}
	}
	return nas.FileRoot{}, fmt.Errorf("%w: папка %s", ErrForbidden, name)
}

func cleanPath(p string) (string, error) {
	rel, err := nas.CleanRel(p)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return rel, nil
}

// locate — проверка доступа и пути, объект для helper.
func (s *Service) locate(ctx context.Context, user, root, p, need string) (nas.FileLoc, error) {
	rel, err := cleanPath(p)
	if err != nil {
		return nas.FileLoc{}, err
	}
	r, err := s.root(ctx, user, root, need)
	if err != nil {
		return nas.FileLoc{}, err
	}
	return nas.FileLoc{Pool: r.Pool, Share: r.Name, User: user, Path: rel}, nil
}

// List — доступные корни.
func (s *Service) List(ctx context.Context, user string) ([]nas.FileRoot, error) {
	return s.Roots.FileRoots(ctx, user)
}

// Dir — содержимое каталога и уровень доступа к папке.
type Dir struct {
	Root      string          `json:"root"`
	Path      string          `json:"path"`
	Level     string          `json:"level"`
	Entries   []nas.FileEntry `json:"entries"`
	Truncated bool            `json:"truncated,omitempty"`
}

// ListDir — содержимое каталога path папки root.
func (s *Service) ListDir(ctx context.Context, user, root, p string) (Dir, error) {
	l, err := s.locate(ctx, user, root, p, nas.LevelRead)
	if err != nil {
		return Dir{}, err
	}
	r, _ := s.root(ctx, user, root, nas.LevelRead)
	var res nas.FilesListResult
	if err := s.Helper.Call(ctx, nas.OpFilesList, nas.FilesLocParams{FileLoc: l}, &res); err != nil {
		return Dir{}, err
	}
	return Dir{Root: root, Path: l.Path, Level: r.Level, Entries: res.Entries, Truncated: res.Truncated}, nil
}

// Stat — сведения об объекте.
func (s *Service) Stat(ctx context.Context, user, root, p string) (nas.FileInfo, error) {
	l, err := s.locate(ctx, user, root, p, nas.LevelRead)
	if err != nil {
		return nas.FileInfo{}, err
	}
	var fi nas.FileInfo
	err = s.Helper.Call(ctx, nas.OpFilesStat, nas.FilesLocParams{FileLoc: l}, &fi)
	return fi, err
}

// Open — файл на чтение (закрывает вызывающий).
func (s *Service) Open(ctx context.Context, user, root, p string) (*os.File, nas.FileInfo, error) {
	l, err := s.locate(ctx, user, root, p, nas.LevelRead)
	if err != nil {
		return nil, nas.FileInfo{}, err
	}
	var fi nas.FileInfo
	f, err := s.Helper.CallFile(ctx, nas.OpFilesOpen, nas.FilesLocParams{FileLoc: l}, &fi)
	return f, fi, err
}

// Upload — запись size байт из r в новый файл name каталога dir. Сначала
// во временный файл, затем переименование без перезаписи; при любой ошибке
// временный файл удаляется. Нехватка места — nas.ErrNoSpace.
func (s *Service) Upload(ctx context.Context, user, root, dir, name, onConflict string, size int64, r io.Reader) (string, error) {
	l, err := s.locate(ctx, user, root, dir, nas.LevelRW)
	if err != nil {
		return "", err
	}
	if err := nas.ValidFileName(name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if onConflict == "" {
		onConflict = nas.ConflictError
	}
	if onConflict != nas.ConflictError && onConflict != nas.ConflictRename {
		return "", fmt.Errorf("%w: on_conflict: error или rename", ErrInvalid)
	}
	p, err := s.writeFile(ctx, l, name, onConflict, size, r, nil)
	if err == nil {
		s.logger().Info("файлы: загружен файл", "root", root, "path", p, "bytes", size, "actor", user)
	}
	return p, err
}

// writeFile — временный файл, копирование, переименование. progress — по
// мере записи (может быть nil).
func (s *Service) writeFile(ctx context.Context, dir nas.FileLoc, name, onConflict string, size int64, r io.Reader, progress func(int64)) (string, error) {
	var cr nas.FilesCreateResult
	f, err := s.Helper.CallFile(ctx, nas.OpFilesCreate, nas.FilesCreateParams{FileLoc: dir, Size: size}, &cr)
	if err != nil {
		return "", err
	}
	discard := func(cause error) (string, error) {
		f.Close()
		if derr := s.Helper.Call(context.WithoutCancel(ctx), nas.OpFilesDiscard, nas.FilesDiscardParams{FileLoc: dir, Temp: cr.Temp}, nil); derr != nil {
			s.logger().Warn("файлы: временный файл не удалён", "path", path.Join(dir.Path, cr.Temp), "err", derr)
		}
		return "", cause
	}
	w := io.Writer(f)
	if progress != nil {
		w = &progressWriter{w: f, fn: progress}
	}
	n, err := io.CopyBuffer(w, &ctxReader{ctx: ctx, r: r}, make([]byte, 1<<20))
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
			err = fmt.Errorf("%w: %v", nas.ErrNoSpace, err)
		}
		return discard(err)
	}
	if size >= 0 && n != size {
		return discard(fmt.Errorf("%w: получено %d байт из %d", ErrInvalid, n, size))
	}
	if err := f.Close(); err != nil {
		return discard(err)
	}
	var res nas.FilesNameResult
	err = s.Helper.Call(ctx, nas.OpFilesCommit, nas.FilesCommitParams{FileLoc: dir, Temp: cr.Temp, Name: name, OnConflict: onConflict}, &res)
	if err != nil {
		s.Helper.Call(context.WithoutCancel(ctx), nas.OpFilesDiscard, nas.FilesDiscardParams{FileLoc: dir, Temp: cr.Temp}, nil)
		return "", err
	}
	return res.Path, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type progressWriter struct {
	w  io.Writer
	fn func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.fn(int64(n))
	return n, err
}

// Mkdir — новая папка name в каталоге dir.
func (s *Service) Mkdir(ctx context.Context, user, root, dir, name string) (string, error) {
	l, err := s.locate(ctx, user, root, dir, nas.LevelRW)
	if err != nil {
		return "", err
	}
	if err := nas.ValidFileName(name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var res nas.FilesNameResult
	if err := s.Helper.Call(ctx, nas.OpFilesMkdir, nas.FilesNameParams{FileLoc: l, Name: name}, &res); err != nil {
		return "", err
	}
	s.logger().Info("файлы: создана папка", "root", root, "path", res.Path, "actor", user)
	return res.Path, nil
}

// Rename — новое имя объекта p в том же каталоге.
func (s *Service) Rename(ctx context.Context, user, root, p, name string) (string, error) {
	l, err := s.locate(ctx, user, root, p, nas.LevelRW)
	if err != nil {
		return "", err
	}
	if l.Path == "" {
		return "", fmt.Errorf("%w: корень папки переименовать нельзя", ErrInvalid)
	}
	if err := nas.ValidFileName(name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var res nas.FilesNameResult
	if err := s.Helper.Call(ctx, nas.OpFilesRename, nas.FilesNameParams{FileLoc: l, Name: name}, &res); err != nil {
		return "", err
	}
	s.logger().Info("файлы: переименовано", "root", root, "from", l.Path, "path", res.Path, "actor", user)
	return res.Path, nil
}

// MaxZipFiles — предел файлов в архиве папки.
const MaxZipFiles = 100000

// Zip пишет папку p архивом zip без сжатия (метод Store) в w. Ссылки и
// особые файлы пропускаются. Ошибка посреди записи обрывает архив.
func (s *Service) Zip(ctx context.Context, user, root, p string, w io.Writer) error {
	l, err := s.locate(ctx, user, root, p, nas.LevelRead)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	count := 0
	var walk func(rel, inZip string) error
	walk = func(rel, inZip string) error {
		var res nas.FilesListResult
		if err := s.Helper.Call(ctx, nas.OpFilesList, nas.FilesLocParams{FileLoc: withPath(l, rel)}, &res); err != nil {
			return err
		}
		if res.Truncated {
			return fmt.Errorf("в каталоге %s больше %d записей", rel, nas.MaxListEntries)
		}
		for _, e := range res.Entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			child, zname := path.Join(rel, e.Name), path.Join(inZip, e.Name)
			switch e.Type {
			case "dir":
				if _, err := zw.CreateHeader(&zip.FileHeader{Name: zname + "/", Method: zip.Store, Modified: e.MTime}); err != nil {
					return err
				}
				if err := walk(child, zname); err != nil {
					return err
				}
			case "file":
				if count++; count > MaxZipFiles {
					return fmt.Errorf("в архиве больше %d файлов", MaxZipFiles)
				}
				f, err := s.Helper.CallFile(ctx, nas.OpFilesOpen, nas.FilesLocParams{FileLoc: withPath(l, child)}, nil)
				if err != nil {
					return err
				}
				zf, err := zw.CreateHeader(&zip.FileHeader{Name: zname, Method: zip.Store, Modified: e.MTime})
				if err == nil {
					_, err = io.Copy(zf, &ctxReader{ctx: ctx, r: f})
				}
				f.Close()
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(l.Path, ""); err != nil {
		return err
	}
	return zw.Close()
}

func withPath(l nas.FileLoc, p string) nas.FileLoc {
	l.Path = p
	return l
}

// ---------- копирование, перемещение, удаление (задача) ----------

// Действия задачи.
const (
	OpCopy   = "copy"
	OpMove   = "move"
	OpDelete = "delete"
)

// MaxTransferPaths — предел объектов в одной задаче.
const MaxTransferPaths = 1000

// TransferRequest — запрос на задачу.
type TransferRequest struct {
	Op         string   `json:"op"`
	Root       string   `json:"root"`
	Paths      []string `json:"paths"`
	ToRoot     string   `json:"to_root,omitempty"`
	ToPath     string   `json:"to_path,omitempty"`
	OnConflict string   `json:"on_conflict,omitempty"` // error (по умолчанию) или rename
}

type transferParams struct {
	TransferRequest
	User string      `json:"user"`
	Src  nas.FileLoc `json:"src"` // корень источника (Path пустой)
	Dst  nas.FileLoc `json:"dst"` // целевой каталог
}

// TransferResult — итог задачи.
type TransferResult struct {
	Done   []string       `json:"done"`
	Failed []FailedObject `json:"failed,omitempty"`
	Files  int            `json:"files"`
	Dirs   int            `json:"dirs"`
	Bytes  int64          `json:"bytes"`
}

// FailedObject — объект, который не удалось обработать.
type FailedObject struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Transfer проверяет права и ставит задачу.
func (s *Service) Transfer(ctx context.Context, user string, req TransferRequest) (tasks.Task, error) {
	if len(req.Paths) == 0 || len(req.Paths) > MaxTransferPaths {
		return tasks.Task{}, fmt.Errorf("%w: paths: от 1 до %d", ErrInvalid, MaxTransferPaths)
	}
	if req.OnConflict == "" {
		req.OnConflict = nas.ConflictError
	}
	if req.OnConflict != nas.ConflictError && req.OnConflict != nas.ConflictRename {
		return tasks.Task{}, fmt.Errorf("%w: on_conflict: error или rename", ErrInvalid)
	}
	need := nas.LevelRead
	if req.Op == OpMove || req.Op == OpDelete {
		need = nas.LevelRW
	} else if req.Op != OpCopy {
		return tasks.Task{}, fmt.Errorf("%w: op: copy, move или delete", ErrInvalid)
	}
	src, err := s.locate(ctx, user, req.Root, "", need)
	if err != nil {
		return tasks.Task{}, err
	}
	seen := map[string]bool{}
	for i, p := range req.Paths {
		rel, err := cleanPath(p)
		if err != nil {
			return tasks.Task{}, err
		}
		if rel == "" {
			return tasks.Task{}, fmt.Errorf("%w: корень папки нельзя %s", ErrInvalid, map[string]string{OpCopy: "копировать", OpMove: "перемещать", OpDelete: "удалять"}[req.Op])
		}
		if seen[rel] {
			return tasks.Task{}, fmt.Errorf("%w: путь %s повторяется", ErrInvalid, rel)
		}
		seen[rel] = true
		req.Paths[i] = rel
	}
	tp := transferParams{TransferRequest: req, User: user, Src: src}
	if req.Op != OpDelete {
		dst, err := s.locate(ctx, user, req.ToRoot, req.ToPath, nas.LevelRW)
		if err != nil {
			return tasks.Task{}, err
		}
		for _, p := range req.Paths {
			if dst.Share == src.Share && (dst.Path == p || strings.HasPrefix(dst.Path, p+"/")) {
				return tasks.Task{}, fmt.Errorf("%w: нельзя %s %s в саму себя", ErrInvalid, map[string]string{OpCopy: "копировать", OpMove: "переместить"}[req.Op], p)
			}
		}
		tp.Dst = dst
	}
	raw, err := json.Marshal(tp)
	if err != nil {
		return tasks.Task{}, err
	}
	if s.Tasks == nil {
		return tasks.Task{}, errors.New("задачи недоступны")
	}
	t, err := s.Tasks.Submit(ctx, TaskTransfer, raw)
	if err == nil {
		s.logger().Info("файлы: задача поставлена", "task", t.ID, "op", req.Op, "root", req.Root, "paths", len(req.Paths),
			"to_root", req.ToRoot, "to_path", req.ToPath, "actor", user)
	}
	return t, err
}

func (s *Service) runTransfer(ctx context.Context, raw json.RawMessage, progress func(int)) (any, error) {
	var p transferParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	// Права проверяются повторно: между постановкой и запуском их могли снять.
	need := nas.LevelRead
	if p.Op != OpCopy {
		need = nas.LevelRW
	}
	if _, err := s.root(ctx, p.User, p.Root, need); err != nil {
		return nil, err
	}
	if p.Op != OpDelete {
		if _, err := s.root(ctx, p.User, p.ToRoot, nas.LevelRW); err != nil {
			return nil, err
		}
	}
	t := &transfer{s: s, ctx: ctx, p: p, progress: progress, res: TransferResult{Done: []string{}}}
	return t.run()
}

type transfer struct {
	s        *Service
	ctx      context.Context
	p        transferParams
	progress func(int)
	res      TransferResult
	total    int64 // байт (копирование) или объектов (удаление, перенос)
	done     int64
	last     int
}

func (t *transfer) tick(n int64) {
	t.done += n
	if t.total > 0 {
		pct := int(t.done * 100 / t.total)
		if pct > 99 {
			pct = 99
		}
		if pct != t.last {
			t.last = pct
			t.progress(pct)
		}
	}
	tasks.Alive(t.ctx, "файлы: обработано")
}

func (t *transfer) run() (any, error) {
	sameRoot := t.p.Op == OpMove && t.p.Src.Share == t.p.Dst.Share && t.p.Src.Pool == t.p.Dst.Pool
	copying := t.p.Op == OpCopy || (t.p.Op == OpMove && !sameRoot)
	if copying {
		for _, p := range t.p.Paths {
			n, err := t.size(withPath(t.p.Src, p))
			if err != nil && t.ctx.Err() != nil {
				return nil, t.ctx.Err()
			}
			t.total += n
		}
	} else {
		t.total = int64(len(t.p.Paths))
	}
	for _, p := range t.p.Paths {
		if err := t.ctx.Err(); err != nil {
			return t.res, err
		}
		var err error
		switch {
		case t.p.Op == OpDelete:
			var r nas.FilesRemoveResult
			err = t.s.long().Call(t.ctx, nas.OpFilesRemove, nas.FilesLocParams{FileLoc: withPath(t.p.Src, p)}, &r)
			t.res.Files += r.Files
			t.res.Dirs += r.Dirs
			t.tick(1)
		case sameRoot:
			var r nas.FilesNameResult
			err = t.s.Helper.Call(t.ctx, nas.OpFilesMove, nas.FilesMoveParams{FileLoc: withPath(t.p.Src, p), To: t.p.Dst.Path}, &r)
			if err == nil {
				t.res.Done = append(t.res.Done, r.Path)
			}
			t.tick(1)
		default:
			var dst string
			dst, err = t.copyTop(p)
			if err == nil && t.p.Op == OpMove {
				var r nas.FilesRemoveResult
				if rerr := t.s.long().Call(t.ctx, nas.OpFilesRemove, nas.FilesLocParams{FileLoc: withPath(t.p.Src, p)}, &r); rerr != nil {
					err = fmt.Errorf("скопировано в %s, но источник не удалён: %w", dst, rerr)
				}
			}
			if err == nil {
				t.res.Done = append(t.res.Done, dst)
			}
		}
		if err != nil {
			if t.ctx.Err() != nil {
				return t.res, t.ctx.Err()
			}
			t.res.Failed = append(t.res.Failed, FailedObject{Path: p, Error: err.Error()})
		} else if t.p.Op == OpDelete {
			t.res.Done = append(t.res.Done, p)
		}
	}
	t.progress(100)
	t.s.logger().Info("файлы: задача завершена", "op", t.p.Op, "root", t.p.Root, "done", len(t.res.Done),
		"failed", len(t.res.Failed), "bytes", t.res.Bytes, "actor", t.p.User)
	if len(t.res.Failed) > 0 {
		return t.res, fmt.Errorf("не выполнено %d из %d: %s — %s", len(t.res.Failed), len(t.p.Paths), t.res.Failed[0].Path, t.res.Failed[0].Error)
	}
	return t.res, nil
}

// size — байт в объекте (каталог — рекурсивно; ссылки не учитываются).
func (t *transfer) size(l nas.FileLoc) (int64, error) {
	var fi nas.FileInfo
	if err := t.s.Helper.Call(t.ctx, nas.OpFilesStat, nas.FilesLocParams{FileLoc: l}, &fi); err != nil {
		return 0, err
	}
	if fi.Type != "dir" {
		return fi.Size, nil
	}
	var res nas.FilesListResult
	if err := t.s.Helper.Call(t.ctx, nas.OpFilesList, nas.FilesLocParams{FileLoc: l}, &res); err != nil {
		return 0, err
	}
	var n int64
	for _, e := range res.Entries {
		switch e.Type {
		case "file":
			n += e.Size
		case "dir":
			m, err := t.size(withPath(l, path.Join(l.Path, e.Name)))
			if err != nil {
				return n, err
			}
			n += m
		}
	}
	return n, nil
}

// copyTop копирует объект p источника в целевой каталог; имя верхнего
// уровня — по on_conflict, внутри новой папки конфликтов нет.
func (t *transfer) copyTop(p string) (string, error) {
	src := withPath(t.p.Src, p)
	var fi nas.FileInfo
	if err := t.s.Helper.Call(t.ctx, nas.OpFilesStat, nas.FilesLocParams{FileLoc: src}, &fi); err != nil {
		return "", err
	}
	_, name := path.Split(p)
	switch fi.Type {
	case "file":
		return t.copyFile(src, fi.Size, t.p.Dst, name, t.p.OnConflict)
	case "dir":
		dst, err := t.mkdirTop(name)
		if err != nil {
			return "", err
		}
		return dst, t.copyDir(src, withPath(t.p.Dst, dst))
	}
	return "", fmt.Errorf("%w: %s — ссылка или особый файл, не копируется", ErrInvalid, p)
}

func (t *transfer) mkdirTop(name string) (string, error) {
	try := name
	for n := 2; ; n++ {
		var r nas.FilesNameResult
		err := t.s.Helper.Call(t.ctx, nas.OpFilesMkdir, nas.FilesNameParams{FileLoc: t.p.Dst, Name: try}, &r)
		if err == nil {
			t.res.Dirs++
			return r.Path, nil
		}
		var re *helper.RemoteError
		if t.p.OnConflict != nas.ConflictRename || !errors.As(err, &re) || re.Code != helper.CodeConflict || n > 999 {
			return "", err
		}
		try = nas.ConflictName(name, n)
	}
}

func (t *transfer) copyDir(src, dst nas.FileLoc) error {
	var res nas.FilesListResult
	if err := t.s.Helper.Call(t.ctx, nas.OpFilesList, nas.FilesLocParams{FileLoc: src}, &res); err != nil {
		return err
	}
	if res.Truncated {
		return fmt.Errorf("в каталоге %s больше %d записей", src.Path, nas.MaxListEntries)
	}
	for _, e := range res.Entries {
		if err := t.ctx.Err(); err != nil {
			return err
		}
		cs := withPath(src, path.Join(src.Path, e.Name))
		switch e.Type {
		case "file":
			if _, err := t.copyFile(cs, e.Size, dst, e.Name, nas.ConflictError); err != nil {
				return err
			}
		case "dir":
			var r nas.FilesNameResult
			if err := t.s.Helper.Call(t.ctx, nas.OpFilesMkdir, nas.FilesNameParams{FileLoc: dst, Name: e.Name}, &r); err != nil {
				return err
			}
			t.res.Dirs++
			if err := t.copyDir(cs, withPath(dst, r.Path)); err != nil {
				return err
			}
		}
		// Ссылки и особые файлы не копируются (как в архиве).
	}
	return nil
}

func (t *transfer) copyFile(src nas.FileLoc, size int64, dstDir nas.FileLoc, name, onConflict string) (string, error) {
	f, err := t.s.Helper.CallFile(t.ctx, nas.OpFilesOpen, nas.FilesLocParams{FileLoc: src}, nil)
	if err != nil {
		return "", err
	}
	defer f.Close()
	p, err := t.s.writeFile(t.ctx, dstDir, name, onConflict, size, f, func(n int64) {
		t.res.Bytes += n
		t.tick(n)
	})
	if err == nil {
		t.res.Files++
	}
	return p, err
}

// ErrNotTransfer — задача не принадлежит файловому менеджеру или чужая.
var ErrNotTransfer = errors.New("задача файлового менеджера не найдена")

// CancelTransfer отменяет свою задачу files.transfer. Уже скопированное
// остаётся в цели, недописанный файл удаляется (временный); при переносе
// между папками источник объекта, копия которого не завершена, не удаляется.
func (s *Service) CancelTransfer(ctx context.Context, user, id string) error {
	if s.Tasks == nil {
		return ErrNotTransfer
	}
	t, err := s.Tasks.Get(ctx, id)
	if err != nil {
		return ErrNotTransfer
	}
	var p transferParams
	if t.Kind != TaskTransfer || json.Unmarshal(t.Params, &p) != nil || p.User != user {
		return ErrNotTransfer
	}
	if err := s.Tasks.Cancel(ctx, id); err != nil {
		return err
	}
	s.logger().Info("файлы: задача отменена", "task", id, "actor", user)
	return nil
}
