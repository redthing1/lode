package instance

import (
	"context"
	"database/sql"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

var ErrCurrentPassword = errors.New("current password is incorrect")
var ErrLastAdministrator = errors.New("keep at least one enabled administrator")
var ErrAccountChanged = errors.New("account changed; review its current settings and try again")

// ChangePassword checks the password outside the write transaction, then fences
// that authentication with the captured generation before replacing credentials.
func (s *Store) ChangePassword(ctx context.Context, actor Principal, current, replacement string) error {
	if err := s.checkPassword(ctx, actor, current); err != nil {
		return err
	}
	hash, err := passwordHash(replacement)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := currentUser(ctx, tx, actor.User); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET password=? WHERE id=?", hash, actor.User.ID); err != nil {
		return err
	}
	if err := invalidateAccount(ctx, tx, actor.User.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) checkPassword(ctx context.Context, actor Principal, password string) error {
	if actor.Token {
		return ErrAccess
	}
	var hash []byte
	var generation int64
	err := s.db.QueryRowContext(ctx, "SELECT password,generation FROM users WHERE id=? AND disabled=0", actor.User.ID).Scan(&hash, &generation)
	if errors.Is(err, sql.ErrNoRows) || err == nil && generation != actor.User.Generation {
		return ErrCredentials
	}
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return ErrCurrentPassword
	}
	return nil
}

func invalidateAccount(ctx context.Context, tx *sql.Tx, id string) error {
	for _, query := range []string{
		"UPDATE users SET generation=generation+1 WHERE id=?",
		"DELETE FROM tokens WHERE user_id=?",
		"DELETE FROM challenges WHERE user_id=?",
	} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	return nil
}

func clearFactors(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM security_keys WHERE user_id=?", id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=?", id)
	return err
}

type AccountUpdate struct {
	Generation      int64
	Admin, Disabled bool
	Password        string
}

func (s *Store) ManagedUser(ctx context.Context, actor Principal, name string) (User, error) {
	if err := administrator(ctx, s.db, actor); err != nil {
		return User{}, err
	}
	return readUser(s.db.QueryRowContext(ctx, "SELECT id,name,admin,disabled,generation FROM users WHERE name=?", name))
}

// UpdateUser changes account authority and credentials atomically. Re-enabling
// never revives revoked credentials, and stale forms cannot overwrite a reset.
func (s *Store) UpdateUser(ctx context.Context, actor Principal, id string, change AccountUpdate) error {
	if err := administrator(ctx, s.db, actor); err != nil {
		return err
	}
	var hash []byte
	if change.Password != "" {
		var err error
		hash, err = passwordHash(change.Password)
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := administrator(ctx, tx, actor); err != nil {
		return err
	}
	user, err := readUser(tx.QueryRowContext(ctx, "SELECT id,name,admin,disabled,generation FROM users WHERE id=?", id))
	if err != nil {
		return err
	}
	if user.Generation != change.Generation {
		return ErrAccountChanged
	}
	if user.Admin == change.Admin && user.Disabled == change.Disabled && hash == nil {
		return nil
	}
	if user.Admin && !user.Disabled && (!change.Admin || change.Disabled) {
		var remaining int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE admin=1 AND disabled=0 AND id<>?", id).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			return ErrLastAdministrator
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET admin=?,disabled=?,password=COALESCE(?,password) WHERE id=?", change.Admin, change.Disabled, hash, id); err != nil {
		return err
	}
	if hash != nil {
		if err := clearFactors(ctx, tx, id); err != nil {
			return err
		}
	}
	if err := invalidateAccount(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}
