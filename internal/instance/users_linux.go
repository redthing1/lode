//go:build linux

package instance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const passwordMin = 12
const passwordMax = 1024
const argonMemory = 64 * 1024
const argonIterations = 3
const argonParallelism = 1
const argonKeyLen = 32

type passwordParams struct{ version, memory, iterations, parallelism, keyLen uint32 }

var currentPasswordParams = passwordParams{version: 1, memory: argonMemory, iterations: argonIterations, parallelism: argonParallelism, keyLen: argonKeyLen}

var workSlots = make(chan struct{}, 2)
var usernamePattern = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)
var ErrInvalid = errors.New("invalid account data")
var ErrSetupUnavailable = errors.New("setup unavailable")

type User struct {
	ID                  string `json:"id"`
	Username            string `json:"username"`
	Admin               bool   `json:"admin"`
	Disabled            bool   `json:"disabled"`
	ForcePasswordChange bool   `json:"forcePasswordChange"`
	AuthVersion         int64  `json:"authVersion"`
}

type Principal struct {
	UserID              string `json:"userId"`
	Username            string `json:"username"`
	Admin               bool   `json:"admin"`
	ForcePasswordChange bool   `json:"forcePasswordChange"`
	AuthVersion         int64  `json:"authVersion"`
	TokenID             string `json:"tokenId,omitempty"`
}

func validUsername(name string) bool {
	if !usernamePattern.MatchString(name) {
		return false
	}
	switch name {
	case "api", "assets", "settings", "projects", "security", "passkey", "verify", "healthz", "login", "logout", "setup", "register":
		return false
	default:
		return true
	}
}

func validatePassword(password string) error {
	if len(password) < passwordMin || len(password) > passwordMax {
		return ErrInvalid
	}
	return nil
}

func hashPassword(ctx context.Context, password string) ([]byte, []byte, error) {
	if err := validatePassword(password); err != nil {
		return nil, nil, err
	}
	select {
	case workSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-workSlots }()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, err
	}
	p := currentPasswordParams
	hash := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, uint8(p.parallelism), p.keyLen)
	return salt, hash, nil
}

func verifyPassword(ctx context.Context, password string, salt, expected []byte, params passwordParams) (bool, error) {
	if params.version != 1 || params.memory < 8*1024 || params.memory > 256*1024 || params.iterations < 1 || params.iterations > 10 || params.parallelism < 1 || params.parallelism > 8 || params.keyLen != uint32(len(expected)) || len(expected) != argonKeyLen || len(salt) < 16 || len(salt) > 64 {
		return false, errors.New("unsupported or invalid password hash parameters")
	}
	select {
	case workSlots <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-workSlots }()
	got := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, uint8(params.parallelism), params.keyLen)
	return subtle.ConstantTimeCompare(got, expected) == 1, nil
}

var dummySalt = []byte("Lode unknown account salt")

func verifyDummy(ctx context.Context, password string) error {
	select {
	case workSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-workSlots }()
	_ = argon2.IDKey([]byte(password), dummySalt, argonIterations, argonMemory, argonParallelism, argonKeyLen)
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func setupDigest(code string) []byte { sum := sha256.Sum256([]byte(code)); return sum[:] }

// InitSetup creates and returns a one-time setup code only for an empty, uninitialized store.
func (s *Store) InitSetup(ctx context.Context) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var users int
		var used int
		var existing []byte
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM users),setup_used,setup_hash FROM instance_settings WHERE id=1`).Scan(&users, &used, &existing); err != nil {
			return err
		}
		if users != 0 || used != 0 || len(existing) != 0 {
			return ErrSetupUnavailable
		}
		_, err := tx.ExecContext(ctx, `UPDATE instance_settings SET setup_hash=? WHERE id=1`, setupDigest(code))
		return err
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

func (s *Store) SetupRequired(ctx context.Context) (bool, error) {
	var users, used int
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM users),setup_used FROM instance_settings WHERE id=1`).Scan(&users, &used)
	return err == nil && users == 0 && used == 0, err
}

func (s *Store) Setup(ctx context.Context, code, username, password string) (User, error) {
	if len(code) > 128 || !validUsername(username) || validatePassword(password) != nil {
		return User{}, ErrInvalid
	}
	salt, hash, err := hashPassword(ctx, password)
	if err != nil {
		return User{}, err
	}
	id, err := newID()
	if err != nil {
		return User{}, err
	}
	var user User
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var users, used int
		var expected []byte
		if e := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM users),setup_used,setup_hash FROM instance_settings WHERE id=1`).Scan(&users, &used, &expected); e != nil {
			return e
		}
		if users != 0 || used != 0 || len(expected) == 0 || subtle.ConstantTimeCompare(expected, setupDigest(code)) != 1 {
			return ErrSetupUnavailable
		}
		now := time.Now().Unix()
		p := currentPasswordParams
		if _, e := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_salt,password_hash,password_version,password_memory,password_iterations,password_parallelism,password_key_len,admin,created_at) VALUES(?,?,?,?,?,?,?,?,?,1,?)`, id, username, salt, hash, p.version, p.memory, p.iterations, p.parallelism, p.keyLen, now); e != nil {
			return mapConflict(e)
		}
		if _, e := tx.ExecContext(ctx, `UPDATE instance_settings SET setup_hash=NULL,setup_used=1 WHERE id=1`); e != nil {
			return e
		}
		user = User{ID: id, Username: username, Admin: true, AuthVersion: 1}
		return nil
	})
	return user, err
}

func (s *Store) CreateUser(ctx context.Context, username, password string, admin bool) (User, error) {
	return s.createUser(ctx, username, password, admin, true)
}

// Register creates an ordinary account after the one-time administrator setup
// has completed. The completion check and insertion share one transaction so
// registration cannot race or precede bootstrap.
func (s *Store) Register(ctx context.Context, username, password string) (User, error) {
	return s.createUser(ctx, username, password, false, false)
}

func (s *Store) createUser(ctx context.Context, username, password string, admin, temporaryPassword bool) (User, error) {
	if !validUsername(username) {
		return User{}, ErrInvalid
	}
	salt, hash, err := hashPassword(ctx, password)
	if err != nil {
		return User{}, err
	}
	id, err := newID()
	if err != nil {
		return User{}, err
	}
	var user User
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if !temporaryPassword {
			var users, used int
			if e := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM users),setup_used FROM instance_settings WHERE id=1`).Scan(&users, &used); e != nil {
				return e
			}
			if users == 0 || used != 1 {
				return ErrSetupUnavailable
			}
		}
		p := currentPasswordParams
		_, e := tx.ExecContext(ctx, `INSERT INTO users(id,username,password_salt,password_hash,password_version,password_memory,password_iterations,password_parallelism,password_key_len,admin,force_password_change,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, username, salt, hash, p.version, p.memory, p.iterations, p.parallelism, p.keyLen, boolInt(admin), boolInt(temporaryPassword), time.Now().Unix())
		if e != nil {
			return mapConflict(e)
		}
		user = User{ID: id, Username: username, Admin: admin, ForcePasswordChange: temporaryPassword, AuthVersion: 1}
		return nil
	})
	return user, err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,admin,disabled,force_password_change,auth_version FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []User
	for rows.Next() {
		var u User
		var admin, disabled, force int
		if err := rows.Scan(&u.ID, &u.Username, &admin, &disabled, &force, &u.AuthVersion); err != nil {
			return nil, err
		}
		u.Admin = admin != 0
		u.Disabled = disabled != 0
		u.ForcePasswordChange = force != 0
		result = append(result, u)
	}
	return result, rows.Err()
}
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	if len(id) == 0 || len(id) > 64 {
		return User{}, ErrInvalid
	}
	var u User
	var a, d, f int
	err := s.db.QueryRowContext(ctx, `SELECT id,username,admin,disabled,force_password_change,auth_version FROM users WHERE id=?`, id).Scan(&u.ID, &u.Username, &a, &d, &f, &u.AuthVersion)
	if err == sql.ErrNoRows {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Admin = a != 0
	u.Disabled = d != 0
	u.ForcePasswordChange = f != 0
	return u, nil
}

func (s *Store) LookupUser(ctx context.Context, username string) (User, error) {
	if len(username) > 32 {
		return User{}, ErrInvalid
	}
	var u User
	var a, d, f int
	name := strings.ToLower(username)
	if !validUsername(name) {
		return User{}, ErrNotFound
	}
	err := s.db.QueryRowContext(ctx, `SELECT id,username,admin,disabled,force_password_change,auth_version FROM users WHERE username=?`, name).Scan(&u.ID, &u.Username, &a, &d, &f, &u.AuthVersion)
	if err == sql.ErrNoRows {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Admin = a != 0
	u.Disabled = d != 0
	u.ForcePasswordChange = f != 0
	return u, nil
}

func (s *Store) AuthenticatePassword(ctx context.Context, username, password string) (Principal, error) {
	normalized := strings.ToLower(username)
	if len(username) > 32 || len(password) > passwordMax || !validUsername(normalized) {
		if err := verifyDummy(ctx, password[:min(len(password), passwordMax)]); err != nil {
			return Principal{}, err
		}
		return Principal{}, ErrUnauthorized
	}
	var id, name string
	var salt, hash []byte
	var admin, disabled, force int
	var version int64
	var hashVersion, memory, iterations, parallelism, keyLen uint32
	err := s.db.QueryRowContext(ctx, `SELECT id,username,password_salt,password_hash,admin,disabled,force_password_change,auth_version,password_version,password_memory,password_iterations,password_parallelism,password_key_len FROM users WHERE username=?`, normalized).Scan(&id, &name, &salt, &hash, &admin, &disabled, &force, &version, &hashVersion, &memory, &iterations, &parallelism, &keyLen)
	if err == sql.ErrNoRows {
		if e := verifyDummy(ctx, password); e != nil {
			return Principal{}, e
		}
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	valid, e := verifyPassword(ctx, password, salt, hash, passwordParams{version: hashVersion, memory: memory, iterations: iterations, parallelism: parallelism, keyLen: keyLen})
	if e != nil {
		return Principal{}, e
	}
	if !valid || disabled != 0 {
		return Principal{}, ErrUnauthorized
	}
	return Principal{UserID: id, Username: name, Admin: admin != 0, ForcePasswordChange: force != 0, AuthVersion: version}, nil
}

func (s *Store) SetPassword(ctx context.Context, userID, password string) (User, error) {
	if len(userID) == 0 || len(userID) > 64 {
		return User{}, ErrInvalid
	}
	salt, hash, err := hashPassword(ctx, password)
	if err != nil {
		return User{}, err
	}
	var u User
	err = s.tx(ctx, func(tx *sql.Tx) error {
		p := currentPasswordParams
		res, e := tx.ExecContext(ctx, `UPDATE users SET password_salt=?,password_hash=?,password_version=?,password_memory=?,password_iterations=?,password_parallelism=?,password_key_len=?,force_password_change=0,auth_version=auth_version+1 WHERE id=?`, salt, hash, p.version, p.memory, p.iterations, p.parallelism, p.keyLen, userID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return scanUserTx(ctx, tx, userID, &u)
	})
	return u, err
}

// SetUserState updates account access and administrator status, preserving one enabled administrator.
func (s *Store) SetUserState(ctx context.Context, userID string, disabled, admin bool) (User, error) {
	if len(userID) == 0 || len(userID) > 64 {
		return User{}, ErrInvalid
	}
	var u User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := scanUserTx(ctx, tx, userID, &u); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if u.Disabled == disabled && u.Admin == admin {
			return nil
		}
		if u.Admin && !u.Disabled && (disabled || !admin) {
			var count int
			if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE admin=1 AND disabled=0 AND id<>?`, userID).Scan(&count); e != nil {
				return e
			}
			if count == 0 {
				return fmt.Errorf("last enabled administrator: %w", ErrConflict)
			}
		}
		_, e := tx.ExecContext(ctx, `UPDATE users SET disabled=?,admin=?,auth_version=auth_version+1 WHERE id=?`, boolInt(disabled), boolInt(admin), userID)
		if e != nil {
			return e
		}
		return scanUserTx(ctx, tx, userID, &u)
	})
	return u, err
}

func scanUserTx(ctx context.Context, tx *sql.Tx, id string, u *User) error {
	var a, d, f int
	e := tx.QueryRowContext(ctx, `SELECT id,username,admin,disabled,force_password_change,auth_version FROM users WHERE id=?`, id).Scan(&u.ID, &u.Username, &a, &d, &f, &u.AuthVersion)
	if e != nil {
		return e
	}
	u.Admin = a != 0
	u.Disabled = d != 0
	u.ForcePasswordChange = f != 0
	return nil
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func mapConflict(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint") || strings.Contains(strings.ToLower(err.Error()), "constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}
