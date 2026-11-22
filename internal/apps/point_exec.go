package apps

// Точки восстановления приложения для обновления из
// каталога. Точка — ro-снимок субтома appdata/<id> и копия прежних файлов
// проекта (*.prev после записи с резервной копией):
//
//	<пул>/.apps/points/<id>/<имя>.data   ro-снимок Btrfs
//	<пул>/.apps/points/<id>/<имя>.files/ compose.user.yaml, user.env, compose.json, secrets-*.env (0600)
//
// Все операции идемпотентны: повтор после прерывания (сбой питания,
// перезапуск gamayund) доводит действие до конца.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Операции точек восстановления.
const (
	OpPointCreate  = "apps.point.create"
	OpPointRestore = "apps.point.restore"
	OpPointDelete  = "apps.point.delete"
	OpPointList    = "apps.point.list"
	PointsDir      = "points"
	pointData      = ".data"
	pointFiles     = ".files"
	oldAppdataPfx  = ".old-" // appdata/.old-<id>: прежний субтом на время восстановления
)

var pointRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// PointParams — apps.point.create/restore/delete/list (Name — кроме list).
type PointParams struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Data — restore: вернуть и данные (иначе только файлы проекта).
	Data bool `json:"data,omitempty"`
}

// Validate проверяет идентификатор и имя точки.
func (p PointParams) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("недопустимый идентификатор приложения")
	}
	if !pointRe.MatchString(p.Name) {
		return errors.New("недопустимое имя точки восстановления")
	}
	return nil
}

// PointListParams — apps.point.list.
type PointListParams struct {
	ID string `json:"id"`
}

// Validate проверяет идентификатор.
func (p PointListParams) Validate() error {
	if !ValidID(p.ID) {
		return errors.New("недопустимый идентификатор приложения")
	}
	return nil
}

// PointResult — итог операций с точками.
type PointResult struct {
	Points []string `json:"points"`
}

// pointPaths — каталог точек приложения, субтом данных, каталог проекта.
func (e *Executor) pointPaths(id string) (points, appdata, project string, err error) {
	s, err := e.loadState()
	if err != nil {
		return "", "", "", err
	}
	if s.Pool == "" {
		return "", "", "", refuse("приложения не настроены")
	}
	if err := e.needPoolRW(s.Pool); err != nil {
		return "", "", "", err
	}
	root := e.appsRoot(s.Pool)
	return filepath.Join(root, PointsDir, id), filepath.Join(root, AppdataDir, id), filepath.Join(root, ProjectsDir, id), nil
}

// copyPrivate копирует файл в 0600 (атомарно).
func copyPrivate(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writePrivate(dst, b)
}

// PointCreate — ro-снимок данных и копия прежних файлов проекта (*.prev).
// Приложение должно быть остановлено (данные согласованы). Повтор с тем же
// именем пересоздаёт точку.
func (e *Executor) PointCreate(ctx context.Context, p PointParams) (PointResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	points, appdata, project, err := e.pointPaths(p.ID)
	if err != nil {
		return PointResult{}, err
	}
	if sv, err := e.isSubvol(appdata); err != nil || !sv {
		return PointResult{}, refuse("нет субтома данных приложения %s", appdata)
	}
	for _, f := range projectFiles {
		if _, err := os.Stat(filepath.Join(project, f+prevSuffix)); err != nil {
			return PointResult{}, refuse("нет прежней версии файла %s", f)
		}
	}
	if err := ensureDir0700(filepath.Dir(points)); err != nil {
		return PointResult{}, err
	}
	if err := ensureDir0700(points); err != nil {
		return PointResult{}, err
	}
	if err := e.dropPoint(ctx, points, p.Name); err != nil {
		return PointResult{}, err
	}
	files := filepath.Join(points, p.Name+pointFiles)
	if err := os.Mkdir(files, 0o700); err != nil {
		return PointResult{}, err
	}
	sec, err := secretFiles(project, prevSuffix)
	if err != nil {
		return PointResult{}, err
	}
	for _, f := range projectFiles {
		sec = append(sec, f+prevSuffix)
	}
	for _, f := range sec {
		if err := copyPrivate(filepath.Join(project, f), filepath.Join(files, strings.TrimSuffix(f, prevSuffix))); err != nil {
			return PointResult{}, err
		}
	}
	if _, err := e.run(ctx, 5*time.Minute, e.Paths.Btrfs, "subvolume", "snapshot", "-r", appdata, filepath.Join(points, p.Name+pointData)); err != nil {
		os.RemoveAll(files)
		return PointResult{}, err
	}
	return e.listPoints(points)
}

// dropPoint удаляет точку (снимок и файлы), если она есть.
func (e *Executor) dropPoint(ctx context.Context, points, name string) error {
	data := filepath.Join(points, name+pointData)
	if sv, err := e.isSubvol(data); err == nil {
		if !sv {
			return refuse("%s не субтом", data)
		}
		if _, err := e.run(ctx, 5*time.Minute, e.Paths.Btrfs, "property", "set", "-ts", data, "ro", "false"); err != nil {
			return err
		}
		if _, err := e.run(ctx, 5*time.Minute, e.Paths.Btrfs, "subvolume", "delete", data); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(filepath.Join(points, name+pointFiles))
}

func (e *Executor) listPoints(points string) (PointResult, error) {
	res := PointResult{Points: []string{}}
	l, err := os.ReadDir(points)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	for _, d := range l {
		if n, ok := strings.CutSuffix(d.Name(), pointData); ok && pointRe.MatchString(n) {
			if _, err := os.Stat(filepath.Join(points, n+pointFiles)); err == nil {
				res.Points = append(res.Points, n)
			}
		}
	}
	sort.Strings(res.Points)
	return res, nil
}

// PointRestore возвращает файлы проекта из точки и (Data) субтом данных:
// прежний субтом → appdata/.old-<id>, rw-снимок точки → appdata/<id>,
// прежний удаляется. Приложение должно быть остановлено.
func (e *Executor) PointRestore(ctx context.Context, p PointParams) (PointResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	points, appdata, project, err := e.pointPaths(p.ID)
	if err != nil {
		return PointResult{}, err
	}
	data, files := filepath.Join(points, p.Name+pointData), filepath.Join(points, p.Name+pointFiles)
	if sv, err := e.isSubvol(data); err != nil || !sv {
		return PointResult{}, refuse("нет точки восстановления %s", p.Name)
	}
	for _, f := range projectFiles {
		if _, err := os.Stat(filepath.Join(files, f)); err != nil {
			return PointResult{}, refuse("в точке %s нет файла %s", p.Name, f)
		}
	}
	// Файлы: текущие секреты убрать, файлы точки записать.
	if err := ensureDir0700(project); err != nil {
		return PointResult{}, err
	}
	cur, err := secretFiles(project, "")
	if err != nil {
		return PointResult{}, err
	}
	keep := map[string]bool{}
	saved, err := secretFiles(files, "")
	if err != nil {
		return PointResult{}, err
	}
	for _, f := range append(append([]string{}, projectFiles...), saved...) {
		keep[f] = true
		if err := copyPrivate(filepath.Join(files, f), filepath.Join(project, f)); err != nil {
			return PointResult{}, err
		}
	}
	for _, f := range cur {
		if !keep[f] {
			if err := os.Remove(filepath.Join(project, f)); err != nil {
				return PointResult{}, err
			}
		}
	}
	if !p.Data {
		return e.listPoints(points)
	}
	old := filepath.Join(filepath.Dir(appdata), oldAppdataPfx+p.ID)
	_, errOld := os.Lstat(old)
	sv, errCur := e.isSubvol(appdata)
	switch {
	case errCur == nil && !sv:
		return PointResult{}, refuse("%s не субтом", appdata)
	case errCur == nil && errors.Is(errOld, os.ErrNotExist):
		if err := os.Rename(appdata, old); err != nil {
			return PointResult{}, err
		}
	case errCur == nil:
		// Прервано после создания нового субтома: остался только прежний.
		if _, err := e.purge(ctx, old); err != nil {
			return PointResult{}, err
		}
		if err := os.Rename(appdata, old); err != nil {
			return PointResult{}, err
		}
	case !errors.Is(errCur, os.ErrNotExist):
		return PointResult{}, errCur
	}
	if _, err := e.run(ctx, 5*time.Minute, e.Paths.Btrfs, "subvolume", "snapshot", data, appdata); err != nil {
		return PointResult{}, fmt.Errorf("восстановление данных из точки %s: %w", p.Name, err)
	}
	if _, err := os.Lstat(old); err == nil {
		if _, err := e.purge(ctx, old); err != nil {
			return PointResult{}, fmt.Errorf("данные восстановлены, прежний субтом %s не удалён: %w", old, err)
		}
	}
	return e.listPoints(points)
}

// PointDelete удаляет точку (идемпотентна).
func (e *Executor) PointDelete(ctx context.Context, p PointParams) (PointResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	points, _, _, err := e.pointPaths(p.ID)
	if err != nil {
		return PointResult{}, err
	}
	if err := e.dropPoint(ctx, points, p.Name); err != nil {
		return PointResult{}, err
	}
	return e.listPoints(points)
}

// PointList — точки приложения (по имени, по возрастанию).
func (e *Executor) PointList(_ context.Context, p PointListParams) (PointResult, error) {
	points, _, _, err := e.pointPaths(p.ID)
	if err != nil {
		return PointResult{}, err
	}
	return e.listPoints(points)
}

// dropAllPoints — все точки приложения (удаление вместе с данными).
func (e *Executor) dropAllPoints(ctx context.Context, points string) error {
	l, err := os.ReadDir(points)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, d := range l {
		if n, ok := strings.CutSuffix(d.Name(), pointData); ok {
			if err := e.dropPoint(ctx, points, n); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(points); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
