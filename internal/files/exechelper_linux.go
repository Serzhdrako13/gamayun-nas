//go:build linux

package files

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
)

// ExecHelper выполняет операции nas.files.* в процессе через nas.Executor —
// для тестов (сервис и API без сокета и root). Ошибки — как у helper.Client
// (*helper.RemoteError с теми же кодами), параметры проходят ту же проверку.
type ExecHelper struct {
	Exec *nas.Executor
	reg  *helper.Registry
}

func (h *ExecHelper) registry() *helper.Registry {
	if h.reg == nil {
		e := h.Exec
		wf := func(fh nas.FileHandle, err error) (any, error) {
			if err != nil {
				return nil, err
			}
			return helper.WithFile{Result: fh.Info, File: fh.File}, nil
		}
		h.reg = helper.NewRegistry(
			helper.Define(nas.OpFilesList, 1e10, func(ctx context.Context, p nas.FilesLocParams) (any, error) { return e.FilesList(ctx, p) }),
			helper.Define(nas.OpFilesStat, 1e10, func(ctx context.Context, p nas.FilesLocParams) (any, error) { return e.FilesStat(ctx, p) }),
			helper.Define(nas.OpFilesOpen, 1e10, func(ctx context.Context, p nas.FilesLocParams) (any, error) { return wf(e.FilesOpen(ctx, p)) }),
			helper.Define(nas.OpFilesCreate, 1e10, func(ctx context.Context, p nas.FilesCreateParams) (any, error) { return wf(e.FilesCreate(ctx, p)) }),
			helper.Define(nas.OpFilesCommit, 1e10, func(ctx context.Context, p nas.FilesCommitParams) (any, error) { return e.FilesCommit(ctx, p) }),
			helper.Define(nas.OpFilesDiscard, 1e10, func(ctx context.Context, p nas.FilesDiscardParams) (any, error) { return e.FilesDiscard(ctx, p) }),
			helper.Define(nas.OpFilesMkdir, 1e10, func(ctx context.Context, p nas.FilesNameParams) (any, error) { return e.FilesMkdir(ctx, p) }),
			helper.Define(nas.OpFilesRename, 1e10, func(ctx context.Context, p nas.FilesNameParams) (any, error) { return e.FilesRename(ctx, p) }),
			helper.Define(nas.OpFilesMove, 1e10, func(ctx context.Context, p nas.FilesMoveParams) (any, error) { return e.FilesMove(ctx, p) }),
			helper.Define(nas.OpFilesRemove, 1e10, func(ctx context.Context, p nas.FilesLocParams) (any, error) { return e.FilesRemove(ctx, p) }),
		)
	}
	return h.reg
}

func (h *ExecHelper) exec(ctx context.Context, op string, params, result any) (*os.File, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	resp, file := h.registry().ExecuteFile(ctx, op, raw)
	if !resp.OK {
		if file != nil {
			file.Close()
		}
		return nil, &helper.RemoteError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if result != nil {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			if file != nil {
				file.Close()
			}
			return nil, err
		}
	}
	return file, nil
}

// Call — как helper.Client.Call; файл результата закрывается.
func (h *ExecHelper) Call(ctx context.Context, op string, params, result any) error {
	f, err := h.exec(ctx, op, params, result)
	if f != nil {
		f.Close()
	}
	return err
}

// CallFile — как helper.Client.CallFile.
func (h *ExecHelper) CallFile(ctx context.Context, op string, params, result any) (*os.File, error) {
	f, err := h.exec(ctx, op, params, result)
	if err == nil && f == nil {
		return nil, fmt.Errorf("%s: ответ без файла", op)
	}
	return f, err
}
