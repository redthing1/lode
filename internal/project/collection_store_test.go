package project

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestCapacityPolicyPersistsAndEnforcesSQLiteRange(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := metadata.capacityPolicy(ctx)
	if err != nil || policy != DefaultCapacityPolicy() {
		t.Fatalf("default capacity policy = %+v, %v; want %+v", policy, err, DefaultCapacityPolicy())
	}
	custom := CapacityPolicy{ReserveBytes: 8 << 30}
	if err := metadata.setCapacityPolicy(ctx, custom); err != nil {
		t.Fatalf("set capacity policy: %v", err)
	}
	if err := metadata.setCapacityPolicy(ctx, CapacityPolicy{ReserveBytes: math.MaxUint64}); err == nil {
		t.Fatal("set capacity policy accepted value beyond SQLite integer range")
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err = OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	if got, err := metadata.capacityPolicy(ctx); err != nil || got != custom {
		t.Fatalf("reopened capacity policy = %+v, %v; want %+v", got, err, custom)
	}
}

func TestCompactionCheckpointIsSingleAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	finished := started.Add(4 * time.Minute)
	if _, found, err := metadata.compaction(ctx); err != nil || found {
		t.Fatalf("initial compaction = found %t, err %v; want absent", found, err)
	}
	if err := metadata.beginCompaction(ctx, "p-pack-A", started); err != nil {
		t.Fatalf("begin compaction: %v", err)
	}
	if err := metadata.beginCompaction(ctx, "", started); err == nil {
		t.Fatal("begin compaction accepted an empty source pack ID")
	}
	if err := metadata.beginCompaction(ctx, string(make([]byte, 1025)), started); err == nil {
		t.Fatal("begin compaction accepted an overlong source pack ID")
	}
	if err := metadata.beginCompaction(ctx, "p-pack-A", started.Add(time.Hour)); err != nil {
		t.Fatalf("idempotent begin: %v", err)
	}
	if err := metadata.beginCompaction(ctx, "p-pack-B", started); !errors.Is(err, ErrMetadataConflict) {
		t.Fatalf("begin competing source = %v; want immutable conflict", err)
	}
	if err := metadata.finishCompactionRewrite(ctx, "p-pack-A", finished); err != nil {
		t.Fatalf("finish rewrite: %v", err)
	}
	if err := metadata.finishCompactionRewrite(ctx, "p-pack-A", finished.Add(time.Hour)); err != nil {
		t.Fatalf("idempotent finish: %v", err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	record, found, err := metadata.compaction(ctx)
	if err != nil || !found || record.SourcePack != "p-pack-A" || !record.StartedAt.Equal(started) || !record.RewrittenAt.Equal(finished) {
		t.Fatalf("reopened compaction = %+v, found=%t, err=%v; want original times", record, found, err)
	}
	if err := metadata.beginCompaction(ctx, "p-pack-A", started.Add(2*time.Hour)); err != nil {
		t.Fatalf("retry completed source: %v", err)
	}
	retried, found, err := metadata.compaction(ctx)
	if err != nil || !found || !retried.StartedAt.Equal(started) || !retried.RewrittenAt.Equal(finished) {
		t.Fatalf("retry changed completed checkpoint = %+v, found=%t, err=%v", retried, found, err)
	}
	if err := metadata.clearCompaction(ctx, "p-pack-B"); !errors.Is(err, ErrMetadataConflict) {
		t.Fatalf("clear competing source = %v; want immutable conflict", err)
	}
	if err := metadata.clearCompaction(ctx, "p-pack-A"); err != nil {
		t.Fatalf("clear completed checkpoint: %v", err)
	}
	if err := metadata.clearCompaction(ctx, "p-pack-A"); err != nil {
		t.Fatalf("idempotent clear: %v", err)
	}
	if _, found, err := metadata.compaction(ctx); err != nil || found {
		t.Fatalf("compaction after clear = found %t, err %v; want absent", found, err)
	}
}

func TestSchemaFourMigrationAddsCapacityAndCompactionState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	root := []byte("schema-four archive root")
	rootID, err := metadata.Import(ctx, "namespace", "format", root, map[string][]byte{"directory": []byte{0, 1, 0xff}})
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	// Remove only schema-five additions to create a faithful v4 state while
	// keeping the archive tables and all earlier metadata intact.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE compaction_state`,
		`DROP TABLE capacity_policy`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare schema-four fixture: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = OpenMetadata(path)
	if err != nil {
		t.Fatalf("migrate schema four: %v", err)
	}
	defer metadata.Close()
	var version int
	if err := metadata.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("migrated schema version = %d, %v; want 5", version, err)
	}
	if got, err := metadata.Root(ctx, "namespace", "format", rootID); err != nil || string(got) != string(root) {
		t.Fatalf("archive after migration = %q, %v", got, err)
	}
	if got, err := metadata.Directory(ctx, "namespace", "format", "directory"); err != nil || string(got) != string([]byte{0, 1, 0xff}) {
		t.Fatalf("directory after migration = %v, %v", got, err)
	}
	if policy, err := metadata.capacityPolicy(ctx); err != nil || policy != DefaultCapacityPolicy() {
		t.Fatalf("migrated capacity policy = %+v, %v", policy, err)
	}
	if _, found, err := metadata.compaction(ctx); err != nil || found {
		t.Fatalf("migrated compaction = found %t, err %v; want absent", found, err)
	}
}
