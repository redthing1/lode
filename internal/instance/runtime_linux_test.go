//go:build linux

package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestInstance(t *testing.T) (*Instance, string, string) {
	t.Helper()
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "instance")
	i, setupCode, err := Create(ctx, directory, "https://example.test/")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if i != nil {
			if err := i.Close(); err != nil {
				t.Errorf("close instance: %v", err)
			}
		}
	})
	return i, setupCode, directory
}

func TestCanonicalURLNormalizesOriginsAndRejectsUnsafeForms(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://EXAMPLE.com/", "https://example.com"},
		{"HTTPS://example.com:443", "https://example.com"},
		{"http://LOCALHOST:80/", "http://localhost"},
		{"http://127.0.0.1:8473/", "http://127.0.0.1:8473"},
		{"http://[::1]:80/", "http://[::1]"},
		{"https://example.com.:8443", "https://example.com:8443"},
	} {
		got, err := CanonicalURL(test.input)
		if err != nil || got != test.want {
			t.Errorf("CanonicalURL(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{
		"http://example.com", "http://localhost.evil", "file:///tmp/site", "https://user@example.com",
		"https://example.com/path", "https://example.com/?query", "https://example.com/#fragment",
		"https://example.com:", "https://example.com:70000", "https://bad_host.example",
	} {
		if got, err := CanonicalURL(input); err == nil {
			t.Errorf("CanonicalURL(%q) = %q, want rejection", input, got)
		}
	}
}

func TestCreatePublishesInitializedInstanceAndOpenNeverRecreatesMissingDB(t *testing.T) {
	i, setupCode, directory := newTestInstance(t)
	if setupCode == "" || i.Config.URL != "https://example.test" {
		t.Fatalf("created instance config/setup = %+v %q", i.Config, setupCode)
	}
	if required, err := i.SetupRequired(context.Background()); err != nil || !required {
		t.Fatalf("SetupRequired = %v, %v", required, err)
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("instance directory mode = %v, %v", info, err)
	}
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	i = nil
	if err := os.Remove(filepath.Join(directory, "instance.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), directory); err == nil || !strings.Contains(err.Error(), "refusing to create a replacement") {
		t.Fatalf("Open missing database error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "instance.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open recreated missing database: %v", err)
	}
}

func TestOpenOwnershipLockAndCloseWaitForProjectLeases(t *testing.T) {
	i, _, directory := newTestInstance(t)
	if _, err := Open(context.Background(), directory); err == nil || !strings.Contains(err.Error(), "instance is in use") {
		t.Fatalf("second Open while active = %v", err)
	}
	info, err := i.CreateProject(context.Background(), "leased")
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := i.Acquire(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Close(); err == nil || !strings.Contains(err.Error(), "active project leases") {
		t.Fatalf("Close with active lease = %v", err)
	}
	release()
	release()
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := i.Acquire(context.Background(), info.ID); err == nil {
		t.Fatal("Acquire succeeded on closed instance")
	}
}

func TestNativeProjectCacheIsBoundedAndOnlyEvictsIdleProjects(t *testing.T) {
	i, _, _ := newTestInstance(t)
	ctx := context.Background()
	var projects []Project
	for index := 0; index < 9; index++ {
		created, err := i.CreateProject(ctx, "project-"+string(rune('a'+index)))
		if err != nil {
			t.Fatal(err)
		}
		projects = append(projects, created)
	}
	var releases []func()
	for index := 0; index < 8; index++ {
		_, release, err := i.Acquire(ctx, projects[index].ID)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, _, err := i.Acquire(ctx, projects[8].ID); err == nil || !strings.Contains(err.Error(), "all project slots are busy") {
		t.Fatalf("ninth active project acquisition = %v", err)
	}
	if len(i.open) != 8 {
		t.Fatalf("open project cache has %d entries, want 8", len(i.open))
	}
	releases[0]()
	if _, release, err := i.Acquire(ctx, projects[8].ID); err != nil {
		t.Fatalf("acquire after an idle slot was released: %v", err)
	} else {
		release()
	}
	for _, release := range releases[1:] {
		release()
	}
	if len(i.open) != 8 {
		t.Fatalf("open project cache grew to %d entries", len(i.open))
	}
}

func TestCreateProjectRegistrationFailurePreservesOrphanAndBackupRefusesIt(t *testing.T) {
	i, _, directory := newTestInstance(t)
	if _, err := i.db.Exec(`CREATE TRIGGER fail_project_registration BEFORE INSERT ON projects BEGIN SELECT RAISE(FAIL,'injected registration failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := i.CreateProject(context.Background(), "orphan")
	if err == nil || !strings.Contains(err.Error(), "preserved at") || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("CreateProject failure = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(directory, "projects"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("preserved project data entries = %#v, %v", entries, err)
	}
	orphanPath := filepath.Join(directory, "projects", entries[0].Name())
	if _, err := os.Stat(filepath.Join(orphanPath, "project.json")); err != nil {
		t.Fatalf("orphan project config missing: %v", err)
	}
	if _, err := i.db.ExecContext(context.Background(), "DROP TRIGGER fail_project_registration"); err != nil {
		t.Fatal(err)
	}
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	i = nil
	backup := filepath.Join(t.TempDir(), "backup")
	if err := Backup(context.Background(), directory, backup); err == nil || !strings.Contains(err.Error(), "unregistered project data") {
		t.Fatalf("backup with orphan error = %v", err)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("backup removed orphan project data: %v", err)
	}
}

func TestResetAccountInvalidatesCredentialsWithoutCreatingSecurityTables(t *testing.T) {
	i, setupCode, _ := newTestInstance(t)
	ctx := context.Background()
	user, err := i.Setup(ctx, setupCode, "owner", "old secure password")
	if err != nil {
		t.Fatal(err)
	}
	reset, err := i.ResetAccount(ctx, user.Username, "new secure password")
	if err != nil {
		t.Fatal(err)
	}
	if !reset.Admin || reset.Disabled || !reset.ForcePasswordChange || reset.AuthVersion != user.AuthVersion+1 {
		t.Fatalf("reset account state = %+v", reset)
	}
	if _, err := i.AuthenticatePassword(ctx, user.Username, "old secure password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old password after reset = %v", err)
	}
	principal, err := i.AuthenticatePassword(ctx, user.Username, "new secure password")
	if err != nil || !principal.ForcePasswordChange || principal.AuthVersion != reset.AuthVersion {
		t.Fatalf("new password principal = %+v, %v", principal, err)
	}
}

func TestResetAccountRevokesTokensAndDeletesExistingSecurityCredentials(t *testing.T) {
	i, setupCode, _ := newTestInstance(t)
	ctx := context.Background()
	user, err := i.Setup(ctx, setupCode, "owner", "old secure password")
	if err != nil {
		t.Fatal(err)
	}
	secret, token, err := i.CreateToken(ctx, user.ID, "recovery", RoleReader, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	security, err := NewSecurity(i.Store, i.Config.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO security_keys VALUES('key',?,'test',x'01',1)",
		"INSERT INTO recovery_codes VALUES(?,x'02')",
		"INSERT INTO ceremonies VALUES('ceremony',?,'binding','login',x'03',9999999999,1)",
	} {
		if _, err := security.store.db.ExecContext(ctx, statement, user.ID); err != nil {
			t.Fatal(err)
		}
	}
	reset, err := i.ResetAccount(ctx, user.Username, "new secure password")
	if err != nil {
		t.Fatal(err)
	}
	if !reset.Admin || reset.Disabled || !reset.ForcePasswordChange {
		t.Fatalf("administrator changed during reset: %+v", reset)
	}
	if _, err := i.AuthenticateToken(ctx, secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("token after reset = %v", err)
	}
	var revoked int
	if err := i.db.QueryRowContext(ctx, "SELECT revoked FROM personal_tokens WHERE id=?", token.ID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("token revocation = %d, %v", revoked, err)
	}
	for _, table := range []string{"security_keys", "recovery_codes", "ceremonies"} {
		var count int
		if err := i.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE user_id=?", user.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after reset = %d, %v", table, count, err)
		}
	}
}
