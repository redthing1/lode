// Package instance owns accounts, project access and the server's local resources.
package instance

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/project"
	_ "modernc.org/sqlite"
)

type Config struct {
	URL            string         `json:"url"`
	Listen         string         `json:"listen"`
	TrustedProxies []netip.Prefix `json:"trusted_proxies,omitempty"`
}

func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("config requires an http(s) origin and listen address")
	}
	if strings.HasSuffix(u.Host, ":") || u.Port() != "" && !validPort(u.Port()) {
		return errors.New("canonical URL has an invalid port")
	}
	if err := protocol.ValidateRPID(u.Hostname()); err != nil {
		return fmt.Errorf("invalid canonical URL hostname (use localhost for local access): %w", err)
	}
	if u.Scheme == "http" && !strings.EqualFold(u.Hostname(), "localhost") {
		return errors.New("canonical URL requires HTTPS except on localhost")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || !validPort(port) || net.ParseIP(host) == nil {
		return errors.New("listen requires an IP address and port, such as 127.0.0.1:8473")
	}
	for _, prefix := range c.TrustedProxies {
		if !prefix.IsValid() {
			return errors.New("trusted_proxies requires IP prefixes")
		}
	}
	return nil
}

func validPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

type Store struct {
	Config    Config
	ID        string
	directory string
	db        *sql.DB
	key       []byte
	lock      *flock.Flock
	lifecycle sync.Mutex // Serializes repository open, eviction, deletion and shutdown.
	mu        sync.Mutex
	projects  map[string]*openedProject
	auth      *webauthn.WebAuthn
}

func digest(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }

// Initialize creates a new instance and returns its one-use administrator setup code.
func Initialize(ctx context.Context, directory string, config Config) (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	destination, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	lock, err := prepareDirectory(destination)
	if err != nil {
		return "", err
	}
	defer lock.Unlock()
	stage, err := os.MkdirTemp(destination, ".lode-init-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	directory = stage
	if err := os.Chmod(directory, 0700); err != nil {
		return "", err
	}
	config.URL = strings.TrimRight(config.URL, "/")
	b, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), append(b, '\n'), 0600); err != nil {
		return "", err
	}
	if err := syncPath(filepath.Join(directory, "config.json")); err != nil {
		return "", err
	}
	db, err := database(directory)
	if err != nil {
		return "", err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `
PRAGMA auto_vacuum=INCREMENTAL;
CREATE TABLE instance(id TEXT PRIMARY KEY,secret BLOB NOT NULL,setup_hash TEXT NOT NULL);
CREATE TABLE users(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,password BLOB NOT NULL,admin INTEGER NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,generation INTEGER NOT NULL DEFAULT 1);
CREATE TABLE tokens(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),name TEXT NOT NULL,hash TEXT NOT NULL UNIQUE,project TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('reader','writer','maintainer')),expires INTEGER NOT NULL);
CREATE TABLE sessions(token TEXT PRIMARY KEY,data BLOB NOT NULL,expiry INTEGER NOT NULL);
CREATE INDEX session_expiry ON sessions(expiry);
CREATE TABLE security_keys(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),rp TEXT NOT NULL,name TEXT NOT NULL,data BLOB NOT NULL,created INTEGER NOT NULL);
CREATE INDEX key_owner ON security_keys(user_id,rp);
CREATE TABLE challenges(id TEXT PRIMARY KEY,binding TEXT NOT NULL,purpose TEXT NOT NULL,user_id TEXT REFERENCES users(id),generation INTEGER NOT NULL,data BLOB NOT NULL,name TEXT NOT NULL,expires INTEGER NOT NULL);
CREATE TABLE recovery_codes(user_id TEXT NOT NULL REFERENCES users(id),hash TEXT NOT NULL,PRIMARY KEY(user_id,hash));
CREATE TABLE projects(id TEXT PRIMARY KEY,owner TEXT NOT NULL REFERENCES users(id),name TEXT NOT NULL,created INTEGER NOT NULL,state TEXT NOT NULL DEFAULT 'creating' CHECK(state IN ('creating','ready','deleting')),UNIQUE(owner,name));
CREATE TABLE members(project TEXT NOT NULL REFERENCES projects(id),user_id TEXT NOT NULL REFERENCES users(id),role TEXT NOT NULL CHECK(role IN ('reader','writer','maintainer')),PRIMARY KEY(project,user_id));
CREATE INDEX member_user ON members(user_id,project);
PRAGMA user_version=7;`)
	if err != nil {
		return "", err
	}
	if err := project.Initialize(ctx, db); err != nil {
		return "", err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	setup := setupCode(key)
	if _, err := db.ExecContext(ctx, "INSERT INTO instance VALUES(?,?,?)", id.String(), key, digest(setup)); err != nil {
		return "", err
	}
	if err := db.Close(); err != nil {
		return "", err
	}
	marker, err := os.OpenFile(filepath.Join(destination, "projects", ".instance"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := io.WriteString(marker, id.String()+"\n")
	if err := errors.Join(writeErr, marker.Sync(), marker.Close()); err != nil {
		return "", err
	}
	if err := publishState(stage, destination); err != nil {
		return "", err
	}
	return setup, nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func setupCode(key []byte) string {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte("administrator setup"))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func database(directory string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", filepath.Join(directory, "instance.sqlite")+"?_pragma=synchronous(EXTRA)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func Open(ctx context.Context, directory string) (_ *Store, err error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	lock, err := lockDirectory(root)
	if err != nil {
		return nil, err
	}
	s, err := openStore(ctx, root, filepath.Join(root, "state"))
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	s.lock = lock
	return s, nil
}

// The caller owns the root lock, including while verifying unpublished state.
func openStore(ctx context.Context, root, state string) (_ *Store, err error) {
	s := &Store{directory: root, projects: map[string]*openedProject{}}
	defer func() {
		if err != nil {
			_ = s.Close(ctx)
		}
	}()
	b, err := os.ReadFile(filepath.Join(state, "config.json"))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s.Config); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("config must contain one JSON object")
	}
	if err := s.Config.Validate(); err != nil {
		return nil, err
	}
	s.Config.URL = strings.TrimRight(s.Config.URL, "/")
	s.db, err = database(state)
	if err != nil {
		return nil, err
	}
	var format int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&format); err != nil {
		return nil, err
	}
	if format != 7 {
		return nil, errors.New("unsupported instance format; create a new instance")
	}
	if err := s.db.QueryRowContext(ctx, "SELECT id,secret FROM instance").Scan(&s.ID, &s.key); err != nil {
		return nil, err
	}
	if len(s.key) != 32 {
		return nil, errors.New("invalid instance secret")
	}
	marker, err := os.ReadFile(filepath.Join(root, "projects", ".instance"))
	if err != nil {
		return nil, fmt.Errorf("read project storage identity: %w", err)
	}
	if strings.TrimSpace(string(marker)) != s.ID {
		return nil, errors.New("project storage belongs to a different instance")
	}
	if err := s.initAuthentication(); err != nil {
		return nil, err
	}
	if err := s.resumeDeletions(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projects {
		if p.references != 0 {
			return errors.New("instance has active project requests")
		}
	}
	var result error
	for id, p := range s.projects {
		result = errors.Join(result, p.close(ctx))
		delete(s.projects, id)
	}
	if s.db != nil {
		result = errors.Join(result, s.db.Close())
		s.db = nil
	}
	if s.lock != nil {
		result = errors.Join(result, s.lock.Unlock())
		s.lock = nil
	}
	return result
}

func validName(name string) bool {
	if len(name) < 1 || len(name) > 64 || name[0] == '-' || name[0] == '_' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func validUsername(name string) bool {
	if !validName(name) {
		return false
	}
	switch name {
	case "api", "assets", "setup", "login", "logout", "verify", "settings", "admin", "projects", "healthz":
		return false
	}
	return true
}

func newID() (string, error) { id, err := uuid.NewRandom(); return id.String(), err }
