//go:build linux

package instance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Security keeps WebAuthn's complete credential and ceremony records. Challenges
// are bound to a browser session and consumed atomically before verification.
type Security struct {
	store *Store
	wa    *webauthn.WebAuthn
}

type Key struct {
	ID         string
	Name       string
	Created    time.Time
	Credential webauthn.Credential
	raw        []byte
}

type webUser struct {
	User
	keys    []webauthn.Credential
	keyData map[string][]byte
}

const maxWebAuthnBody = 1 << 20

func boundWebAuthnRequest(r *http.Request) error {
	if r == nil || r.Body == nil || r.ContentLength > maxWebAuthnBody {
		return ErrInvalid
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxWebAuthnBody)
	return nil
}

func (u webUser) WebAuthnID() []byte                         { return []byte(u.ID) }
func (u webUser) WebAuthnName() string                       { return u.Username }
func (u webUser) WebAuthnDisplayName() string                { return u.Username }
func (u webUser) WebAuthnCredentials() []webauthn.Credential { return u.keys }

func NewSecurity(s *Store, origin string) (*Security, error) {
	canonical, err := CanonicalURL(origin)
	if err != nil || canonical != origin {
		return nil, ErrInvalid
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil, ErrInvalid
	}
	wa, err := webauthn.New(&webauthn.Config{RPDisplayName: "Lode", RPID: u.Hostname(), RPOrigins: []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationPreferred},
		Timeouts:               webauthn.TimeoutsConfig{Login: webauthn.TimeoutConfig{Enforce: true, Timeout: 3 * time.Minute}, Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: 3 * time.Minute}}})
	if err != nil {
		return nil, err
	}
	return &Security{store: s, wa: wa}, nil
}

func (s *Security) Keys(ctx context.Context, userID string) ([]Key, error) {
	if len(userID) == 0 || len(userID) > 64 {
		return nil, ErrInvalid
	}
	rows, err := s.store.db.QueryContext(ctx, "SELECT id,name,credential,created FROM security_keys WHERE user_id=? ORDER BY created,id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []Key
	for rows.Next() {
		var k Key
		var data []byte
		var created int64
		if err := rows.Scan(&k.ID, &k.Name, &data, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &k.Credential); err != nil {
			return nil, err
		}
		k.Created = time.Unix(created, 0).UTC()
		k.raw = append([]byte(nil), data...)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Security) user(ctx context.Context, id string) (webUser, error) {
	u, err := s.store.GetUser(ctx, id)
	if err != nil {
		return webUser{}, err
	}
	if u.Disabled {
		return webUser{}, ErrUnauthorized
	}
	keys, err := s.Keys(ctx, id)
	w := webUser{User: u, keyData: make(map[string][]byte, len(keys))}
	for _, k := range keys {
		w.keys = append(w.keys, k.Credential)
		w.keyData[k.ID] = k.raw
	}
	return w, err
}

func (s *Security) Begin(ctx context.Context, userID, binding, purpose string) (string, any, error) {
	if len(userID) == 0 || len(userID) > 64 || len(binding) == 0 || len(binding) > 256 || len(purpose) > 16 {
		return "", nil, ErrInvalid
	}
	u, err := s.user(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	var options any
	var session *webauthn.SessionData
	switch purpose {
	case "register":
		if len(u.keys) >= 10 {
			return "", nil, errors.New("at most ten security keys may be registered")
		}
		var exclusions []protocol.CredentialDescriptor
		for _, k := range u.keys {
			exclusions = append(exclusions, k.Descriptor())
		}
		options, session, err = s.wa.BeginRegistration(u, webauthn.WithExclusions(exclusions))
	case "login":
		options, session, err = s.wa.BeginLogin(u)
	default:
		return "", nil, ErrInvalid
	}
	if err != nil {
		return "", nil, err
	}
	id, err := newID()
	if err != nil {
		return "", nil, err
	}
	data, err := json.Marshal(session)
	if err != nil {
		return "", nil, err
	}
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM ceremonies WHERE expires<=? OR (user_id=? AND binding=? AND purpose=?)", time.Now().Unix(), userID, binding, purpose); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO ceremonies VALUES(?,?,?,?,?,?,?)", id, userID, binding, purpose, data, time.Now().Add(3*time.Minute).Unix(), u.AuthVersion)
		return err
	})
	return id, options, err
}

func (s *Security) take(ctx context.Context, userID, binding, purpose, id string) (webUser, webauthn.SessionData, error) {
	if len(userID) == 0 || len(userID) > 64 || len(binding) == 0 || len(binding) > 256 || len(purpose) == 0 || len(purpose) > 16 || len(id) == 0 || len(id) > 64 {
		return webUser{}, webauthn.SessionData{}, ErrUnauthorized
	}
	u, err := s.user(ctx, userID)
	if err != nil {
		return u, webauthn.SessionData{}, err
	}
	var data []byte
	err = s.store.db.QueryRowContext(ctx, `DELETE FROM ceremonies WHERE id=? AND user_id=? AND binding=? AND purpose=? AND expires>? AND auth_version=? AND EXISTS(SELECT 1 FROM users WHERE id=? AND auth_version=? AND disabled=0) RETURNING data`, id, userID, binding, purpose, time.Now().Unix(), u.AuthVersion, userID, u.AuthVersion).Scan(&data)
	if err != nil {
		return u, webauthn.SessionData{}, ErrUnauthorized
	}
	var session webauthn.SessionData
	err = json.Unmarshal(data, &session)
	return u, session, err
}

func (s *Security) FinishRegistration(ctx context.Context, userID, binding, id, name string, r *http.Request) ([]string, error) {
	name = strings.TrimSpace(name)
	if len(userID) == 0 || len(userID) > 64 || len(binding) == 0 || len(binding) > 256 || len(name) == 0 || len(name) > 80 {
		return nil, errors.New("give this key a name of at most 80 characters")
	}
	u, session, err := s.take(ctx, userID, binding, "register", id)
	if err != nil {
		return nil, err
	}
	if err = boundWebAuthnRequest(r); err != nil {
		return nil, err
	}
	credential, err := s.wa.FinishRegistration(u, session, r)
	if err != nil {
		return nil, errors.New("security key registration could not be verified; try again")
	}
	data, err := json.Marshal(credential)
	if err != nil {
		return nil, err
	}
	var codes []string
	err = s.store.tx(ctx, func(tx *sql.Tx) error {
		var authVersion int64
		var disabled int
		if err := tx.QueryRowContext(ctx, `SELECT auth_version,disabled FROM users WHERE id=?`, userID).Scan(&authVersion, &disabled); err == sql.ErrNoRows || disabled != 0 || authVersion != u.AuthVersion {
			return ErrUnauthorized
		} else if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM security_keys WHERE user_id=?", userID).Scan(&count); err != nil {
			return err
		}
		if count >= 10 {
			return ErrInvalid
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO security_keys VALUES(?,?,?,?,?)", base64.RawURLEncoding.EncodeToString(credential.ID), userID, name, data, time.Now().Unix()); err != nil {
			return err
		}
		if count == 0 {
			var err error
			codes, err = replaceRecovery(ctx, tx, userID)
			if err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, "UPDATE users SET auth_version=auth_version+1 WHERE id=? AND auth_version=? AND disabled=0", userID, u.AuthVersion)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrUnauthorized
		}
		return nil
	})
	return codes, err
}

func (s *Security) FinishLogin(ctx context.Context, userID, binding, id string, r *http.Request) error {
	u, session, err := s.take(ctx, userID, binding, "login", id)
	if err != nil {
		return err
	}
	if err = boundWebAuthnRequest(r); err != nil {
		return err
	}
	credential, err := s.wa.FinishLogin(u, session, r)
	if err != nil || credential.Authenticator.CloneWarning {
		return ErrUnauthorized
	}
	data, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	keyID := base64.RawURLEncoding.EncodeToString(credential.ID)
	oldData := u.keyData[keyID]
	if len(oldData) == 0 {
		return ErrUnauthorized
	}
	result, err := s.store.db.ExecContext(ctx, "UPDATE security_keys SET credential=? WHERE id=? AND user_id=? AND credential=? AND EXISTS(SELECT 1 FROM users WHERE id=? AND auth_version=? AND disabled=0)", data, keyID, userID, oldData, userID, u.AuthVersion)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrUnauthorized
	}
	return err
}

func replaceRecovery(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=?", userID); err != nil {
		return nil, err
	}
	var result []string
	for range 10 {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		code := base64.RawURLEncoding.EncodeToString(b[:])
		hash := sha256.Sum256([]byte(code))
		if _, err := tx.ExecContext(ctx, "INSERT INTO recovery_codes VALUES(?,?)", userID, hash[:]); err != nil {
			return nil, err
		}
		result = append(result, code)
	}
	return result, nil
}

func (s *Security) Recovery(ctx context.Context, userID string) ([]string, error) {
	if len(userID) == 0 || len(userID) > 64 {
		return nil, ErrInvalid
	}
	var codes []string
	err := s.store.tx(ctx, func(tx *sql.Tx) error {
		var disabled int
		if err := tx.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id=?`, userID).Scan(&disabled); err == sql.ErrNoRows || disabled != 0 {
			return ErrUnauthorized
		} else if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM security_keys WHERE user_id=?", userID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return errors.New("add a security key before generating recovery codes")
		}
		var err error
		codes, err = replaceRecovery(ctx, tx, userID)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE users SET auth_version=auth_version+1 WHERE id=? AND disabled=0`, userID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrUnauthorized
		}
		return nil
	})
	return codes, err
}

func (s *Security) UseRecovery(ctx context.Context, userID, code string) error {
	if len(userID) == 0 || len(userID) > 64 || len(code) != 22 {
		return ErrUnauthorized
	}
	if _, err := base64.RawURLEncoding.DecodeString(code); err != nil {
		return ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(code))
	result, err := s.store.db.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=? AND hash=? AND EXISTS(SELECT 1 FROM users WHERE id=? AND disabled=0)", userID, hash[:], userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrUnauthorized
	}
	return err
}

func (s *Security) RemoveKey(ctx context.Context, userID, id string) error {
	if len(userID) == 0 || len(userID) > 64 || len(id) == 0 || len(id) > base64.RawURLEncoding.EncodedLen(1023) {
		return ErrInvalid
	}
	return s.store.tx(ctx, func(tx *sql.Tx) error {
		var disabled int
		if e := tx.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id=?`, userID).Scan(&disabled); e == sql.ErrNoRows || disabled != 0 {
			return ErrUnauthorized
		} else if e != nil {
			return e
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM security_keys WHERE id=? AND user_id=?", id, userID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id=? AND NOT EXISTS(SELECT 1 FROM security_keys WHERE user_id=?)", userID, userID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE users SET auth_version=auth_version+1 WHERE id=?", userID)
		return err
	})
}
