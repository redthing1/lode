package kopia

import (
	"context"
	"errors"
	"fmt"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"

	"github.com/redthing1/lode/internal/model"
)

const operationLabel = "lode-operation"

// Protect creates a recognized snapshot root. Repeated serialized calls for the
// same operation and tree return the existing handle. The caller must flush.
func Protect(ctx context.Context, w repo.RepositoryWriter, root Entry, operation string) (manifest.ID, error) {
	if operation == "" {
		return "", errors.New("root operation identity is required")
	}
	matches, err := w.FindManifests(ctx, map[string]string{"type": snapshot.ManifestType, operationLabel: operation})
	if err != nil {
		return "", err
	}
	if len(matches) > 1 {
		return "", errors.New("multiple roots for one operation require reconciliation")
	}
	if len(matches) == 1 {
		existing, err := LoadRoot(ctx, w, matches[0].ID)
		if err != nil {
			return "", err
		}
		if !sameStoredEntry(existing, root) {
			return "", fmt.Errorf("%w: operation already protects a different tree", model.ErrConflict)
		}
		if err := verifyTree(ctx, w, existing); err != nil {
			return "", err
		}
		return matches[0].ID, nil
	}
	if root.State().Kind != model.Directory {
		return "", errors.New("snapshot root must be a directory")
	}
	if err := verifyTree(ctx, w, root); err != nil {
		return "", err
	}
	now := fs.UTCTimestampFromTime(w.Time())
	return snapshot.SaveSnapshot(ctx, w, &snapshot.Manifest{
		Source:    snapshot.SourceInfo{Host: "lode", UserName: "lode", Path: "/projects"},
		StartTime: now, EndTime: now, RootEntry: root.data.Clone(),
		Tags: map[string]string{operationLabel: operation},
	})
}

func LoadRoot(ctx context.Context, r repo.Repository, id manifest.ID) (Entry, error) {
	root, err := snapshot.LoadSnapshot(ctx, r, id)
	if err != nil {
		return Entry{}, err
	}
	if root.IncompleteReason != "" || root.RootEntry == nil || root.RootEntry.Type != snapshot.EntryTypeDirectory || root.RootEntry.ObjectID.String() == "" {
		return Entry{}, errors.New("invalid or incomplete snapshot root")
	}
	return Entry{data: root.RootEntry.Clone()}, nil
}

// Release removes only a Lode-owned native root, and tolerates an absent root.
// Application retention intent and release journaling belong to the service.
func Release(ctx context.Context, w repo.RepositoryWriter, id manifest.ID) error {
	root, err := snapshot.LoadSnapshot(ctx, w, id)
	if errors.Is(err, snapshot.ErrSnapshotNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if root.Tags[operationLabel] == "" {
		return errors.New("refusing to release an unmanaged root")
	}
	return w.DeleteManifest(ctx, id)
}

// Collect runs native maintenance with full safety. The caller must exclusively
// admit engine activity. Every native root is checked before destructive work.
func Collect(ctx context.Context, r repo.DirectRepository) error {
	return repo.DirectWriteSession(ctx, r, repo.WriteSessionOptions{Purpose: "lode collection"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		roots, err := w.FindManifests(ctx, map[string]string{"type": snapshot.ManifestType})
		if err != nil {
			return err
		}
		for _, metadata := range roots {
			root, err := LoadRoot(ctx, w, metadata.ID)
			if err != nil {
				return fmt.Errorf("collection preflight %s: %w", metadata.ID, err)
			}
			if err := verifyTree(ctx, w, root); err != nil {
				return fmt.Errorf("collection preflight %s: %w", metadata.ID, err)
			}
		}
		return snapshotmaintenance.Run(ctx, w, maintenance.ModeFull, false, maintenance.SafetyFull)
	})
}

func verifyTree(ctx context.Context, r repo.Repository, entry Entry) error {
	if entry.State().Kind == model.File {
		_, err := r.VerifyObject(ctx, entry.data.ObjectID)
		return err
	}
	children, err := readDirectory(ctx, r, entry)
	if err != nil {
		return err
	}
	for _, name := range sortedNames(children) {
		if err := verifyTree(ctx, r, children[name]); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
