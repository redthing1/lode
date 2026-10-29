package instance

import (
	"database/sql"
	"errors"
	"time"
)

// Sessions implements SCS persistence using the instance database.
type Sessions struct{ db *sql.DB }

const SessionLifetime = 24 * time.Hour

func (s *Store) Sessions() *Sessions { return &Sessions{s.db} }

func (s *Sessions) Find(token string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRow("SELECT data FROM sessions WHERE token=? AND expiry>? AND length(data)>0", token, time.Now().Unix()).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return data, err == nil, err
}
func (s *Sessions) Delete(token string) error {
	// An in-flight request may commit after logout. Keep an empty row until
	// every session loaded before deletion has passed its absolute deadline.
	_, err := s.db.Exec("INSERT INTO sessions VALUES(?,x'',?) ON CONFLICT(token) DO UPDATE SET data=x'',expiry=excluded.expiry", token, time.Now().Add(SessionLifetime).Unix()+1)
	return err
}
func (s *Sessions) Commit(token string, data []byte, expiry time.Time) error {
	now := time.Now()
	if !expiry.After(now) {
		return nil
	}
	if expiry.After(now.Add(SessionLifetime)) || len(data) == 0 {
		return errors.New("invalid session lifetime or data")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM sessions WHERE expiry<=?", time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO sessions VALUES(?,?,?) ON CONFLICT(token) DO UPDATE SET data=excluded.data,expiry=excluded.expiry WHERE length(sessions.data)>0", token, data, expiry.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
