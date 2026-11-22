// Package nas — локальные пользователи, группы и общие папки.
//
// Две стороны. Executor работает в gamayun-helper (root): учётки ОС, пароль
// Samba, include-файл шар — только фиксированные операции с проверенными
// параметрами. Service работает в gamayund (без root): хранит модель в
// SQLite, вычисляет эффективные права и отдаёт helper готовый список шар.
package nas

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Операции helper.
const (
	OpUserEnsure   = "nas.user.ensure"
	OpUserPassword = "nas.user.password"
	OpUserDelete   = "nas.user.delete"
	OpSharesApply  = "nas.shares.apply"
	OpUserAccess   = "nas.user.access"  // оболочка, SSH, sudo
	OpAccessSetup  = "nas.access.setup" // группы, drop-in sshd и sudoers

	OpIdentityGet = "system.identity.get" // имя сервера, часовой пояс, NTP (чтение)
	OpHostnameSet = "system.hostname.set"
	OpTimezoneSet = "system.timezone.set"
	OpHomesApply  = "nas.homes.apply"  // домашние каталоги в хранилище
	OpGuardApply  = "ssh.guard.apply"  // fail2ban для sshd
	OpGuardStatus = "ssh.guard.status" // состояние и заблокированные адреса (чтение)
	OpGuardUnban  = "ssh.guard.unban"

	OpSSHApply  = "ssh.service.apply"  // служба SSH и drop-in sshd
	OpSSHStatus = "ssh.service.status" // (чтение)
	OpPower     = "system.power"       // выключение и перезагрузка
)

// Системные группы доступа ОС. Членство в них ведёт только helper
// по модели gamayund (флаг ssh, состав administrators).
const (
	SSHGroup  = "gamayun-ssh"  // AllowGroups в drop-in sshd
	SudoGroup = "gamayun-sudo" // sudo через drop-in /etc/sudoers.d
)

// MinUID — учётки с меньшим UID helper не меняет ни при каких условиях.
const MinUID = 1000

// Учётки приложений Docker: app-<id>, UID/GID из диапазона
// [AppUIDMin, AppUIDMax]. Операции nas.user.* их не трогают никогда.
const (
	AppUserPrefix = "app-"
	AppUIDMin     = 59000
	AppUIDMax     = 59999
)

// IsAppUser — имя или UID учётки приложения.
func IsAppUser(name string, uid int) bool {
	return strings.HasPrefix(name, AppUserPrefix) || (uid >= AppUIDMin && uid <= AppUIDMax)
}

// UnixGroup — первичная группа всех учёток NAS и группа файлов шар.
// Учётка ОС считается принадлежащей NAS только если её первичная группа —
// эта; чужие (системные) учётки helper не трогает.
const UnixGroup = "gamayun-users"

// Уровни доступа.
const (
	LevelNone = "none"
	LevelRead = "read"
	LevelRW   = "rw"
)

// Виды субъектов доступа.
const (
	KindUser  = "user"
	KindGroup = "group"
)

// Стандартные группы: создаются при старте, не удаляются.
const (
	GroupAdmins = "administrators" // члены входят в веб как администраторы
	GroupUsers  = "users"          // группа по умолчанию для новых пользователей
)

// SystemGroups — стандартные группы.
var SystemGroups = []string{GroupAdmins, GroupUsers}

// IsSystemGroup — стандартная ли группа.
func IsSystemGroup(name string) bool { return name == GroupAdmins || name == GroupUsers }

// Ошибки сервиса (API сопоставляет их с кодами HTTP).
var (
	ErrInvalid  = errors.New("неверный запрос")
	ErrNotFound = errors.New("не найдено")
	ErrConflict = errors.New("конфликт состояния")
)

func wrap(base error, format string, a ...any) error {
	return fmt.Errorf("%w: %s", base, fmt.Sprintf(format, a...))
}

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,30}$`)
	shareRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	poolRe    = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	commentRe = regexp.MustCompile(`^[^\x00-\x1f\x7f]{0,120}$`)
)

// ValidName — имя пользователя или группы NAS (строчные латинские, до 31).
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidShare — имя общей папки (имя ресурса SMB и каталога).
func ValidShare(s string) bool { return shareRe.MatchString(s) }

// ReservedShare — имена секций smb.conf, недопустимые для общей папки
// (без учёта регистра): [global] ломает конфигурацию Samba (найдено на стенде),
// [printers]/[homes] — особые секции.
func ReservedShare(s string) bool {
	switch strings.ToLower(s) {
	case "global", "printers", HomesShare:
		return true
	}
	return false
}

// Share — шара для helper: готовый результат расчёта прав.
type Share struct {
	Name     string   `json:"name"`
	Pool     string   `json:"pool"`
	Comment  string   `json:"comment"`
	ReadOnly bool     `json:"read_only"` // пул в read-only: запись всем запрещена
	Read     []string `json:"read"`      // пользователи с доступом на чтение
	Write    []string `json:"write"`     // пользователи с чтением и записью
}

// SharesParams — параметры nas.shares.apply: полный перечень шар.
type SharesParams struct {
	Shares []Share `json:"shares"`
	// Netbios — имя сервера в Samba (netbios name); пусто — по умолчанию (имя хоста).
	Netbios string `json:"netbios,omitempty"`
	// Homes — пул службы домашних папок (ресурс [homes]); пусто — служба выключена.
	Homes         string `json:"homes,omitempty"`
	HomesReadOnly bool   `json:"homes_read_only,omitempty"`
	// ShadowCopy — писать строки vfs_shadow_copy2. Ставит helper
	// по наличию модуля Samba; из gamayund не передаётся.
	ShadowCopy bool `json:"-"`
}

// Validate проверяет все значения: они попадают в конфигурацию Samba.
func (p SharesParams) Validate() error {
	if len(p.Shares) > 256 {
		return errors.New("слишком много шар")
	}
	if p.Netbios != "" && !ValidHostname(p.Netbios) {
		return fmt.Errorf("недопустимое имя сервера %q", p.Netbios)
	}
	if p.Homes != "" && !poolRe.MatchString(p.Homes) {
		return errors.New("личные папки: недопустимое имя пула")
	}
	seen := map[string]bool{}
	for _, s := range p.Shares {
		if !ValidShare(s.Name) || seen[strings.ToLower(s.Name)] || ReservedShare(s.Name) {
			return fmt.Errorf("недопустимое или повторное имя шары %q", s.Name)
		}
		seen[strings.ToLower(s.Name)] = true
		if !poolRe.MatchString(s.Pool) {
			return fmt.Errorf("шара %s: недопустимое имя пула", s.Name)
		}
		if !commentRe.MatchString(s.Comment) {
			return fmt.Errorf("шара %s: недопустимый комментарий", s.Name)
		}
		for _, l := range [][]string{s.Read, s.Write} {
			for _, u := range l {
				if !ValidName(u) {
					return fmt.Errorf("шара %s: недопустимое имя пользователя %q", s.Name, u)
				}
			}
		}
	}
	return nil
}

// UserParams — имя учётки.
type UserParams struct {
	Name string `json:"name"`
}

// Validate проверяет имя.
func (p UserParams) Validate() error {
	if !ValidName(p.Name) {
		return fmt.Errorf("недопустимое имя пользователя %q", p.Name)
	}
	if IsAppUser(p.Name, -1) {
		return fmt.Errorf("имя %q зарезервировано для учёток приложений", p.Name)
	}
	return nil
}

// AccessParams — доступ учётки к ОС: SSH (оболочка bash и группа
// SSHGroup; иначе nologin) и sudo (группа SudoGroup).
type AccessParams struct {
	Name string `json:"name"`
	SSH  bool   `json:"ssh"`
	Sudo bool   `json:"sudo"`
	// SambaDisabled — отключённый пользователь: запись Samba отключена
	// (smbpasswd -d), открытые сессии smbd пользователя завершаются.
	// Нулевое значение — Samba включена.
	SambaDisabled bool `json:"samba_disabled,omitempty"`
}

// Validate проверяет имя.
func (p AccessParams) Validate() error { return UserParams{p.Name}.Validate() }

// SetupParams — без параметров: содержимое drop-in фиксировано в helper.
type SetupParams struct{}

// Validate всегда успешна.
func (SetupParams) Validate() error { return nil }

// PasswordParams — смена пароля ОС и Samba. В журнале аудита helper значение
// пароля заменяется на *** (internal/helper auditRequest).
type PasswordParams struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Validate проверяет имя и пароль.
func (p PasswordParams) Validate() error {
	if err := (UserParams{p.Name}).Validate(); err != nil {
		return err
	}
	return ValidPassword(p.Password)
}

// ValidPassword — требования к паролю: 8–128 символов без управляющих.
func ValidPassword(pw string) error {
	if len(pw) < 8 || len(pw) > 128 {
		return errors.New("пароль: от 8 до 128 байт")
	}
	for _, r := range pw {
		if r < 0x20 || r == 0x7f {
			return errors.New("пароль: недопустимые управляющие символы")
		}
	}
	return nil
}

// Result — итог операции helper.
type Result struct {
	Commands []string `json:"commands,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Changed  bool     `json:"changed,omitempty"` // операция что-то изменила
}
