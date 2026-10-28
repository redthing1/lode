//go:build linux

package web

import (
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/alexedwards/scs/v2"
)

type sessionStore struct {
	db      *sql.DB
	mu      sync.Mutex
	cleaned time.Time
}

func sessions(db *sql.DB, secure bool) *scs.SessionManager {
	s := scs.New()
	s.Store = &sessionStore{db: db}
	s.Lifetime = 24 * time.Hour
	s.IdleTimeout = 30 * time.Minute
	s.Cookie.Name = "lode-local"
	if secure {
		s.Cookie.Name = "__Host-lode"
	}
	s.Cookie.Secure = secure
	s.Cookie.HttpOnly = true
	s.Cookie.Persist = false
	return s
}

func (s *sessionStore) Delete(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token=?", token)
	return err
}
func (s *sessionStore) Find(token string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRow("SELECT data FROM sessions WHERE token=? AND expiry>?", token, time.Now().Unix()).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return data, err == nil, err
}
func (s *sessionStore) Commit(token string, data []byte, expiry time.Time) error {
	s.mu.Lock()
	if time.Since(s.cleaned) > time.Minute {
		if _, err := s.db.Exec("DELETE FROM sessions WHERE expiry<=?", time.Now().Unix()); err != nil {
			s.mu.Unlock()
			return err
		}
		s.cleaned = time.Now()
	}
	s.mu.Unlock()
	_, err := s.db.Exec("INSERT INTO sessions VALUES(?,?,?) ON CONFLICT(token) DO UPDATE SET data=excluded.data,expiry=excluded.expiry", token, data, expiry.Unix())
	return err
}

// Login attempts are bounded globally and per account. Expired buckets are
// removed, and the map has a fixed ceiling even for invented usernames.
type attempts struct {
	mu      sync.Mutex
	entries map[string]attempt
	global  attempt
}
type attempt struct {
	start time.Time
	count int
}

func (a *attempts) allow(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if now.Sub(a.global.start) > time.Minute {
		a.global = attempt{start: now}
	}
	if a.global.count >= 60 {
		return false
	}
	a.global.count++
	if a.entries == nil {
		a.entries = make(map[string]attempt)
	}
	for k, v := range a.entries {
		if now.Sub(v.start) > time.Minute {
			delete(a.entries, k)
		}
	}
	v, ok := a.entries[key]
	if !ok {
		v.start = now
	}
	v.count++
	a.entries[key] = v
	return v.count <= 10
}
