package kopia

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
)

const protectionKeyLabel = "lode-protection-key"

var (
	ErrRootNotFound           = errors.New("native protection root not found")
	ErrRootConflict           = errors.New("native protection root conflicts with requested tree")
	ErrInvalidRootKey         = errors.New("native protection key is empty")
	ErrRootContentUnavailable = errors.New("native protection tree references unavailable content")
)

// Roots manages Kopia-native snapshot roots for opaque project-operation keys.
// It owns no repository lifecycle or retention policy.
type Roots struct {
	repository repo.DirectRepository
	namespace  string
	mu         sync.Mutex
}

// NewRoots creates the native root manager for a configured direct repository.
func NewRoots(repository repo.DirectRepository) (*Roots, error) {
	if repository == nil {
		return nil, errors.New("nil direct repository")
	}
	uniqueID := repository.UniqueID()
	if len(uniqueID) == 0 {
		return nil, errors.New("Kopia repository has an empty unique ID")
	}
	return &Roots{repository: repository, namespace: hex.EncodeToString(uniqueID)}, nil
}

// Namespace returns the repository-scoped identity used for native object IDs.
func (r *Roots) Namespace() string {
	if r == nil {
		return ""
	}
	return r.namespace
}

// Validate confirms that archive metadata exactly describes this repository's
// native tree and that every referenced object's backing contents are present
// and not marked deleted. It creates or removes no native roots.
func (r *Roots) Validate(ctx context.Context, archive Archive) error {
	if r == nil || r.repository == nil {
		return errors.New("nil native root manager")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.repository.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh repository before validating archive: %w", err)
	}
	_, err := r.validateArchive(ctx, archive)
	return err
}

// Protect validates archive bytes and referenced content, then creates one
// Kopia snapshot root for key. Repeated calls with the same tree are idempotent.
func (r *Roots) Protect(ctx context.Context, key string, archive Archive) error {
	if err := r.validateKey(key); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.repository.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh repository before validating protection: %w", err)
	}
	root, err := r.validateArchive(ctx, archive)
	if err != nil {
		return err
	}
	err = repo.DirectWriteSession(ctx, r.repository, repo.WriteSessionOptions{Purpose: "protect Lode operation root"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		if err := w.Refresh(ctx); err != nil {
			return fmt.Errorf("refresh writer before protecting root: %w", err)
		}
		manifests, err := r.matchingSnapshots(ctx, w, key)
		if err != nil {
			return err
		}
		for _, existing := range manifests {
			if !sameRootDescriptor(existing.RootEntry, root) {
				return fmt.Errorf("%w: key %q already protects another root", ErrRootConflict, key)
			}
		}
		if len(manifests) > 0 {
			return nil
		}
		now := fs.UTCTimestampFromTime(w.Time())
		_, err = snapshot.SaveSnapshot(ctx, w, &snapshot.Manifest{
			Source:    snapshot.SourceInfo{Host: "lode", UserName: "lode", Path: "/projects"},
			StartTime: now,
			EndTime:   now,
			RootEntry: root,
			Tags:      map[string]string{protectionKeyLabel: key},
		})
		return err
	})
	if err != nil {
		return err
	}
	if err := r.repository.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh repository after protecting root: %w", err)
	}
	return nil
}

// Check confirms that at least one exact native root for key is visible and
// that the requested tree's directory and payload objects remain available.
func (r *Roots) Check(ctx context.Context, key string, archive Archive) error {
	if err := r.validateKey(key); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.repository.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh repository before validating root: %w", err)
	}
	root, err := r.validateArchive(ctx, archive)
	if err != nil {
		return err
	}
	manifests, err := r.matchingSnapshots(ctx, r.repository, key)
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		return fmt.Errorf("%w: key %q", ErrRootNotFound, key)
	}
	for _, existing := range manifests {
		if !sameRootDescriptor(existing.RootEntry, root) {
			return fmt.Errorf("%w: key %q protects another root", ErrRootConflict, key)
		}
	}
	return nil
}

// Release removes every native snapshot manifest tagged with key. Releasing an
// absent key is safe to retry.
func (r *Roots) Release(ctx context.Context, key string) error {
	if err := r.validateKey(key); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.repository.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh repository before releasing root: %w", err)
	}
	err := repo.DirectWriteSession(ctx, r.repository, repo.WriteSessionOptions{Purpose: "release Lode operation root"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		if err := w.Refresh(ctx); err != nil {
			return fmt.Errorf("refresh writer before releasing root: %w", err)
		}
		manifests, err := r.matchingSnapshots(ctx, w, key)
		if err != nil {
			return err
		}
		for _, existing := range manifests {
			if err := w.DeleteManifest(ctx, existing.ID); err != nil {
				return fmt.Errorf("delete native root %s: %w", existing.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := r.repository.Refresh(ctx); err != nil {
		return fmt.Errorf("refresh repository after releasing root: %w", err)
	}
	return nil
}

func (r *Roots) validateKey(key string) error {
	if r == nil || r.repository == nil {
		return errors.New("nil native root manager")
	}
	if key == "" {
		return ErrInvalidRootKey
	}
	return nil
}

func (r *Roots) validateArchive(ctx context.Context, archive Archive) (*snapshot.DirEntry, error) {
	tree, err := OpenArchive(r.namespace, archive)
	if err != nil {
		return nil, err
	}
	root := tree.RootDescriptor()
	actual, err := ExportTree(ctx, r.repository, root)
	if err != nil {
		return nil, fmt.Errorf("export native tree for root validation: %w", err)
	}
	if !sameArchiveBytes(actual, archive) {
		return nil, fmt.Errorf("%w: archive does not match native directory objects", ErrRootConflict)
	}
	if err := r.verifyArchiveContents(ctx, archive); err != nil {
		return nil, err
	}
	return root, nil
}

func sameArchiveBytes(a, b Archive) bool {
	if a.Format != b.Format || !bytes.Equal(a.Root, b.Root) || len(a.Directories) != len(b.Directories) {
		return false
	}
	for id, data := range a.Directories {
		if !bytes.Equal(data, b.Directories[id]) {
			return false
		}
	}
	return true
}

func (r *Roots) verifyArchiveContents(ctx context.Context, archive Archive) error {
	seen := map[object.ID]bool{}
	verify := func(id object.ID) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		contentIDs, err := r.repository.VerifyObject(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: verify native object %s: %w", ErrRootContentUnavailable, id, err)
		}
		for _, contentID := range contentIDs {
			info, err := r.repository.ContentInfo(ctx, contentID)
			if err != nil {
				return fmt.Errorf("%w: inspect content %s for object %s: %w", ErrRootContentUnavailable, contentID, id, err)
			}
			if info.Deleted {
				return fmt.Errorf("%w: content %s for object %s is marked deleted", ErrRootContentUnavailable, contentID, id)
			}
		}
		return nil
	}
	var root snapshot.DirEntry
	if err := json.Unmarshal(archive.Root, &root); err != nil {
		return fmt.Errorf("decode native root descriptor: %w", err)
	}
	if err := verify(root.ObjectID); err != nil {
		return err
	}
	for _, raw := range archive.Directories {
		dir, err := decodeDirectory(raw)
		if err != nil {
			return fmt.Errorf("decode native directory during content verification: %w", err)
		}
		for _, child := range dir.Entries {
			if err := verify(child.ObjectID); err != nil {
				return err
			}
		}
	}
	return nil
}

type protectedSnapshot struct {
	ID        manifest.ID
	RootEntry *snapshot.DirEntry
}

func (r *Roots) matchingSnapshots(ctx context.Context, reader repo.Repository, key string) ([]protectedSnapshot, error) {
	entries, err := reader.FindManifests(ctx, map[string]string{
		manifest.TypeLabelKey: snapshot.ManifestType,
		protectionKeyLabel:    key,
	})
	if err != nil {
		return nil, fmt.Errorf("find native roots for key %q: %w", key, err)
	}
	var result []protectedSnapshot
	for _, entry := range entries {
		root, err := snapshot.LoadSnapshot(ctx, reader, entry.ID)
		if err != nil {
			return nil, fmt.Errorf("load native root %s: %w", entry.ID, err)
		}
		if root.Tags[protectionKeyLabel] != key {
			continue
		}
		if root.IncompleteReason != "" || root.RootEntry == nil || root.RootEntry.Type != snapshot.EntryTypeDirectory {
			return nil, fmt.Errorf("native root %s is incomplete", entry.ID)
		}
		result = append(result, protectedSnapshot{ID: entry.ID, RootEntry: root.RootEntry})
	}
	return result, nil
}

func sameRootDescriptor(a, b *snapshot.DirEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	jsonA, errA := json.Marshal(a)
	jsonB, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(jsonA, jsonB)
}
