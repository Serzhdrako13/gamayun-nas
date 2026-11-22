//go:build !linux

package nas

import (
	"context"
	"os"
)

// FileHandle — открытый файл и сведения о нём (только Linux).
type FileHandle struct {
	File *os.File
	Info any
}

func (e *Executor) FilesList(context.Context, FilesLocParams) (FilesListResult, error) {
	return FilesListResult{}, errDirOpsUnsupported()
}
func (e *Executor) FilesStat(context.Context, FilesLocParams) (FileInfo, error) {
	return FileInfo{}, errDirOpsUnsupported()
}
func (e *Executor) FilesOpen(context.Context, FilesLocParams) (FileHandle, error) {
	return FileHandle{}, errDirOpsUnsupported()
}
func (e *Executor) FilesCreate(context.Context, FilesCreateParams) (FileHandle, error) {
	return FileHandle{}, errDirOpsUnsupported()
}
func (e *Executor) FilesCommit(context.Context, FilesCommitParams) (FilesNameResult, error) {
	return FilesNameResult{}, errDirOpsUnsupported()
}
func (e *Executor) FilesDiscard(context.Context, FilesDiscardParams) (FilesNameResult, error) {
	return FilesNameResult{}, errDirOpsUnsupported()
}
func (e *Executor) FilesMkdir(context.Context, FilesNameParams) (FilesNameResult, error) {
	return FilesNameResult{}, errDirOpsUnsupported()
}
func (e *Executor) FilesRename(context.Context, FilesNameParams) (FilesNameResult, error) {
	return FilesNameResult{}, errDirOpsUnsupported()
}
func (e *Executor) FilesMove(context.Context, FilesMoveParams) (FilesNameResult, error) {
	return FilesNameResult{}, errDirOpsUnsupported()
}
func (e *Executor) FilesRemove(context.Context, FilesLocParams) (FilesRemoveResult, error) {
	return FilesRemoveResult{}, errDirOpsUnsupported()
}
