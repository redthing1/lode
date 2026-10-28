//go:build linux

package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func saveInstanceProject(t *testing.T, p *project.Project, name string, data []byte) string {
	t.Helper()
	changes := []model.Entry{instanceFileEntry(name, data)}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		changes = append(changes, model.Entry{Path: parent, Kind: model.Directory})
	}
	model.Sort(changes)
	request := project.Request{
		ID: project.NewID(), Branch: "main", Author: "backup test", Message: "save bytes",
		Changes: changes,
	}
	commit, err := p.Save(context.Background(), request, project.Source{Write: func(_ model.Entry, _ project.Basis, out io.Writer) error {
		_, err := io.Copy(out, bytes.NewReader(data))
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	return commit.ID
}

func instanceFileEntry(name string, data []byte) model.Entry {
	return model.Entry{Path: name, Kind: model.File, Size: int64(len(data)), SHA256: projectHash(data)}
}

func projectHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestBackupCreatesColdSelfContainedInstance(t *testing.T) {
	i, setupCode, source := newTestInstance(t)
	ctx := context.Background()
	user, err := i.Setup(ctx, setupCode, "owner", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	created, err := i.CreateProject(ctx, user.ID, "assets")
	if err != nil {
		t.Fatal(err)
	}
	p, release, err := i.Acquire(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 1, 2, 0xfe, 0xff}
	commitID := saveInstanceProject(t, p, "assets/project.bin", data)
	release()
	destination := filepath.Join(t.TempDir(), "backup")
	if err := Backup(ctx, source, destination); err == nil || !strings.Contains(err.Error(), "stop its server first") {
		t.Fatalf("backup of running instance = %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("running-instance backup destination exists: %v", err)
	}
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	sourceID := i.Config.ID
	i = nil
	if err := Backup(ctx, source, destination); err != nil {
		t.Fatalf("cold backup: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup contains a live instance lock: %v", err)
	}

	copy, err := Open(ctx, destination)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	if copy.Config.ID != sourceID || copy.Config.URL != "https://example.test" {
		t.Fatalf("backup config = %+v", copy.Config)
	}
	principal, err := copy.AuthenticatePassword(ctx, user.Username, "correct horse battery staple")
	if err != nil || principal.UserID != user.ID {
		t.Fatalf("backup account = %+v, %v", principal, err)
	}
	projects, err := copy.Projects(ctx, user.ID)
	if err != nil || len(projects) != 1 || projects[0].ID != created.ID || projects[0].Slug != created.Slug || projects[0].OwnerID != user.ID || projects[0].Owner != user.Username {
		t.Fatalf("backup projects = %#v, %v", projects, err)
	}
	versioned, releaseCopy, err := copy.Acquire(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	err = versioned.Read(ctx, commitID, "assets/project.bin", func(_ model.Entry, reader io.Reader) error {
		var err error
		got, err = io.ReadAll(reader)
		return err
	})
	releaseCopy()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored project bytes = %v, %v", got, err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	copy = nil

	// The original remains independently usable after backup publication.
	original, err := Open(ctx, source)
	if err != nil {
		t.Fatalf("reopen original: %v", err)
	}
	if original.Config.ID != sourceID {
		t.Fatalf("source instance identity changed: %s", original.Config.ID)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupPreservesCurrentSchemaAndCredentials(t *testing.T) {
	ctx := context.Background()
	i, setupCode, source := newTestInstance(t)
	user, err := i.Setup(ctx, setupCode, "owner", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	security, err := NewSecurity(i.Store, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	auth := newSyntheticAuthenticator(t, "backup-legacy-preserved-key")
	registerSynthetic(t, ctx, security, user, auth, "legacy-enrollment-session")
	keysBefore, err := security.Keys(ctx, user.ID)
	if err != nil || len(keysBefore) != 1 {
		t.Fatalf("legacy key count %d: %v", len(keysBefore), err)
	}
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(source, "instance.sqlite")
	sourceBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "current-backup")
	if err := Backup(ctx, source, destination); err != nil {
		t.Fatalf("backup current source: %v", err)
	}
	sourceAfter, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceAfter, sourceBefore) {
		t.Fatal("backup migrated or otherwise changed the source database")
	}

	copy, err := Open(ctx, destination)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	var version int
	if err := copy.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatalf("backup schema version = %d, err %v", version, err)
	}
	backupSecurity, err := NewSecurity(copy.Store, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	keysAfter, err := backupSecurity.Keys(ctx, user.ID)
	if err != nil || len(keysAfter) != 1 || !bytes.Equal(keysAfter[0].raw, keysBefore[0].raw) {
		t.Fatalf("backup key count %d, error %v", len(keysAfter), err)
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRejectsDestinationsAndCleansCancelledWork(t *testing.T) {
	i, _, source := newTestInstance(t)
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	i = nil
	if err := Backup(context.Background(), source, filepath.Join(source, "inside")); err == nil {
		t.Fatal("backup inside source succeeded")
	}
	destination := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "keep")
	if err := os.WriteFile(marker, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Backup(context.Background(), source, destination); err == nil {
		t.Fatal("backup replaced an existing destination")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "untouched" {
		t.Fatalf("existing destination marker = %q, %v", data, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelDestination := filepath.Join(t.TempDir(), "cancelled")
	if err := Backup(cancelled, source, cancelDestination); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backup error = %v", err)
	}
	if _, err := os.Lstat(cancelDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled destination exists: %v", err)
	}
}

func TestBackupRefusesAnIndependentlyOpenProject(t *testing.T) {
	i, setupCode, source := newTestInstance(t)
	owner := setupTestOwner(t, i, setupCode, "owner")
	created, err := i.CreateProject(context.Background(), owner.ID, "locked")
	if err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(source, "projects", created.ID)
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	i = nil
	locked, err := project.Open(context.Background(), projectPath)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "backup")
	if err := Backup(context.Background(), source, destination); err == nil || !strings.Contains(err.Error(), "backup registered project") {
		t.Fatalf("backup with independently open project = %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed backup published a destination: %v", err)
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := filepath.Glob(filepath.Join(filepath.Dir(destination), ".backup.partial-*")); len(entries) != 0 {
		t.Fatalf("failed backup staging data remains: %v", entries)
	}
}

func TestImportProjectPreservesIDAndContent(t *testing.T) {
	ctx := context.Background()
	sourceDir := filepath.Join(t.TempDir(), "legacy-project")
	source, err := project.Create(ctx, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("opaque legacy project bytes\x00\xff")
	commitID := saveInstanceProject(t, source, "nested/file.bin", data)
	projectID := source.ID()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	i, setupCode, _ := newTestInstance(t)
	owner := setupTestOwner(t, i, setupCode, "owner")
	imported, err := i.ImportProject(ctx, owner.ID, "legacy", sourceDir)
	if err != nil {
		t.Fatalf("import project: %v", err)
	}
	if imported.ID != projectID || imported.Slug != "legacy" || imported.OwnerID != owner.ID || imported.Owner != owner.Username {
		t.Fatalf("imported metadata = %+v; source ID %s", imported, projectID)
	}
	if _, err := i.ImportProject(ctx, owner.ID, "legacy-again", sourceDir); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate ID import = %v", err)
	}
	p, release, err := i.Acquire(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	err = p.Read(ctx, commitID, "nested/file.bin", func(_ model.Entry, reader io.Reader) error {
		var err error
		got, err = io.ReadAll(reader)
		return err
	})
	release()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("imported file bytes = %v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, "project.json")); err != nil {
		t.Fatalf("import modified legacy source: %v", err)
	}
}
