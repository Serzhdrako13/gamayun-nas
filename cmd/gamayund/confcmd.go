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
	"time"

	"gamayun-nas/internal/auth"
	"gamayun-nas/internal/confbackup"
	"gamayun-nas/internal/config"
	"gamayun-nas/internal/helper"
	"gamayun-nas/internal/nas"
	"gamayun-nas/internal/store"
)

const configUsage = `Использование:
  gamayund config export -o <файл> [-config файл] [-db база]
        выгрузить копию настроек (без паролей и секретов)
  gamayund config save [-config файл] [-db база]
        сохранить копию во все пулы сейчас (нужен gamayun-helper)
  gamayund config list [-config файл] [-db база]
        копии в пулах (нужен gamayun-helper)
  gamayund config fetch -pool <пул> -name <файл> -o <файл>
        скопировать копию из пула в файл
  gamayund config plan (-file <файл> | -pool <пул> -name <файл>)
        проверить копию и показать, что будет восстановлено (ничего не меняет)
  gamayund config restore (-file <файл> | -pool <пул> -name <файл>) [-answers ответы.json] [-yes]
        восстановить; пароли и секреты задаются заново (мастер)

Запускать от владельца базы: sudo -u gamayun gamayund config ...
`

// confHelper — клиент gamayun-helper; подменяется в тестах.
var confHelper = func(cfg config.Config) confbackup.HelperClient {
	return &helper.Client{Socket: cfg.HelperSocket, Timeout: 4 * time.Minute}
}

func runConfig(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stderr, configUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("gamayund config "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	confPath := fs.String("config", "", "файл конфигурации JSON")
	dbPath := fs.String("db", "", "файл SQLite, переопределяет database")
	out := fs.String("o", "", "файл для записи копии")
	file := fs.String("file", "", "файл копии")
	pool := fs.String("pool", "", "пул с копией")
	name := fs.String("name", "", "имя файла копии в пуле")
	answersPath := fs.String("answers", "", "ответы мастера (JSON, права 0600) — для запуска без терминала")
	yes := fs.Bool("yes", false, "не спрашивать подтверждение")
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
		db, err := store.Open(ctx, cfg.Database)
		if err != nil {
			return err
		}
		defer db.Close()
		svc := &confbackup.Service{DB: db, Helper: confHelper(cfg)}
		load := func() (confbackup.Backup, confbackup.Data, error) {
			var data []byte
			switch {
			case *file != "" && *pool == "":
				f, err := os.Open(*file)
				if err != nil {
					return confbackup.Backup{}, confbackup.Data{}, err
				}
				defer f.Close()
				return confbackup.Decode(f, store.LatestVersion())
			case *file == "" && *pool != "" && *name != "":
				if data, err = svc.Read(ctx, *pool, *name); err != nil {
					return confbackup.Backup{}, confbackup.Data{}, err
				}
				return confbackup.DecodeBytes(data, store.LatestVersion())
			}
			return confbackup.Backup{}, confbackup.Data{}, errors.New("нужен -file или -pool и -name")
		}
		switch cmd {
		case "export":
			if *out == "" {
				return errors.New("нужен -o")
			}
			b, _, err := confbackup.Collect(ctx, db, nil, time.Now())
			if err != nil {
				return err
			}
			data, err := confbackup.Encode(b)
			if err != nil {
				return err
			}
			if err := writeNew(*out, data); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "копия настроек записана: %s (%d байт); часовой пояс не включён — выгрузка из CLI без helper\n", *out, len(data))
			return nil
		case "save":
			res, _, err := svc.SaveToPools(ctx, true)
			for _, p := range res {
				if p.Error != "" {
					fmt.Fprintf(stdout, "%s\tошибка: %s\n", p.Pool, p.Error)
				} else {
					fmt.Fprintf(stdout, "%s\t%s\tудалено старых: %d\n", p.Pool, p.Name, len(p.Removed))
				}
			}
			return err
		case "list":
			res, err := svc.List(ctx)
			if err != nil {
				return err
			}
			for _, p := range res {
				if p.Error != "" {
					fmt.Fprintf(stdout, "%s\tошибка: %s\n", p.Pool, p.Error)
					continue
				}
				for _, f := range p.Files {
					fmt.Fprintf(stdout, "%s\t%s\t%d\n", p.Pool, f.Name, f.Size)
				}
			}
			return nil
		case "fetch":
			if *out == "" || *pool == "" || *name == "" {
				return errors.New("нужны -pool, -name и -o")
			}
			data, err := svc.Read(ctx, *pool, *name)
			if err != nil {
				return err
			}
			if _, _, err := confbackup.DecodeBytes(data, store.LatestVersion()); err != nil {
				fmt.Fprintf(stderr, "предупреждение: %v\n", err)
			}
			if err := writeNew(*out, data); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "записано: %s\n", *out)
			return nil
		case "plan", "restore":
			b, d, err := load()
			if err != nil {
				return err
			}
			plan, err := confbackup.MakePlan(ctx, db, b, d)
			if err != nil {
				return err
			}
			printPlan(stdout, plan)
			if cmd == "plan" {
				return nil
			}
			var a confbackup.Answers
			if *answersPath != "" {
				if a, err = readAnswers(*answersPath); err != nil {
					return err
				}
			} else if a, err = askAnswers(stdin, stderr, plan); err != nil {
				return err
			}
			if err := plan.Check(a); err != nil {
				return err
			}
			if plan.SetupDone && !*yes {
				fmt.Fprintln(stderr, "Система уже настроена: существующие пользователи, группы и папки будут изменены по копии.")
			}
			if !*yes && !confirm(stdin, stderr, "Восстановить настройки? Введите «да»: ") {
				return errors.New("отменено")
			}
			res, err := confbackup.Apply(ctx, db, newRestoreTarget(cfg, db), d, a, "")
			if err != nil {
				return err
			}
			for _, s := range res.Steps {
				mark := "ок"
				if !s.OK {
					mark = "ОШИБКА: " + s.Error
				}
				fmt.Fprintf(stdout, "  %s — %s\n", s.What, mark)
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(stdout, "  предупреждение: %s\n", w)
			}
			if res.Error != "" {
				return fmt.Errorf("восстановление прервано: %s; исправьте причину и запустите снова — сделанное будет пропущено", res.Error)
			}
			fmt.Fprintln(stdout, "настройки восстановлены; перезапустите gamayund: systemctl restart gamayund")
			return nil
		default:
			fmt.Fprint(stderr, configUsage)
			return errors.New("неизвестная команда " + cmd)
		}
	}()
	if err != nil {
		fmt.Fprintln(stderr, "gamayund config:", err)
		return 1
	}
	return 0
}

// newRestoreTarget — сервис NAS с учётками веба, как в демоне.
var newRestoreTarget = func(cfg config.Config, db *sql.DB) confbackup.Target {
	n := nas.NewService(&helper.Client{Socket: cfg.HelperSocket, Timeout: 2 * time.Minute}, db)
	n.Web = auth.Accounts{S: auth.New(db, auth.Options{})}
	return n
}

// writeNew — файл 0600, не перезаписывая существующий.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func printPlan(w io.Writer, p confbackup.Plan) {
	fmt.Fprintf(w, "Копия от %s, версия %s (формат %d, схема %d), сервер %q\n", p.CreatedAt, p.AppVersion, p.FormatVersion, p.SchemaVersion, p.Hostname)
	for _, u := range p.Users {
		st := "создать"
		if u.Exists {
			st = "есть"
		}
		role := "пользователь"
		if u.Admin {
			role = "администратор"
		}
		fmt.Fprintf(w, "  пользователь %s (%s): %s\n", u.Name, role, st)
	}
	for _, g := range p.Groups {
		fmt.Fprintf(w, "  группа %s: %s\n", g.Name, g.Action)
	}
	for _, s := range p.Shares {
		fmt.Fprintf(w, "  папка %s: %s %s\n", s.Name, s.Action, s.Reason)
	}
	for _, s := range p.Settings {
		fmt.Fprintf(w, "  настройка: %s\n", s)
	}
	for _, s := range p.Secrets {
		fmt.Fprintf(w, "  секрет задаётся заново: %s\n", s.Title)
	}
	for _, s := range p.Warnings {
		fmt.Fprintf(w, "  предупреждение: %s\n", s)
	}
}

// readAnswers — JSON с правами не шире 0600 (там пароли).
func readAnswers(path string) (confbackup.Answers, error) {
	var a confbackup.Answers
	fi, err := os.Stat(path)
	if err != nil {
		return a, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return a, fmt.Errorf("%s: права должны быть 0600 (в файле пароли)", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("%s: %v", path, err)
	}
	return a, nil
}

func confirm(stdin io.Reader, stderr io.Writer, prompt string) bool {
	fmt.Fprint(stderr, prompt)
	line, _ := bufio.NewReader(io.LimitReader(stdin, 64)).ReadString('\n')
	return strings.TrimSpace(strings.ToLower(line)) == "да"
}

// askAnswers — мастер с терминала: новые пароли по умолчанию; «оставить
// прежний» — только если он есть, с предупреждением.
func askAnswers(stdin io.Reader, stderr io.Writer, p confbackup.Plan) (confbackup.Answers, error) {
	a := confbackup.Answers{Passwords: map[string]string{}, Keep: map[string]bool{}}
	f, ok := stdin.(*os.File)
	if !ok || !isTerminal(int(f.Fd())) {
		return a, errors.New("мастер восстановления требует терминал; без терминала — -answers файл.json (см. runbook)")
	}
	fmt.Fprintln(stderr, "Пароли и секреты в копию не входят — задайте новые.")
	for _, u := range p.Users {
		if u.CanKeep && confirm(stdin, stderr, "У "+u.Name+" на этой системе уже есть пароль. Оставить прежний (НЕБЕЗОПАСНО)? «да» — оставить: ") {
			a.Keep[u.Name], a.AcceptInsecure = true, true
			continue
		}
		pw, err := readPassword(stdin, stderr, u.Name)
		if err != nil {
			return a, err
		}
		a.Passwords[u.Name] = pw
	}
	for _, s := range p.Secrets {
		if s.CanKeep && confirm(stdin, stderr, s.Title+": оставить прежний (НЕБЕЗОПАСНО)? «да» — оставить: ") {
			a.Keep[s.Key], a.AcceptInsecure = true, true
			continue
		}
		v, err := readNoEcho(int(f.Fd()), stderr, s.Title+" (пусто — канал будет выключен): ")
		if err != nil {
			return a, err
		}
		switch s.Key {
		case confbackup.SecretSMTP:
			a.SMTPPassword = v
		case confbackup.SecretWebhook:
			a.WebhookSecret = v
		case confbackup.SecretWebhookURL:
			a.WebhookURL = v
		}
	}
	return a, nil
}
