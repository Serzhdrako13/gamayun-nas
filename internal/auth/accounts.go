package auth

import (
	"context"
	"errors"
)

// Accounts — адаптер для internal/nas: единая учётка администратора
// веба и пользователя NAS. Пароль веба хранится здесь (argon2id), список
// и состав групп ведёт nas; is_admin и disabled синхронизируются из него.
type Accounts struct{ S *Service }

// Validate проверяет пароль по правилам веба (строже, чем для Samba).
func (a Accounts) Validate(name, pw string) error { return ValidatePassword(pw, name) }

// SetPassword создаёт или обновляет учётку веба; сессии пользователя
// отзываются (смена пароля).
func (a Accounts) SetPassword(ctx context.Context, name, pw string, admin, disabled bool) error {
	if err := ValidatePassword(pw, name); err != nil {
		return err
	}
	h, err := a.S.hash(ctx, pw)
	if err != nil {
		return err
	}
	now := a.S.now()
	u, err := a.S.st.UserByName(ctx, name)
	if errors.Is(err, ErrUserNotFound) {
		if u, err = a.S.st.CreateUser(ctx, name, h, admin, false, now); err != nil {
			return err
		}
		return a.setFlags(ctx, u.ID, admin, disabled)
	}
	if err != nil {
		return err
	}
	if err := a.S.st.SetPassword(ctx, u.ID, h, now); err != nil {
		return err
	}
	if err := a.setFlags(ctx, u.ID, admin, disabled); err != nil {
		return err
	}
	_, err = a.S.st.RevokeSessions(ctx, u.ID, nil)
	return err
}

// SetFlags задаёт признак администратора и отключение; при разжаловании
// или отключении сессии отзываются. Нет учётки веба — ничего не делает
// (она появится при первой установке пароля).
func (a Accounts) SetFlags(ctx context.Context, name string, admin, disabled bool) error {
	u, err := a.S.st.UserByName(ctx, name)
	if errors.Is(err, ErrUserNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return a.setFlags(ctx, u.ID, admin, disabled)
}

func (a Accounts) setFlags(ctx context.Context, id int64, admin, disabled bool) error {
	var old User
	err := a.S.st.DB.QueryRowContext(ctx, "SELECT is_admin, disabled FROM users WHERE id = ?", id).Scan(&old.IsAdmin, &old.Disabled)
	if err != nil {
		return err
	}
	if old.IsAdmin == admin && old.Disabled == disabled {
		return nil
	}
	if _, err := a.S.st.DB.ExecContext(ctx, "UPDATE users SET is_admin = ?, disabled = ?, updated_at = ? WHERE id = ?",
		admin, disabled, ms(a.S.now()), id); err != nil {
		return err
	}
	if (old.IsAdmin && !admin) || (!old.Disabled && disabled) {
		_, err = a.S.st.RevokeSessions(ctx, id, nil)
	}
	return err
}

// Delete удаляет учётку веба (сессии — каскадом).
func (a Accounts) Delete(ctx context.Context, name string) error {
	_, err := a.S.st.DB.ExecContext(ctx, "DELETE FROM users WHERE name = ?", name)
	return err
}

// Admins — имена учёток веба с признаком администратора.
func (a Accounts) Admins(ctx context.Context) ([]string, error) {
	rows, err := a.S.st.DB.QueryContext(ctx, "SELECT name FROM users WHERE is_admin = 1 ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Params — параметры argon2id сервиса.
func (s *Service) Params() Params { return s.o.Params }
