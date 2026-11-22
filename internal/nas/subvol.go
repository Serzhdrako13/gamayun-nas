package nas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Каталог общей папки (и службы домашних папок) — отдельный подтом
// Btrfs, чтобы снимать его снапшоты независимо от других папок.

// FSInfo — сведения о файловой системе каталога.
type FSInfo struct {
	Btrfs    bool // ФС — Btrfs
	Subvol   bool // каталог — корень подтома Btrfs
	ReadOnly bool // ФС смонтирована только для чтения
	Total    uint64
	Avail    uint64
}

// SnapshotsDir — каталог снапшотов пула относительно точки монтирования.
const SnapshotsDir = ".snapshots"

// SnapshotFormat — имя снапшота (UTC) в формате vfs_shadow_copy2 Samba
// (shadow:format) и Go (time.Format).
const (
	SnapshotFormat   = "@GMT-%Y.%m.%d-%H.%M.%S"
	SnapshotGoLayout = "@GMT-2006.01.02-15.04.05"
)

// ShareSnapDir — каталог снапшотов общей папки: <mount_root>/<пул>/.snapshots/<шара>.
func ShareSnapDir(mountRoot, pool, share string) string {
	return filepath.Join(mountRoot, pool, SnapshotsDir, share)
}

// shadowCopyModules — где Debian/Arch держат модуль vfs_shadow_copy2.
var shadowCopyModules = []string{
	"/usr/lib/x86_64-linux-gnu/samba/vfs/shadow_copy2.so",
	"/usr/lib/aarch64-linux-gnu/samba/vfs/shadow_copy2.so",
	"/usr/lib/arm-linux-gnueabihf/samba/vfs/shadow_copy2.so",
	"/usr/lib/samba/vfs/shadow_copy2.so",
}

// hasShadowCopy — установлен ли модуль vfs_shadow_copy2. Без модуля
// «vfs objects = shadow_copy2» не даёт подключиться к ресурсу, поэтому
// строки снапшотов в конфигурацию тогда не пишутся.
func (e *Executor) hasShadowCopy() bool {
	if e.ShadowCopy != nil {
		return e.ShadowCopy()
	}
	for _, p := range shadowCopyModules {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func (e *Executor) fsInfo(dir string) (FSInfo, error) {
	if e.FS != nil {
		return e.FS(dir)
	}
	return ProbeFS(dir)
}

// ensureSubvolume делает каталог dir подтомом Btrfs, если это возможно без
// риска для данных: каталога нет — создаётся подтом; есть пустой обычный
// каталог — он удаляется и на его месте создаётся подтом; непустой обычный каталог не трогается (снапшоты для такой
// папки недоступны до конвертации после alpha). Не Btrfs — ничего (каталог
// создаст install -d, как раньше). Родитель dir должен существовать.
func (e *Executor) ensureSubvolume(ctx context.Context, res *Result, dir string) error {
	pfs, err := e.fsInfo(filepath.Dir(dir))
	if err != nil || !pfs.Btrfs {
		return nil
	}
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case !st.IsDir():
		return nil // ссылка или файл: install -d сообщит об ошибке, как раньше
	default:
		fs, err := e.fsInfo(dir)
		if err != nil || fs.Subvol {
			return nil
		}
		empty, err := dirEmpty(dir)
		if err != nil || !empty {
			return nil
		}
		// rmdir удаляет только пустой каталог: файл, записанный по SMB
		// между проверкой и удалением, даёт ENOTEMPTY — каталог остаётся.
		if err := os.Remove(dir); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: пустой каталог не заменён подтомом: %v", dir, err))
			return nil
		}
		res.Changed = true
	}
	r, err := e.run(ctx, res, nil, or(e.Tools.Btrfs, "/usr/bin/btrfs"), "subvolume", "create", dir)
	if err == nil && r.ExitCode != 0 {
		err = fail("btrfs subvolume create", r)
	}
	if err != nil {
		// Папка важнее снапшотов: каталог создаст install -d, снапшоты для
		// неё будут недоступны (видно в вебе), причина — в предупреждении.
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: подтом не создан, папка — обычный каталог без снимков: %v", dir, err))
		return nil
	}
	res.Changed = true
	return nil
}

func dirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

// IsMountPoint — каталог dir — точка монтирования (устройство отличается от
// родителя), как mountpoint(1). Для пакетов helper (снапшоты).
func IsMountPoint(dir string) bool { return isMountPoint(dir) }
