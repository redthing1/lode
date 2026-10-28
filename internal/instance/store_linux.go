//go:build linux

package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")
var ErrUnauthorized = errors.New("unauthorized")

// Store owns identity, membership, and personal-token records for one instance.
type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	return openStore(path, true)
}

func openStore(path string, create bool) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	newFile := false
	if create {
		file, openErr := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if openErr == nil {
			newFile = true
			if closeErr := file.Close(); closeErr != nil {
				return nil, closeErr
			}
		} else if !errors.Is(openErr, os.ErrExist) {
			return nil, openErr
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("instance database is missing; refusing to create a replacement (restore a backup or initialize a new instance explicitly)")
		}
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("instance database is not a regular file")
	}
	if err := os.Chmod(abs, 0600); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	query := u.Query()
	query.Add("_busy_timeout", "5000")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "synchronous(EXTRA)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	const schema = `
CREATE TABLE instance_settings (
	id INTEGER PRIMARY KEY CHECK (id=1), setup_hash BLOB, setup_used INTEGER NOT NULL DEFAULT 0 CHECK(setup_used IN (0,1))
);
INSERT INTO instance_settings(id) VALUES (1);
CREATE TABLE users (
	id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE BINARY, password_salt BLOB NOT NULL,
	password_hash BLOB NOT NULL, password_version INTEGER NOT NULL DEFAULT 1,
	password_memory INTEGER NOT NULL DEFAULT 65536, password_iterations INTEGER NOT NULL DEFAULT 3,
	password_parallelism INTEGER NOT NULL DEFAULT 1, password_key_len INTEGER NOT NULL DEFAULT 32,
	admin INTEGER NOT NULL DEFAULT 0 CHECK(admin IN (0,1)),
	disabled INTEGER NOT NULL DEFAULT 0 CHECK(disabled IN (0,1)), force_password_change INTEGER NOT NULL DEFAULT 0 CHECK(force_password_change IN (0,1)),
	auth_version INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL
);
CREATE TABLE memberships (
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	role TEXT NOT NULL CHECK(role IN ('reader','writer','maintainer')), PRIMARY KEY(user_id, project_id)
);
CREATE TABLE projects (
	id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id), slug TEXT NOT NULL,
	created INTEGER NOT NULL, deleting INTEGER NOT NULL DEFAULT 0 CHECK(deleting IN (0,1)), UNIQUE(owner_id,slug)
);
CREATE TABLE personal_tokens (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL, prefix TEXT NOT NULL, secret_hash BLOB NOT NULL UNIQUE,
	project_id TEXT REFERENCES projects(id) ON DELETE CASCADE, role TEXT NOT NULL CHECK(role IN ('reader','writer','maintainer')),
	created_at INTEGER NOT NULL, expires_at INTEGER, last_used_at INTEGER, revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1))
);
CREATE INDEX personal_tokens_owner ON personal_tokens(user_id,created_at);
CREATE TABLE security_keys(id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL, credential BLOB NOT NULL, created INTEGER NOT NULL);
CREATE INDEX security_keys_owner ON security_keys(user_id);
CREATE TABLE ceremonies(id TEXT PRIMARY KEY, user_id TEXT REFERENCES users(id), binding TEXT NOT NULL, purpose TEXT NOT NULL, data BLOB NOT NULL, expires INTEGER NOT NULL, auth_version INTEGER NOT NULL);
CREATE TABLE recovery_codes(user_id TEXT NOT NULL REFERENCES users(id), hash BLOB NOT NULL, PRIMARY KEY(user_id,hash));
CREATE TABLE sessions(token TEXT PRIMARY KEY,data BLOB NOT NULL,expiry INTEGER NOT NULL);
`
	if newFile {
		tx, txErr := db.Begin()
		if txErr == nil {
			_, txErr = tx.Exec(schema)
		}
		if txErr == nil {
			_, txErr = tx.Exec("PRAGMA user_version=3")
		}
		if txErr == nil {
			txErr = tx.Commit()
		} else if tx != nil {
			_ = tx.Rollback()
		}
		if txErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize instance store: %w", txErr)
		}
	}
	if err := validateStore(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func validateStore(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 3 {
		return fmt.Errorf("unsupported instance database version %d (want 3)", version)
	}
	for _, table := range []string{"instance_settings", "users", "memberships", "projects", "personal_tokens", "security_keys", "ceremonies", "recovery_codes", "sessions"} {
		var exists int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return fmt.Errorf("instance database is missing required table %s", table)
		}
	}
	return nil
}

func tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	t, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return tx(ctx, s.db, fn)
}
