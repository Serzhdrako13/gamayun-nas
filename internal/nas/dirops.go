package nas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Операции helper над вложенными каталогами общей папки.
const (
	OpShareMkdir  = "nas.share.mkdir"
	OpShareRename = "nas.share.rename"
	OpShareRmdir  = "nas.share.rmdir"
	OpShareMove   = "nas.share.move" // перенос в другой каталог той же шары
)

// MaxDirName — предел длины имени каталога в байтах (NAME_MAX).
const MaxDirName = 255

// ShareDirMode — режим созданного каталога: rwxrws---, как у каталога шары
// (install -d -m 2770 -o root -g gamayun-users).
const ShareDirMode os.FileMode = 0o770 | os.ModeSetgid

// ValidDirName проверяет имя новой папки (mkdir, новое имя при rename):
// непустое, не длиннее MaxDirName байт, корректный UTF-8, не «.» и не «..»,
// без управляющих символов (C0, DEL, C1) и по правилам Windows: без
// < > : " / \ | ? *, без точки или пробела в конце, не имя устройства
// (CON, PRN, AUX, NUL, COM1–9, LPT1–9, в том числе с расширением).
// Существующие каталоги в пути (Path) этой проверкой не ограничиваются:
// CleanRel пропускает любые имена, кроме «.», «..» и управляющих.
func ValidDirName(name string) error {
	if name == "" {
		return errors.New("пустое имя папки")
	}
	if len(name) > MaxDirName {
		return fmt.Errorf("имя папки длиннее %d байт", MaxDirName)
	}
	if !utf8.ValidString(name) {
		return errors.New("имя папки не в UTF-8")
	}
	if name == "." || name == ".." {
		return errors.New("имена «.» и «..» недопустимы")
	}
	return winNameError(name)
}

func validPoolShare(pool, share string) error {
	if !poolRe.MatchString(pool) {
		return errors.New("недопустимое имя пула")
	}
	if !ValidShare(share) {
		return errors.New("недопустимое имя папки")
	}
	return nil
}

// ShareMkdirParams — параметры nas.share.mkdir: создать каталог Name в
// каталоге Path ("" — корень шары).
type ShareMkdirParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Path  string `json:"path"` // родитель относительно шары; "" — корень
	Name  string `json:"name"`
}

// Validate проверяет значения до обращения к файловой системе.
func (p ShareMkdirParams) Validate() error {
	if err := validPoolShare(p.Pool, p.Share); err != nil {
		return err
	}
	rel, err := CleanRel(p.Path)
	if err != nil {
		return err
	}
	if err := ValidDirName(p.Name); err != nil {
		return err
	}
	if len(path.Join(rel, p.Name)) > MaxDirPath {
		return errors.New("путь слишком длинный")
	}
	return nil
}

// ShareRenameParams — параметры nas.share.rename: переименовать каталог
// Path в Name в пределах того же родителя.
type ShareRenameParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Path  string `json:"path"` // переименуемый каталог; корень шары нельзя
	Name  string `json:"name"` // новое имя (не путь)
}

// Validate проверяет значения до обращения к файловой системе.
func (p ShareRenameParams) Validate() error {
	if err := validPoolShare(p.Pool, p.Share); err != nil {
		return err
	}
	rel, err := CleanRel(p.Path)
	if err != nil {
		return err
	}
	if rel == "" {
		return errors.New("корень папки переименовать нельзя")
	}
	if err := ValidDirName(p.Name); err != nil {
		return err
	}
	if len(path.Join(path.Dir(rel), p.Name)) > MaxDirPath {
		return errors.New("путь слишком длинный")
	}
	return nil
}

// ShareMoveParams — параметры nas.share.move: перенести каталог Path
// в каталог To той же общей папки ("" — корень), имя сохраняется.
type ShareMoveParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Path  string `json:"path"` // переносимый каталог; корень шары нельзя
	To    string `json:"to"`   // новый родитель относительно шары; "" — корень
}

// Validate проверяет значения до обращения к файловой системе: перенос в
// себя или в свой подкаталог — отказ (ядро тоже отвергает его EINVAL).
// Имя не меняется и правилам Windows не подвергается: переносить
// можно и существующие каталоги с несовместимыми именами.
func (p ShareMoveParams) Validate() error {
	if err := validPoolShare(p.Pool, p.Share); err != nil {
		return err
	}
	rel, err := CleanRel(p.Path)
	if err != nil {
		return err
	}
	if rel == "" {
		return errors.New("корень папки перенести нельзя")
	}
	to, err := CleanRel(p.To)
	if err != nil {
		return fmt.Errorf("целевой каталог: %v", err)
	}
	if to == rel || strings.HasPrefix(to, rel+"/") {
		return errors.New("нельзя перенести папку в саму себя или в её подкаталог")
	}
	_, name := splitRel(rel)
	if len(path.Join(to, name)) > MaxDirPath {
		return errors.New("путь слишком длинный")
	}
	return nil
}

// ShareRmdirParams — параметры nas.share.rmdir: удалить пустой каталог Path.
type ShareRmdirParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Path  string `json:"path"` // удаляемый каталог; корень шары нельзя
}

// Validate проверяет значения до обращения к файловой системе.
func (p ShareRmdirParams) Validate() error {
	if err := validPoolShare(p.Pool, p.Share); err != nil {
		return err
	}
	rel, err := CleanRel(p.Path)
	if err != nil {
		return err
	}
	if rel == "" {
		return errors.New("корень папки удалить нельзя")
	}
	return nil
}

// ShareDirOpResult — ответ nas.share.mkdir / rename / rmdir.
type ShareDirOpResult struct {
	Path    string `json:"path"`    // итоговый путь относительно шары (для rmdir — удалённый)
	Changed bool   `json:"changed"` // false — rename в то же имя
}

// Ошибки операций над каталогами: ErrInvalid — параметры; ErrNotFound —
// каталога (или компонента пути) нет; ErrRefused — пул не смонтирован,
// компонент пути или цель — ссылка/файл, ФС без RENAME_NOREPLACE;
// ErrConflict — имя занято, каталог не пуст. Прочее — ошибка без обёртки.

// splitRel делит проверенный относительный путь на родителя и имя.
func splitRel(rel string) (parent, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

// shareBase — путь каталога шары после проверки, что пул смонтирован
// (иначе каталог оказался бы на корневой ФС).
func (e *Executor) shareBase(pool, share string) (string, error) {
	if !e.poolMounted(pool) {
		return "", fmt.Errorf("%w: пул %s не смонтирован", ErrRefused, pool)
	}
	return ShareDir(e.MountRoot, pool, share), nil
}

// ShareMkdir создаёт каталог root:gamayun-users 2770. Все обращения —
// *at-вызовы от дескриптора каталога шары, каждый компонент открывается с
// O_NOFOLLOW: ссылки (внутрь и наружу) и «..» не проходят.
// При сбое после mkdir созданный каталог удаляется (состояние как до вызова).
func (e *Executor) ShareMkdir(_ context.Context, p ShareMkdirParams) (ShareDirOpResult, error) {
	if err := p.Validate(); err != nil {
		return ShareDirOpResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	base, err := e.shareBase(p.Pool, p.Share)
	if err != nil {
		return ShareDirOpResult{}, err
	}
	rel := path.Join(p.Path, p.Name)
	chown := e.DirChown
	if chown == nil {
		chown = e.defaultDirChown
	}
	if err := dirMkdir(base, p.Path, p.Name, chown); err != nil {
		return ShareDirOpResult{}, err
	}
	return ShareDirOpResult{Path: rel, Changed: true}, nil
}

// ShareRename переименовывает каталог в пределах родителя атомарно, без
// перезаписи (renameat2 RENAME_NOREPLACE): занятое имя — ErrConflict.
func (e *Executor) ShareRename(_ context.Context, p ShareRenameParams) (ShareDirOpResult, error) {
	if err := p.Validate(); err != nil {
		return ShareDirOpResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	base, err := e.shareBase(p.Pool, p.Share)
	if err != nil {
		return ShareDirOpResult{}, err
	}
	parent, old := splitRel(p.Path)
	changed, err := dirRename(base, parent, old, p.Name)
	if err != nil {
		return ShareDirOpResult{}, err
	}
	return ShareDirOpResult{Path: path.Join(parent, p.Name), Changed: changed}, nil
}

// ShareMove переносит каталог в другой каталог той же шары атомарно, без
// перезаписи (renameat2 RENAME_NOREPLACE между дескрипторами родителей,
// оба пути открыты *at + O_NOFOLLOW): занятое имя — ErrConflict, перенос в
// тот же родитель — Changed=false.
func (e *Executor) ShareMove(_ context.Context, p ShareMoveParams) (ShareDirOpResult, error) {
	if err := p.Validate(); err != nil {
		return ShareDirOpResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	base, err := e.shareBase(p.Pool, p.Share)
	if err != nil {
		return ShareDirOpResult{}, err
	}
	parent, name := splitRel(p.Path)
	changed, err := dirMove(base, parent, name, p.To)
	if err != nil {
		return ShareDirOpResult{}, err
	}
	return ShareDirOpResult{Path: path.Join(p.To, name), Changed: changed}, nil
}

// ShareRmdir удаляет только пустой каталог (rmdir, не рекурсивно).
func (e *Executor) ShareRmdir(_ context.Context, p ShareRmdirParams) (ShareDirOpResult, error) {
	if err := p.Validate(); err != nil {
		return ShareDirOpResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	base, err := e.shareBase(p.Pool, p.Share)
	if err != nil {
		return ShareDirOpResult{}, err
	}
	parent, name := splitRel(p.Path)
	if err := dirRmdir(base, parent, name); err != nil {
		return ShareDirOpResult{}, err
	}
	return ShareDirOpResult{Path: p.Path, Changed: true}, nil
}

// defaultDirChown — владелец root, группа gamayun-users (по открытому fd).
func (e *Executor) defaultDirChown(fd int) error {
	gid, err := groupID(or(e.GroupFile, "/etc/group"), UnixGroup)
	if err != nil {
		return err
	}
	return fchown(fd, 0, gid)
}

// groupID ищет gid группы в файле формата /etc/group.
func groupID(file, group string) (int, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 3 && f[0] == group {
			gid, err := strconv.Atoi(f[2])
			if err != nil || gid < 0 {
				return 0, fmt.Errorf("группа %s: неверный gid %q", group, f[2])
			}
			return gid, nil
		}
	}
	return 0, fmt.Errorf("группа %s не найдена", group)
}
