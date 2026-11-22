//go:build !linux

package disks

import "errors"

func systemStat(string) (FileStat, error) {
	return FileStat{}, errors.New("stat устройств поддержан только в Linux")
}
