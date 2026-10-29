package instance

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var ErrCredentials = errors.New("invalid credentials")
var ErrAccess = errors.New("access denied")
var ErrUsername = errors.New("username must use lowercase letters, numbers, hyphens or underscores and must not be reserved")
var ErrUserExists = errors.New("username is already in use")
var ErrPassword = errors.New("password must be 12–72 bytes")

type User struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Admin      bool   `json:"admin"`
	Disabled   bool   `json:"disabled"`
	Generation int64  `json:"-"`
}

// Principal carries credential limits in addition to current account authority.
// Personal tokens never inherit site-administrator authority.
type Principal struct {
	User    User
	Token   bool
	Project string
	Role    Role
}

func passwordHash(password string) ([]byte, error) {
	if len(password) < 12 || len(password) > 72 {
		return nil, ErrPassword
	}
	return bcrypt.GenerateFromPassword([]byte(password), 12)
}

func (s *Store) NeedsSetup(ctx context.Context) (bool, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT setup_hash FROM instance").Scan(&hash)
	return hash != "", err
}

// SetupCode is for the local operator. It can be recovered after interrupted
// initialization and stops working once administrator setup is complete.
func (s *Store) SetupCode(ctx context.Context) (string, error) {
	needed, err := s.NeedsSetup(ctx)
	if err != nil {
		return "", err
	}
	if !needed {
		return "", ErrAccess
	}
	return setupCode(s.key), nil
}

func (s *Store) Setup(ctx context.Context, code, name, password string) (User, error) {
	if subtle.ConstantTimeCompare([]byte(code), []byte(setupCode(s.key))) != 1 {
		return User{}, ErrCredentials
	}
	if !validUsername(name) {
		return User{}, ErrUsername
	}
	hash, err := passwordHash(password)
	if err != nil {
		return User{}, err
	}
	id, err := newID()
	if err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var expected string
	if err := tx.QueryRowContext(ctx, "SELECT setup_hash FROM instance").Scan(&expected); err != nil {
		return User{}, err
	}
	if expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(digest(code))) != 1 {
		return User{}, ErrCredentials
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,name,password,admin) VALUES(?,?,?,1)", id, name, hash); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE instance SET setup_hash=''"); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: id, Name: name, Admin: true, Generation: 1}, nil
}

func (s *Store) Password(ctx context.Context, name, password string) (User, error) {
	var user User
	var hash []byte
	err := s.db.QueryRowContext(ctx, "SELECT id,name,password,admin,disabled,generation FROM users WHERE name=?", name).Scan(&user.ID, &user.Name, &hash, &user.Admin, &user.Disabled, &user.Generation)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return User{}, err
	}
	if missing {
		// Use the same bcrypt cost for unavailable and existing accounts.
		hash = []byte("$2a$12$Bstd5zrZoDzwrc0LoSLEM.kECyH7/.PwY/l.ehKl/LJUbimZca.au")
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || missing || user.Disabled {
		return User{}, ErrCredentials
	}
	return user, nil
}

func (s *Store) User(ctx context.Context, id string) (User, error) {
	user, err := readUser(s.db.QueryRowContext(ctx, "SELECT id,name,admin,disabled,generation FROM users WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) || err == nil && user.Disabled {
		return User{}, ErrCredentials
	}
	return user, err
}

type userQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readUser(row *sql.Row) (User, error) {
	var user User
	err := row.Scan(&user.ID, &user.Name, &user.Admin, &user.Disabled, &user.Generation)
	return user, err
}

func currentUser(ctx context.Context, q userQuery, expected User) (User, error) {
	user, err := readUser(q.QueryRowContext(ctx, "SELECT id,name,admin,disabled,generation FROM users WHERE id=?", expected.ID))
	if errors.Is(err, sql.ErrNoRows) || err == nil && (user.Disabled || user.Generation != expected.Generation) {
		return User{}, ErrCredentials
	}
	return user, err
}

type TokenOptions struct {
	Name, Project string
	Role          Role
	Expires       time.Time
}

func (s *Store) CreateToken(ctx context.Context, user User, options TokenOptions) (string, error) {
	if options.Name == "" || len(options.Name) > 100 || !options.Role.Allows(Reader) || (!options.Expires.IsZero() && !options.Expires.After(time.Now())) {
		return "", errors.New("invalid token name or expiry")
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	token := "ld_" + rand.Text() + rand.Text()
	expires := int64(0)
	if !options.Expires.IsZero() {
		expires = options.Expires.Unix()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := currentUser(ctx, tx, user); err != nil {
		return "", err
	}
	if options.Project != "" {
		var owner, name string
		err := tx.QueryRowContext(ctx, "SELECT u.name,p.name FROM projects p JOIN users u ON p.owner=u.id WHERE p.id=?", options.Project).Scan(&owner, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		// Recheck within insertion's transaction so deletion cannot leave an
		// orphan scope. Personal tokens never acquire browser admin authority.
		if _, err := projectFor(ctx, tx, Principal{User: user, Token: true, Role: Reader}, owner, name, Reader); err != nil {
			return "", err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO tokens VALUES(?,?,?,?,?,?,?)", id, user.ID, options.Name, digest(token), options.Project, options.Role, expires)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) Token(ctx context.Context, token string) (Principal, error) {
	var p Principal
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.name,u.admin,u.generation,t.project,t.role,t.expires FROM tokens t JOIN users u ON u.id=t.user_id WHERE t.hash=? AND u.disabled=0`, digest(token)).Scan(&p.User.ID, &p.User.Name, &p.User.Admin, &p.User.Generation, &p.Project, &p.Role, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrCredentials
	}
	if err != nil {
		return Principal{}, err
	}
	if expires != 0 && expires <= time.Now().Unix() {
		return Principal{}, ErrCredentials
	}
	p.Token = true
	return p, nil
}

type TokenInfo struct {
	ID, Name, Project string
	Scope             string
	Role              Role
	Expires           time.Time
}

func (s *Store) Tokens(ctx context.Context, user User) ([]TokenInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.name,t.project,t.role,t.expires,COALESCE(u.name||'/'||p.name,'')
 FROM tokens t LEFT JOIN projects p ON p.id=t.project LEFT JOIN users u ON u.id=p.owner WHERE t.user_id=? ORDER BY t.name,t.id`, user.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []TokenInfo
	for rows.Next() {
		var token TokenInfo
		var expires int64
		if err := rows.Scan(&token.ID, &token.Name, &token.Project, &token.Role, &expires, &token.Scope); err != nil {
			return nil, err
		}
		if expires != 0 {
			token.Expires = time.Unix(expires, 0)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) RevokeToken(ctx context.Context, user User, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := currentUser(ctx, tx, user); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tokens WHERE user_id=? AND id=?", user.ID, id); err != nil {
		return err
	}
	return tx.Commit()
}

func administrator(ctx context.Context, q userQuery, actor Principal) error {
	if actor.Token {
		return ErrAccess
	}
	user, err := currentUser(ctx, q, actor.User)
	if err != nil {
		return err
	}
	if !user.Admin {
		return ErrAccess
	}
	return nil
}

// CreateUser provisions an ordinary account. Site administration is available
// only through an authenticated administrator session, never a personal token.
func (s *Store) CreateUser(ctx context.Context, actor Principal, name, password string) (User, error) {
	if err := administrator(ctx, s.db, actor); err != nil {
		return User{}, err
	}
	if !validUsername(name) {
		return User{}, ErrUsername
	}
	hash, err := passwordHash(password)
	if err != nil {
		return User{}, err
	}
	id, err := newID()
	if err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if err := administrator(ctx, tx, actor); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,name,password,admin) VALUES(?,?,?,0)", id, name, hash); err != nil {
		var constraint *sqlite.Error
		if errors.As(err, &constraint) && constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			return User{}, ErrUserExists
		}
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: id, Name: name, Generation: 1}, nil
}

func (s *Store) Users(ctx context.Context, actor Principal, after string) ([]User, string, error) {
	if err := administrator(ctx, s.db, actor); err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,name,admin,disabled,generation FROM users WHERE name>? ORDER BY name LIMIT 101", after)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Name, &user.Admin, &user.Disabled, &user.Generation); err != nil {
			return nil, "", err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(users) > 100 {
		return users[:100], users[99].Name, nil
	}
	return users, "", nil
}

// UserNames resolves authors of already-authorized project history, including
// disabled accounts. It does not expose account credentials or permissions.
func (s *Store) UserNames(ctx context.Context, ids []string) (map[string]string, error) {
	names := map[string]string{}
	if len(ids) == 0 {
		return names, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,name FROM users WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}
