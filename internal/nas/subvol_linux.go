//go:build linux

package nas

import "golang.org/x/sys/unix"

// btrfsMagic — BTRFS_SUPER_MAGIC; btrfsSubvolIno — номер inode корня
// подтома (BTRFS_FIRST_FREE_OBJECTID).
const (
	btrfsMagic     = 0x9123683E
	btrfsSubvolIno = 256
)

// ProbeFS — ФС каталога (statfs) и признак корня подтома (stat без
// следования по ссылке).
func ProbeFS(dir string) (FSInfo, error) {
	var sf unix.Statfs_t
	if err := unix.Statfs(dir, &sf); err != nil {
		return FSInfo{}, err
	}
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return FSInfo{}, err
	}
	bs := uint64(sf.Bsize)
	fi := FSInfo{
		Btrfs:    uint32(sf.Type) == btrfsMagic,
		ReadOnly: sf.Flags&unix.ST_RDONLY != 0,
		Total:    sf.Blocks * bs,
		Avail:    sf.Bavail * bs,
	}
	fi.Subvol = fi.Btrfs && st.Ino == btrfsSubvolIno && st.Mode&unix.S_IFMT == unix.S_IFDIR
	return fi, nil
}
