package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/object"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

type collectionClockContextKey struct{}

type collectionClock struct {
	base   time.Time
	offset atomic.Int64
}

func (c *collectionClock) now() time.Time {
	return c.base.Add(time.Duration(c.offset.Load())).UTC()
}

type collectionClockStorageOptions struct {
	Path string `json:"path"`
}

type collectionClockStorage struct {
	blob.Storage
	clock   *collectionClock
	options collectionClockStorageOptions
}

func init() {
	blob.AddSupportedStorage("lode-project-collection-clock", collectionClockStorageOptions{}, func(ctx context.Context, options *collectionClockStorageOptions, create bool) (blob.Storage, error) {
		clock, ok := ctx.Value(collectionClockContextKey{}).(*collectionClock)
		if !ok {
			return nil, errors.New("collection test clock missing")
		}
		storage, err := filesystem.New(ctx, &filesystem.Options{Path: options.Path}, create)
		if err != nil {
			return nil, err
		}
		return &collectionClockStorage{Storage: storage, clock: clock, options: *options}, nil
	})
}

func (s *collectionClockStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: "lode-project-collection-clock", Config: &s.options}
}

func (s *collectionClockStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, options blob.PutOptions) error {
	if options.SetModTime.IsZero() {
		options.SetModTime = s.clock.now()
	}
	return s.Storage.PutBlob(ctx, id, data, options)
}

type collectionIntegrationFixture struct {
	t        *testing.T
	ctx      context.Context
	dir      string
	config   string
	clock    *collectionClock
	repo     repo.DirectRepository
	metadata *Metadata
	service  *Service
	closed   bool
}

func newCollectionIntegrationFixture(t *testing.T) *collectionIntegrationFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("native filesystem collection integration test")
	}
	ctx := context.Background()
	dir := t.TempDir()
	clock := &collectionClock{base: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	ctx = context.WithValue(ctx, collectionClockContextKey{}, clock)
	storageInfo := blob.ConnectionInfo{Type: "lode-project-collection-clock", Config: &collectionClockStorageOptions{Path: filepath.Join(dir, "native")}}
	storage, err := blob.NewStorage(ctx, storageInfo, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Initialize(ctx, storage, nil, "collection-test-password"); err != nil {
		_ = storage.Close(ctx)
		t.Fatal(err)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	storage, err = blob.NewStorage(ctx, storageInfo, false)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "writer.config")
	if err := repo.Connect(ctx, config, storage, "collection-test-password", &repo.ConnectOptions{
		ClientOptions:  repo.ClientOptions{Username: "lode", Hostname: "collection-test"},
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(dir, "cache"), ContentCacheSizeBytes: 1 << 20, MetadataCacheSizeBytes: 1 << 20},
	}); err != nil {
		_ = storage.Close(ctx)
		t.Fatal(err)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	native, err := repo.Open(ctx, config, "collection-test-password", &repo.Options{TimeNowFunc: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	direct := native.(repo.DirectRepository)
	if err := repo.WriteSession(ctx, direct, repo.WriteSessionOptions{Purpose: "disable background maintenance for collection integration"}, func(ctx context.Context, writer repo.RepositoryWriter) error {
		params := maintenance.DefaultParams()
		params.Owner = writer.ClientOptions().UsernameAtHost()
		params.QuickCycle.Enabled, params.FullCycle.Enabled = false, false
		return maintenance.SetParams(ctx, writer, &params)
	}); err != nil {
		_ = direct.Close(context.WithoutCancel(ctx))
		t.Fatal(err)
	}
	if err := direct.Close(context.WithoutCancel(ctx)); err != nil {
		t.Fatal(err)
	}
	return openCollectionIntegrationFixture(t, dir, config, clock, "")
}

func openCollectionIntegrationFixture(t *testing.T, dir, config string, clock *collectionClock, crashMode string) *collectionIntegrationFixture {
	t.Helper()
	ctx := context.WithValue(context.Background(), collectionClockContextKey{}, clock)
	native, err := repo.Open(ctx, config, "collection-test-password", &repo.Options{TimeNowFunc: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	direct := native.(repo.DirectRepository)
	objects, err := lodekopia.NewObjects(direct)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := lodekopia.NewRoots(direct)
	if err != nil {
		t.Fatal(err)
	}
	maintainer, err := lodekopia.NewMaintenance(direct)
	if err != nil {
		t.Fatal(err)
	}
	var contentMaintenance ContentMaintenance = maintainer
	if crashMode != "" {
		contentMaintenance = collectionCrashMaintenance{ContentMaintenance: maintainer, mode: crashMode}
	}
	metadata, err := OpenMetadata(filepath.Join(dir, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ctx, metadata, roots, Options{Objects: objects, Maintenance: contentMaintenance, Now: clock.now})
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	fixture := &collectionIntegrationFixture{t: t, ctx: ctx, dir: dir, config: config, clock: clock, repo: direct, metadata: metadata, service: service}
	t.Cleanup(fixture.close)
	return fixture
}

func (f *collectionIntegrationFixture) close() {
	if f.closed {
		return
	}
	f.closed = true
	if f.service != nil {
		if err := f.service.Close(); err != nil {
			f.t.Error(err)
		}
	}
	if f.metadata != nil {
		if err := f.metadata.Close(); err != nil {
			f.t.Error(err)
		}
	}
	if f.repo != nil {
		if err := f.repo.Close(context.WithoutCancel(f.ctx)); err != nil {
			f.t.Error(err)
		}
	}
}

type collectionCrashMaintenance struct {
	ContentMaintenance
	mode string
}

func (m collectionCrashMaintenance) RunCollection(ctx context.Context, options lodekopia.CollectionOptions) (lodekopia.CollectionReport, error) {
	if m.mode == "before-rewrite" {
		before := options.BeforeRewrite
		options.BeforeRewrite = func(candidate lodekopia.PackCandidate) error {
			if before == nil {
				return errors.New("missing service checkpoint callback")
			}
			if err := before(candidate); err != nil {
				return err
			}
			os.Exit(86)
			return nil
		}
	}
	if m.mode == "after-rewrite" {
		options.AfterRewrite = func(string) error {
			// The native adapter calls this after the content rewrite and its
			// schedule marker are durable, but before the service acknowledges SQL.
			os.Exit(87)
			return nil
		}
	}
	return m.ContentMaintenance.RunCollection(ctx, options)
}

func (f *collectionIntegrationFixture) apply(operationID, base, message string, files map[string][]byte, absent ...string) Commit {
	f.t.Helper()
	request := ChangeRequest{OperationID: operationID, Branch: "main", Base: base, Author: "collection-test", Message: message}
	for path, payload := range files {
		request.Changes = append(request.Changes, Change{Path: path, Kind: model.File, SHA256: sha256Hex(payload), Size: int64(len(payload))})
	}
	for _, path := range absent {
		request.Changes = append(request.Changes, Change{Path: path, Kind: model.Absent})
	}
	commit, err := f.service.Apply(f.ctx, request, func(path model.Path) (io.ReadCloser, error) {
		payload, ok := files[path.String()]
		if !ok {
			return nil, errors.New("missing collection fixture payload")
		}
		return io.NopCloser(bytes.NewReader(payload)), nil
	})
	if err != nil {
		f.t.Fatalf("apply %s: %v", operationID, err)
	}
	return commit
}

func collectionPayload(size int, seed int64) []byte {
	result := make([]byte, size)
	_, _ = rand.New(rand.NewSource(seed)).Read(result)
	return result
}

func TestExpireThenCollectReclaimsPhysicalBytesAndPreservesProtectedSharedContents(t *testing.T) {
	f := newCollectionIntegrationFixture(t)
	oldPayload := collectionPayload(8<<20, 101)
	sharedPayload := collectionPayload(256<<10, 202)
	oldEdit := []byte("old edit")
	newEdit := []byte("new edit")
	base := f.apply("collection-base", "", "base", map[string][]byte{
		"old-only.bin": oldPayload,
		"shared.bin":   sharedPayload,
		"edit.bin":     oldEdit,
	})
	baseCommit, err := f.metadata.commit(f.ctx, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseArchive, err := f.service.archive(f.ctx, baseCommit.Tree)
	if err != nil {
		t.Fatal(err)
	}
	tip := f.apply("collection-tip", base.ID, "tip", map[string][]byte{"edit.bin": newEdit}, "old-only.bin")
	if err := f.service.CreateTag(f.ctx, "keep-tip", tip.ID); err != nil {
		t.Fatalf("tag retained tip: %v", err)
	}
	if head, err := f.service.Head(f.ctx, "main"); err != nil || head != tip.ID {
		t.Fatalf("branch tip = %q, %v; want %q", head, err, tip.ID)
	}
	if tag, err := f.service.Tag(f.ctx, "keep-tip"); err != nil || tag.CommitID != tip.ID {
		t.Fatalf("tag = %+v, %v; want protected tip", tag, err)
	}
	if err := f.service.SetRetentionPolicy(f.ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}

	// Move beyond operation and root grace ages before expiring the no-longer
	// protected ancestor. Native writes and repository time share this clock.
	f.clock.offset.Add(int64(48 * time.Hour))
	if got, err := f.service.ExplainRetention(f.ctx, base.ID); err != nil || len(got.Reasons) != 0 {
		t.Fatalf("base retention reasons before expiry = %+v, %v; want none", got, err)
	}
	before, err := f.service.StorageStats(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := f.service.Expire(f.ctx, []string{base.ID})
	if err != nil || len(expired.Expired) != 1 || expired.Expired[0] != base.ID {
		t.Fatalf("expire base = %+v, %v", expired, err)
	}
	if err := f.service.CreateTag(f.ctx, "too-late", base.ID); !errors.Is(err, ErrPayloadExpired) {
		t.Fatalf("tag expired version error = %v, want ErrPayloadExpired", err)
	}
	if _, _, err := f.service.OpenFile(f.ctx, base.ID, "old-only.bin"); !errors.Is(err, ErrPayloadExpired) {
		t.Fatalf("open expired payload error = %v, want ErrPayloadExpired", err)
	}
	archived, _, err := f.service.Entries(f.ctx, base.ID, []SelectionRule{{Included: true}}, "", 100)
	if got, ok := nodeByPath(archived)["old-only.bin"]; err != nil || !ok || got.Kind != model.File {
		t.Fatalf("expired ancestor metadata was not retained: entries=%+v err=%v", archived, err)
	}
	for path, want := range map[string][]byte{"shared.bin": sharedPayload, "edit.bin": newEdit} {
		reader, _, err := f.service.OpenFile(f.ctx, tip.ID, path)
		if err != nil {
			t.Fatalf("open retained %s: %v", path, err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("retained %s bytes len=%d read=%v close=%v", path, len(got), readErr, closeErr)
		}
	}

	fullSafety := maintenance.SafetyFull
	// Clock steps track SafetyFull age and GC margins. One bounded cycle per
	// transition exposes snapshot GC, rewrite, index removal, and orphan cleanup.
	steps := []time.Duration{
		0,
		fullSafety.MinContentAgeSubjectToGC + fullSafety.MarginBetweenSnapshotGC + time.Hour,
		fullSafety.MarginBetweenSnapshotGC + time.Hour,
		fullSafety.MarginBetweenSnapshotGC + time.Hour,
		fullSafety.MarginBetweenSnapshotGC + time.Hour,
		fullSafety.PackDeleteMinAge + fullSafety.MinRewriteToOrphanDeletionDelay + time.Hour,
	}
	var last CollectionResult
	for i, step := range steps {
		if step > 0 {
			f.clock.offset.Add(int64(step))
		}
		last, err = f.service.Collect(f.ctx, CollectRequest{MaxRewriteBytes: 1 << 30})
		if err != nil {
			t.Fatalf("collect cycle %d: %v", i+1, err)
		}
		t.Logf("collect cycle %d: source=%s rewritten=%d source-gone=%t deferred=%q blobs=%d", i+1, last.SourcePack, last.RewrittenCount, last.SourceGone, last.DeferredReason, last.After.BlobBytes)
	}
	t.Logf("physical collection stats: before=%d after=%d pack=%d indexed-live=%d", before.BlobBytes, last.After.BlobBytes, last.After.PackBytes, last.After.IndexedLiveBytes)
	if reclaimed := before.BlobBytes - last.After.BlobBytes; reclaimed < int64(len(oldPayload))/2 {
		t.Fatalf("physical reclaim=%d, want substantial reclaim from %d-byte unique payload; stats before=%+v after=%+v", reclaimed, len(oldPayload), before, last.After)
	}
	oldPath, err := model.ParsePath("old-only.bin")
	if err != nil {
		t.Fatal(err)
	}
	if reader, _, err := f.service.objects.OpenFile(f.ctx, baseArchive, oldPath); reader != nil {
		_ = reader.Close()
		t.Fatal("expired unique payload remained readable from its original native archive")
	} else if !errors.Is(err, object.ErrObjectNotFound) && !errors.Is(err, content.ErrContentNotFound) {
		t.Fatalf("original native archive read error = %v, want native not-found", err)
	}
	for path, want := range map[string][]byte{"shared.bin": sharedPayload, "edit.bin": newEdit} {
		reader, _, err := f.service.OpenFile(f.ctx, tip.ID, path)
		if err != nil {
			t.Fatalf("open retained %s after collection: %v", path, err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("retained %s after collection bytes len=%d read=%v close=%v", path, len(got), readErr, closeErr)
		}
	}
	if head, err := f.service.Head(f.ctx, "main"); err != nil || head != tip.ID {
		t.Fatalf("branch tip after collection = %q, %v; want %q", head, err, tip.ID)
	}
	if tag, err := f.service.Tag(f.ctx, "keep-tip"); err != nil || tag.CommitID != tip.ID {
		t.Fatalf("tag after collection = %+v, %v; want protected tip", tag, err)
	}
	archived, _, err = f.service.Entries(f.ctx, base.ID, []SelectionRule{{Included: true}}, "", 100)
	if got, ok := nodeByPath(archived)["old-only.bin"]; err != nil || !ok || got.Kind != model.File {
		t.Fatalf("expired ancestor directory metadata after collection = %+v err=%v", archived, err)
	}
	if _, err := f.service.ExplainRetention(f.ctx, tip.ID); err != nil {
		t.Fatalf("tagged tip lost retention metadata: %v", err)
	}
	if record, found, err := f.metadata.compaction(f.ctx); err != nil || found {
		t.Fatalf("completed compaction checkpoint = %+v, found=%t, err=%v; want cleared", record, found, err)
	}
}

func TestCollectionCrashSubprocessHelper(t *testing.T) {
	mode := os.Getenv("LODE_COLLECTION_CRASH_MODE")
	if mode == "" {
		return
	}
	base, err := time.Parse(time.RFC3339Nano, os.Getenv("LODE_COLLECTION_CLOCK_BASE"))
	if err != nil {
		t.Fatal(err)
	}
	offset, err := strconv.ParseInt(os.Getenv("LODE_COLLECTION_CLOCK_OFFSET"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	clock := &collectionClock{base: base}
	clock.offset.Store(offset)
	fixture := openCollectionIntegrationFixture(t, os.Getenv("LODE_COLLECTION_DIR"), os.Getenv("LODE_COLLECTION_CONFIG"), clock, mode)
	if _, err := fixture.service.Collect(fixture.ctx, CollectRequest{MaxRewriteBytes: 1 << 30}); err != nil {
		t.Fatalf("subprocess Collect before requested crash point: %v", err)
	}
	t.Fatalf("subprocess collection returned without hitting crash point %q", mode)
}

func TestCollectionAdvancesToAnotherPackBeforePhysicalDeletion(t *testing.T) {
	f := newCollectionIntegrationFixture(t)
	oldA, oldB := collectionPayload(4<<20, 401), collectionPayload(4<<20, 402)
	sharedA, sharedB := collectionPayload(128<<10, 403), collectionPayload(128<<10, 404)
	first := f.apply("batch-first", "", "first pack", map[string][]byte{"old-a.bin": oldA, "shared-a.bin": sharedA})
	second := f.apply("batch-second", first.ID, "second pack", map[string][]byte{"old-b.bin": oldB, "shared-b.bin": sharedB})
	tip := f.apply("batch-tip", second.ID, "keep shared files", nil, "old-a.bin", "old-b.bin")
	if err := f.service.SetRetentionPolicy(f.ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	f.clock.offset.Add(int64(48 * time.Hour))
	if result, err := f.service.Expire(f.ctx, []string{first.ID, second.ID}); err != nil || len(result.Expired) != 2 {
		t.Fatalf("expire obsolete pack payloads: %+v, %v", result, err)
	}
	firstRun, err := f.service.Collect(f.ctx, CollectRequest{})
	if err != nil || firstRun.SourcePack == "" || firstRun.RewrittenCount == 0 || firstRun.SourceGone {
		t.Fatalf("first bounded compaction should await native deletion: %+v, %v", firstRun, err)
	}
	if _, exists, err := f.metadata.compaction(f.ctx); err != nil || exists {
		t.Fatalf("completed first copy still blocks another pack: exists=%t, %v", exists, err)
	}
	// One second merely advances the fixture's fixed timestamps. Native pack
	// deletion, rewrite, and two-GC safety delays remain hours or days long.
	f.clock.offset.Add(int64(time.Second))
	secondRun, err := f.service.Collect(f.ctx, CollectRequest{})
	if err != nil || secondRun.SourcePack == "" || secondRun.SourcePack == firstRun.SourcePack || secondRun.RewrittenCount == 0 || secondRun.SourceGone {
		t.Fatalf("next pack could not proceed before first physical deletion: first=%+v second=%+v error=%v", firstRun, secondRun, err)
	}
	if _, exists, err := f.metadata.compaction(f.ctx); err != nil || exists {
		t.Fatalf("completed second copy left a retry checkpoint: exists=%t, %v", exists, err)
	}
	for range 5 {
		f.clock.offset.Add(int64(maintenance.SafetyFull.MarginBetweenSnapshotGC + time.Hour))
		if _, err := f.service.Collect(f.ctx, CollectRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{firstRun.SourcePack, secondRun.SourcePack} {
		if _, err := f.repo.BlobReader().GetMetadata(f.ctx, blob.ID(source)); !errors.Is(err, blob.ErrBlobNotFound) {
			t.Fatalf("native GC did not reclaim completed source %s: %v", source, err)
		}
	}
	for path, want := range map[string][]byte{"shared-a.bin": sharedA, "shared-b.bin": sharedB} {
		reader, _, err := f.service.OpenFile(f.ctx, tip.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("retained %s after multiple pack collection: read=%v close=%v equal=%t", path, readErr, closeErr, bytes.Equal(got, want))
		}
	}
}

func TestCollectionRestartRecoversBothRewriteCheckpointBoundaries(t *testing.T) {
	for _, test := range []struct {
		name                    string
		crashMode               string
		wantLiveAtSourceOnCrash bool
		wantRecoveryRewrite     bool
		exitCode                int
	}{
		{name: "checkpoint before rewrite", crashMode: "before-rewrite", wantLiveAtSourceOnCrash: true, wantRecoveryRewrite: true, exitCode: 86},
		{name: "native rewrite before SQL acknowledgement", crashMode: "after-rewrite", wantLiveAtSourceOnCrash: false, wantRecoveryRewrite: false, exitCode: 87},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCollectionIntegrationFixture(t)
			oldPayload := collectionPayload(8<<20, 301)
			sharedPayload := collectionPayload(256<<10, 302)
			oldEdit, newEdit := []byte("old edit"), []byte("new edit")
			if err := fixture.service.SetCapacityPolicy(fixture.ctx, CapacityPolicy{ReserveBytes: 1 << 20}); err != nil {
				t.Fatal(err)
			}
			base := fixture.apply("crash-collection-base", "", "base", map[string][]byte{
				"old-only.bin": oldPayload,
				"shared.bin":   sharedPayload,
				"edit.bin":     oldEdit,
			})
			baseCommit, err := fixture.metadata.commit(fixture.ctx, base.ID)
			if err != nil {
				t.Fatal(err)
			}
			baseArchive, err := fixture.service.archive(fixture.ctx, baseCommit.Tree)
			if err != nil {
				t.Fatal(err)
			}
			tip := fixture.apply("crash-collection-tip", base.ID, "tip", map[string][]byte{"edit.bin": newEdit}, "old-only.bin")
			if err := fixture.service.CreateTag(fixture.ctx, "keep-tip", tip.ID); err != nil {
				t.Fatal(err)
			}
			if err := fixture.service.SetRetentionPolicy(fixture.ctx, RetentionPolicy{}); err != nil {
				t.Fatal(err)
			}
			fixture.clock.offset.Add(int64(48 * time.Hour))
			if expired, err := fixture.service.Expire(fixture.ctx, []string{base.ID}); err != nil || len(expired.Expired) != 1 {
				t.Fatalf("expire base before crash: %+v, %v", expired, err)
			}
			before, err := fixture.service.StorageStats(fixture.ctx)
			if err != nil {
				t.Fatal(err)
			}
			dir, config := fixture.dir, fixture.config
			baseClock := fixture.clock.base.Format(time.RFC3339Nano)
			offset := strconv.FormatInt(fixture.clock.offset.Load(), 10)
			fixture.close()

			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(binary, "-test.run=^TestCollectionCrashSubprocessHelper$")
			command.Env = append(os.Environ(),
				"LODE_COLLECTION_CRASH_MODE="+test.crashMode,
				"LODE_COLLECTION_CLOCK_BASE="+baseClock,
				"LODE_COLLECTION_CLOCK_OFFSET="+offset,
				"LODE_COLLECTION_DIR="+dir,
				"LODE_COLLECTION_CONFIG="+config,
			)
			output, runErr := command.CombinedOutput()
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != test.exitCode {
				t.Fatalf("crash subprocess error=%v exit=%v output=%s; want exit %d at %s", runErr, exitCode(runErr), output, test.exitCode, test.crashMode)
			}

			clock := &collectionClock{base: fixtureClockBase(t, baseClock)}
			clock.offset.Store(fixture.clock.offset.Load())
			reopened := openCollectionIntegrationFixture(t, dir, config, clock, "")
			record, found, err := reopened.metadata.compaction(reopened.ctx)
			if err != nil || !found || record.SourcePack == "" || !record.RewrittenAt.IsZero() {
				t.Fatalf("compaction checkpoint after %s crash = %+v, found=%t, err=%v", test.crashMode, record, found, err)
			}
			liveAtSource, err := liveContentsInPack(reopened.ctx, reopened.repo, record.SourcePack)
			if err != nil {
				t.Fatal(err)
			}
			if (liveAtSource > 0) != test.wantLiveAtSourceOnCrash {
				t.Fatalf("source %s has %d live contents after crash; want live=%t", record.SourcePack, liveAtSource, test.wantLiveAtSourceOnCrash)
			}

			recovery, err := reopened.service.Collect(reopened.ctx, CollectRequest{MaxRewriteBytes: 1 << 30})
			if err != nil {
				t.Fatalf("recover source %s: %v", record.SourcePack, err)
			}
			if (recovery.RewrittenCount > 0) != test.wantRecoveryRewrite {
				t.Fatalf("recovery rewrote %d contents for %s boundary; want rewrite=%t (source=%s)", recovery.RewrittenCount, test.crashMode, test.wantRecoveryRewrite, recovery.SourcePack)
			}
			if recovery.SourcePack != record.SourcePack {
				t.Fatalf("recovery source %q, want persisted source %q", recovery.SourcePack, record.SourcePack)
			}
			if live, err := liveContentsInPack(reopened.ctx, reopened.repo, record.SourcePack); err != nil || live != 0 {
				t.Fatalf("live source records after recovery = %d, %v; want zero", live, err)
			}

			fullSafety := maintenance.SafetyFull
			for _, step := range []time.Duration{
				fullSafety.MarginBetweenSnapshotGC + time.Hour,
				fullSafety.MarginBetweenSnapshotGC + time.Hour,
				fullSafety.MarginBetweenSnapshotGC + time.Hour,
				fullSafety.PackDeleteMinAge + fullSafety.MinRewriteToOrphanDeletionDelay + time.Hour,
			} {
				clock.offset.Add(int64(step))
				if _, err := reopened.service.Collect(reopened.ctx, CollectRequest{MaxRewriteBytes: 1 << 30}); err != nil {
					t.Fatalf("finish native safety collection for %s: %v", test.crashMode, err)
				}
			}
			if _, pending, err := reopened.metadata.compaction(reopened.ctx); err != nil || pending {
				t.Fatalf("compaction checkpoint after recovery = pending %t, err %v; want cleared", pending, err)
			}
			after, err := reopened.service.StorageStats(reopened.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if reclaimed := before.BlobBytes - after.BlobBytes; reclaimed < int64(len(oldPayload))/2 {
				t.Fatalf("reclaimed %d physical bytes after %s recovery, want >%d", reclaimed, test.crashMode, len(oldPayload)/2)
			}
			for path, want := range map[string][]byte{"shared.bin": sharedPayload, "edit.bin": newEdit} {
				reader, _, err := reopened.service.OpenFile(reopened.ctx, tip.ID, path)
				if err != nil {
					t.Fatalf("open retained %s after recovery: %v", path, err)
				}
				got, readErr := io.ReadAll(reader)
				closeErr := reader.Close()
				if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("retained %s after recovery bytes len=%d read=%v close=%v", path, len(got), readErr, closeErr)
				}
			}
			oldPath, err := model.ParsePath("old-only.bin")
			if err != nil {
				t.Fatal(err)
			}
			if reader, _, err := reopened.service.objects.OpenFile(reopened.ctx, baseArchive, oldPath); reader != nil {
				_ = reader.Close()
				t.Fatal("expired unique payload remained readable from original archive after recovery")
			} else if !errors.Is(err, object.ErrObjectNotFound) && !errors.Is(err, content.ErrContentNotFound) {
				t.Fatalf("original native archive read error after recovery = %v", err)
			}
		})
	}
}

func liveContentsInPack(ctx context.Context, repository repo.DirectRepository, source string) (int, error) {
	count := 0
	err := repository.ContentReader().IterateContents(ctx, content.IterateOptions{IncludeDeleted: true}, func(info content.Info) error {
		if !info.Deleted && string(info.PackBlobID) == source {
			count++
		}
		return nil
	})
	return count, err
}

func fixtureClockBase(t *testing.T, value string) time.Time {
	t.Helper()
	base, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func exitCode(err error) int {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 0
}
