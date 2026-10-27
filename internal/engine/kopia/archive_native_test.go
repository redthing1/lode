package kopia_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

type archiveClockKey struct{}

type archiveClock struct{ offset atomic.Int64 }

func (c *archiveClock) now() time.Time { return time.Now().UTC().Add(time.Duration(c.offset.Load())) }

type archiveStorageOptions struct {
	Path string `json:"path"`
}

type archiveClockStorage struct {
	blob.Storage
	clock   *archiveClock
	options archiveStorageOptions
}

func init() {
	blob.AddSupportedStorage("lode-metadata-proof-clock", archiveStorageOptions{}, func(ctx context.Context, opt *archiveStorageOptions, create bool) (blob.Storage, error) {
		clock, ok := ctx.Value(archiveClockKey{}).(*archiveClock)
		if !ok {
			return nil, errors.New("metadata proof clock missing")
		}
		st, err := filesystem.New(ctx, &filesystem.Options{Path: opt.Path}, create)
		if err != nil {
			return nil, err
		}
		return &archiveClockStorage{Storage: st, clock: clock, options: *opt}, nil
	})
}

func (s *archiveClockStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: "lode-metadata-proof-clock", Config: &s.options}
}

func (s *archiveClockStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if opts.SetModTime.IsZero() {
		opts.SetModTime = s.clock.now()
	}
	return s.Storage.PutBlob(ctx, id, data, opts)
}

type archiveFixture struct {
	t      *testing.T
	ctx    context.Context
	dir    string
	clock  *archiveClock
	rep    repo.DirectRepository
	config string
}

type payloadDenyingRepository struct {
	repo.Repository
	denied map[string]bool
}

func (r payloadDenyingRepository) OpenObject(ctx context.Context, id object.ID) (object.Reader, error) {
	if r.denied[id.String()] {
		return nil, fmt.Errorf("file payload %q must not be opened by metadata export", id.String())
	}
	return r.Repository.OpenObject(ctx, id)
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	c := &archiveClock{}
	f := &archiveFixture{t: t, dir: t.TempDir(), clock: c}
	f.ctx = context.WithValue(t.Context(), archiveClockKey{}, c)
	st, err := blob.NewStorage(f.ctx, blob.ConnectionInfo{Type: "lode-metadata-proof-clock", Config: &archiveStorageOptions{Path: filepath.Join(f.dir, "repository")}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Initialize(f.ctx, st, nil, "temporary-metadata-proof-password"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.config = f.connect("writer")
	r, err := repo.Open(f.ctx, f.config, "temporary-metadata-proof-password", &repo.Options{TimeNowFunc: c.now})
	if err != nil {
		t.Fatal(err)
	}
	f.rep = r.(repo.DirectRepository)
	t.Cleanup(func() {
		if err := f.rep.Close(context.WithoutCancel(f.ctx)); err != nil {
			t.Error(err)
		}
	})
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		params := maintenance.DefaultParams()
		params.Owner = w.ClientOptions().UsernameAtHost()
		params.QuickCycle.Enabled, params.FullCycle.Enabled = false, false
		return maintenance.SetParams(ctx, w, &params)
	})
	return f
}

func (f *archiveFixture) connect(name string) string {
	f.t.Helper()
	st, err := blob.NewStorage(f.ctx, blob.ConnectionInfo{Type: "lode-metadata-proof-clock", Config: &archiveStorageOptions{Path: filepath.Join(f.dir, "repository")}}, false)
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close(f.ctx)
	config := filepath.Join(f.dir, name+".config")
	if err := repo.Connect(f.ctx, config, st, "temporary-metadata-proof-password", &repo.ConnectOptions{
		ClientOptions:  repo.ClientOptions{Username: "lode", Hostname: name},
		CachingOptions: content.CachingOptions{CacheDirectory: filepath.Join(f.dir, name+"-cache"), ContentCacheSizeBytes: 1 << 20, MetadataCacheSizeBytes: 1 << 20},
	}); err != nil {
		f.t.Fatal(err)
	}
	return config
}

func (f *archiveFixture) reader(name string) repo.Repository {
	f.t.Helper()
	r, err := repo.Open(f.ctx, f.connect(name), "temporary-metadata-proof-password", &repo.Options{TimeNowFunc: f.clock.now})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *archiveFixture) write(fn func(context.Context, repo.RepositoryWriter) error) {
	f.t.Helper()
	if err := repo.WriteSession(f.ctx, f.rep, repo.WriteSessionOptions{Purpose: "metadata archive proof"}, fn); err != nil {
		f.t.Fatal(err)
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *archiveFixture) saveRoot(w repo.RepositoryWriter, root *snapshot.DirEntry, operation string) (manifest.ID, error) {
	now := fs.UTCTimestampFromTime(w.Time())
	return snapshot.SaveSnapshot(f.ctx, w, &snapshot.Manifest{Source: snapshot.SourceInfo{Host: "lode", UserName: "lode", Path: "/projects"}, StartTime: now, EndTime: now, RootEntry: root.Clone(), Tags: map[string]string{"lode-proof": operation}})
}

func archivePayload(size int, seed int64) []byte {
	result := make([]byte, size)
	_, _ = rand.New(rand.NewSource(seed)).Read(result)
	return result
}

func captureProofFile(ctx context.Context, w repo.RepositoryWriter, name string, data []byte) (*snapshot.DirEntry, error) {
	writer := w.NewObjectWriter(ctx, object.WriterOptions{Compressor: "zstd-fastest", MetadataCompressor: "zstd-fastest"})
	if _, err := io.Copy(writer, bytes.NewReader(data)); err != nil {
		_ = writer.Close()
		return nil, err
	}
	id, err := writer.Result()
	closeErr := writer.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return &snapshot.DirEntry{Name: name, Type: snapshot.EntryTypeFile, Permissions: 0o644, FileSize: int64(len(data)), ModTime: fs.UTCTimestampFromTime(w.Time()), ObjectID: id}, nil
}

func writeProofDirectory(ctx context.Context, w repo.RepositoryWriter, name string, entries ...*snapshot.DirEntry) (*snapshot.DirEntry, error) {
	var builder snapshotfs.DirManifestBuilder
	for _, entry := range entries {
		builder.AddEntry(entry)
	}
	dir := builder.Build(fs.UTCTimestampFromTime(w.Time()), "")
	id, err := snapshotfs.WriteDirManifest(ctx, w, name, dir, compression.Name("zstd-fastest"))
	if err != nil {
		return nil, err
	}
	return &snapshot.DirEntry{Name: name, Type: snapshot.EntryTypeDirectory, Permissions: 0o755, ModTime: fs.UTCTimestampFromTime(w.Time()), ObjectID: id, DirSummary: dir.Summary}, nil
}

func proofPath(t *testing.T, value string) model.Path {
	t.Helper()
	p, err := model.ParsePath(value)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func readProofObject(ctx context.Context, r repo.Repository, id object.ID) ([]byte, error) {
	reader, err := r.OpenObject(ctx, id)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func TestArchivedMetadataSurvivesNativePayloadCollectionAndMergesRetainedDescendants(t *testing.T) {
	f := newArchiveFixture(t)
	baseA, baseB, obsolete := archivePayload(256<<10, 1), archivePayload(256<<10, 2), archivePayload(8<<20, 3)
	localA, targetB := archivePayload(256<<10, 4), archivePayload(256<<10, 5)
	var baseRoot, localRoot, targetRoot *snapshot.DirEntry
	var oldOnly *snapshot.DirEntry
	deniedPayloads := make(map[string]bool)
	var baseID manifest.ID
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		a, err := captureProofFile(ctx, w, "a.bin", baseA)
		if err != nil {
			return err
		}
		deniedPayloads[a.ObjectID.String()] = true
		b, err := captureProofFile(ctx, w, "b.bin", baseB)
		if err != nil {
			return err
		}
		deniedPayloads[b.ObjectID.String()] = true
		oldOnly, err = captureProofFile(ctx, w, "obsolete.bin", obsolete)
		if err != nil {
			return err
		}
		deniedPayloads[oldOnly.ObjectID.String()] = true
		child, err := writeProofDirectory(ctx, w, "dir", a, b)
		if err != nil {
			return err
		}
		baseRoot, err = writeProofDirectory(ctx, w, "", child, oldOnly)
		if err != nil {
			return err
		}
		baseID, err = f.saveRoot(w, baseRoot, "base")
		return err
	})
	archived, err := lodekopia.ExportTree(f.ctx, payloadDenyingRepository{Repository: f.rep, denied: deniedPayloads}, baseRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Persist the ancestor before releasing its native protection. Reload from
	// a newly opened SQLite connection after collection, without the source archive.
	databasePath := filepath.Join(f.dir, "metadata.sqlite")
	metadata, err := project.OpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveID, err := metadata.Import(f.ctx, "native-fixture", archived.Format, archived.Root, archived.Directories)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	archiveFormat := archived.Format
	metadataBytes := len(archived.Root)
	for _, raw := range archived.Directories {
		metadataBytes += len(raw)
	}
	t.Logf("ancestor metadata: %d directory records, %d bytes", len(archived.Directories), metadataBytes)
	baseMetadata, err := lodekopia.OpenArchive("native-fixture", archived)
	if err != nil {
		t.Fatal(err)
	}
	if got := baseMetadata.RootDescriptor(); got == nil || got.Permissions != baseRoot.Permissions || got.ObjectID != baseRoot.ObjectID {
		t.Fatalf("root descriptor attributes were not preserved: %+v", got)
	}
	otherNamespace, err := lodekopia.OpenArchive("independent-repository", archived)
	if err != nil {
		t.Fatal(err)
	}
	firstRef, err := baseMetadata.Lookup(proofPath(t, "dir/a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	otherRef, err := otherNamespace.Lookup(proofPath(t, "dir/a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if firstRef.ContentID == otherRef.ContentID {
		t.Fatal("same native object ID from independent repositories compared equal")
	}
	loaded, err := lodekopia.LoadArchive(f.ctx, archived.Format, archived.Root, func(_ context.Context, id string) ([]byte, error) {
		data, ok := archived.Directories[id]
		if !ok {
			return nil, fmt.Errorf("missing raw record %q", id)
		}
		return data, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := lodekopia.LoadArchive(canceled, archived.Format, archived.Root, func(_ context.Context, id string) ([]byte, error) {
		return archived.Directories[id], nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled archive load returned %v", err)
	}
	loadedTree, err := lodekopia.OpenArchive("native-fixture", loaded)
	if err != nil {
		t.Fatal(err)
	}
	// Both constructors own their validated bytes; mutation of the source
	// archive cannot change a previously opened view.
	archived.Root[0] ^= 0xff
	for _, raw := range archived.Directories {
		raw[0] ^= 0xff
	}
	if got, err := loadedTree.Lookup(proofPath(t, "dir/a.bin")); err != nil || got.Kind != model.File {
		t.Fatalf("loaded archive changed after source mutation: %+v, %v", got, err)
	}
	if got, err := baseMetadata.Lookup(proofPath(t, "obsolete.bin")); err != nil || got.Kind != model.File {
		t.Fatalf("base metadata lookup before release = %+v, %v", got, err)
	}
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		a, err := captureProofFile(ctx, w, "a.bin", localA)
		if err != nil {
			return err
		}
		b, err := captureProofFile(ctx, w, "b.bin", baseB)
		if err != nil {
			return err
		}
		child, err := writeProofDirectory(ctx, w, "dir", a, b)
		if err != nil {
			return err
		}
		localRoot, err = writeProofDirectory(ctx, w, "", child)
		if err != nil {
			return err
		}
		_, err = f.saveRoot(w, localRoot, "local")
		if err != nil {
			return err
		}
		a, err = captureProofFile(ctx, w, "a.bin", baseA)
		if err != nil {
			return err
		}
		b, err = captureProofFile(ctx, w, "b.bin", targetB)
		if err != nil {
			return err
		}
		child, err = writeProofDirectory(ctx, w, "dir", a, b)
		if err != nil {
			return err
		}
		targetRoot, err = writeProofDirectory(ctx, w, "", child)
		if err != nil {
			return err
		}
		_, err = f.saveRoot(w, targetRoot, "target")
		return err
	})
	bytesBefore := storedProofBytes(t, f)
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error { return w.DeleteManifest(ctx, baseID) })
	collect := func(label string) {
		t.Helper()
		err := repo.DirectWriteSession(f.ctx, f.rep, repo.WriteSessionOptions{Purpose: "metadata proof collection: " + label}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
			return snapshotmaintenance.Run(ctx, w, maintenance.ModeFull, false, maintenance.SafetyFull)
		})
		if err != nil {
			t.Fatalf("full-safety collection %s: %v", label, err)
		}
		if err := f.rep.Refresh(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	advanceSafetyClock := func(d time.Duration) {
		t.Helper()
		f.clock.offset.Add(int64(d))
	}
	safety := maintenance.SafetyFull
	// Five full cycles expose each native retention boundary: initial GC;
	// stale-content age plus a second GC; then three GC-margin intervals for
	// rewrite, deleted-index removal, and orphan-pack deletion. All waits derive
	// from SafetyFull rather than using an arbitrary long retry loop.
	collect("initial snapshot GC")
	advanceSafetyClock(safety.MinContentAgeSubjectToGC + safety.MarginBetweenSnapshotGC + time.Hour)
	collect("mark expired payload")
	for phase := range 3 {
		advanceSafetyClock(safety.MarginBetweenSnapshotGC + time.Hour)
		collect(fmt.Sprintf("rewrite and remove expired packs, phase %d", phase+1))
	}

	packID, contentIDs, physicalBytes, indexedBytes, err := largestMixedPack(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if physicalBytes-indexedBytes < int64(len(obsolete))/2 {
		t.Fatalf("fixture did not preserve the single mixed-pack case: pack=%s physical=%d indexed=%d", packID, physicalBytes, indexedBytes)
	}
	// This assertion preserves the unfavorable single-pack observation: full
	// SafetyFull runs can drop deleted indexes while leaving their physical
	// bytes in a lone short pack. Kopia's automatic short-pack rewrite skips the
	// first pack per prefix, so full runs alone do not fix this layout.
	t.Logf("SafetyFull left a single mixed pack: %s physical=%d indexed-live=%d; explicitly rewriting %d live contents", packID, physicalBytes, indexedBytes, len(contentIDs))
	// The explicit native rewrite is a separate, supported maintenance action;
	// SafetyFull enforces rewrite age here, and a later full-safety run applies
	// Kopia's normal orphan deletion delays. This does not imply automatic
	// full-maintenance scheduling compacts every mixed pack.
	var rewritten uint64
	err = repo.DirectWriteSession(f.ctx, f.rep, repo.WriteSessionOptions{Purpose: "targeted native compaction proof"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		stats, err := maintenance.RewriteContents(ctx, w, &maintenance.RewriteContentsOptions{ContentIDs: contentIDs, Parallel: 1}, maintenance.SafetyFull)
		if err != nil {
			return err
		}
		rewritten = stats.RewrittenContentCount
		return nil
	})
	if err != nil {
		t.Fatalf("native targeted rewrite of live mixed-pack contents: %v", err)
	}
	if rewritten == 0 {
		t.Fatal("native SafetyFull rewrite skipped every live content in the mixed pack")
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	advanceSafetyClock(safety.PackDeleteMinAge + safety.MinRewriteToOrphanDeletionDelay + time.Hour)
	collect("delete orphaned pre-compaction mixed pack")
	bytesAfter := storedProofBytes(t, f)
	t.Logf("native store before collection: %d bytes; after: %d bytes; reduction: %d bytes", bytesBefore, bytesAfter, bytesBefore-bytesAfter)
	if bytesAfter >= bytesBefore {
		t.Fatal("collection did not reclaim physical storage")
	}
	newReader := f.reader("after-collection")
	defer newReader.Close(context.WithoutCancel(f.ctx))
	if _, err := readProofObject(f.ctx, newReader, oldOnly.ObjectID); !errors.Is(err, object.ErrObjectNotFound) && !errors.Is(err, content.ErrContentNotFound) {
		t.Fatalf("released base payload read error = %v, want native not-found", err)
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
		if err != nil {
			t.Fatalf("read retained descendant %s: %v", check.path, err)
		}
		if !bytes.Equal(got, check.want) {
			t.Fatalf("retained descendant %s bytes differ from expected", check.path)
		}
	}
	metadata, err = project.OpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	rootBytes, err := metadata.Root(f.ctx, "native-fixture", archiveFormat, archiveID)
	if err != nil {
		t.Fatal(err)
	}
	reopenedArchive, err := lodekopia.LoadArchive(f.ctx, archiveFormat, rootBytes, func(ctx context.Context, id string) ([]byte, error) {
		return metadata.Directory(ctx, "native-fixture", archiveFormat, id)
	})
	if err != nil {
		t.Fatal(err)
	}
	baseMetadata, err = lodekopia.OpenArchive("native-fixture", reopenedArchive)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := baseMetadata.Lookup(proofPath(t, "obsolete.bin")); err != nil || got.Kind != model.File {
		t.Fatalf("archived base metadata after collection = %+v, %v", got, err)
	}
	baseAEntry, err := baseMetadata.Lookup(proofPath(t, "dir/a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	baseBEntry, err := baseMetadata.Lookup(proofPath(t, "dir/b.bin"))
	if err != nil {
		t.Fatal(err)
	}
	localArchive, err := lodekopia.ExportTree(f.ctx, newReader, localRoot)
	if err != nil {
		t.Fatal(err)
	}
	targetArchive, err := lodekopia.ExportTree(f.ctx, newReader, targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	localTree, err := lodekopia.OpenArchive("native-fixture", localArchive)
	if err != nil {
		t.Fatal(err)
	}
	targetTree, err := lodekopia.OpenArchive("native-fixture", targetArchive)
	if err != nil {
		t.Fatal(err)
	}
	localAEntry, err := localTree.Lookup(proofPath(t, "dir/a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	targetBEntry, err := targetTree.Lookup(proofPath(t, "dir/b.bin"))
	if err != nil {
		t.Fatal(err)
	}
	localBEntry, err := localTree.Lookup(proofPath(t, "dir/b.bin"))
	if err != nil {
		t.Fatal(err)
	}
	targetAEntry, err := targetTree.Lookup(proofPath(t, "dir/a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	mergedA, err := model.Reconcile(baseAEntry, localAEntry, targetAEntry)
	if err != nil {
		t.Fatal(err)
	}
	mergedB, err := model.Reconcile(baseBEntry, localBEntry, targetBEntry)
	if err != nil {
		t.Fatal(err)
	}
	if mergedA != localAEntry || mergedB != targetBEntry {
		t.Fatal("independent descendant edits did not reconcile")
	}
	var mergedRoot *snapshot.DirEntry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		a, err := fileEntryForObject(ctx, f.rep, localRoot, "dir/a.bin")
		if err != nil {
			return err
		}
		b, err := fileEntryForObject(ctx, f.rep, targetRoot, "dir/b.bin")
		if err != nil {
			return err
		}
		child, err := writeProofDirectory(ctx, w, "dir", a, b)
		if err != nil {
			return err
		}
		mergedRoot, err = writeProofDirectory(ctx, w, "", child)
		if err != nil {
			return err
		}
		_, err = f.saveRoot(w, mergedRoot, "merged")
		return err
	})
	mergedReader := f.reader("merged-independent")
	defer mergedReader.Close(context.WithoutCancel(f.ctx))
	mergedArchive, err := lodekopia.ExportTree(f.ctx, mergedReader, mergedRoot)
	if err != nil {
		t.Fatal(err)
	}
	mergedTree, err := lodekopia.OpenArchive("native-fixture", mergedArchive)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		path string
		want []byte
	}{{"dir/a.bin", localA}, {"dir/b.bin", targetB}} {
		entry, err := mergedTree.Lookup(proofPath(t, check.path))
		if err != nil {
			t.Fatal(err)
		}
		id, err := parseScopedObjectID(entry.ContentID, "native-fixture")
		if err != nil {
			t.Fatal(err)
		}
		nativeEntry, err := fileEntryForObject(f.ctx, mergedReader, mergedRoot, check.path)
		if err != nil {
			t.Fatal(err)
		}
		if nativeEntry.FileSize != int64(len(check.want)) || nativeEntry.ObjectID != id {
			t.Fatalf("merged %s metadata differs: %+v", check.path, nativeEntry)
		}
		got, err := readProofObject(f.ctx, mergedReader, id)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, check.want) {
			t.Fatalf("merged %s bytes differ from expected", check.path)
		}
	}
}

func fileEntryForObject(ctx context.Context, r repo.Repository, root *snapshot.DirEntry, path string) (*snapshot.DirEntry, error) {
	entry, err := snapshotfs.GetNestedEntry(ctx, snapshotfs.EntryFromDirEntry(r, root), strings.Split(path, "/"))
	if err != nil {
		return nil, err
	}
	native, ok := entry.(snapshot.HasDirEntry)
	if !ok {
		return nil, fmt.Errorf("entry %q lacks native metadata", path)
	}
	return native.DirEntry().Clone(), nil
}

func storedProofBytes(t *testing.T, f *archiveFixture) int64 {
	t.Helper()
	var total int64
	if err := f.rep.BlobReader().ListBlobs(f.ctx, "", func(m blob.Metadata) error {
		total += m.Length
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

func largestMixedPack(t *testing.T, f *archiveFixture) (blob.ID, []content.ID, int64, int64, error) {
	t.Helper()
	var bestID blob.ID
	var bestIDs []content.ID
	var bestPhysical, bestIndexed int64
	if err := f.rep.ContentReader().IteratePacks(f.ctx, content.IteratePackOptions{IncludePacksWithOnlyDeletedContent: true, IncludeContentInfos: true}, func(p content.PackInfo) error {
		metadata, err := f.rep.BlobReader().GetMetadata(f.ctx, p.PackID)
		if err != nil {
			return err
		}
		gap := metadata.Length - p.TotalSize
		if p.TotalSize == 0 || gap <= bestPhysical-bestIndexed {
			return nil
		}
		var ids []content.ID
		for _, info := range p.ContentInfos {
			if !info.Deleted {
				ids = append(ids, info.ContentID)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		bestID, bestPhysical, bestIndexed, bestIDs = p.PackID, metadata.Length, p.TotalSize, ids
		return nil
	}); err != nil {
		return "", nil, 0, 0, err
	}
	if bestID == "" {
		return "", nil, 0, 0, errors.New("no live pack with physical bytes beyond indexed contents")
	}
	return bestID, bestIDs, bestPhysical, bestIndexed, nil
}

func parseScopedObjectID(value, namespace string) (object.ID, error) {
	prefix := fmt.Sprintf("kopia:v1:%d:%s", len(namespace), namespace)
	if !strings.HasPrefix(value, prefix) {
		return object.EmptyID, fmt.Errorf("content ID is outside namespace %q", namespace)
	}
	return object.ParseID(strings.TrimPrefix(value, prefix))
}
