// gamayun-catalog — сборка, подпись и проверка каталога приложений gamayun-nas
//
// Каталог — каталог файлов:
//
//	apps/<id>/compose.yaml   compose + x-gamayun (обязателен)
//	apps/<id>/<прочее>       значок, скриншоты
//	index.json               список приложений с sha256 файлов (build)
//	index.json.sig           подпись ed25519 (sign)
//
// Команды:
//
//	gamayun-catalog keygen -out key            key (0600, закрытый) + key.pub
//	gamayun-catalog build  -dir D -serial N    проверить приложения, записать index.json
//	gamayun-catalog sign   -dir D -key key     подписать index.json
//	gamayun-catalog verify -dir D -pub key.pub проверить подпись и файлы
//	gamayun-catalog pack   -dir D -pub key.pub -out catalog-N.tar.gz  проверить и упаковать
//
// Закрытый ключ хранится офлайн у владельца каталога; в репозиторий не попадает.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gamayun-nas/internal/catalog"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "keygen":
		err = keygen(args)
	case "build":
		err = build(args)
	case "sign":
		err = sign(args)
	case "verify":
		err = verify(args)
	case "pack":
		err = pack(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "использование: gamayun-catalog keygen|build|sign|verify|pack [флаги] (подробно: -h)")
	os.Exit(2)
}

func keygen(args []string) error {
	fl := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fl.String("out", "", "файл закрытого ключа (рядом — .pub)")
	fl.Parse(args)
	if *out == "" {
		return errors.New("нужен -out")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s уже существует — не перезаписываю", *out)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(*out+".pub", []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Println("ключ:", catalog.KeyID(pub))
	return nil
}

func readKey(path string, size int) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != size {
		return nil, fmt.Errorf("%s: неверный ключ", path)
	}
	return k, nil
}

// collect — файлы приложений: id → путь внутри каталога → содержимое.
func collect(dir string) (map[string]map[string][]byte, error) {
	res := map[string]map[string][]byte{}
	root := filepath.Join(dir, "apps")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: символические ссылки запрещены", rel)
		}
		if d.IsDir() {
			return nil
		}
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if len(parts) != 2 || !catalog.SafePath(parts[1]) {
			return fmt.Errorf("%s: файл вне apps/<id>/ или недопустимое имя", rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if res[parts[0]] == nil {
			res[parts[0]] = map[string][]byte{}
		}
		res[parts[0]][parts[1]] = b
		return nil
	})
	return res, err
}

func build(args []string) error {
	fl := flag.NewFlagSet("build", flag.ExitOnError)
	dir := fl.String("dir", ".", "каталог")
	serial := fl.Int64("serial", 0, "номер каталога (только растёт)")
	fl.Parse(args)
	if *serial <= 0 {
		return errors.New("нужен -serial > 0")
	}
	apps, err := collect(*dir)
	if err != nil {
		return err
	}
	ix := catalog.Index{Schema: catalog.IndexSchema, Serial: *serial, Created: time.Now().UTC().Truncate(time.Second)}
	var bad []string
	ids := make([]string, 0, len(apps))
	for id := range apps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		files := apps[id]
		app, errs := catalog.Parse(files["compose.yaml"])
		if files["compose.yaml"] == nil {
			errs = []error{errors.New("нет compose.yaml")}
		} else if len(errs) == 0 && app.Manifest.ID != id {
			errs = []error{fmt.Errorf("id %q не совпадает с каталогом", app.Manifest.ID)}
		}
		for _, e := range errs {
			bad = append(bad, id+": "+e.Error())
		}
		if len(errs) > 0 {
			continue
		}
		ia := catalog.IndexApp{ID: id, Version: app.Manifest.Version, AppVersion: app.Manifest.AppVersion, Name: app.Manifest.Name, Files: map[string]string{}}
		for p, b := range files {
			ia.Files[p] = catalog.SHA256(b)
		}
		ix.Apps = append(ix.Apps, ia)
	}
	if len(bad) > 0 {
		return fmt.Errorf("каталог не собран:\n  %s", strings.Join(bad, "\n  "))
	}
	if err := ix.Validate(); err != nil {
		return err
	}
	b, err := ix.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, catalog.IndexFile), b, 0o644); err != nil {
		return err
	}
	os.Remove(filepath.Join(*dir, catalog.SigFile)) // старая подпись больше не годится
	fmt.Printf("index.json: serial %d, приложений %d\n", ix.Serial, len(ix.Apps))
	return nil
}

func sign(args []string) error {
	fl := flag.NewFlagSet("sign", flag.ExitOnError)
	dir := fl.String("dir", ".", "каталог")
	key := fl.String("key", "", "закрытый ключ")
	fl.Parse(args)
	seed, err := readKey(*key, ed25519.SeedSize)
	if err != nil {
		return err
	}
	idx, err := os.ReadFile(filepath.Join(*dir, catalog.IndexFile))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, catalog.SigFile), catalog.Sign(ed25519.NewKeyFromSeed(seed), idx), 0o644)
}

func verify(args []string) error {
	fl := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fl.String("dir", ".", "каталог")
	pubf := fl.String("pub", "", "открытый ключ")
	fl.Parse(args)
	pub, err := readKey(*pubf, ed25519.PublicKeySize)
	if err != nil {
		return err
	}
	idx, err := os.ReadFile(filepath.Join(*dir, catalog.IndexFile))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(filepath.Join(*dir, catalog.SigFile))
	if err != nil {
		return err
	}
	ix, err := catalog.Verify(idx, sig, []ed25519.PublicKey{pub})
	if err != nil {
		return err
	}
	apps, err := collect(*dir)
	if err != nil {
		return err
	}
	for _, a := range ix.Apps {
		for p := range a.Files {
			if err := a.CheckFile(p, apps[a.ID][p]); err != nil {
				return err
			}
		}
		for p := range apps[a.ID] {
			if _, ok := a.Files[p]; !ok {
				return fmt.Errorf("%s: лишний файл %s вне индекса", a.ID, p)
			}
		}
	}
	fmt.Printf("подпись верна (ключ %s), serial %d, приложений %d\n", catalog.KeyID(pub), ix.Serial, len(ix.Apps))
	return nil
}

func pack(args []string) error {
	fl := flag.NewFlagSet("pack", flag.ExitOnError)
	dir := fl.String("dir", ".", "каталог")
	pubf := fl.String("pub", "", "открытый ключ (проверка перед упаковкой)")
	out := fl.String("out", "", "файл архива .tar.gz")
	fl.Parse(args)
	if *out == "" {
		return errors.New("нужен -out")
	}
	pub, err := readKey(*pubf, ed25519.PublicKeySize)
	if err != nil {
		return err
	}
	apps, err := collect(*dir)
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	for id, m := range apps {
		for p, b := range m {
			files["apps/"+id+"/"+p] = b
		}
	}
	for _, n := range []string{catalog.IndexFile, catalog.SigFile} {
		if files[n], err = os.ReadFile(filepath.Join(*dir, n)); err != nil {
			return err
		}
	}
	ix, _, err := catalog.Check(files, []ed25519.PublicKey{pub})
	if err != nil {
		return err
	}
	b, err := catalog.Pack(files)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: serial %d, приложений %d, %d байт, sha256 %s\n", *out, ix.Serial, len(ix.Apps), len(b), catalog.SHA256(b))
	return nil
}
