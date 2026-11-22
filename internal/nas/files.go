package nas

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Операции helper веб-файлового менеджера. Права пользователя на
// общую папку проверяет gamayund (как Samba: списки чтения/записи); helper
// отвечает за безопасность пути: каждый компонент открывается
// openat(O_NOFOLLOW) от каталога шары, ссылки и «..» не проходят. Содержимое
// файлов не идёт через протокол: nas.files.open/create возвращают открытый
// дескриптор (helper.WithFile), читает и пишет его gamayund.
const (
	OpFilesList    = "nas.files.list"
	OpFilesStat    = "nas.files.stat"
	OpFilesOpen    = "nas.files.open"
	OpFilesCreate  = "nas.files.create"
	OpFilesCommit  = "nas.files.commit"
	OpFilesDiscard = "nas.files.discard"
	OpFilesMkdir   = "nas.files.mkdir"
	OpFilesRename  = "nas.files.rename"
	OpFilesMove    = "nas.files.move"
	OpFilesRemove  = "nas.files.remove"
)

// ErrNoSpace — на пуле не хватает места для загрузки или копии.
var ErrNoSpace = errors.New("недостаточно места")

// TempPrefix — префикс временного файла загрузки/копии в целевом каталоге;
// такие файлы не показываются в списке.
const TempPrefix = ".gamayun-up-"

// MaxListEntries — предел записей в одном ответе списка каталога.
const MaxListEntries = 10000

// MaxRemoveDepth — предел вложенности при рекурсивном удалении (по одному
// дескриптору на уровень).
const MaxRemoveDepth = 128

// FileLoc — объект в общей папке. Share = HomesShare — домашняя папка
// пользователя User (<пул>/shares/homes/<User>); иначе User — автор
// действия (владелец создаваемых файлов).
type FileLoc struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	User  string `json:"user"`
	Path  string `json:"path"` // относительно корня папки; "" — корень
}

func (l FileLoc) validate() error {
	if !poolRe.MatchString(l.Pool) {
		return errors.New("недопустимое имя пула")
	}
	if l.Share != HomesShare && !ValidShare(l.Share) {
		return errors.New("недопустимое имя папки")
	}
	if !ValidName(l.User) {
		return errors.New("недопустимое имя пользователя")
	}
	_, err := CleanRel(l.Path)
	return err
}

func (l FileLoc) notRoot() error {
	if l.Path == "" {
		return errors.New("корень папки для этой операции недопустим")
	}
	return nil
}

// FileEntry — запись каталога.
type FileEntry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // file, dir, link, other
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// FilesListResult — ответ nas.files.list.
type FilesListResult struct {
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
}

// FileInfo — ответ nas.files.stat / open.
type FileInfo struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// FilesLocParams — параметры list/stat/open/remove.
type FilesLocParams struct {
	FileLoc
}

// Validate проверяет значения до обращения к ФС.
func (p FilesLocParams) Validate() error { return p.validate() }

// FilesCreateParams — создать временный файл в каталоге Path для записи
// Size байт (проверка места); ответ — дескриптор на запись и имя Temp.
type FilesCreateParams struct {
	FileLoc
	Size int64 `json:"size"`
}

// Validate проверяет значения до обращения к ФС.
func (p FilesCreateParams) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Size < 0 {
		return errors.New("отрицательный размер")
	}
	return nil
}

// FilesCreateResult — имя временного файла.
type FilesCreateResult struct {
	Temp string `json:"temp"`
}

// Режимы конфликта имени при commit.
const (
	ConflictError  = "error"  // имя занято — ErrConflict, временный файл остаётся
	ConflictRename = "rename" // «имя (2).ext», «имя (3).ext», …
)

// FilesCommitParams — переименовать временный файл Temp каталога Path в Name.
type FilesCommitParams struct {
	FileLoc
	Temp       string `json:"temp"`
	Name       string `json:"name"`
	OnConflict string `json:"on_conflict"`
}

// Validate проверяет значения до обращения к ФС.
func (p FilesCommitParams) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	if err := validTemp(p.Temp); err != nil {
		return err
	}
	if err := ValidFileName(p.Name); err != nil {
		return err
	}
	if p.OnConflict != ConflictError && p.OnConflict != ConflictRename {
		return errors.New("on_conflict: error или rename")
	}
	return nil
}

// FilesNameResult — итоговое имя/путь.
type FilesNameResult struct {
	Path string `json:"path"`
}

// FilesDiscardParams — удалить временный файл Temp каталога Path.
type FilesDiscardParams struct {
	FileLoc
	Temp string `json:"temp"`
}

// Validate проверяет значения до обращения к ФС.
func (p FilesDiscardParams) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	return validTemp(p.Temp)
}

// FilesNameParams — mkdir (Name в каталоге Path) и rename (Path → Name).
type FilesNameParams struct {
	FileLoc
	Name string `json:"name"`
}

// Validate проверяет значения до обращения к ФС.
func (p FilesNameParams) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	return ValidFileName(p.Name)
}

// FilesMoveParams — перенести Path в каталог To той же папки.
type FilesMoveParams struct {
	FileLoc
	To string `json:"to"`
}

// Validate проверяет значения до обращения к ФС.
func (p FilesMoveParams) Validate() error {
	if err := p.validate(); err != nil {
		return err
	}
	if err := p.notRoot(); err != nil {
		return err
	}
	to, err := CleanRel(p.To)
	if err != nil {
		return fmt.Errorf("целевой каталог: %v", err)
	}
	if to == p.Path || strings.HasPrefix(to, p.Path+"/") {
		return errors.New("нельзя перенести папку в саму себя или в её подкаталог")
	}
	return nil
}

// FilesRemoveResult — сколько удалено.
type FilesRemoveResult struct {
	Files int `json:"files"`
	Dirs  int `json:"dirs"`
}

// ValidFileName — имя нового файла или папки: правила ValidDirName
// (UTF-8, ≤ 255 байт, без управляющих, правила Windows) и не временное.
func ValidFileName(name string) error {
	if err := ValidDirName(name); err != nil {
		return err
	}
	if strings.HasPrefix(name, TempPrefix) {
		return errors.New("имя зарезервировано")
	}
	return nil
}

func validTemp(t string) error {
	if !strings.HasPrefix(t, TempPrefix) || len(t) != len(TempPrefix)+16 {
		return errors.New("недопустимое имя временного файла")
	}
	for _, c := range t[len(TempPrefix):] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("недопустимое имя временного файла")
		}
	}
	return nil
}

// ConflictName — n-е свободное имя: «a.txt» → «a (2).txt»; у папок и имён
// без точки — в конце; скрытые «.x» — «.x (2)».
func ConflictName(name string, n int) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if base == "" {
		base, ext = name, ""
	}
	return base + " (" + strconv.Itoa(n) + ")" + ext
}

// fileBase — каталог корня папки после проверки, что пул смонтирован.
func (e *Executor) fileBase(l FileLoc) (string, error) {
	if !e.poolMounted(l.Pool) {
		return "", fmt.Errorf("%w: пул %s не смонтирован", ErrRefused, l.Pool)
	}
	if l.Share == HomesShare {
		return filepath.Join(HomesDir(e.MountRoot, l.Pool), l.User), nil
	}
	return ShareDir(e.MountRoot, l.Pool, l.Share), nil
}

// fileOwner — владелец создаваемого файла: пользователь; группа —
// gamayun-users в общих папках (как force group Samba), в домашней —
// основная группа пользователя. Режимы — как create/directory mask Samba.
func (e *Executor) fileOwner(l FileLoc) (uid, gid int, fileMode, dirMode uint32, err error) {
	uid, pgid, err := userIDs(or(e.PasswdFile, "/etc/passwd"), l.User)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if l.Share == HomesShare {
		return uid, pgid, 0o600, 0o700, nil
	}
	gid, err = groupID(or(e.GroupFile, "/etc/group"), UnixGroup)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return uid, gid, 0o660, 0o2770, nil
}

// userIDs — uid и основной gid пользователя из файла формата /etc/passwd.
func userIDs(file, user string) (int, int, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 4 && f[0] == user {
			uid, err1 := strconv.Atoi(f[2])
			gid, err2 := strconv.Atoi(f[3])
			if err1 != nil || err2 != nil || uid <= 0 || gid < 0 {
				return 0, 0, fmt.Errorf("пользователь %s: неверные uid/gid", user)
			}
			return uid, gid, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: пользователь ОС %s", ErrNotFound, user)
}

func (e *Executor) chownFile(fd, uid, gid int) error {
	if e.FileChown != nil {
		return e.FileChown(fd, uid, gid)
	}
	return fchown(fd, uid, gid)
}
