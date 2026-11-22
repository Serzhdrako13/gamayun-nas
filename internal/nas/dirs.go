package nas

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// OpShareDirs — операция helper: список вложенных каталогов общей папки.
const OpShareDirs = "nas.share.dirs"

// Пределы выдачи дерева папок.
const (
	MaxDirDepth   = 3    // глубина «по запросу»
	MaxDirEntries = 500  // записей на один каталог
	MaxDirTotal   = 2000 // записей всего в ответе
	MaxDirPath    = 1024 // длина относительного пути
)

// ShareDirsParams — параметры nas.share.dirs. Путь только относительный,
// внутри каталога шары <mount_root>/<пул>/shares/<шара>.
type ShareDirsParams struct {
	Pool  string `json:"pool"`
	Share string `json:"share"`
	Path  string `json:"path"`  // "" — корень шары; иначе a/b/c
	Depth int    `json:"depth"` // 1..MaxDirDepth
}

// Validate проверяет значения до обращения к файловой системе.
func (p ShareDirsParams) Validate() error {
	if !poolRe.MatchString(p.Pool) {
		return errors.New("недопустимое имя пула")
	}
	if !ValidShare(p.Share) {
		return errors.New("недопустимое имя папки")
	}
	if p.Depth < 1 || p.Depth > MaxDirDepth {
		return fmt.Errorf("глубина должна быть 1..%d", MaxDirDepth)
	}
	_, err := CleanRel(p.Path)
	return err
}

// CleanRel разбирает относительный путь внутри шары. Отвергает абсолютные
// пути, «..», пустые и «.» компоненты, обратные слэши, управляющие символы.
func CleanRel(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if len(p) > MaxDirPath {
		return "", errors.New("путь слишком длинный")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", errors.New("недопустимый символ в пути")
		}
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return "", errors.New("путь должен быть относительным, без / по краям")
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." {
			return "", errors.New("недопустимый компонент пути")
		}
	}
	return p, nil
}

// DirNode — каталог в дереве; Children == nil — глубже не запрашивалось.
type DirNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"` // относительно шары
	Children []*DirNode `json:"children"`
	Partial  bool       `json:"partial,omitempty"` // часть записей отброшена пределом
}

// ShareDirsResult — ответ nas.share.dirs.
type ShareDirsResult struct {
	Path    string     `json:"path"`
	Dirs    []*DirNode `json:"dirs"`
	Partial bool       `json:"partial,omitempty"`
	Missing bool       `json:"missing,omitempty"` // запрошенного каталога нет (или это не каталог)
}

// ShareDirs перечисляет только каталоги. Обход идёт через os.Root: ссылки,
// ведущие за пределы каталога шары, и «..» не проходят на уровне ядра.
// Дополнительно ссылки внутри шары не показываются и не раскрываются.
func (e *Executor) ShareDirs(_ context.Context, p ShareDirsParams) (ShareDirsResult, error) {
	rel, err := CleanRel(p.Path)
	if err != nil {
		return ShareDirsResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	base := ShareDir(e.MountRoot, p.Pool, p.Share)
	root, err := os.OpenRoot(base)
	if err != nil {
		return ShareDirsResult{}, fmt.Errorf("%w: каталог папки недоступен", ErrRefused)
	}
	defer root.Close()
	// Каждый компонент запрошенного пути — настоящий каталог, не ссылка.
	cur := ""
	if rel != "" {
		for _, c := range strings.Split(rel, "/") {
			cur = path.Join(cur, c)
			st, err := root.Lstat(cur)
			if err != nil || !st.IsDir() { // нет, файл или ссылка (Lstat не следует за ссылкой)
				return ShareDirsResult{Path: rel, Dirs: []*DirNode{}, Missing: true}, nil
			}
		}
	}
	res := ShareDirsResult{Path: rel, Dirs: []*DirNode{}}
	total := 0
	var walk func(dir string, depth int) ([]*DirNode, bool)
	walk = func(dir string, depth int) ([]*DirNode, bool) {
		open := dir
		if open == "" {
			open = "."
		}
		f, err := root.Open(open)
		if err != nil {
			return []*DirNode{}, false
		}
		ents, _ := f.ReadDir(-1)
		f.Close()
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		out := []*DirNode{}
		partial := false
		for _, de := range ents {
			if de.Type()&fs.ModeSymlink != 0 || !de.IsDir() {
				continue
			}
			if len(out) >= MaxDirEntries || total >= MaxDirTotal {
				partial = true
				break
			}
			total++
			n := &DirNode{Name: de.Name(), Path: path.Join(dir, de.Name())}
			if depth > 1 {
				var pp bool
				n.Children, pp = walk(n.Path, depth-1)
				n.Partial = pp
			}
			out = append(out, n)
		}
		return out, partial
	}
	res.Dirs, res.Partial = walk(rel, p.Depth)
	return res, nil
}
