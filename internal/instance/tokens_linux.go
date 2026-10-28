//go:build linux

package instance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"strings"
	"time"
)

type Role string

const (
	RoleReader     Role = "reader"
	RoleWriter     Role = "writer"
	RoleMaintainer Role = "maintainer"
)

type Token struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	ProjectID  string     `json:"projectId,omitempty"`
	Role       Role       `json:"role"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool       `json:"revoked"`
}

func validRole(r Role) bool { return r == RoleReader || r == RoleWriter || r == RoleMaintainer }
func rank(r Role) int {
	switch r {
	case RoleReader:
		return 1
	case RoleWriter:
		return 2
	case RoleMaintainer:
		return 3
	}
	return 0
}
func lesser(a, b Role) Role {
	if rank(a) < rank(b) {
		return a
	}
	return b
}

func (s *Store) PutMembership(ctx context.Context, userID, projectID string, role Role) error {
	if len(userID) == 0 || len(userID) > 64 || len(projectID) == 0 || len(projectID) > 128 || !validRole(role) {
		return ErrInvalid
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := mutableMembership(ctx, tx, userID, projectID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO memberships(user_id,project_id,role) VALUES(?,?,?) ON CONFLICT(user_id,project_id) DO UPDATE SET role=excluded.role`, userID, projectID, string(role))
		if err != nil {
			return mapConflict(err)
		}
		return nil
	})
}

func (s *Store) RemoveMembership(ctx context.Context, userID, projectID string) error {
	if len(userID) == 0 || len(userID) > 64 || len(projectID) == 0 || len(projectID) > 128 {
		return ErrInvalid
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := mutableMembership(ctx, tx, userID, projectID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM memberships WHERE user_id=? AND project_id=?`, userID, projectID)
		return err
	})
}

func mutableMembership(ctx context.Context, tx *sql.Tx, userID, projectID string) error {
	var ownerID string
	err := tx.QueryRowContext(ctx, "SELECT owner_id FROM projects WHERE id=? AND NOT deleting", projectID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if userID == ownerID {
		return ErrConflict
	}
	return nil
}

const projectRoleQuery = `SELECT CASE WHEN u.admin OR p.owner_id=u.id THEN 'maintainer' ELSE COALESCE(m.role,'') END
	FROM projects p JOIN users u ON u.id=? LEFT JOIN memberships m ON m.project_id=p.id AND m.user_id=u.id
	WHERE p.id=? AND NOT p.deleting`

func (s *Store) RoleFor(ctx context.Context, userID, projectID string) (Role, error) {
	if len(userID) == 0 || len(userID) > 64 || len(projectID) == 0 || len(projectID) > 128 {
		return "", ErrInvalid
	}
	var role string
	err := s.db.QueryRowContext(ctx, projectRoleQuery, userID, projectID).Scan(&role)
	if err == sql.ErrNoRows || err == nil && role == "" {
		return "", ErrNotFound
	}
	return Role(role), err
}

// Authorize reevaluates current account state, membership, and token restrictions on every operation.
func (s *Store) Authorize(ctx context.Context, p Principal, projectID string, required Role) error {
	if len(p.UserID) == 0 || len(p.UserID) > 64 || len(projectID) == 0 || len(projectID) > 128 || !validRole(required) {
		return ErrInvalid
	}
	var disabled, force int
	var version int64
	err := s.db.QueryRowContext(ctx, `SELECT disabled,force_password_change,auth_version FROM users WHERE id=?`, p.UserID).Scan(&disabled, &force, &version)
	if err == sql.ErrNoRows || disabled != 0 || force != 0 || version != p.AuthVersion {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	current, err := s.RoleFor(ctx, p.UserID, projectID)
	if err == ErrNotFound {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if p.TokenID != "" {
		var scope sql.NullString
		var tokenRole string
		var expiry sql.NullInt64
		var revoked int
		if err := s.db.QueryRowContext(ctx, `SELECT project_id,role,expires_at,revoked FROM personal_tokens WHERE id=? AND user_id=?`, p.TokenID, p.UserID).Scan(&scope, &tokenRole, &expiry, &revoked); err == sql.ErrNoRows {
			return ErrUnauthorized
		} else if err != nil {
			return err
		}
		if revoked != 0 || expiry.Valid && expiry.Int64 <= time.Now().UTC().Unix() || !validRole(Role(tokenRole)) {
			return ErrUnauthorized
		}
		if scope.Valid && scope.String != projectID {
			return ErrUnauthorized
		}
		current = lesser(current, Role(tokenRole))
	}
	if rank(current) < rank(required) {
		return ErrUnauthorized
	}
	return nil
}

func (s *Store) CreateToken(ctx context.Context, ownerID, name string, role Role, projectID string, expiresAt time.Time) (string, Token, error) {
	name = strings.TrimSpace(name)
	if len(ownerID) == 0 || len(ownerID) > 64 || name == "" || len(name) > 80 || len(projectID) > 128 || !validRole(role) {
		return "", Token{}, ErrInvalid
	}
	now := time.Now().UTC()
	if expiresAt.IsZero() {
		expiresAt = now.Add(90 * 24 * time.Hour)
	}
	if !expiresAt.After(now) || expiresAt.After(now.Add(365*24*time.Hour)) {
		return "", Token{}, ErrInvalid
	}
	id, err := newID()
	if err != nil {
		return "", Token{}, err
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", Token{}, err
	}
	secret := "lode_" + base64.RawURLEncoding.EncodeToString(b)
	prefix := secret[:14]
	digest := sha256.Sum256([]byte(secret))
	exp := expiresAt.UTC().Unix()
	var pid any
	if projectID != "" {
		pid = projectID
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var disabled, force int
		if e := tx.QueryRowContext(ctx, `SELECT disabled,force_password_change FROM users WHERE id=?`, ownerID).Scan(&disabled, &force); e == sql.ErrNoRows {
			return ErrUnauthorized
		} else if e != nil {
			return e
		}
		if disabled != 0 || force != 0 {
			return ErrUnauthorized
		}
		if projectID != "" {
			var current Role
			e := tx.QueryRowContext(ctx, projectRoleQuery, ownerID, projectID).Scan(&current)
			if e == sql.ErrNoRows {
				return ErrUnauthorized
			}
			if e != nil {
				return e
			}
			if rank(current) < rank(role) {
				return ErrUnauthorized
			}
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO personal_tokens(id,user_id,name,prefix,secret_hash,project_id,role,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, ownerID, name, prefix, digest[:], pid, string(role), now.Unix(), exp)
		if e != nil {
			return mapConflict(e)
		}
		return nil
	})
	if err != nil {
		return "", Token{}, err
	}
	t := Token{ID: id, UserID: ownerID, Name: name, Prefix: prefix, ProjectID: projectID, Role: role, CreatedAt: now, ExpiresAt: &expiresAt}
	return secret, t, nil
}

func (s *Store) AuthenticateToken(ctx context.Context, secret string) (Principal, error) {
	if len(secret) != 48 || !strings.HasPrefix(secret, "lode_") {
		return Principal{}, ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(secret))
	now := time.Now().UTC().Unix()
	var p Principal
	var role string
	var expiry sql.NullInt64
	var revoked, disabled, force int
	err := s.db.QueryRowContext(ctx, `SELECT t.id,t.user_id,u.username,u.disabled,u.force_password_change,u.auth_version,t.role,t.expires_at,t.revoked FROM personal_tokens t JOIN users u ON u.id=t.user_id WHERE t.secret_hash=?`, digest[:]).Scan(&p.TokenID, &p.UserID, &p.Username, &disabled, &force, &p.AuthVersion, &role, &expiry, &revoked)
	if err == sql.ErrNoRows {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	if revoked != 0 || disabled != 0 || force != 0 || expiry.Valid && expiry.Int64 <= now {
		return Principal{}, ErrUnauthorized
	}
	// Personal tokens carry project authority only, even when their owner is an administrator.
	p.Admin = false
	p.ForcePasswordChange = force != 0
	if !validRole(Role(role)) {
		return Principal{}, ErrUnauthorized
	}
	// Update usage at most once per minute; this does not change authorization state.
	_, err = s.db.ExecContext(ctx, `UPDATE personal_tokens SET last_used_at=? WHERE id=? AND (last_used_at IS NULL OR last_used_at<?)`, now, p.TokenID, now-60)
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}

func (s *Store) ListTokens(ctx context.Context, ownerID string) ([]Token, error) {
	if len(ownerID) == 0 || len(ownerID) > 64 {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,name,prefix,project_id,role,created_at,expires_at,last_used_at,revoked FROM personal_tokens WHERE user_id=? ORDER BY created_at DESC,id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Token
	for rows.Next() {
		var t Token
		var pid sql.NullString
		var exp, last sql.NullInt64
		var revoked int
		var role string
		var created int64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &pid, &role, &created, &exp, &last, &revoked); err != nil {
			return nil, err
		}
		t.Role = Role(role)
		t.CreatedAt = time.Unix(created, 0).UTC()
		if pid.Valid {
			t.ProjectID = pid.String
		}
		if exp.Valid {
			x := time.Unix(exp.Int64, 0).UTC()
			t.ExpiresAt = &x
		}
		if last.Valid {
			x := time.Unix(last.Int64, 0).UTC()
			t.LastUsedAt = &x
		}
		t.Revoked = revoked != 0
		result = append(result, t)
	}
	return result, rows.Err()
}

func (s *Store) RevokeToken(ctx context.Context, ownerID, tokenID string) error {
	if len(ownerID) == 0 || len(ownerID) > 64 || len(tokenID) == 0 || len(tokenID) > 64 {
		return ErrInvalid
	}
	res, err := s.db.ExecContext(ctx, `UPDATE personal_tokens SET revoked=1 WHERE id=? AND user_id=?`, tokenID, ownerID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
