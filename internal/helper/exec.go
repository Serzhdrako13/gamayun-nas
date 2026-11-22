package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrTimeout — внешняя команда не уложилась в таймаут и была остановлена.
var ErrTimeout = errors.New("превышено время выполнения команды")

// ErrToolMissing — утилиты нет в системе (не установлен пакет).
var ErrToolMissing = errors.New("не установлена утилита")

// toolPackages — пакет Debian для утилит, которых может не быть на чистой
// установке (сообщение оператору; зависимости пакета gamayun-nas их ставят).
var toolPackages = map[string]string{
	"sgdisk": "gdisk", "mdadm": "mdadm", "lvm": "lvm2", "dmsetup": "dmsetup",
	"integritysetup": "cryptsetup-bin", "mkfs.btrfs": "btrfs-progs", "btrfs": "btrfs-progs",
	"smartctl": "smartmontools", "wipefs": "util-linux", "blkdiscard": "util-linux",
	"partx": "util-linux", "blkid": "util-linux", "smbpasswd": "samba-common-bin",
	"smbcontrol": "samba-common-bin", "smbstatus": "samba", "testparm": "samba-common-bin",
	"fail2ban-client": "fail2ban",
}

// missingTool — понятная ошибка «нет утилиты» вместо fork/exec.
func missingTool(path string) error {
	name := filepath.Base(path)
	if pkg := toolPackages[name]; pkg != "" {
		return fmt.Errorf("%w %s (пакет %s)", ErrToolMissing, name, pkg)
	}
	return fmt.Errorf("%w %s", ErrToolMissing, path)
}

// MaxCommandTimeout — верхняя граница таймаута одной команды.
const MaxCommandTimeout = 30 * time.Minute

// CommandResult — итог внешней команды.
type CommandResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool // вывод обрезан по MaxOutput
}

// Runner — единственная точка запуска внешних программ в helper.
//
// Правила: путь к программе абсолютный и нормализованный, аргументы —
// массив (без shell, без подстановок), окружение фиксированное, таймаут
// обязателен; по таймауту или отмене завершается вся группа процессов.
// Операции реестра формируют аргументы сами из проверенных параметров —
// строка команды от клиента сюда не попадает.
type Runner struct {
	MaxOutput int      // байт на stdout и stderr каждый; 0 — 1 МиБ
	Env       []string // nil — DefaultEnv
}

// DefaultEnv — окружение внешних команд: предсказуемая локаль для разбора вывода.
var DefaultEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C"}

// Run запускает path с аргументами args. Ненулевой код выхода не ошибка:
// он возвращается в CommandResult.ExitCode, решение — за операцией.
func (r *Runner) Run(ctx context.Context, timeout time.Duration, path string, args ...string) (CommandResult, error) {
	return r.RunInput(ctx, timeout, nil, path, args...)
}

// RunInput — Run с данными на стандартном входе (пароль smbpasswd -s).
func (r *Runner) RunInput(ctx context.Context, timeout time.Duration, stdin []byte, path string, args ...string) (CommandResult, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return CommandResult{}, fmt.Errorf("путь к программе должен быть абсолютным и нормализованным: %q", path)
	}
	if timeout <= 0 || timeout > MaxCommandTimeout {
		return CommandResult{}, fmt.Errorf("таймаут должен быть в (0, %s]: %s", MaxCommandTimeout, timeout)
	}
	for _, a := range args {
		if strings.IndexByte(a, 0) >= 0 {
			return CommandResult{}, errors.New("аргумент содержит NUL")
		}
	}
	max := r.MaxOutput
	if max <= 0 {
		max = 1 << 20
	}
	env := r.Env
	if env == nil {
		env = DefaultEnv
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return CommandResult{ExitCode: -1}, missingTool(path)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Dir = "/"
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := &capBuffer{max: max}
	stderr := &capBuffer{max: max}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	setProcessGroup(cmd)

	err := cmd.Run()
	res := CommandResult{
		ExitCode:  -1,
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("%w (%s): %s", ErrTimeout, timeout, path)
		}
		return res, ctx.Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return res, err
	}
	return res, nil
}

// capBuffer хранит не более max байт, остальное отбрасывает. bytes.Buffer
// не встраивается: иначе io.Copy взял бы его ReadFrom в обход лимита.
type capBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *capBuffer) Bytes() []byte { return b.buf.Bytes() }
