package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"gamayun-nas/internal/auth"
	"gamayun-nas/internal/config"
	"gamayun-nas/internal/events"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/store"
)

const adminUsage = `Использование:
  gamayund admin create -name <имя> [-config файл] [-db база]   создать администратора
  gamayund admin passwd -name <имя> [-config файл] [-db база]   сменить пароль, отозвать все сессии
  gamayund admin list [-config файл] [-db база]                 список учётных записей

Пароль читается со стандартного ввода: с терминала — без эха и дважды,
иначе — первая строка (например, из файла с правами 0600).
Запускать от владельца базы: sudo -u gamayun gamayund admin ...
`

// runAdmin — подкоманда управления администраторами (первичная настройка
// и восстановление доступа с локальной консоли).
func runAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("gamayund admin "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	confPath := fs.String("config", "", "файл конфигурации JSON")
	dbPath := fs.String("db", "", "файл SQLite, переопределяет database")
	name := fs.String("name", "", "имя учётной записи")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "лишние аргументы: %v\n", fs.Args())
		return 2
	}
	cfg := config.Default()
	if *confPath != "" {
		var err error
		if cfg, err = config.Load(*confPath); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if *dbPath != "" {
		cfg.Database = *dbPath
	}
	if err := checkOwner(cfg.Database); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	err := func() error {
		switch cmd {
		case "create", "passwd":
			if *name == "" {
				return errors.New("нужен -name")
			}
			if cmd == "create" {
				if err := auth.ValidateName(*name); err != nil {
					return err
				}
			}
			db, err := store.Open(ctx, cfg.Database)
			if err != nil {
				return err
			}
			defer db.Close()
			st := auth.Store{DB: db}
			var u auth.User
			if cmd == "passwd" {
				if u, err = st.UserByName(ctx, *name); err != nil {
					return fmt.Errorf("%s: %w", *name, err)
				}
			} else if _, err := st.UserByName(ctx, *name); err == nil {
				return fmt.Errorf("%s: %w", *name, auth.ErrUserExists)
			}
			pw, err := readPassword(stdin, stderr, *name)
			if err != nil {
				return err
			}
			hash, err := auth.HashPassword(pw, auth.DefaultParams)
			if err != nil {
				return err
			}
			evlog := events.NewLog(db)
			now := time.Now()
			if cmd == "create" {
				if u, err = st.CreateUser(ctx, *name, hash, true, false, now); err != nil {
					return err
				}
				emit(ctx, evlog, auth.EventUserCreated, map[string]any{"user": u.Name, "admin": true, "via": "cli"})
				fmt.Fprintf(stdout, "администратор %s создан\n", u.Name)
				syncOSPassword(ctx, cfg, db, u.Name, pw, stdout, stderr)
				// Первичная настройка выполнена: токен установки больше не нужен
				// (демон удалит файл при следующем запуске или первой попытке).
				return nil
			}
			if err := st.SetPassword(ctx, u.ID, hash, now); err != nil {
				return err
			}
			n, err := st.RevokeSessions(ctx, u.ID, nil)
			if err != nil {
				return err
			}
			emit(ctx, evlog, auth.EventPasswordChanged, map[string]any{"user": u.Name, "via": "cli", "revoked_sessions": n})
			fmt.Fprintf(stdout, "пароль %s изменён, отозвано сессий: %d\n", u.Name, n)
			syncOSPassword(ctx, cfg, db, u.Name, pw, stdout, stderr)
			// Сброс забытого пароля с консоли. Временная блокировка
			// входа по перебору хранится в памяти gamayund и истекает сама.
			if u.Disabled {
				fmt.Fprintf(stderr, "предупреждение: учётная запись %s отключена — вход по-прежнему невозможен; включите её другим администратором\n", u.Name)
			}
			fmt.Fprintln(stdout, "если вход временно заблокирован после неудачных попыток — дождитесь снятия блокировки или перезапустите gamayund: systemctl restart gamayund")
			return nil
		case "list":
			db, err := store.Open(ctx, cfg.Database)
			if err != nil {
				return err
			}
			defer db.Close()
			users, err := auth.Store{DB: db}.Users(ctx)
			if err != nil {
				return err
			}
			if len(users) == 0 {
				fmt.Fprintln(stdout, "учётных записей нет; создайте: gamayund admin create -name <имя>")
			}
			for _, u := range users {
				role := "пользователь"
				if u.IsAdmin {
					role = "администратор"
				}
				state := ""
				if u.Disabled {
					state = " (отключена)"
				}
				fmt.Fprintf(stdout, "%s\t%s%s\tсоздана %s\n", u.Name, role, state, u.CreatedAt.Local().Format("2006-01-02 15:04"))
			}
			return nil
		default:
			fmt.Fprint(stderr, adminUsage)
			return errors.New("неизвестная команда " + cmd)
		}
	}()
	if err != nil {
		fmt.Fprintln(stderr, "gamayund admin:", err)
		return 1
	}
	return 0
}

// checkOwner не даёт root создать или изменить базу, принадлежащую другому
// пользователю: файлы WAL, созданные root, демон (gamayun) не сможет писать.
func checkOwner(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("запуск от root: база %s недоступна (%v); запустите от владельца базы: sudo -u gamayun gamayund admin ...", path, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
		return fmt.Errorf("запуск от root, а база %s принадлежит uid %d; запустите от её владельца: sudo -u gamayun gamayund admin ...", path, st.Uid)
	}
	return nil
}

func emit(ctx context.Context, l *events.Log, typ string, data map[string]any) {
	b, _ := json.Marshal(data)
	l.Emit(ctx, events.Event{Type: typ, Data: b}, true)
}

// readPassword — с терминала без эха и с подтверждением; иначе первая строка.
func readPassword(stdin io.Reader, stderr io.Writer, name string) (string, error) {
	var pw string
	if f, ok := stdin.(*os.File); ok && isTerminal(int(f.Fd())) {
		p1, err := readNoEcho(int(f.Fd()), stderr, "Новый пароль для "+name+": ")
		if err != nil {
			return "", err
		}
		if err := auth.ValidatePassword(p1, name); err != nil {
			return "", err
		}
		p2, err := readNoEcho(int(f.Fd()), stderr, "Повторите пароль: ")
		if err != nil {
			return "", err
		}
		if p1 != p2 {
			return "", errors.New("пароли не совпадают")
		}
		pw = p1
	} else {
		line, err := bufio.NewReader(io.LimitReader(stdin, auth.MaxPasswordBytes+2)).ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && line != "") {
			return "", errors.New("пароль не прочитан со стандартного ввода")
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if err := auth.ValidatePassword(pw, name); err != nil {
		return "", err
	}
	return pw, nil
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func readNoEcho(fd int, stderr io.Writer, prompt string) (string, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	t := *old
	t.Lflag &^= unix.ECHO
	t.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &t); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
	fmt.Fprint(stderr, prompt)
	defer fmt.Fprintln(stderr)
	var buf [auth.MaxPasswordBytes + 2]byte
	n, err := unix.Read(fd, buf[:])
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(buf[:n]), "\r\n"), nil
}

// osPasswordSetter — синхронизация пароля учётки NAS (ОС + Samba) через
// gamayun-helper; подменяется в тестах.
var osPasswordSetter = func(cfg config.Config, db *sql.DB) interface {
	SyncAfterWeb(ctx context.Context, name, password string) error
} {
	n := nas.NewService(&helper.Client{Socket: cfg.HelperSocket, Timeout: time.Minute}, db)
	// Веб-учётки — как в демоне: Sync сразу создаёт пользователя NAS из
	// администратора веба (раньше — только после перезапуска gamayund).
	n.Web = auth.Accounts{S: auth.New(db, auth.Options{})}
	return n
}

// syncOSPassword — тот же пароль для SMB, SSH и sudo, что и для веба
// (единая учётка). Helper недоступен — предупреждение, а не ошибка:
// CLI — средство восстановления входа в веб; пароль ОС/Samba обновится
// при следующей смене пароля в вебе.
func syncOSPassword(ctx context.Context, cfg config.Config, db *sql.DB, name, pw string, stdout, stderr io.Writer) {
	if err := osPasswordSetter(cfg, db).SyncAfterWeb(ctx, name, pw); err != nil {
		fmt.Fprintf(stderr, "предупреждение: пароль SMB/ОС для %s не обновлён (%v); повторите смену пароля в вебе\n", name, err)
		return
	}
	fmt.Fprintf(stdout, "пароль SMB/ОС для %s обновлён\n", name)
}
