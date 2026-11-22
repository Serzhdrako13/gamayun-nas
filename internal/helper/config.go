package helper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"gamayun-nas/internal/storage/home"
)

var devNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Config — конфигурация gamayun-helper (JSON, неизвестные поля — ошибка).
type Config struct {
	// DestructiveAllowIDs — стабильные идентичности дисков (disks.Identity,
	// например "wwn-0x5000000000000101"), с которыми разрешены разрушающие и
	// изменяющие операции пулов. Пустой список — без ограничения.
	DestructiveAllowIDs []string `json:"destructive_allow_ids"`
	// MountRoot — каталог точек монтирования пулов (<mount_root>/<пул>).
	MountRoot string `json:"mount_root"`
	// SambaInclude — include-файл общих папок; в основной smb.conf
	// добавляется строка «include = <файл>» вручную.
	SambaInclude string `json:"samba_include"`
	// SambaConf — основной smb.conf (только чтение: проверка include).
	SambaConf string `json:"samba_conf"`
	// SSHDevAccess — режим разработки SSH: учётки, входящие по ключу
	// (root — prohibit-password); служба SSH не останавливается. Только для
	// стенда и тестовых серверов; в продукте пусто.
	SSHDevAccess []string `json:"ssh_dev_access"`
}

// DefaultConfig — без разрешённых дисков: разрушающие операции запрещены.
func DefaultConfig() Config {
	return Config{DestructiveAllowIDs: []string{}, MountRoot: "/srv/gamayun-nas",
		SambaInclude: "/etc/samba/gamayun-shares.conf", SambaConf: "/etc/samba/smb.conf"}
}

// LoadConfig читает файл поверх значений по умолчанию и проверяет его.
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("конфигурация helper: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("конфигурация helper %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return c, fmt.Errorf("конфигурация helper %s: лишние данные после объекта", path)
	}
	return c, c.Validate()
}

// Validate проверяет значения.
func (c Config) Validate() error {
	seen := map[string]bool{}
	for _, id := range c.DestructiveAllowIDs {
		if !home.ValidDiskID(id) {
			return fmt.Errorf("destructive_allow_ids: недопустимая идентичность %q", id)
		}
		if seen[id] {
			return fmt.Errorf("destructive_allow_ids: повтор %q", id)
		}
		seen[id] = true
	}
	if !filepath.IsAbs(c.MountRoot) || filepath.Clean(c.MountRoot) != c.MountRoot || c.MountRoot == "/" {
		return fmt.Errorf("mount_root должен быть абсолютным нормализованным путём, не /: %q", c.MountRoot)
	}
	if !filepath.IsAbs(c.SambaInclude) || filepath.Clean(c.SambaInclude) != c.SambaInclude {
		return fmt.Errorf("samba_include должен быть абсолютным нормализованным путём: %q", c.SambaInclude)
	}
	for _, u := range c.SSHDevAccess {
		if !devNameRe.MatchString(u) {
			return fmt.Errorf("ssh_dev_access: недопустимое имя %q", u)
		}
	}
	if c.SambaConf != "" && (!filepath.IsAbs(c.SambaConf) || filepath.Clean(c.SambaConf) != c.SambaConf) {
		return fmt.Errorf("samba_conf должен быть абсолютным нормализованным путём: %q", c.SambaConf)
	}
	return nil
}
