package instance

import (
	"testing"
	"time"
)

func TestSessionRevocationSurvivesLateCommitAndExpiry(t *testing.T) {
	s, _ := instanceFixture(t)
	sessions := s.Sessions()
	expiry := time.Now().Add(SessionLifetime)
	if err := sessions.Commit("old", []byte("session"), expiry); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Delete("old"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Commit("old", []byte("late"), expiry); err != nil {
		t.Fatal(err)
	}
	if _, found, err := sessions.Find("old"); err != nil || found {
		t.Fatal("revoked session revived", err)
	}
	var until int64
	if err := s.db.QueryRow("SELECT expiry FROM sessions WHERE token='old'").Scan(&until); err != nil {
		t.Fatal(err)
	}
	if !time.Unix(until, 0).After(expiry) {
		t.Fatal("revocation expires before captured session deadline")
	}
	// After the revocation marker is eligible for collection, an old request's
	// absolute deadline has passed. A late write must not recreate its token.
	if _, err := s.db.Exec("UPDATE sessions SET expiry=0 WHERE token='old'"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Commit("new", []byte("new"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Commit("old", []byte("late"), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := sessions.Find("old"); err != nil || found {
		t.Fatal("expired session recreated", err)
	}
	if err := sessions.Commit("unbounded", []byte("invalid"), time.Now().Add(2*SessionLifetime)); err == nil {
		t.Fatal("unbounded lifetime accepted")
	}
}
