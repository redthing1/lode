package instance

import (
	"bytes"
	"context"
	"crypto/rand"
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

var ErrKey = errors.New("security key verification failed; try again")
var ErrKeyName = errors.New("key names must contain 1–100 bytes")
var ErrKeyLimit = errors.New("at most ten keys may be registered")
var ErrKeyRegistered = errors.New("this key is already registered")

const challengeLifetime = 3 * time.Minute

type Key struct {
	ID, Name string
	Created  time.Time
}

type Ceremony struct {
	ID      string `json:"id"`
	Options any    `json:"options"`
}

type keyUser struct {
	User
	credentials []webauthn.Credential
	records     map[string][]byte
}

func (u keyUser) WebAuthnID() []byte                         { return []byte(u.ID) }
func (u keyUser) WebAuthnName() string                       { return u.Name }
func (u keyUser) WebAuthnDisplayName() string                { return u.Name }
func (u keyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func (s *Store) initAuthentication() error {
	origin, err := url.Parse(s.Config.URL)
	if err != nil {
		return err
	}
	s.auth, err = webauthn.New(&webauthn.Config{
		RPDisplayName: "Lode", RPID: origin.Hostname(), RPOrigins: []string{s.Config.URL},
		AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: challengeLifetime},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: challengeLifetime},
		},
	})
	return err
}

func (s *Store) Keys(ctx context.Context, user User) ([]Key, error) {
	if _, err := currentUser(ctx, s.db, user); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,name,created FROM security_keys WHERE user_id=? AND rp=? ORDER BY created,id", user.ID, s.auth.Config.RPID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []Key
	for rows.Next() {
		var key Key
		var created int64
		if err := rows.Scan(&key.ID, &key.Name, &created); err != nil {
			return nil, err
		}
		key.Created = time.Unix(created, 0)
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) keyUser(ctx context.Context, expected User) (keyUser, error) {
	user, err := currentUser(ctx, s.db, expected)
	if err != nil {
		return keyUser{}, err
	}
	u := keyUser{User: user, records: map[string][]byte{}}
	rows, err := s.db.QueryContext(ctx, "SELECT id,data FROM security_keys WHERE user_id=? AND rp=? ORDER BY id", user.ID, s.auth.Config.RPID)
	if err != nil {
		return keyUser{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var data []byte
		var credential webauthn.Credential
		if err := rows.Scan(&id, &data); err != nil {
			return keyUser{}, err
		}
		if err := json.Unmarshal(data, &credential); err != nil {
			return keyUser{}, err
		}
		if base64.RawURLEncoding.EncodeToString(credential.ID) != id {
			return keyUser{}, errors.New("stored credential identity mismatch")
		}
		u.credentials = append(u.credentials, credential)
		u.records[id] = data
	}
	return u, rows.Err()
}

// beginChallenge records only server-generated protocol state. Each browser has
// at most one current challenge; consumption is atomic across simultaneous finishes.
func (s *Store) beginChallenge(ctx context.Context, binding, purpose, name string, user User, options any, session *webauthn.SessionData) (Ceremony, error) {
	if binding == "" || len(binding) > 256 {
		return Ceremony{}, ErrKey
	}
	id, err := newID()
	if err != nil {
		return Ceremony{}, err
	}
	data, err := json.Marshal(session)
	if err != nil {
		return Ceremony{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ceremony{}, err
	}
	defer tx.Rollback()
	var userID any
	if user.ID != "" {
		if _, err := currentUser(ctx, tx, user); err != nil {
			return Ceremony{}, err
		}
		userID = user.ID
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM challenges WHERE expires<=? OR binding=?", time.Now().Unix(), binding); err != nil {
		return Ceremony{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO challenges VALUES(?,?,?,?,?,?,?,?)", id, binding, purpose, userID, user.Generation, data, name, time.Now().Add(challengeLifetime).Unix()); err != nil {
		return Ceremony{}, err
	}
	if err := tx.Commit(); err != nil {
		return Ceremony{}, err
	}
	return Ceremony{ID: id, Options: options}, nil
}

type challenge struct {
	user    User
	name    string
	session webauthn.SessionData
}

func (s *Store) takeChallenge(ctx context.Context, binding, purpose, id string) (challenge, error) {
	var c challenge
	var userID sql.NullString
	var data []byte
	err := s.db.QueryRowContext(ctx, "DELETE FROM challenges WHERE id=? AND binding=? AND purpose=? AND expires>? RETURNING user_id,generation,data,name", id, binding, purpose, time.Now().Unix()).Scan(&userID, &c.user.Generation, &data, &c.name)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrKey
	}
	if err != nil {
		return c, err
	}
	c.user.ID = userID.String
	if err := json.Unmarshal(data, &c.session); err != nil {
		return c, err
	}
	return c, nil
}

func boundAssertion(r *http.Request) error {
	if r == nil || r.Body == nil || r.ContentLength > 1<<20 {
		return ErrKey
	}
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	return nil
}

func (s *Store) BeginRegistration(ctx context.Context, actor Principal, binding, password, name string, discoverable bool) (Ceremony, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return Ceremony{}, ErrKeyName
	}
	if err := s.checkPassword(ctx, actor, password); err != nil {
		return Ceremony{}, err
	}
	u, err := s.keyUser(ctx, actor.User)
	if err != nil {
		return Ceremony{}, err
	}
	if len(u.credentials) >= 10 {
		return Ceremony{}, ErrKeyLimit
	}
	selection := protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired, ResidentKey: protocol.ResidentKeyRequirementDiscouraged, RequireResidentKey: protocol.ResidentKeyNotRequired()}
	if discoverable {
		selection.ResidentKey, selection.RequireResidentKey = protocol.ResidentKeyRequirementRequired, protocol.ResidentKeyRequired()
	}
	var excluded []protocol.CredentialDescriptor
	for _, credential := range u.credentials {
		excluded = append(excluded, credential.Descriptor())
	}
	options, session, err := s.auth.BeginRegistration(u, webauthn.WithAuthenticatorSelection(selection), webauthn.WithExclusions(excluded))
	if err != nil {
		return Ceremony{}, err
	}
	return s.beginChallenge(ctx, binding, "register", name, u.User, options, session)
}

// FinishRegistration returns the generation committed by this authenticated
// mutation, so the caller can renew its session without adopting a later reset.
func (s *Store) FinishRegistration(ctx context.Context, actor Principal, binding, id string, r *http.Request) (User, []string, error) {
	if actor.Token {
		return User{}, nil, ErrAccess
	}
	c, err := s.takeChallenge(ctx, binding, "register", id)
	if err != nil {
		return User{}, nil, err
	}
	if c.user.ID != actor.User.ID || c.user.Generation != actor.User.Generation {
		return User{}, nil, ErrKey
	}
	u, err := s.keyUser(ctx, c.user)
	if err != nil {
		return User{}, nil, err
	}
	if err := boundAssertion(r); err != nil {
		return User{}, nil, err
	}
	credential, err := s.auth.FinishRegistration(u, c.session, r)
	if err != nil || credential == nil || !credential.Flags.UserVerified {
		return User{}, nil, ErrKey
	}
	data, err := json.Marshal(credential)
	if err != nil {
		return User{}, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, nil, err
	}
	defer tx.Rollback()
	if _, err := currentUser(ctx, tx, u.User); err != nil {
		return User{}, nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM security_keys WHERE user_id=? AND rp=?", u.ID, s.auth.Config.RPID).Scan(&count); err != nil {
		return User{}, nil, err
	}
	if count >= 10 {
		return User{}, nil, ErrKeyLimit
	}
	inserted, err := tx.ExecContext(ctx, "INSERT INTO security_keys VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", base64.RawURLEncoding.EncodeToString(credential.ID), u.ID, s.auth.Config.RPID, c.name, data, time.Now().Unix())
	if err != nil {
		return User{}, nil, err
	}
	if n, err := inserted.RowsAffected(); err != nil || n != 1 {
		return User{}, nil, ErrKeyRegistered
	}
	var codes []string
	if count == 0 {
		codes, err = replaceRecovery(ctx, tx, u.ID)
		if err != nil {
			return User{}, nil, err
		}
	}
	if err := invalidateAccount(ctx, tx, u.ID); err != nil {
		return User{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, nil, err
	}
	u.Generation++
	return u.User, codes, nil
}

func (s *Store) BeginKeyLogin(ctx context.Context, binding, username string) (Ceremony, error) {
	if binding == "" || len(binding) > 256 {
		return Ceremony{}, ErrKey
	}
	if username == "" {
		options, session, err := s.auth.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			return Ceremony{}, err
		}
		return s.beginChallenge(ctx, binding, "login", "", User{}, options, session)
	}
	user, err := readUser(s.db.QueryRowContext(ctx, "SELECT id,name,admin,disabled,generation FROM users WHERE name=?", username))
	if errors.Is(err, sql.ErrNoRows) || err == nil && user.Disabled {
		return s.unavailableKeyLogin(ctx, binding)
	}
	if err != nil {
		return Ceremony{}, err
	}
	u, err := s.keyUser(ctx, user)
	if err != nil {
		return Ceremony{}, err
	}
	if len(u.credentials) == 0 {
		return s.unavailableKeyLogin(ctx, binding)
	}
	return s.beginAssertion(ctx, binding, "login", u)
}

// Unknown accounts get a nonmatching request and no valid challenge. This keeps
// the visible username-first flow consistent without allowing discoverable fallback.
func (s *Store) unavailableKeyLogin(ctx context.Context, binding string) (Ceremony, error) {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM challenges WHERE expires<=? OR binding=?", time.Now().Unix(), binding); err != nil {
		return Ceremony{}, err
	}
	options, _, err := s.auth.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return Ceremony{}, err
	}
	options.Response.AllowedCredentials = []protocol.CredentialDescriptor{{Type: protocol.PublicKeyCredentialType, CredentialID: []byte(rand.Text())}}
	id, err := newID()
	return Ceremony{ID: id, Options: options}, err
}

func (s *Store) BeginVerification(ctx context.Context, user User, binding string) (Ceremony, error) {
	u, err := s.keyUser(ctx, user)
	if err != nil {
		return Ceremony{}, err
	}
	return s.beginAssertion(ctx, binding, "verify", u)
}

func (s *Store) beginAssertion(ctx context.Context, binding, purpose string, u keyUser) (Ceremony, error) {
	options, session, err := s.auth.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return Ceremony{}, ErrKey
	}
	return s.beginChallenge(ctx, binding, purpose, "", u.User, options, session)
}

func (s *Store) FinishKeyLogin(ctx context.Context, binding, id string, r *http.Request) (User, error) {
	return s.finishAssertion(ctx, binding, id, "login", User{}, r)
}

func (s *Store) FinishVerification(ctx context.Context, user User, binding, id string, r *http.Request) (User, error) {
	return s.finishAssertion(ctx, binding, id, "verify", user, r)
}

func (s *Store) finishAssertion(ctx context.Context, binding, id, purpose string, expected User, r *http.Request) (User, error) {
	c, err := s.takeChallenge(ctx, binding, purpose, id)
	if err != nil {
		return User{}, err
	}
	if purpose == "verify" && (expected.ID == "" || c.user.ID != expected.ID || c.user.Generation != expected.Generation) {
		return User{}, ErrKey
	}
	if c.session.UserVerification != protocol.VerificationRequired {
		return User{}, ErrKey
	}
	if err := boundAssertion(r); err != nil {
		return User{}, err
	}
	var u keyUser
	var credential *webauthn.Credential
	if c.user.ID != "" {
		u, err = s.keyUser(ctx, c.user)
		if err != nil {
			return User{}, err
		}
		credential, err = s.auth.FinishLogin(u, c.session, r)
	} else {
		lookup := func(rawID, handle []byte) (webauthn.User, error) {
			var id string
			if err := s.db.QueryRowContext(ctx, "SELECT user_id FROM security_keys WHERE id=? AND rp=?", base64.RawURLEncoding.EncodeToString(rawID), s.auth.Config.RPID).Scan(&id); err != nil || !bytes.Equal(handle, []byte(id)) {
				return nil, ErrKey
			}
			user, err := s.User(ctx, id)
			if err != nil {
				return nil, err
			}
			u, err = s.keyUser(ctx, user)
			return u, err
		}
		var verified webauthn.User
		verified, credential, err = s.auth.FinishPasskeyLogin(lookup, c.session, r)
		if err == nil && (verified == nil || !bytes.Equal(verified.WebAuthnID(), u.WebAuthnID())) {
			return User{}, ErrKey
		}
	}
	if err != nil || credential == nil || !credential.Flags.UserVerified || credential.Authenticator.CloneWarning {
		return User{}, ErrKey
	}
	data, err := json.Marshal(credential)
	if err != nil {
		return User{}, err
	}
	keyID := base64.RawURLEncoding.EncodeToString(credential.ID)
	previous := u.records[keyID]
	if len(previous) == 0 {
		return User{}, ErrKey
	}
	result, err := s.db.ExecContext(ctx, `UPDATE security_keys SET data=? WHERE id=? AND user_id=? AND rp=? AND data=? AND EXISTS(SELECT 1 FROM users WHERE id=? AND generation=? AND disabled=0)`, data, keyID, u.ID, s.auth.Config.RPID, previous, u.ID, u.Generation)
	if err != nil {
		return User{}, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return User{}, ErrKey
	}
	return u.User, nil
}
