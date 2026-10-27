package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
)

func TestLocalCollectionOwnershipSurvivesMoveAndRemovesRuntimeLock(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "store")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	local := openInitializedLocal(t, directory)
	id := local.Service.ID()
	if _, err := local.Service.Collect(ctx, CollectRequest{}); err != nil {
		t.Fatalf("first local collection: %v", err)
	}
	params, err := maintenance.GetParams(ctx, local.repository)
	if err != nil || params.Owner != "lode@local" || params.QuickCycle.Enabled || params.FullCycle.Enabled {
		t.Fatalf("native coordinator identity/configuration: %+v, %v", params, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	locks, err := filepath.Glob(filepath.Join(directory, ".connect-runtime-*.mlock"))
	if err != nil || len(locks) != 0 {
		t.Fatalf("owned runtime maintenance locks after close: %v, %v", locks, err)
	}
	moved := filepath.Join(filepath.Dir(directory), "moved")
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	reopened := openInitializedLocal(t, moved)
	if reopened.Service.ID() != id {
		t.Fatal("moving a store changed its project identity")
	}
	if _, err := reopened.Service.Collect(ctx, CollectRequest{}); err != nil {
		t.Fatalf("collection after store move: %v", err)
	}
}

func TestLocalCollectionDoesNotReplaceAnExistingNativeMaintenanceOwner(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	if err := repo.DirectWriteSession(ctx, local.repository, repo.WriteSessionOptions{Purpose: "test foreign owner"}, func(ctx context.Context, writer repo.DirectRepositoryWriter) error {
		params := maintenance.DefaultParams()
		params.Owner = "other@owner"
		return maintenance.SetParams(ctx, writer, &params)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := local.Service.Collect(ctx, CollectRequest{})
	var ownerError maintenance.NotOwnedError
	if !errors.As(err, &ownerError) || ownerError.Owner != "other@owner" {
		t.Fatalf("foreign-owner collection: %v", err)
	}
	params, err := maintenance.GetParams(ctx, local.repository)
	if err != nil || params.Owner != "other@owner" {
		t.Fatalf("foreign owner changed: %+v, %v", params, err)
	}
}

func TestCollectionSkippedNativeLockPreservesRetryCheckpoint(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	// This represents a completed source whose native blob disappeared before
	// the service acknowledged the previous cycle. A skipped run cannot infer
	// that its own rewrite/schedule reconciliation was executed.
	source := "p-already-collected-source"
	if err := local.metadata.beginCompaction(ctx, source, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	nativeLock := flock.New(local.repository.ConfigFilename() + ".mlock")
	locked, err := nativeLock.TryLock()
	if err != nil || !locked {
		t.Fatalf("hold native maintenance lock: %t, %v", locked, err)
	}
	defer func() { _ = nativeLock.Close() }()
	report, err := local.Service.Collect(ctx, CollectRequest{})
	if err != nil || report.Executed {
		t.Fatalf("lock-contended collection: %+v, %v", report, err)
	}
	if _, exists, err := local.metadata.compaction(ctx); err != nil || !exists {
		t.Fatalf("skipped native cycle cleared retry state: exists=%t, %v", exists, err)
	}
	if err := nativeLock.Close(); err != nil {
		t.Fatal(err)
	}
	if report, err = local.Service.Collect(ctx, CollectRequest{}); err != nil || !report.Executed {
		t.Fatalf("retry after releasing native lock: %+v, %v", report, err)
	}
	if _, exists, err := local.metadata.compaction(ctx); err != nil || exists {
		t.Fatalf("executed native cycle failed to clear completed source: exists=%t, %v", exists, err)
	}
}
