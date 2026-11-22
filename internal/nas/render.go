package nas

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ShareDir — каталог шары: <mount_root>/<пул>/shares/<шара>.
func ShareDir(mountRoot, pool, share string) string {
	return filepath.Join(mountRoot, pool, "shares", share)
}

const netbiosPrefix = "netbios name = "

// Header — первая строка include-файла.
const Header = "# Создан gamayun-nas. Не редактировать: файл перезаписывается.\n"

// Render строит include-файл Samba. Доступ — только по явным спискам
// пользователей (группы раскрыты в gamayund), гостевой вход выключен.
// При read_only пула список записи пуст и ставится read only = yes.
func Render(mountRoot string, shares []Share) string {
	return RenderParams(mountRoot, SharesParams{Shares: shares})
}

// RenderParams — Render с именем сервера (netbios name) и ресурсом [homes]
// globals идут первыми — до первой секции файл относится к [global].
// [homes] показывает пользователю его каталог из учётки ОС (homes/<user>,
// владелец — он сам, 0700): без force group, права только владельцу.
func RenderParams(mountRoot string, p SharesParams) string {
	shares := p.Shares
	var b strings.Builder
	b.WriteString(Header)
	if p.Netbios != "" {
		fmt.Fprintf(&b, "%s%s\n", netbiosPrefix, p.Netbios)
	}
	if p.Homes != "" {
		// path задан явно (а не из учётки ОС): ресурс не зависит от того,
		// удалось ли сменить домашний каталог учётки у работающего пользователя.
		fmt.Fprintf(&b, "\n[homes]\n\tcomment = Личная папка\n\tpath = %s/%%S\n\tbrowseable = no\n\tguest ok = no\n\tvalid users = %%S\n",
			HomesDir(mountRoot, p.Homes))
		if p.HomesReadOnly {
			b.WriteString("\tread only = yes\n")
		} else {
			b.WriteString("\tread only = no\n")
		}
		b.WriteString("\tcreate mask = 0600\n\tdirectory mask = 0700\n")
		if p.ShadowCopy {
			writeShadow(&b, ShareSnapDir(mountRoot, p.Homes, HomesShare))
		}
	}
	for _, s := range shares {
		fmt.Fprintf(&b, "\n[%s]\n", s.Name)
		if s.Comment != "" {
			fmt.Fprintf(&b, "\tcomment = %s\n", s.Comment)
		}
		fmt.Fprintf(&b, "\tpath = %s\n", ShareDir(mountRoot, s.Pool, s.Name))
		b.WriteString("\tbrowseable = yes\n\tguest ok = no\n")
		read := union(s.Read, s.Write)
		if len(read) == 0 {
			// Никому нет доступа: пустой valid users в Samba означает «всем».
			b.WriteString("\tvalid users = " + "gamayun-nobody\n")
		} else {
			b.WriteString("\tvalid users = " + strings.Join(read, " ") + "\n")
		}
		if s.ReadOnly || len(s.Write) == 0 {
			b.WriteString("\tread only = yes\n")
		} else {
			// read only = no дал бы запись всем из valid users: write list лишь добавляет
			// права при read only = yes.
			b.WriteString("\tread only = yes\n\twrite list = " + strings.Join(s.Write, " ") + "\n")
		}
		// inherit acls: в папках с выдачей приложению default ACL
		// наследуется новыми файлами SMB, маски Samba его не урезают.
		fmt.Fprintf(&b, "\tforce group = %s\n\tcreate mask = 0660\n\tdirectory mask = 0770\n\tinherit acls = yes\n", UnixGroup)
		if p.ShadowCopy {
			writeShadow(&b, ShareSnapDir(mountRoot, s.Pool, s.Name))
		}
	}
	return b.String()
}

// writeShadow — «Предыдущие версии» Windows из снапшотов папки.
// Корень папки — корень подтома (свой st_dev), поэтому Samba считает его
// точкой монтирования, и путь файла в снапшоте — путь от корня папки; для
// [homes] точка — подтом homes, путь — <user>/… Снапшоты только для чтения
// (подтомы Btrfs -r), каталог снапшотов — вне пути папки. Формат имён —
// умолчание модуля (SnapshotFormat, UTC): «%» в smb.conf не пишем.
func writeShadow(b *strings.Builder, snapdir string) {
	fmt.Fprintf(b, "\tvfs objects = shadow_copy2\n\tshadow:snapdir = %s\n\tshadow:sort = desc\n\tshadow:localtime = no\n", snapdir)
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]string{a, b} {
		for _, x := range l {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	return out
}
