package instance

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
)

var ErrRecovery = errors.New("recovery code is invalid or already used")
var ErrNoKeys = errors.New("register a key before creating recovery codes")

// RecoverAccount is a local operator action. Opening the store holds the
// instance lock, so a separate recovery command cannot run alongside a server.
// Recovery enables the account without changing its role or project ownership.
func (s *Store) RecoverAccount(ctx context.Context, name, password string) error {
	hash, err := passwordHash(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	user, err := readUser(tx.QueryRowContext(ctx, "SELECT id,name,admin,disabled,generation FROM users WHERE name=?", name))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET password=?,disabled=0 WHERE id=?", hash, user.ID); err != nil {
		return err
	}
	if err := clearFactors(ctx, tx, user.ID); err != nil {
		return err
	}
	if err := invalidateAccount(ctx, tx, user.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceRecovery(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=?", id); err != nil {
		return nil, err
	}
	codes := make([]string, 10)
	for i := range codes {
		codes[i] = rand.Text()
		if _, err := tx.ExecContext(ctx, "INSERT INTO recovery_codes VALUES(?,?)", id, digest(codes[i])); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

func (s *Store) RecoveryCodes(ctx context.Context, actor Principal, password string) (User, []string, error) {
	if err := s.checkPassword(ctx, actor, password); err != nil {
		return User{}, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, nil, err
	}
	defer tx.Rollback()
	user, err := currentUser(ctx, tx, actor.User)
	if err != nil {
		return User{}, nil, err
	}
	var keys int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM security_keys WHERE user_id=? AND rp=?", user.ID, s.auth.Config.RPID).Scan(&keys); err != nil {
		return User{}, nil, err
	}
	if keys == 0 {
		return User{}, nil, ErrNoKeys
	}
	codes, err := replaceRecovery(ctx, tx, user.ID)
	if err != nil {
		return User{}, nil, err
	}
	if err := invalidateAccount(ctx, tx, user.ID); err != nil {
		return User{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, nil, err
	}
	user.Generation++
	return user, codes, nil
}

// UseRecovery completes a password-authenticated pending sign-in. Its caller
// must keep this returned generation when creating the fully authenticated session.
func (s *Store) UseRecovery(ctx context.Context, expected User, code string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	user, err := currentUser(ctx, tx, expected)
	if err != nil {
		return User{}, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=? AND hash=?", user.ID, digest(strings.TrimSpace(code)))
	if err != nil {
		return User{}, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return User{}, ErrRecovery
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) RemoveKey(ctx context.Context, actor Principal, password, id string) (User, error) {
	if err := s.checkPassword(ctx, actor, password); err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	user, err := currentUser(ctx, tx, actor.User)
	if err != nil {
		return User{}, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM security_keys WHERE id=? AND user_id=? AND rp=?", id, user.ID, s.auth.Config.RPID)
	if err != nil {
		return User{}, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return User{}, ErrKey
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=? AND NOT EXISTS(SELECT 1 FROM security_keys WHERE user_id=? AND rp=?)", user.ID, user.ID, s.auth.Config.RPID); err != nil {
		return User{}, err
	}
	if err := invalidateAccount(ctx, tx, user.ID); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	user.Generation++
	return user, nil
}
