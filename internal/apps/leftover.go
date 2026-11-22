package apps

// Остатки контейнеров после остановки Docker.
//
// containerd работает с KillMode=process (иначе перезапуск containerd убивал
// бы контейнеры), а процессы контейнеров живут в своих docker-<id>.scope.
// Если dockerd не остановил контейнер сам (dockerd упал или был убит, пока
// containerd и shim работали), systemctl stop обеих служб оставляет shim и
// процессы контейнера — у них свои пространства имён с bind-монтированиями
// пула. Пул после этого не отмонтировать («target is busy»), а после
// аварийного RO Btrfs суперблок остаётся живым и mount rw не проходит
// (проверено на стенде). Поэтому после остановки: найти остатки, остановить их
// (TERM, затем KILL) и проверить, что пул ничем не занят; иначе — ошибка с
// причиной.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gamayun-nas/internal/storage/home"
)

// Сроки остановки остатков (тесты сокращают).
var (
	leftoverTerm = 10 * time.Second
	leftoverKill = 5 * time.Second
)

// ShimName — имя shim containerd в пакете gamayun-nas-apps.
const ShimName = "containerd-shim-runc-v2"

var scopeRe = regexp.MustCompile(`(?m)/(docker-[0-9a-f]{12,64}\.scope)(/|$)`)

// leftoverProc — процесс контейнера или shim вне остановленных служб.
type leftoverProc struct {
	pid   int
	comm  string
	scope string // docker-<id>.scope; пусто — shim
}

func (p leftoverProc) String() string {
	if p.scope != "" {
		return fmt.Sprintf("%d %s (%s)", p.pid, p.comm, p.scope)
	}
	return fmt.Sprintf("%d %s", p.pid, p.comm)
}

func (e *Executor) pids() []int {
	ents, err := os.ReadDir(e.Paths.Proc)
	if err != nil {
		return nil
	}
	var out []int
	for _, d := range ents {
		if n, err := strconv.Atoi(d.Name()); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func (e *Executor) procFile(pid int, name string) string {
	return filepath.Join(e.Paths.Proc, strconv.Itoa(pid), name)
}

func (e *Executor) comm(pid int) string {
	b, _ := os.ReadFile(e.procFile(pid, "comm"))
	return strings.TrimSpace(string(b))
}

// leftovers — процессы в docker-*.scope и shim пакета (по пути исполняемого
// файла). Пустой Paths.Proc — проверка отключена.
func (e *Executor) leftovers() []leftoverProc {
	if e.Paths.Proc == "" {
		return nil
	}
	shim := filepath.Join(filepath.Dir(e.Paths.Dockerd), ShimName)
	var out []leftoverProc
	for _, pid := range e.pids() {
		if cg, err := os.ReadFile(e.procFile(pid, "cgroup")); err == nil {
			if m := scopeRe.FindSubmatch(cg); m != nil {
				out = append(out, leftoverProc{pid: pid, comm: e.comm(pid), scope: string(m[1])})
				continue
			}
		}
		if exe, err := os.Readlink(e.procFile(pid, "exe")); err == nil && strings.TrimSuffix(exe, " (deleted)") == shim {
			out = append(out, leftoverProc{pid: pid, comm: e.comm(pid)})
		}
	}
	return out
}

func (e *Executor) kill(pid int, sig syscall.Signal) error {
	if e.Kill != nil {
		return e.Kill(pid, sig)
	}
	return syscall.Kill(pid, sig)
}

// reapLeftovers останавливает оставшиеся процессы контейнеров и shim: TERM,
// ожидание, KILL. Возвращает то, что было найдено (для журнала).
func (e *Executor) reapLeftovers(ctx context.Context) []leftoverProc {
	found := e.leftovers()
	if len(found) == 0 {
		return nil
	}
	wait := func(d time.Duration) bool {
		end := time.Now().Add(d)
		for {
			if len(e.leftovers()) == 0 {
				return true
			}
			if time.Now().After(end) || ctx.Err() != nil {
				return false
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	scopes := map[string]bool{}
	for _, p := range found {
		if p.scope != "" {
			scopes[p.scope] = true
		}
		e.kill(p.pid, syscall.SIGTERM)
	}
	if !wait(leftoverTerm) {
		for _, p := range e.leftovers() {
			e.kill(p.pid, syscall.SIGKILL)
		}
		wait(leftoverKill)
	}
	// Пустые scope systemd убирает сам; оставшиеся — остановить (без ошибки:
	// итог проверяет poolBusy).
	if len(scopes) > 0 {
		args := []string{"stop"}
		for s := range scopes {
			args = append(args, s)
		}
		sort.Strings(args[1:])
		e.Runner.Run(ctx, 30*time.Second, nil, e.Paths.Systemctl, args...)
	}
	return found
}

// nestedMounts — монтирования внутри <пул>/.apps в пространстве имён helper
// (overlay корней контейнеров: если dockerd убит, их никто не снимает, и
// umount пула — «target is busy»). Глубокие — первыми.
func (e *Executor) nestedMounts(pool string) []string {
	if pool == "" {
		return nil
	}
	b, err := os.ReadFile(e.Paths.Mountinfo)
	if err != nil {
		return nil
	}
	apps := e.appsRoot(pool)
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 4 {
			if t := unescapeMount(f[4]); strings.HasPrefix(t, apps+"/") {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// umountNested снимает монтирования внутри <пул>/.apps (после остановки
// Docker); ошибки — в итоговую проверку poolBusy.
func (e *Executor) umountNested(ctx context.Context, pool string) []string {
	var done []string
	for _, t := range e.nestedMounts(pool) {
		if _, err := e.run(ctx, time.Minute, e.Paths.Umount, t); err == nil {
			done = append(done, t)
		}
	}
	return done
}

// poolBusy — чем занят пул приложений после остановки Docker: оставшиеся
// процессы контейнеров, монтирования тома пула в чужих пространствах имён
// (bind-монтирования контейнеров), открытые файлы, текущий или корневой
// каталог внутри <пул>/.apps. Пусто — свободен.
func (e *Executor) poolBusy(pool string) []string {
	if e.Paths.Proc == "" || pool == "" {
		return nil
	}
	var why []string
	for _, p := range e.leftovers() {
		why = append(why, "процесс контейнера "+p.String())
	}
	for _, t := range e.nestedMounts(pool) {
		why = append(why, "внутри пула смонтировано "+t)
	}
	mp := e.mountPoint(pool)
	lv, _ := filepath.EvalSymlinks(filepath.Join(e.Paths.Dev, home.VGName(pool), home.LVName))
	apps := e.appsRoot(pool)
	inApps := func(p string) bool { return p == apps || strings.HasPrefix(p, apps+"/") }
	self := os.Getpid()
	cache := map[string]string{}
	res := func(src string) string { // источник монтирования → устройство (кеш на проход)
		r, ok := cache[src]
		if !ok {
			r, _ = filepath.EvalSymlinks(src)
			cache[src] = r
		}
		return r
	}
	for _, pid := range e.pids() {
		if pid == self {
			continue
		}
		if lv != "" {
			if b, err := os.ReadFile(e.procFile(pid, "mountinfo")); err == nil {
				if t := foreignMount(b, lv, mp, res); t != "" {
					why = append(why, fmt.Sprintf("том пула смонтирован в %s в пространстве имён процесса %d %s", t, pid, e.comm(pid)))
					continue
				}
			}
		}
		for _, l := range []string{"cwd", "root"} {
			if t, err := os.Readlink(e.procFile(pid, l)); err == nil && inApps(t) {
				why = append(why, fmt.Sprintf("процесс %d %s: %s %s", pid, e.comm(pid), l, t))
			}
		}
		fds, _ := os.ReadDir(e.procFile(pid, "fd"))
		for _, fd := range fds {
			if t, err := os.Readlink(filepath.Join(e.procFile(pid, "fd"), fd.Name())); err == nil && inApps(t) {
				why = append(why, fmt.Sprintf("процесс %d %s: открыт %s", pid, e.comm(pid), t))
				break
			}
		}
	}
	return dedupLimit(why, 5)
}

// poolHolders — процессы с текущим/корневым каталогом или открытыми
// файлами на пуле (точка mp и всё внутри): из-за них umount пула — «target
// is busy». Только pid и имя процесса (без аргументов и окружения) и путь.
func (e *Executor) poolHolders(mp string) []string {
	if e.Paths.Proc == "" || mp == "" {
		return nil
	}
	in := func(p string) bool { return p == mp || strings.HasPrefix(p, mp+"/") }
	self := os.Getpid()
	var why []string
	for _, pid := range e.pids() {
		if pid == self {
			continue
		}
		for _, l := range []string{"cwd", "root"} {
			if t, err := os.Readlink(e.procFile(pid, l)); err == nil && in(t) {
				why = append(why, fmt.Sprintf("процесс %d %s: %s %s", pid, e.comm(pid), l, t))
			}
		}
		fds, _ := os.ReadDir(e.procFile(pid, "fd"))
		for _, fd := range fds {
			if t, err := os.Readlink(filepath.Join(e.procFile(pid, "fd"), fd.Name())); err == nil && in(t) {
				why = append(why, fmt.Sprintf("процесс %d %s: открыт %s", pid, e.comm(pid), t))
				break
			}
		}
	}
	return dedupLimit(why, 5)
}

// foreignMount — точка монтирования тома lv, отличная от mp и не вложенная в
// неё (вложенные — субтомы и overlay Docker внутри пула в том же
// пространстве имён); пусто — нет.
func foreignMount(b []byte, lv, mp string, resolve func(string) string) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		for i := 6; i < len(f)-2; i++ {
			if f[i] != "-" {
				continue
			}
			if src := f[i+2]; src != lv && (!strings.HasPrefix(src, "/") || resolve(src) != lv) {
				break
			}
			if t := unescapeMount(f[4]); t != mp && !strings.HasPrefix(t, mp+"/") {
				return t
			}
			break
		}
	}
	return ""
}

// unescapeMount — \040 и подобные в путях mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func dedupLimit(s []string, n int) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	if len(out) > n {
		out = append(out[:n], fmt.Sprintf("и ещё %d", len(out)-n))
	}
	return out
}
