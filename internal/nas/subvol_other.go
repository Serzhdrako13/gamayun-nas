//go:build !linux

package nas

import "fmt"

// ProbeFS на других ОС не поддерживается: подтомы Btrfs есть только в Linux.
func ProbeFS(string) (FSInfo, error) {
	return FSInfo{}, fmt.Errorf("%w: сведения о ФС только в Linux", ErrRefused)
}
