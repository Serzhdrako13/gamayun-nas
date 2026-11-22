//go:build linux

package disks

import "syscall"

// systemStat — stat(2) с разбором номеров устройств Linux.
func systemStat(path string) (FileStat, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return FileStat{}, err
	}
	return FileStat{
		Block: st.Mode&syscall.S_IFMT == syscall.S_IFBLK,
		Rdev:  decodeDev(uint64(st.Rdev)),
		Dev:   decodeDev(uint64(st.Dev)),
	}, nil
}
