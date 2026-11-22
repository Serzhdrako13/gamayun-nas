//go:build !linux

package nas

import "fmt"

// Операции над каталогами шар опираются на openat(O_NOFOLLOW) и
// renameat2(RENAME_NOREPLACE) Linux; на других ОС отказ.

func errDirOpsUnsupported() error {
	return fmt.Errorf("%w: операции с каталогами поддерживаются только в Linux", ErrRefused)
}

func dirMkdir(_, _, _ string, _ func(fd int) error) error { return errDirOpsUnsupported() }

func dirRename(_, _, _, _ string) (bool, error) { return false, errDirOpsUnsupported() }

func dirMove(_, _, _, _ string) (bool, error) { return false, errDirOpsUnsupported() }

func dirRmdir(_, _, _ string) error { return errDirOpsUnsupported() }

func fchown(_, _, _ int) error { return errDirOpsUnsupported() }
