//go:build linux

package instance

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestVersionOneStoreMigrationPreservesKeysAndDropsPendingCeremonies(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "instance.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(ctx, "migration-user", "a sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	security, err := NewSecurity(store, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	auth := newSyntheticAuthenticator(t, "migration-preserved-credential")
	registerSynthetic(t, ctx, security, user, auth, "enrollment-session")
	before, err := security.Keys(ctx, user.ID)
	if err != nil || len(before) != 1 {
		t.Fatalf("registered key count %d, error %v", len(before), err)
	}
	if _, _, err := security.Begin(ctx, user.ID, "browser", "login"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE ceremonies_v1(id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), binding TEXT NOT NULL, purpose TEXT NOT NULL, data BLOB NOT NULL, expires INTEGER NOT NULL, auth_version INTEGER NOT NULL);
		INSERT INTO ceremonies_v1 SELECT * FROM ceremonies;
		DROP TABLE ceremonies;
		ALTER TABLE ceremonies_v1 RENAME TO ceremonies;
		PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	var version int
	if err := upgraded.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("migrated schema version %d, error %v", version, err)
	}
	upgradedSecurity, err := NewSecurity(upgraded, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	after, err := upgradedSecurity.Keys(ctx, user.ID)
	if err != nil || len(after) != 1 || string(after[0].raw) != string(before[0].raw) {
		t.Fatalf("migrated key changed or disappeared: count %d, error %v", len(after), err)
	}
	var pending int
	if err := upgraded.db.QueryRowContext(ctx, "SELECT count(*) FROM ceremonies").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending v1 ceremony count %d, error %v", pending, err)
	}
	if _, _, err := upgradedSecurity.BeginPasskey(ctx, "anonymous-binding", ""); err != nil {
		t.Fatalf("anonymous challenge could not be stored after migration: %v", err)
	}
	var anonymous sql.NullString
	if err := upgraded.db.QueryRowContext(ctx, "SELECT user_id FROM ceremonies WHERE purpose='passkey'").Scan(&anonymous); err != nil || anonymous.Valid {
		t.Fatalf("passkey ceremony user_id is not null: %+v, error %v", anonymous, err)
	}
}
