package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
)

func TestOpenLocalClaimsOwnerBeforeOpeningStorage(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "project")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	owner, err := acquireProjectOwner(filepath.Join(directory, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// Opening even the credentials must wait until ownership is available.
	// A busy store should not reach any native/SQLite initialization work.
	if err := os.WriteFile(filepath.Join(directory, "token"), []byte("invalid credentials"), 0600); err != nil {
		t.Fatal(err)
	}
	if local, err := OpenLocal(ctx, directory); !errors.Is(err, ErrProjectBusy) {
		if local != nil {
			_ = local.Close()
		}
		t.Fatalf("OpenLocal before owner release=%v, want ErrProjectBusy", err)
	}
	if matches, err := filepath.Glob(filepath.Join(directory, ".connect-runtime-*")); err != nil || len(matches) != 0 {
		t.Fatalf("busy open touched native runtime files=%v err=%v", matches, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if local, err := OpenLocal(ctx, directory); err == nil || errors.Is(err, ErrProjectBusy) {
		if local != nil {
			_ = local.Close()
		}
		t.Fatalf("OpenLocal after owner release=%v, want credentials error", err)
	}
	probe, err := acquireProjectOwner(filepath.Join(directory, "metadata.sqlite"))
	if err != nil {
		t.Fatalf("failed open leaked ownership: %v", err)
	}
	_ = probe.Close()
}

type blockingCloseRepository struct {
	repo.DirectRepository
	entered chan struct{}
	resume  chan struct{}
}

func (r *blockingCloseRepository) Close(ctx context.Context) error {
	close(r.entered)
	<-r.resume
	return r.DirectRepository.Close(ctx)
}

func TestLocalOwnerCoversStorageClose(t *testing.T) {
	local := openTestLocal(t)
	directory := filepath.Dir(local.metadata.path)
	blocked := &blockingCloseRepository{DirectRepository: local.repository, entered: make(chan struct{}), resume: make(chan struct{})}
	local.repository = blocked
	var release sync.Once
	defer release.Do(func() { close(blocked.resume) })
	closed := make(chan error, 1)
	go func() { closed <- local.Close() }()
	select {
	case <-blocked.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("local close did not reach native close")
	}
	// The service is stopped and SQLite has closed, but the native repository
	// has not finished closing. Neither a backup nor a new Local may enter.
	if !local.Service.closed.Load() {
		t.Fatal("storage closed before stopping the service")
	}
	if owner, err := acquireProjectOwner(local.metadata.path); !errors.Is(err, ErrProjectBusy) {
		if owner != nil {
			_ = owner.Close()
		}
		t.Fatalf("owner during native close=%v", err)
	}
	if other, err := OpenLocal(context.Background(), directory); !errors.Is(err, ErrProjectBusy) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("open during native close=%v", err)
	}
	release.Do(func() { close(blocked.resume) })
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	owner, err := acquireProjectOwner(local.metadata.path)
	if err != nil {
		t.Fatalf("owner after native close=%v", err)
	}
	_ = owner.Close()
}

func TestInitializeAndOpenLocalDurablyReopensPrivateProject(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	parent := t.TempDir()
	directory := filepath.Join(parent, "project")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	assertMode(t, directory, 0700)
	assertMode(t, filepath.Join(directory, "state.json"), 0600)
	assertMode(t, filepath.Join(directory, "token"), 0600)
	assertMode(t, filepath.Join(directory, "metadata.sqlite"), 0600)
	tokenBytes, err := os.ReadFile(filepath.Join(directory, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(tokenBytes) != strings.TrimSpace(string(tokenBytes))+"\n" || len(strings.TrimSpace(string(tokenBytes))) != 64 {
		t.Fatalf("unexpected token file encoding")
	}
	for _, entry := range mustReadDir(t, parent) {
		if strings.HasPrefix(entry.Name(), ".lode-project-init-") {
			t.Fatalf("staging directory remained after initialization: %q", entry.Name())
		}
	}

	first, err := OpenLocal(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token != strings.TrimSuffix(string(tokenBytes), "\n") {
		t.Fatal("opened token differs from private token file")
	}
	projectID := first.Service.ID()
	if matches, err := filepath.Glob(filepath.Join(directory, ".connect-runtime-*.json")); err != nil || len(matches) != 0 {
		t.Fatalf("disposable Kopia config remained after open: %#v, %v", matches, err)
	}
	second, err := OpenLocal(ctx, directory)
	if !errors.Is(err, ErrProjectBusy) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second owner error = %v, want ErrProjectBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}

	// The temporary connection file was removed. A second open must regenerate
	// it from the canonical content path, not a staging-path config.
	reopened, err := OpenLocal(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Service.ID() != projectID {
		t.Fatalf("project ID changed across reopen: %q != %q", reopened.Service.ID(), projectID)
	}
	if reopened.Token != strings.TrimSuffix(string(tokenBytes), "\n") {
		t.Fatal("API token changed across reopen")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if matches, err := filepath.Glob(filepath.Join(directory, ".connect-runtime-*.json")); err != nil || len(matches) != 0 {
		t.Fatalf("disposable Kopia config remained after reopen: %#v, %v", matches, err)
	}
}

func TestInitializeLocalLeavesExistingDestinationUntouched(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(directory, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeLocal(ctx, directory); err == nil {
		t.Fatal("InitializeLocal accepted an existing destination")
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "keep me" {
		t.Fatalf("existing destination changed: %q, %v", body, err)
	}
}

func TestOpenLocalRejectsMissingOrNonPrivateCredentials(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "project")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "token"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocal(ctx, directory); err == nil {
		t.Fatal("OpenLocal accepted a group/world-readable token")
	}
	if err := os.Chmod(filepath.Join(directory, "token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocal(ctx, directory); err == nil {
		t.Fatal("OpenLocal accepted missing native credentials")
	}
}

func requireLinuxLocalProject(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("local project durability currently requires Linux")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %q = %04o, want %04o", path, got, want)
	}
}

func mustReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
