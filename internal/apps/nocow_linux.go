//go:build linux

package apps

import (
	"os"

	"golang.org/x/sys/unix"
)

// fsNoCOW — FS_NOCOW_FL (chattr +C).
const fsNoCOW = 0x00800000

func getFlags(path string) (int, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	v, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		f.Close()
		return 0, nil, err
	}
	return v, f, nil
}

// chattrC — chattr +C каталога (новые файлы в нём — без CoW и контрольных сумм Btrfs).
func chattrC(path string) error {
	v, f, err := getFlags(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if v&fsNoCOW != 0 {
		return nil
	}
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, v|fsNoCOW)
}

// hasNoCOW — у каталога стоит +C.
func hasNoCOW(path string) (bool, error) {
	v, f, err := getFlags(path)
	if err != nil {
		return false, err
	}
	f.Close()
	return v&fsNoCOW != 0, nil
}

// StatfsReadOnly — ФС под path только для чтения (ST_RDONLY: флаги точки
// монтирования и суперблока, включая принудительный RO после ошибки Btrfs).
func StatfsReadOnly(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, err
	}
	return st.Flags&unix.ST_RDONLY != 0, nil
}
