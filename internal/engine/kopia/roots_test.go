package kopia_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
)

const rootsTestKeyLabel = "lode-protection-key"

func TestNativeRootsProtectRetryCheckAndRelease(t *testing.T) {
	f := newArchiveFixture(t)
	baseBytes := archivePayload(128<<10, 101)
	otherBytes := archivePayload(128<<10, 102)
	baseRoot, baseArchive := makeRootArchive(t, f, "base.bin", baseBytes)
	_, otherArchive := makeRootArchive(t, f, "other.bin", otherBytes)

	roots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := roots.Namespace(), hex.EncodeToString(f.rep.UniqueID()); got != want {
		t.Fatalf("repository namespace = %q, want hex unique ID %q", got, want)
	}
	if err := roots.Check(f.ctx, "operation-1", baseArchive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("Check before Protect = %v, want root not found", err)
	}
	if err := roots.Protect(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatal(err)
	}
	if err := roots.Protect(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatalf("idempotent Protect retry: %v", err)
	}
	if err := roots.Check(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatalf("Check after Protect: %v", err)
	}
	if err := roots.Check(f.ctx, "wrong-operation", baseArchive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("Check with wrong key = %v, want root not found", err)
	}
	if err := roots.Release(f.ctx, "wrong-operation"); err != nil {
		t.Fatalf("Release with wrong key: %v", err)
	}
	if err := roots.Check(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatalf("wrong-key release removed another root: %v", err)
	}

	// A fresh repository handle models recovery after a caller lost the reply
	// from Protect. The stable key finds the already flushed native root.
	reopened := f.reader("roots-reopened").(repo.DirectRepository)
	defer reopened.Close(context.WithoutCancel(f.ctx))
	reopenedRoots, err := lodekopia.NewRoots(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopenedRoots.Protect(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatalf("Protect after reopening repository: %v", err)
	}
	if err := reopenedRoots.Check(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatalf("Check after reopening repository: %v", err)
	}
	assertRootCount(t, f, "operation-1", 1)
	assertProtectedFile(t, f.ctx, reopened, baseRoot, "base.bin", baseBytes)

	if err := reopenedRoots.Check(f.ctx, "operation-1", otherArchive); !errors.Is(err, lodekopia.ErrRootConflict) {
		t.Fatalf("Check with another native tree = %v, want root conflict", err)
	}
	if err := reopenedRoots.Protect(f.ctx, "operation-1", otherArchive); !errors.Is(err, lodekopia.ErrRootConflict) {
		t.Fatalf("Protect with another native tree = %v, want root conflict", err)
	}
	if err := reopenedRoots.Check(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatalf("mismatched tree changed the original protection: %v", err)
	}

	// Exact duplicate native manifests are accepted by Check and removed
	// together by Release, making cleanup safe after an ambiguous retry.
	createExactDuplicateRoot(t, f, "operation-1", baseRoot)
	if err := reopenedRoots.Check(f.ctx, "operation-1", baseArchive); err != nil {
		t.Fatalf("Check with exact duplicate roots: %v", err)
	}
	assertRootCount(t, f, "operation-1", 2)
	if err := reopenedRoots.Release(f.ctx, "operation-1"); err != nil {
		t.Fatalf("Release roots: %v", err)
	}
	if err := reopenedRoots.Release(f.ctx, "operation-1"); err != nil {
		t.Fatalf("idempotent Release retry: %v", err)
	}
	assertRootCount(t, f, "operation-1", 0)
	if err := reopenedRoots.Check(f.ctx, "operation-1", baseArchive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("Check after Release = %v, want root not found", err)
	}
	assertProtectedFile(t, f.ctx, reopened, baseRoot, "base.bin", baseBytes)
}

func makeRootArchive(t *testing.T, f *archiveFixture, name string, payload []byte) (*snapshot.DirEntry, lodekopia.Archive) {
	t.Helper()
	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		file, err := captureProofFile(ctx, w, name, payload)
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, w, "", file)
		return err
	})
	archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatal(err)
	}
	return root, archive
}

func createExactDuplicateRoot(t *testing.T, f *archiveFixture, key string, root *snapshot.DirEntry) {
	t.Helper()
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		now := fs.UTCTimestampFromTime(w.Time().Add(time.Second))
		_, err := snapshot.SaveSnapshot(ctx, w, &snapshot.Manifest{
			Source:    snapshot.SourceInfo{Host: "lode", UserName: "lode", Path: "/projects"},
			StartTime: now,
			EndTime:   now,
			RootEntry: root.Clone(),
			Tags:      map[string]string{rootsTestKeyLabel: key},
		})
		return err
	})
}

func assertRootCount(t *testing.T, f *archiveFixture, key string, want int) {
	t.Helper()
	if err := f.rep.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	manifests, err := f.rep.FindManifests(f.ctx, map[string]string{
		manifest.TypeLabelKey: snapshot.ManifestType,
		rootsTestKeyLabel:     key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != want {
		t.Fatalf("root count for %q = %d, want %d", key, len(manifests), want)
	}
}

func assertProtectedFile(t *testing.T, ctx context.Context, r repo.Repository, root *snapshot.DirEntry, path string, want []byte) {
	t.Helper()
	entry, err := fileEntryForObject(ctx, r, root, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readProofObject(ctx, r, entry.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("protected file %s differs from expected bytes", path)
	}
}

func TestNativeRootsConflictOnChangedRootAttributes(t *testing.T) {
	f := newArchiveFixture(t)
	payload := archivePayload(4096, 103)
	root, archive := makeRootArchive(t, f, "root.bin", payload)
	roots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := roots.Protect(f.ctx, "operation-attrs", archive); err != nil {
		t.Fatal(err)
	}
	var changed snapshot.DirEntry
	if err := json.Unmarshal(archive.Root, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Permissions ^= 0o111
	changedRoot, err := json.Marshal(&changed)
	if err != nil {
		t.Fatal(err)
	}
	changedArchive := archive
	changedArchive.Root = changedRoot
	if err := roots.Check(f.ctx, "operation-attrs", changedArchive); !errors.Is(err, lodekopia.ErrRootConflict) {
		t.Fatalf("Check with changed root attributes = %v, want root conflict", err)
	}
	if err := roots.Protect(f.ctx, "operation-attrs", changedArchive); !errors.Is(err, lodekopia.ErrRootConflict) {
		t.Fatalf("Protect with changed root attributes = %v, want root conflict", err)
	}
	assertProtectedFile(t, f.ctx, f.rep, root, "root.bin", payload)
}

func TestNativeRootsValidateRejectsForgedArchiveWithoutCreatingRoot(t *testing.T) {
	f := newArchiveFixture(t)
	payload := archivePayload(8192, 104)
	_, archive := makeRootArchive(t, f, "source.bin", payload)
	roots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := roots.Validate(f.ctx, archive); err != nil {
		t.Fatalf("Validate genuine archive: %v", err)
	}

	forged := archive
	forged.Directories = make(map[string][]byte, len(archive.Directories))
	for id, raw := range archive.Directories {
		forged.Directories[id] = append([]byte(nil), raw...)
	}
	var directory snapshot.DirManifest
	for id, raw := range forged.Directories {
		if err := json.Unmarshal(raw, &directory); err != nil {
			t.Fatal(err)
		}
		if len(directory.Entries) == 0 {
			t.Fatal("fixture root directory unexpectedly empty")
		}
		directory.Entries[0].Permissions ^= 0o111
		forged.Directories[id], err = json.Marshal(&directory)
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if err := roots.Validate(f.ctx, forged); !errors.Is(err, lodekopia.ErrRootConflict) {
		t.Fatalf("Validate forged native directory bytes = %v, want root conflict", err)
	}
	assertRootCount(t, f, "validation-operation", 0)
}

func TestNativeRootsValidateRejectsMissingPayloadBeforeProtection(t *testing.T) {
	f := newArchiveFixture(t)
	missingID, err := object.ParseID("g00000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		missing := &snapshot.DirEntry{Name: "missing.bin", Type: snapshot.EntryTypeFile, Permissions: 0o644, ObjectID: missingID}
		var err error
		root, err = writeProofDirectory(ctx, w, "", missing)
		return err
	})
	archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatalf("export directory metadata with missing file payload: %v", err)
	}
	roots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := roots.Validate(f.ctx, archive); !errors.Is(err, lodekopia.ErrRootContentUnavailable) {
		t.Fatalf("Validate archive with missing payload = %v, want unavailable content", err)
	}
	if err := roots.Protect(f.ctx, "missing-payload", archive); !errors.Is(err, lodekopia.ErrRootContentUnavailable) {
		t.Fatalf("Protect archive with missing payload = %v, want unavailable content", err)
	}
	assertRootCount(t, f, "missing-payload", 0)
}
