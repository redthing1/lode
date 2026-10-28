//go:build linux

package instance

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir() + "/instance.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func insertTestProject(t *testing.T, s *Store, ownerID, projectID string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO projects(id,owner_id,slug,created) VALUES(?,?,?,?)`, projectID, ownerID, projectID, time.Now().Unix()); err != nil {
		t.Fatalf("insert test project %q: %v", projectID, err)
	}
}

func TestMalformedCredentialsFailWithoutPanicking(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct{ username, password string }{
		{"!", "short"}, {"", ""}, {strings.Repeat("x", 33), "short"},
		{"valid-user", strings.Repeat("p", 1025)},
	} {
		if _, err := s.AuthenticatePassword(context.Background(), tc.username, tc.password); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("malformed credentials returned %v", err)
		}
	}
}

func TestSetupIsOneTimeAndPasswordAuthentication(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	code, err := s.InitSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.InitSetup(ctx); !errors.Is(err, ErrSetupUnavailable) {
		t.Fatalf("second setup code: %v", err)
	}
	u, err := s.Setup(ctx, code, "owner", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Admin || u.ID == "" {
		t.Fatalf("unexpected bootstrap user: %+v", u)
	}
	if _, err = s.Setup(ctx, code, "other", "correct horse battery staple"); !errors.Is(err, ErrSetupUnavailable) {
		t.Fatalf("reused setup code: %v", err)
	}
	p, err := s.AuthenticatePassword(ctx, "OWNER", "correct horse battery staple")
	if err != nil || p.UserID != u.ID {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	if _, err = s.AuthenticatePassword(ctx, "owner", "wrong password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong password error: %v", err)
	}
	if _, err = s.AuthenticatePassword(ctx, "missing-user", "wrong password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown user error: %v", err)
	}
	var version, memory, iterations, parallelism, keyLen uint32
	if err = s.db.QueryRow(`SELECT password_version,password_memory,password_iterations,password_parallelism,password_key_len FROM users WHERE id=?`, u.ID).Scan(&version, &memory, &iterations, &parallelism, &keyLen); err != nil {
		t.Fatal(err)
	}
	if version != 1 || memory != argonMemory || iterations != argonIterations || parallelism != argonParallelism || keyLen != argonKeyLen {
		t.Fatalf("Argon2id settings: %d %d %d %d %d", version, memory, iterations, parallelism, keyLen)
	}
	oldParams := currentPasswordParams
	defer func() { currentPasswordParams = oldParams }()
	currentPasswordParams.memory = 32 * 1024
	if _, err = s.CreateUser(ctx, "next-user", "a sufficiently long password", false); err != nil {
		t.Fatal(err)
	}
	currentPasswordParams = oldParams
	if _, err = s.AuthenticatePassword(ctx, "owner", "correct horse battery staple"); err != nil {
		t.Fatalf("parameter change invalidated existing hash: %v", err)
	}
	if _, err = s.db.Exec(`UPDATE users SET password_memory=0 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticatePassword(ctx, "owner", "correct horse battery staple"); err == nil {
		t.Fatal("accepted unsupported stored password parameters")
	}
	_, err = s.db.Exec(`UPDATE users SET password_memory=? WHERE id=?`, argonMemory, u.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoreEscapesPathAndUsesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity ? #.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database mode %o", info.Mode().Perm())
	}
	s.db.SetMaxOpenConns(2)
	connections := make([]*sql.Conn, 0, 2)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		conn, e := s.db.Conn(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		connections = append(connections, conn)
		var foreignKeys, syncLevel, busy int
		if e = conn.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); e != nil {
			t.Fatal(e)
		}
		if e = conn.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&syncLevel); e != nil {
			t.Fatal(e)
		}
		if e = conn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busy); e != nil {
			t.Fatal(e)
		}
		if foreignKeys != 1 || syncLevel != 3 || busy < 5000 {
			t.Fatalf("connection %d pragmas foreign_keys=%d synchronous=%d busy_timeout=%d", i, foreignKeys, syncLevel, busy)
		}
	}
}

func TestOpenStoreRejectsUnsupportedSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("PRAGMA user_version=99"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenStore(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported instance database version 99") {
		t.Fatalf("OpenStore error = %v, want unsupported version error", err)
	}
}

func TestNoopUserStateChangeKeepsAuthVersion(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, err := s.CreateUser(ctx, "member", "a sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.SetUserState(ctx, user.ID, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if after.AuthVersion != before.AuthVersion {
		t.Fatalf("no-op account state changed auth version: %d -> %d", before.AuthVersion, after.AuthVersion)
	}
}
func TestConcurrentSetupHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	code, err := s.InitSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"admin-one", "admin-two"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_, e := s.Setup(ctx, code, n, "a sufficiently long password")
			errs <- e
		}(name)
	}
	wg.Wait()
	close(errs)
	success, failed := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, ErrSetupUnavailable) {
			failed++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || failed != 1 {
		t.Fatalf("setup outcomes success=%d unavailable=%d", success, failed)
	}
}

func TestMembershipTokenIntersectionAndRevocation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin, err := s.CreateUser(ctx, "admin", "a sufficiently long password", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetPassword(ctx, admin.ID, "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	insertTestProject(t, s, admin.ID, "project-a")
	user, err := s.CreateUser(ctx, "member", "another sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutMembership(ctx, admin.ID, "project-a", RoleReader); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner membership mutation = %v, want conflict", err)
	}
	if err = s.RemoveMembership(ctx, admin.ID, "project-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner membership removal = %v, want conflict", err)
	}
	if role, err := s.RoleFor(ctx, admin.ID, "project-a"); err != nil || role != RoleMaintainer {
		t.Fatalf("implicit owner role = %q, %v", role, err)
	}
	if _, err = s.SetPassword(ctx, user.ID, "another sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	if err = s.PutMembership(ctx, user.ID, "project-a", RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	secret, token, err := s.CreateToken(ctx, user.ID, "automation", RoleWriter, "project-a", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.AuthenticateToken(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Authorize(ctx, p, "project-a", RoleWriter); err != nil {
		t.Fatal(err)
	}
	if err = s.Authorize(ctx, p, "project-a", RoleMaintainer); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("token exceeded its cap: %v", err)
	}
	if err = s.Authorize(ctx, p, "project-b", RoleReader); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("token escaped project scope: %v", err)
	}
	if err = s.PutMembership(ctx, user.ID, "project-a", RoleReader); err != nil {
		t.Fatal(err)
	}
	if err = s.Authorize(ctx, p, "project-a", RoleWriter); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("stale token membership retained: %v", err)
	}
	if _, err = s.SetUserState(ctx, user.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled token owner authenticated: %v", err)
	}
	if _, err = s.SetUserState(ctx, user.ID, false, false); err != nil {
		t.Fatal(err)
	}
	p, err = s.AuthenticateToken(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeToken(ctx, user.ID, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token authenticated: %v", err)
	}
	if err = s.Authorize(ctx, p, "project-a", RoleReader); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token principal remained authorized: %v", err)
	}
	_ = admin
}

func TestOwnerRoleAndTokenCeilingRespectProjectLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner", "temporary owner password", false)
	if err != nil {
		t.Fatal(err)
	}
	owner, err = s.SetPassword(ctx, owner.ID, "a permanent owner password")
	if err != nil {
		t.Fatal(err)
	}
	insertTestProject(t, s, owner.ID, "owned-project")
	if role, err := s.RoleFor(ctx, owner.ID, "owned-project"); err != nil || role != RoleMaintainer {
		t.Fatalf("implicit owner role = %q, %v", role, err)
	}
	if _, err := s.RoleFor(ctx, owner.ID, "missing-project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project role = %v, want not found", err)
	}

	secret, _, err := s.CreateToken(ctx, owner.ID, "limited", RoleWriter, "owned-project", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := s.AuthenticateToken(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(ctx, principal, "owned-project", RoleWriter); err != nil {
		t.Fatalf("owner token below owner authority was denied: %v", err)
	}
	if err := s.Authorize(ctx, principal, "owned-project", RoleMaintainer); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("owner token exceeded its role ceiling: %v", err)
	}

	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET deleting=1 WHERE id=?", "owned-project"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoleFor(ctx, owner.ID, "owned-project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting project role = %v, want not found", err)
	}
	if err := s.Authorize(ctx, principal, "owned-project", RoleReader); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deleting project remained authorized: %v", err)
	}
}

func TestTokenExpiryAndLastAdministratorProtection(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin, err := s.CreateUser(ctx, "admin", "a sufficiently long password", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetPassword(ctx, admin.ID, "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetUserState(ctx, admin.ID, true, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled final admin: %v", err)
	}
	if _, err = s.SetUserState(ctx, admin.ID, false, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("demoted final admin: %v", err)
	}
	secret, tok, err := s.CreateToken(ctx, admin.ID, "short", RoleReader, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.AuthenticateToken(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Admin {
		t.Fatal("personal token inherited instance-admin authority")
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE personal_tokens SET expires_at=? WHERE id=?`, time.Now().Add(-time.Minute).Unix(), tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token authenticated: %v", err)
	}
}

func TestForcePasswordChangeAndAuthVersionRecheck(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner", "temporary owner password", true)
	if err != nil {
		t.Fatal(err)
	}
	owner, err = s.SetPassword(ctx, owner.ID, "a permanent owner password")
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "member", "temporary password", false)
	if err != nil {
		t.Fatal(err)
	}
	if !u.ForcePasswordChange {
		t.Fatal("new account does not require a password change")
	}
	insertTestProject(t, s, owner.ID, "project-a")
	if err = s.PutMembership(ctx, u.ID, "project-a", RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	first, err := s.AuthenticatePassword(ctx, "member", "temporary password")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateToken(ctx, u.ID, "automation", RoleWriter, "", time.Time{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("force-change account created token: %v", err)
	}
	if err = s.Authorize(ctx, first, "project-a", RoleReader); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("force-change account accessed project: %v", err)
	}
	changed, err := s.SetPassword(ctx, u.ID, "permanent password is long")
	if err != nil {
		t.Fatal(err)
	}
	if changed.ForcePasswordChange || changed.AuthVersion != first.AuthVersion+1 {
		t.Fatalf("password update state: %+v", changed)
	}
	if err = s.Authorize(ctx, first, "project-a", RoleReader); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("stale principal remained authorized: %v", err)
	}
	current, err := s.AuthenticatePassword(ctx, "member", "permanent password is long")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Authorize(ctx, current, "project-a", RoleReader); err != nil {
		t.Fatalf("fresh principal denied: %v", err)
	}
	secret, _, err := s.CreateToken(ctx, u.ID, "automation", RoleWriter, "project-a", time.Time{})
	if err != nil {
		t.Fatalf("post-change token creation: %v", err)
	}
	if _, err = s.db.Exec(`UPDATE users SET force_password_change=1 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("force-change account authenticated token: %v", err)
	}
	if _, err = s.SetPassword(ctx, u.ID, "another permanent password"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateToken(ctx, secret); err != nil {
		t.Fatalf("token failed after password change completed: %v", err)
	}
}
