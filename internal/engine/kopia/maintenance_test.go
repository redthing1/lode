package kopia_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
)

func TestMaintenanceInspectUsesNativeBlobAndIndexMetadata(t *testing.T) {
	f := newArchiveFixture(t)
	manager, err := lodekopia.NewMaintenance(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Namespace() == "" {
		t.Fatal("expected repository namespace")
	}

	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		file, err := captureProofFile(ctx, writer, "payload.bin", archivePayload(4096, 71))
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", file)
		if err != nil {
			return err
		}
		_, err = f.saveRoot(writer, root, "maintenance-stats")
		return err
	})

	stats, err := manager.Inspect(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var blobBytes, packBytes int64
	var packCount int64
	if err := f.rep.BlobReader().ListBlobs(f.ctx, "", func(meta blob.Metadata) error {
		blobBytes += meta.Length
		if meta.BlobID[0] == 'p' || meta.BlobID[0] == 'q' {
			packBytes += meta.Length
			packCount++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stats.BlobBytes != blobBytes || stats.PackBytes != packBytes || stats.PackCount != packCount {
		t.Fatalf("stats physical bytes disagree with native blob listing: got %+v, blobs=%d packs=%d/%d", stats, blobBytes, packBytes, packCount)
	}
	if stats.IndexedLiveBytes <= 0 || stats.PackCount == 0 {
		t.Fatalf("expected indexed payload and pack metadata: %+v", stats)
	}
	if !stats.CapacityKnown {
		t.Fatalf("clock storage wrapper should pass through underlying filesystem capacity: %+v", stats)
	}
}

func TestMaintenanceInspectReturnsCanceledReadContext(t *testing.T) {
	f := newArchiveFixture(t)
	manager, err := lodekopia.NewMaintenance(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := manager.Inspect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect error = %v, want context.Canceled", err)
	}
}

func TestMaintenanceRunFullUsesNativeSafetyFullCycle(t *testing.T) {
	f := newArchiveFixture(t)
	manager, err := lodekopia.NewMaintenance(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		file, err := captureProofFile(ctx, writer, "kept.bin", archivePayload(2048, 72))
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", file)
		if err != nil {
			return err
		}
		_, err = f.saveRoot(writer, root, "maintenance-full-cycle")
		return err
	})

	// The fixture disables automatic scheduling, while this explicit call requests
	// exactly Kopia's full mode with its unmodified SafetyFull timings.
	report, err := manager.RunFull(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Executed {
		t.Fatal("native full maintenance did not execute (likely lock contention)")
	}
	if report.Before.PackBytes == 0 || report.After.PackBytes == 0 {
		t.Fatalf("full cycle report omitted native pack measurements: %+v", report)
	}
	if report.After.IndexedLiveBytes <= 0 {
		t.Fatalf("full cycle should retain the live snapshot content: %+v", report.After)
	}
	manifests, err := f.rep.FindManifests(f.ctx, map[string]string{"lode-proof": "maintenance-full-cycle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) == 0 {
		t.Fatal("full maintenance removed the retained snapshot")
	}
}

type scheduleWriteFailureRepository struct {
	repo.DirectRepository
	scheduleWrites *atomic.Int64
}

func (r scheduleWriteFailureRepository) NewDirectWriter(ctx context.Context, options repo.WriteSessionOptions) (context.Context, repo.DirectRepositoryWriter, error) {
	ctx, writer, err := r.DirectRepository.NewDirectWriter(ctx, options)
	if err != nil {
		return ctx, nil, err
	}
	return ctx, scheduleWriteFailureWriter{DirectRepositoryWriter: writer, scheduleWrites: r.scheduleWrites}, nil
}

type scheduleWriteFailureWriter struct {
	repo.DirectRepositoryWriter
	scheduleWrites *atomic.Int64
}

func (w scheduleWriteFailureWriter) BlobStorage() blob.Storage {
	return scheduleWriteFailureStorage{Storage: w.DirectRepositoryWriter.BlobStorage(), scheduleWrites: w.scheduleWrites}
}

type scheduleWriteFailureStorage struct {
	blob.Storage
	scheduleWrites *atomic.Int64
}

func (s scheduleWriteFailureStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, options blob.PutOptions) error {
	if id == "kopia.maintenance" {
		if s.scheduleWrites.Add(1) >= 3 {
			return errors.New("injected maintenance schedule persistence failure")
		}
	}
	return s.Storage.PutBlob(ctx, id, data, options)
}

func TestMaintenanceCollectionPropagatesSchedulePersistenceFailure(t *testing.T) {
	f := newArchiveFixture(t)
	var scheduleWrites atomic.Int64
	manager, err := lodekopia.NewMaintenance(scheduleWriteFailureRepository{DirectRepository: f.rep, scheduleWrites: &scheduleWrites})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.RunCollection(f.ctx, lodekopia.CollectionOptions{MaxRewriteBytes: 1})
	if err == nil || !strings.Contains(err.Error(), "persist bounded rewrite schedule") || scheduleWrites.Load() < 4 {
		t.Fatalf("RunCollection error = %v, schedule writes=%d; want explicit bounded rewrite schedule persistence failure", err, scheduleWrites.Load())
	}
	schedule, err := maintenance.GetSchedule(f.ctx, f.rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range schedule.Runs[maintenance.TaskDeleteOrphanedBlobsFull] {
		if run.Success {
			t.Fatalf("full orphan deletion ran after bounded schedule persistence failed: %+v", run)
		}
	}
}

func TestMaintenanceRunCollectionBoundsRewriteAndReclaimsSingleMixedPack(t *testing.T) {
	f := newArchiveFixture(t)
	manager, err := lodekopia.NewMaintenance(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	baseA, baseB := archivePayload(256<<10, 81), archivePayload(256<<10, 82)
	obsolete := archivePayload(8<<20, 83)
	localA, targetB := archivePayload(256<<10, 84), archivePayload(256<<10, 85)
	var baseRoot, localRoot, targetRoot *snapshot.DirEntry
	var oldOnly *snapshot.DirEntry
	var baseSnapshot manifest.ID
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		a, err := captureProofFile(ctx, writer, "a.bin", baseA)
		if err != nil {
			return err
		}
		b, err := captureProofFile(ctx, writer, "b.bin", baseB)
		if err != nil {
			return err
		}
		oldOnly, err = captureProofFile(ctx, writer, "obsolete.bin", obsolete)
		if err != nil {
			return err
		}
		child, err := writeProofDirectory(ctx, writer, "dir", a, b)
		if err != nil {
			return err
		}
		baseRoot, err = writeProofDirectory(ctx, writer, "", child, oldOnly)
		if err != nil {
			return err
		}
		baseSnapshot, err = f.saveRoot(writer, baseRoot, "bounded-collection-base")
		return err
	})
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		a, err := captureProofFile(ctx, writer, "a.bin", localA)
		if err != nil {
			return err
		}
		b, err := captureProofFile(ctx, writer, "b.bin", baseB)
		if err != nil {
			return err
		}
		child, err := writeProofDirectory(ctx, writer, "dir", a, b)
		if err != nil {
			return err
		}
		localRoot, err = writeProofDirectory(ctx, writer, "", child)
		if err != nil {
			return err
		}
		if _, err := f.saveRoot(writer, localRoot, "bounded-collection-local"); err != nil {
			return err
		}
		a, err = captureProofFile(ctx, writer, "a.bin", baseA)
		if err != nil {
			return err
		}
		b, err = captureProofFile(ctx, writer, "b.bin", targetB)
		if err != nil {
			return err
		}
		child, err = writeProofDirectory(ctx, writer, "dir", a, b)
		if err != nil {
			return err
		}
		targetRoot, err = writeProofDirectory(ctx, writer, "", child)
		if err != nil {
			return err
		}
		_, err = f.saveRoot(writer, targetRoot, "bounded-collection-target")
		return err
	})
	bytesBefore := storedProofBytes(t, f)
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		return writer.DeleteManifest(ctx, baseSnapshot)
	})

	// The first run records a truthful bounded no-work scan. This prevents the
	// native full run from expanding into an automatic whole-repository rewrite.
	initial, err := manager.RunCollection(f.ctx, lodekopia.CollectionOptions{MaxRewriteBytes: 16 << 20})
	if err != nil || !initial.Executed || !initial.RewriteScheduleRecorded || initial.SourcePack != "" {
		t.Fatalf("initial bounded full collection = %+v, %v", initial, err)
	}

	// First age the unique base content into GC scope, then allow two full-safety
	// GC observations to make deleted-index removal and orphan deletion eligible.
	f.clock.offset.Add(int64(maintenance.SafetyFull.MinContentAgeSubjectToGC + maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
	lowBudget, err := manager.RunCollection(f.ctx, lodekopia.CollectionOptions{MaxRewriteBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if lowBudget.SourcePack != "" || lowBudget.RewrittenCount != 0 || lowBudget.RewrittenBytes != 0 {
		t.Fatalf("low rewrite budget selected/copied a pack: %+v", lowBudget)
	}
	var selected lodekopia.PackCandidate
	var resumed lodekopia.PackCandidate
	var beforeCalls, afterCalls int
	stopBeforeCopy := errors.New("simulated interruption before native rewrite")
	_, err = manager.RunCollection(f.ctx, lodekopia.CollectionOptions{
		MaxRewriteBytes: 16 << 20,
		BeforeRewrite: func(candidate lodekopia.PackCandidate) error {
			beforeCalls++
			selected = candidate
			return stopBeforeCopy
		},
		AfterRewrite: func(string) error { return nil },
	})
	if !errors.Is(err, stopBeforeCopy) || selected.ID == "" {
		t.Fatalf("pre-copy interruption = %v, candidate=%+v", err, selected)
	}
	rewrite, err := manager.RunCollection(f.ctx, lodekopia.CollectionOptions{
		// SourceRewritten deliberately simulates a stale checkpoint hint. Native
		// GC can make a previously rewritten content live in its old pack again;
		// RunCollection must inspect and re-copy current live IDs on every retry.
		SourcePack: selected.ID, SourceRewritten: true,
		MaxRewriteBytes: 16 << 20,
		BeforeRewrite: func(candidate lodekopia.PackCandidate) error {
			beforeCalls++
			resumed = candidate
			return nil
		},
		AfterRewrite: func(source string) error {
			afterCalls++
			if source != selected.ID {
				return fmt.Errorf("acknowledged source %q differs from selected pack %q", source, selected.ID)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if beforeCalls != 2 || afterCalls != 1 || rewrite.SourcePack != selected.ID || resumed.ID != selected.ID || !rewrite.RewriteScheduleRecorded || !rewrite.RewriteComplete {
		t.Fatalf("bounded rewrite checkpoint/report: before=%d after=%d selected=%+v resumed=%+v report=%+v", beforeCalls, afterCalls, selected, resumed, rewrite)
	}
	if rewrite.SourceGone {
		t.Fatalf("expected native safety gates to retain the physical source pack after rewrite: %+v", rewrite)
	}
	if selected.LiveContentCount == 0 || selected.IndexedLiveBytes <= 0 || selected.ObsoleteBytes <= selected.OriginalOverheadBytes || selected.ReclaimableEstimate <= 0 || selected.ObsoleteBytes < (selected.IndexedLiveBytes+2)/3 {
		t.Fatalf("selected pack does not satisfy measured live/waste criteria: %+v", selected)
	}
	if rewrite.RewrittenCount == 0 || rewrite.RewrittenCount > uint64(selected.LiveContentCount) || rewrite.RewrittenBytes > uint64(selected.IndexedLiveBytes) {
		t.Fatalf("rewrite escaped single-pack live-byte bound: candidate=%+v report=%+v", selected, rewrite)
	}

	// The native rewrite marker delays orphan deletion. A first follow-up gives
	// Kopia the next separated GC observation and lets its stale-index compaction
	// run; if that run also reports a newer deletion marker, another full cycle
	// lets the unchanged one-hour rewrite/delete gate pass.
	f.clock.offset.Add(int64(maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
	cleanup, err := manager.RunCollection(f.ctx, lodekopia.CollectionOptions{
		SourcePack: selected.ID, SourceRewritten: true, MaxRewriteBytes: 16 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cleanup.Executed {
		t.Fatalf("native full safety did not execute: %+v", cleanup)
	}
	if !cleanup.SourceGone {
		f.clock.offset.Add(int64(maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
		cleanup, err = manager.RunCollection(f.ctx, lodekopia.CollectionOptions{
			SourcePack: selected.ID, SourceRewritten: true, MaxRewriteBytes: 16 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !cleanup.SourceGone {
		f.clock.offset.Add(int64(maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
		cleanup, err = manager.RunCollection(f.ctx, lodekopia.CollectionOptions{
			SourcePack: selected.ID, SourceRewritten: true, MaxRewriteBytes: 16 << 20,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !cleanup.SourceGone {
		schedule, scheduleErr := maintenance.GetSchedule(f.ctx, f.rep)
		var remaining []content.Info
		_ = f.rep.ContentReader().IteratePacks(f.ctx, content.IteratePackOptions{Prefixes: content.PackBlobIDPrefixes, IncludePacksWithOnlyDeletedContent: true, IncludeContentInfos: true}, func(pack content.PackInfo) error {
			if string(pack.PackID) == selected.ID {
				remaining = append(remaining, pack.ContentInfos...)
			}
			return nil
		})
		t.Fatalf("native full safety did not remove source pack: %+v schedule=%+v scheduleErr=%v remaining=%+v", cleanup, schedule, scheduleErr, remaining)
	}
	bytesAfter := storedProofBytes(t, f)
	if bytesAfter >= bytesBefore {
		t.Fatalf("bounded collection did not reclaim physical blob bytes: before=%d after=%d", bytesBefore, bytesAfter)
	}
	newReader := f.reader("bounded-collection-reader")
	defer newReader.Close(context.WithoutCancel(f.ctx))
	if _, err := readProofObject(f.ctx, newReader, oldOnly.ObjectID); !errors.Is(err, object.ErrObjectNotFound) && !errors.Is(err, content.ErrContentNotFound) {
		t.Fatalf("expired payload read error = %v, want native not-found", err)
	}
	for _, check := range []struct {
		root *snapshot.DirEntry
		path string
		want []byte
	}{
		{localRoot, "dir/a.bin", localA}, {localRoot, "dir/b.bin", baseB},
		{targetRoot, "dir/a.bin", baseA}, {targetRoot, "dir/b.bin", targetB},
	} {
		entry, err := fileEntryForObject(f.ctx, newReader, check.root, check.path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readProofObject(f.ctx, newReader, entry.ObjectID)
		if err != nil || !bytes.Equal(got, check.want) {
			t.Fatalf("retained %s bytes=%d err=%v", check.path, len(got), err)
		}
	}
}
