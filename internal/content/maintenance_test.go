package content

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	repoContent "github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
)

type collectionClockStorageConfig struct {
	Path string `json:"path"`
}

type collectionClockStorage struct {
	blob.Storage
	path string
}

var collectionClockNanos atomic.Int64

func init() {
	blob.AddSupportedStorage("lode-collection-clock-test", collectionClockStorageConfig{}, func(ctx context.Context, cfg *collectionClockStorageConfig, create bool) (blob.Storage, error) {
		st, err := newStorage(ctx, cfg.Path, create)
		if err != nil {
			return nil, err
		}
		return &collectionClockStorage{Storage: st, path: cfg.Path}, nil
	})
}

func (s *collectionClockStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: "lode-collection-clock-test", Config: &collectionClockStorageConfig{Path: s.path}}
}

func (s *collectionClockStorage) DisplayName() string { return "collection clock test storage" }

func (s *collectionClockStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	opts.SetModTime = time.Unix(0, collectionClockNanos.Load())
	return s.Storage.PutBlob(ctx, id, data, opts)
}

func TestCollectUsesNativeFullMaintenanceAndPreservesRetainedRoot(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir(), "native-collection-test-password", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)

	releasedBytes := bytes.Repeat([]byte("released-content-"), 4096)
	retainedBytes := bytes.Repeat([]byte("retained-content-"), 512)
	released, _, err := s.Capture(ctx, "released-version", []Entry{fileEntry("old.bin", releasedBytes)}, source(map[string][]byte{"old.bin": releasedBytes}))
	if err != nil {
		t.Fatal(err)
	}
	retained, _, err := s.Capture(ctx, "retained-version", []Entry{fileEntry("keep.bin", retainedBytes)}, source(map[string][]byte{"keep.bin": retainedBytes}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, released); err != nil {
		t.Fatal(err)
	}

	collection, err := s.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if collection.BeforeBytes <= 0 || collection.AfterBytes <= 0 {
		t.Fatalf("collection byte measurements = %+v", collection)
	}
	params, err := maintenance.GetParams(ctx, s.repository)
	if err != nil {
		t.Fatal(err)
	}
	if params.Owner != s.repository.ClientOptions().UsernameAtHost() {
		t.Fatalf("maintenance owner = %q, want %q", params.Owner, s.repository.ClientOptions().UsernameAtHost())
	}
	roots, err := s.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots["released-version"]) != 0 || len(roots["retained-version"]) != 1 || roots["retained-version"][0] != retained {
		t.Fatalf("roots after collect: %#v", roots)
	}
	tree, err := readSnapshotTree(ctx, s, retained)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tree["keep.bin"], retainedBytes) {
		t.Fatal("retained bytes changed during native maintenance")
	}
}

func TestCollectReclaimsReleasedContentUnderSafetyFull(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	password := "native-collection-reclamation-test-password"
	s, err := Open(ctx, dir, password, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			_ = s.Close(ctx)
		}
	}()

	const releasedSize = 8 << 20
	const retainedSize = 64 << 10
	var releasedRoots []string
	var releasedObjects []string
	retainedRoots := make([]string, 0, 6)
	retainedBytes := make([][]byte, 0, 6)
	for i := 0; i < 6; i++ {
		oldBytes := make([]byte, releasedSize)
		keepBytes := make([]byte, retainedSize)
		if _, err := rand.Read(oldBytes); err != nil {
			t.Fatal(err)
		}
		if _, err := rand.Read(keepBytes); err != nil {
			t.Fatal(err)
		}
		keep := fileEntry("retained.bin", keepBytes)
		old := fileEntry("released.bin", oldBytes)
		baseID, captured, err := s.Capture(ctx, fmt.Sprintf("released-%02d", i), []Entry{old, keep}, source(map[string][]byte{
			"released.bin": oldBytes,
			"retained.bin": keepBytes,
		}))
		if err != nil {
			t.Fatal(err)
		}
		retainedID, _, err := s.Capture(ctx, fmt.Sprintf("retained-%02d", i), []Entry{captured[1]}, nil)
		if err != nil {
			t.Fatal(err)
		}
		releasedRoots = append(releasedRoots, baseID)
		releasedObjects = append(releasedObjects, captured[0].Object)
		retainedRoots = append(retainedRoots, retainedID)
		retainedBytes = append(retainedBytes, keepBytes)
		if err := s.Release(ctx, baseID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s = nil

	// A repository-local clock makes native SafetyFull ages eligible without
	// adding a clock control to production Store or changing native schedules.
	collectionClockNanos.Store(time.Now().UnixNano())
	storagePath := filepath.Join(dir, "repository")
	baseStorage, err := newStorage(ctx, storagePath, false)
	if err != nil {
		t.Fatal(err)
	}
	clockStorage := &collectionClockStorage{Storage: baseStorage, path: storagePath}
	if err := repo.Connect(ctx, filepath.Join(dir, "kopia.config"), clockStorage, password, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Hostname: "local", Username: "lode", Description: "Lode content store"},
		CachingOptions: repoContent.CachingOptions{
			CacheDirectory:              filepath.Join(dir, "cache"),
			ContentCacheSizeBytes:       256 << 20,
			ContentCacheSizeLimitBytes:  512 << 20,
			MetadataCacheSizeBytes:      64 << 20,
			MetadataCacheSizeLimitBytes: 128 << 20,
		},
	}); err != nil {
		_ = baseStorage.Close(ctx)
		t.Fatal(err)
	}
	if err := baseStorage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Unix(0, collectionClockNanos.Load()) }
	r, err := repo.Open(ctx, filepath.Join(dir, "kopia.config"), password, &repo.Options{
		DisableRepositoryLog: true,
		TimeNowFunc:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	s = &Store{repository: r}

	first, err := s.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	collectionClockNanos.Add(int64(maintenance.SafetyFull.MinContentAgeSubjectToGC + maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
	last := first
	for cycle := 1; cycle < 16; cycle++ {
		if cycle > 1 {
			collectionClockNanos.Add(int64(maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
		}
		last, err = s.Collect(ctx)
		if err != nil {
			t.Fatalf("full collection cycle %d: %v", cycle, err)
		}
	}
	reclaimed := first.BeforeBytes - last.AfterBytes
	t.Logf("clock-accelerated SafetyFull fixture: 6 released 8 MiB objects; 16 native full cycles; physical repository bytes %d -> %d (reclaimed %d)", first.BeforeBytes, last.AfterBytes, reclaimed)
	if reclaimed < 20<<20 {
		t.Fatalf("native full collection reclaimed %d bytes; want at least %d", reclaimed, 20<<20)
	}

	// Reopen through the production content adapter after removing its cache, so
	// retained and obsolete object checks use fresh storage and cache state.
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s = nil
	if err := os.RemoveAll(filepath.Join(dir, "cache")); err != nil {
		t.Fatal(err)
	}
	readerStore, err := Open(ctx, dir, password, false)
	if err != nil {
		t.Fatal(err)
	}
	defer readerStore.Close(ctx)
	reader := readerStore.repository

	for i, root := range retainedRoots {
		if _, err := snapshot.LoadSnapshot(ctx, reader, manifest.ID(root)); err != nil {
			t.Fatalf("load retained root %d after reopen: %v", i, err)
		}
		entries, err := readSnapshotTree(ctx, readerStore, root)
		if err != nil {
			t.Fatalf("read retained root %d after reopen: %v", i, err)
		}
		if !bytes.Equal(entries["retained.bin"], retainedBytes[i]) {
			t.Fatalf("retained root %d bytes changed", i)
		}
	}
	for i, root := range releasedRoots {
		if _, err := snapshot.LoadSnapshot(ctx, reader, manifest.ID(root)); !errors.Is(err, snapshot.ErrSnapshotNotFound) {
			t.Fatalf("released manifest %d error = %v, want not-found", i, err)
		}
	}
	for i, ref := range releasedObjects {
		id, err := object.ParseID(ref)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := reader.OpenObject(ctx, id)
		if err == nil {
			_, readErr := io.ReadAll(obj)
			_ = obj.Close()
			if readErr == nil {
				t.Fatalf("released object %d remained readable", i)
			}
			err = readErr
		}
		if !errors.Is(err, object.ErrObjectNotFound) && !errors.Is(err, repoContent.ErrContentNotFound) {
			t.Fatalf("released object %d read error = %v, want native not-found", i, err)
		}
	}
}
