package kopia_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
)

const costMeasureStorageType = "lode-cost-measure-clock"

type costMeasureStorageOptions struct{ Path string }
type costMeasureKey struct{}
type costMeasure struct {
	peakBytes     atomic.Int64
	peakPackBytes atomic.Int64
	puts          atomic.Int64
	readsMu       sync.Mutex
	reads         []costReadEvent
	corruptID     blob.ID
}

type costReadEvent struct {
	id     blob.ID
	offset int64
	length int64
}

type costMeasureStorage struct {
	blob.Storage
	clock   *archiveClock
	measure *costMeasure
	options costMeasureStorageOptions
}

func init() {
	blob.AddSupportedStorage(costMeasureStorageType, costMeasureStorageOptions{}, func(ctx context.Context, options *costMeasureStorageOptions, create bool) (blob.Storage, error) {
		clock, ok := ctx.Value(archiveClockKey{}).(*archiveClock)
		if !ok {
			return nil, fmt.Errorf("cost measurement clock missing")
		}
		measure, ok := ctx.Value(costMeasureKey{}).(*costMeasure)
		if !ok {
			return nil, fmt.Errorf("cost measurement collector missing")
		}
		storage, err := filesystem.New(ctx, &filesystem.Options{Path: options.Path}, create)
		if err != nil {
			return nil, err
		}
		return &costMeasureStorage{Storage: storage, clock: clock, measure: measure, options: *options}, nil
	})
}

func (s *costMeasureStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: costMeasureStorageType, Config: &s.options}
}

func (s *costMeasureStorage) GetBlob(ctx context.Context, id blob.ID, offset, length int64, output blob.OutputBuffer) error {
	s.measure.readsMu.Lock()
	s.measure.reads = append(s.measure.reads, costReadEvent{id: id, offset: offset, length: length})
	corrupt := s.measure.corruptID == id
	s.measure.readsMu.Unlock()
	if !corrupt {
		return s.Storage.GetBlob(ctx, id, offset, length, output)
	}
	var buffer costOutputBuffer
	if err := s.Storage.GetBlob(ctx, id, offset, length, &buffer); err != nil {
		return err
	}
	if buffer.Len() > 0 {
		buffer.Bytes()[buffer.Len()-1] ^= 0xff
	}
	_, err := output.Write(buffer.Bytes())
	return err
}

func (s *costMeasureStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, options blob.PutOptions) error {
	if options.SetModTime.IsZero() {
		options.SetModTime = s.clock.now()
	}
	if err := s.Storage.PutBlob(ctx, id, data, options); err != nil {
		return err
	}
	s.measure.puts.Add(1)
	var total int64
	if err := s.Storage.ListBlobs(ctx, "", func(meta blob.Metadata) error {
		if meta.Length < 0 || total > int64(^uint64(0)>>1)-meta.Length {
			return fmt.Errorf("invalid blob length while measuring peak")
		}
		total += meta.Length
		return nil
	}); err != nil {
		return err
	}
	for current := s.measure.peakBytes.Load(); total > current; current = s.measure.peakBytes.Load() {
		if s.measure.peakBytes.CompareAndSwap(current, total) {
			break
		}
	}
	var packTotal int64
	for _, prefix := range content.PackBlobIDPrefixes {
		if err := s.Storage.ListBlobs(ctx, prefix, func(meta blob.Metadata) error {
			if meta.Length < 0 || packTotal > int64(^uint64(0)>>1)-meta.Length {
				return fmt.Errorf("invalid pack length while measuring peak")
			}
			packTotal += meta.Length
			return nil
		}); err != nil {
			return err
		}
	}
	for current := s.measure.peakPackBytes.Load(); packTotal > current; current = s.measure.peakPackBytes.Load() {
		if s.measure.peakPackBytes.CompareAndSwap(current, packTotal) {
			break
		}
	}
	return nil
}

type costOutputBuffer struct{ bytes.Buffer }

func (b *costOutputBuffer) Length() int { return b.Len() }

func (m *costMeasure) resetReads() {
	m.readsMu.Lock()
	m.reads = nil
	m.readsMu.Unlock()
}

func (m *costMeasure) readEvents() []costReadEvent {
	m.readsMu.Lock()
	defer m.readsMu.Unlock()
	return append([]costReadEvent(nil), m.reads...)
}

func packReadEvents(events []costReadEvent) []costReadEvent {
	var result []costReadEvent
	for _, event := range events {
		if len(event.id) > 0 && (event.id[0] == 'p' || event.id[0] == 'q') {
			result = append(result, event)
		}
	}
	return result
}

type measuredPack struct {
	id           blob.ID
	physical     int64
	indexedBytes int64
	liveBytes    int64
	deletedBytes int64
	liveStart    uint32
	liveEnd      uint32
	liveIDs      []content.ID
}

func newCostArchiveFixture(t *testing.T) (*archiveFixture, *costMeasure) {
	t.Helper()
	clock := &archiveClock{}
	measure := &costMeasure{}
	directory := t.TempDir()
	ctx := context.WithValue(t.Context(), archiveClockKey{}, clock)
	ctx = context.WithValue(ctx, costMeasureKey{}, measure)
	connection := blob.ConnectionInfo{Type: costMeasureStorageType, Config: &costMeasureStorageOptions{Path: filepath.Join(directory, "repository")}}
	storage, err := blob.NewStorage(ctx, connection, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Initialize(ctx, storage, nil, "temporary-cost-measure-password"); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "writer.config")
	storage, err = blob.NewStorage(ctx, connection, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Connect(ctx, config, storage, "temporary-cost-measure-password", &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Username: "lode", Hostname: "cost-test"},
		CachingOptions: content.CachingOptions{
			CacheDirectory: filepath.Join(directory, "cache"), ContentCacheSizeBytes: 1 << 20, MetadataCacheSizeBytes: 1 << 20,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	opened, err := repo.Open(ctx, config, "temporary-cost-measure-password", &repo.Options{TimeNowFunc: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	f := &archiveFixture{t: t, ctx: ctx, dir: directory, clock: clock, rep: opened.(repo.DirectRepository), config: config}
	t.Cleanup(func() {
		if err := f.rep.Close(context.WithoutCancel(f.ctx)); err != nil {
			t.Error(err)
		}
	})
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		params := maintenance.DefaultParams()
		params.Owner = writer.ClientOptions().UsernameAtHost()
		params.QuickCycle.Enabled, params.FullCycle.Enabled = false, false
		return maintenance.SetParams(ctx, writer, &params)
	})
	return f, measure
}

func TestMaintenanceCollectionCostMeasurements(t *testing.T) {
	cases := []struct {
		name        string
		deadBytes   int
		liveBytes   int
		independent int
		deadLast    bool
	}{
		{name: "64KiB-obsolete-1KiB-live", deadBytes: 64 << 10, liveBytes: 1 << 10, independent: 1},
		{name: "64KiB-obsolete-at-pack-tail-1KiB-live", deadBytes: 64 << 10, liveBytes: 1 << 10, independent: 1, deadLast: true},
		{name: "small-mostly-live", deadBytes: 64 << 10, liveBytes: 1 << 20, independent: 1},
		{name: "two-independent-small-mixes", deadBytes: 64 << 10, liveBytes: 1 << 10, independent: 2},
	}

	for caseIndex, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, meter := newCostArchiveFixture(t)
			var bases []manifest.ID
			for i := 0; i < tc.independent; i++ {
				bases = append(bases, makeCostFixture(t, f, fmt.Sprintf("%s-%d", tc.name, i), tc.deadBytes, tc.liveBytes, int64(caseIndex*19+i+1), tc.deadLast))
			}
			before := storedProofBytes(t, f)
			beforePackBytes := storedProofPackBytes(t, f)
			f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
				for _, id := range bases {
					if err := writer.DeleteManifest(ctx, id); err != nil {
						return err
					}
				}
				return nil
			})

			manager, err := lodekopia.NewMaintenance(f.rep)
			if err != nil {
				t.Fatal(err)
			}
			f.clock.offset.Add(int64(maintenance.SafetyFull.MinContentAgeSubjectToGC + maintenance.SafetyFull.PackDeleteMinAge + 2*time.Hour))
			preflight, err := manager.RunCollection(f.ctx, lodekopia.CollectionOptions{MaxRewriteBytes: 0})
			if err != nil {
				t.Fatalf("pre-rewrite native GC observation: %v", err)
			}
			t.Logf("pre-rewrite SafetyFull: blobs=%d packs=%d", preflight.After.BlobBytes, preflight.After.PackBytes)
			preRewritePacks := livePacksWithWaste(t, f)
			var recoveryPack measuredPack
			for _, pack := range preRewritePacks {
				if pack.deletedBytes > recoveryPack.deletedBytes {
					recoveryPack = pack
				}
			}
			if recoveryPack.id == "" || recoveryPack.deletedBytes == 0 {
				t.Fatal("pre-rewrite fixture lacks a mixed pack with current deleted content")
			}
			measurePackIndexRecovery(t, f, meter, recoveryPack, tc.liveBytes >= 1<<20)

			var adapterRewrittenCount, adapterRewrittenBytes uint64
			var rewriteSourcePacks = map[string]bool{}
			var maxPeakBlobOver, maxPeakPackOver int64
			for cycle := 0; cycle < 3; cycle++ {
				if cycle != 0 {
					f.clock.offset.Add(int64(maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
				}
				baselineBlobs := storedProofBytes(t, f)
				baselinePacks := storedProofPackBytes(t, f)
				meter.peakBytes.Store(baselineBlobs)
				meter.peakPackBytes.Store(baselinePacks)
				meter.resetReads()
				report, err := manager.RunCollection(f.ctx, lodekopia.CollectionOptions{
					MaxRewriteBytes: 16 << 20,
					BeforeRewrite:   func(lodekopia.PackCandidate) error { return nil },
					AfterRewrite:    func(string) error { return nil },
				})
				if err != nil {
					t.Fatalf("bounded full-safety cycle %d: %v", cycle+1, err)
				}
				adapterReads := packReadEvents(meter.readEvents())
				adapterRewrittenCount += report.RewrittenCount
				adapterRewrittenBytes += report.RewrittenBytes
				if report.RewrittenCount > 0 {
					rewriteSourcePacks[report.SourcePack] = true
				}
				if over := meter.peakBytes.Load() - baselineBlobs; over > maxPeakBlobOver {
					maxPeakBlobOver = over
				}
				if over := meter.peakPackBytes.Load() - baselinePacks; over > maxPeakPackOver {
					maxPeakPackOver = over
				}
				t.Logf("budgeted cycle=%d selected=%q rewritten=%d/%d source_gone=%t rewrite_complete=%t recovery_reads=%+v blob_bytes=%d→%d pack_peak_over=%d", cycle+1, report.SourcePack, report.RewrittenCount, report.RewrittenBytes, report.SourceGone, report.RewriteComplete, adapterReads, baselineBlobs, report.After.BlobBytes, meter.peakPackBytes.Load()-baselinePacks)
				if tc.liveBytes >= 1<<20 && report.RewrittenCount > 0 {
					t.Fatalf("mostly-live candidate was copied despite its poor obsolete-to-copy ratio: %+v", report)
				}
			}
			if tc.liveBytes >= 1<<20 && adapterRewrittenCount != 0 {
				t.Fatalf("mostly-live fixture rewrote %d content objects / %d bytes", adapterRewrittenCount, adapterRewrittenBytes)
			}
			if tc.liveBytes < 1<<20 && adapterRewrittenCount == 0 {
				t.Fatal("small obsolete content was not compacted")
			}
			if tc.independent > 1 && len(rewriteSourcePacks) < tc.independent {
				t.Fatalf("only %d independent source packs were compacted, want %d: %+v", len(rewriteSourcePacks), tc.independent, rewriteSourcePacks)
			}
			// Healthy output packs have no original extents absent from the current
			// content index. Repeated full-safety calls should not copy them again.
			var preCleanupPackBytes = storedProofPackBytes(t, f)
			for cycle := 0; cycle < 3; cycle++ {
				f.clock.offset.Add(int64(maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
				baselineBlobs := storedProofBytes(t, f)
				meter.peakBytes.Store(baselineBlobs)
				meter.resetReads()
				report, err := manager.RunCollection(f.ctx, lodekopia.CollectionOptions{
					MaxRewriteBytes: 16 << 20,
					BeforeRewrite:   func(lodekopia.PackCandidate) error { return nil },
					AfterRewrite:    func(string) error { return nil },
				})
				if err != nil {
					t.Fatalf("healthy native safety cycle %d: %v", cycle+1, err)
				}
				if report.RewrittenCount != 0 {
					t.Fatalf("healthy repeated cleanup rewrote %d contents from %q", report.RewrittenCount, report.SourcePack)
				}
				t.Logf("healthy cycle=%d selected=%q rewritten=%d pack_reads=%+v", cycle+1, report.SourcePack, report.RewrittenCount, packReadEvents(meter.readEvents()))
			}
			finalBlobBytes := storedProofBytes(t, f)
			finalPackBytes := storedProofPackBytes(t, f)
			reclaimedPackBytes := beforePackBytes - finalPackBytes
			t.Logf("collection cost result: initial_blob_bytes=%d initial_pack_bytes=%d pre_cleanup_pack_bytes=%d final_blob_bytes=%d final_pack_bytes=%d reclaimed_pack_bytes=%d adapter_rewritten_count=%d adapter_rewritten_bytes=%d unique_source_packs=%d peak_blob_over_baseline=%d peak_pack_over_baseline=%d", before, beforePackBytes, preCleanupPackBytes, finalBlobBytes, finalPackBytes, reclaimedPackBytes, adapterRewrittenCount, adapterRewrittenBytes, len(rewriteSourcePacks), maxPeakBlobOver, maxPeakPackOver)
			if tc.liveBytes < 1<<20 && finalPackBytes >= beforePackBytes {
				t.Fatalf("native SafetyFull did not reclaim small obsolete content: initial=%d after=%d pre-cleanup=%d", beforePackBytes, finalPackBytes, preCleanupPackBytes)
			}
			if tc.liveBytes < 1<<20 {
				minimumReclaimed := int64(tc.independent * tc.deadBytes * 3 / 4)
				if reclaimedPackBytes < minimumReclaimed {
					t.Fatalf("native SafetyFull reclaimed only %d pack bytes, want at least %d for %d obsolete payloads", reclaimedPackBytes, minimumReclaimed, tc.independent)
				}
			}
			if tc.liveBytes >= 1<<20 && finalPackBytes < beforePackBytes/2 {
				t.Fatalf("mostly-live fixture unexpectedly lost its protected payload packs: before=%d after=%d", beforePackBytes, finalPackBytes)
			}
		})
	}
}

func measurePackIndexRecovery(t *testing.T, f *archiveFixture, meter *costMeasure, pack measuredPack, testCorruptFallback bool) {
	t.Helper()
	stopAfterRead := errors.New("stop after native pack-index recovery measurement")
	var recovered []content.Info
	var recoveryErr error
	var reads []costReadEvent
	var putsBefore, putsAfter int64
	err := repo.DirectWriteSession(f.ctx, f.rep, repo.WriteSessionOptions{Purpose: "measure native pack index recovery"}, func(ctx context.Context, writer repo.DirectRepositoryWriter) error {
		meter.resetReads()
		putsBefore = meter.puts.Load()
		recovered, recoveryErr = writer.ContentManager().RecoverIndexFromPackBlob(ctx, pack.id, pack.physical, false)
		putsAfter = meter.puts.Load()
		reads = meter.readEvents()
		if recoveryErr != nil {
			return recoveryErr
		}
		return stopAfterRead
	})
	if !errors.Is(err, stopAfterRead) {
		t.Fatalf("RecoverIndexFromPackBlob error = %v, recovery error=%v", err, recoveryErr)
	}
	if putsAfter != putsBefore {
		t.Fatalf("commit=false recovery wrote %d blobs", putsAfter-putsBefore)
	}
	if len(recovered) == 0 {
		t.Fatalf("native local index recovery for %s returned no original content infos", pack.id)
	}
	var live, moved, deletedOrDropped int
	for _, original := range recovered {
		if original.PackBlobID != pack.id || int64(original.PackOffset)+int64(original.PackedLength) > pack.physical {
			t.Fatalf("recovered content has invalid original pack geometry: %+v pack=%s length=%d", original, pack.id, pack.physical)
		}
		current, err := f.rep.ContentReader().ContentInfo(f.ctx, original.ContentID)
		if err != nil {
			if errors.Is(err, content.ErrContentNotFound) {
				deletedOrDropped++
				continue
			}
			t.Fatalf("look up recovered content %s in current native index: %v", original.ContentID, err)
		}
		if current.PackBlobID != pack.id {
			moved++
			continue
		}
		if current.PackOffset != original.PackOffset || current.PackedLength != original.PackedLength {
			t.Fatalf("current location disagrees with original local index for %s: current=%+v recovered=%+v", original.ContentID, current, original)
		}
		if current.Deleted {
			deletedOrDropped++
		} else {
			live++
		}
	}
	if live == 0 || deletedOrDropped == 0 {
		t.Fatalf("recovered local index did not show both live and obsolete extents: live=%d moved=%d deleted_or_dropped=%d", live, moved, deletedOrDropped)
	}
	var packReads []costReadEvent
	for _, event := range reads {
		if event.id == pack.id {
			packReads = append(packReads, event)
		}
	}
	t.Logf("RecoverIndexFromPackBlob(commit=false): pack=%s physical=%d recovered=%d live_same_offset=%d moved=%d deleted_or_dropped=%d puts=%d pack_reads=%+v", pack.id, pack.physical, len(recovered), live, moved, deletedOrDropped, putsAfter-putsBefore, packReads)
	if pack.physical >= 8192 && len(packReads) != 1 {
		t.Errorf("valid local index recovery read ranges %+v, want one bounded tail read", packReads)
	}
	if pack.physical >= 8192 && len(packReads) == 1 && (packReads[0].offset != pack.physical-8192 || packReads[0].length != 8192) {
		t.Errorf("valid local index recovery range=%+v, want tail offset=%d length=8192", packReads[0], pack.physical-8192)
	}

	if !testCorruptFallback {
		return
	}
	meter.readsMu.Lock()
	meter.corruptID = pack.id
	meter.readsMu.Unlock()
	meter.resetReads()
	putsBefore = meter.puts.Load()
	var corruptErr error
	err = repo.DirectWriteSession(f.ctx, f.rep, repo.WriteSessionOptions{Purpose: "measure corrupt native recovery fallback"}, func(ctx context.Context, writer repo.DirectRepositoryWriter) error {
		_, corruptErr = writer.ContentManager().RecoverIndexFromPackBlob(ctx, pack.id, pack.physical, false)
		reads = meter.readEvents()
		putsAfter = meter.puts.Load()
		if corruptErr == nil {
			return errors.New("corrupted postamble unexpectedly recovered")
		}
		return corruptErr
	})
	meter.readsMu.Lock()
	meter.corruptID = ""
	meter.readsMu.Unlock()
	if err == nil || putsAfter != putsBefore {
		t.Fatalf("corrupt recovery error=%v, blob writes=%d; want an error and no writes", err, putsAfter-putsBefore)
	}
	fullRead := false
	for _, event := range reads {
		if event.id == pack.id && event.offset == 0 && event.length == -1 {
			fullRead = true
		}
	}
	if !fullRead {
		t.Fatalf("corrupt local-index recovery did not demonstrate full-pack fallback: %+v", reads)
	}
	var corruptPackReads []costReadEvent
	for _, event := range reads {
		if event.id == pack.id {
			corruptPackReads = append(corruptPackReads, event)
		}
	}
	t.Logf("corrupt local-index recovery error=%v; fallback pack reads=%+v; writes=%d", err, corruptPackReads, putsAfter-putsBefore)
}

func makeCostFixture(t *testing.T, f *archiveFixture, label string, deadBytes, liveBytes int, seed int64, deadLast bool) manifest.ID {
	t.Helper()
	var base manifest.ID
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		var dead, live *snapshot.DirEntry
		var err error
		if deadLast {
			live, err = captureProofFile(ctx, writer, "retained.bin", archivePayload(liveBytes, seed+1))
			if err != nil {
				return err
			}
			dead, err = captureProofFile(ctx, writer, "obsolete.bin", archivePayload(deadBytes, seed))
		} else {
			dead, err = captureProofFile(ctx, writer, "obsolete.bin", archivePayload(deadBytes, seed))
			if err != nil {
				return err
			}
			live, err = captureProofFile(ctx, writer, "retained.bin", archivePayload(liveBytes, seed+1))
		}
		if err != nil {
			return err
		}
		baseRoot, err := writeProofDirectory(ctx, writer, "", dead, live)
		if err != nil {
			return err
		}
		base, err = f.saveRoot(writer, baseRoot, "cost-base-"+label)
		if err != nil {
			return err
		}
		keptRoot, err := writeProofDirectory(ctx, writer, "", live)
		if err != nil {
			return err
		}
		_, err = f.saveRoot(writer, keptRoot, "cost-retained-"+label)
		return err
	})
	return base
}

func livePacksWithWaste(t *testing.T, f *archiveFixture) []measuredPack {
	t.Helper()
	var packs []measuredPack
	err := f.rep.ContentReader().IteratePacks(f.ctx, content.IteratePackOptions{
		Prefixes:                           content.PackBlobIDPrefixes,
		IncludePacksWithOnlyDeletedContent: true,
		IncludeContentInfos:                true,
	}, func(info content.PackInfo) error {
		meta, err := f.rep.BlobReader().GetMetadata(f.ctx, info.PackID)
		if err != nil {
			return err
		}
		pack := measuredPack{id: info.PackID, physical: meta.Length}
		for _, contentInfo := range info.ContentInfos {
			pack.indexedBytes += int64(contentInfo.PackedLength)
			if contentInfo.Deleted {
				pack.deletedBytes += int64(contentInfo.PackedLength)
				continue
			}
			if len(pack.liveIDs) == 0 || contentInfo.PackOffset < pack.liveStart {
				pack.liveStart = contentInfo.PackOffset
			}
			if end := contentInfo.PackOffset + contentInfo.PackedLength; end > pack.liveEnd {
				pack.liveEnd = end
			}
			pack.liveBytes += int64(contentInfo.PackedLength)
			pack.liveIDs = append(pack.liveIDs, contentInfo.ContentID)
		}
		if len(pack.liveIDs) > 0 && pack.physical > pack.liveBytes {
			packs = append(packs, pack)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return packs
}

func storedProofPackBytes(t *testing.T, f *archiveFixture) int64 {
	t.Helper()
	var total int64
	for _, prefix := range content.PackBlobIDPrefixes {
		if err := f.rep.BlobReader().ListBlobs(f.ctx, prefix, func(meta blob.Metadata) error {
			if meta.Length < 0 || total > int64(^uint64(0)>>1)-meta.Length {
				return fmt.Errorf("invalid native pack blob length")
			}
			total += meta.Length
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return total
}
