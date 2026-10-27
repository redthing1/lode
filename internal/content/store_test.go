package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"

	"github.com/redthing1/lode/internal/model"
)

func fileEntry(path string, data []byte) Entry {
	h := sha256.Sum256(data)
	return Entry{Entry: model.Entry{Path: path, Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}}
}
func source(data map[string][]byte) func(model.Entry, io.Writer) error {
	return func(entry model.Entry, out io.Writer) error {
		b, ok := data[entry.Path]
		if !ok {
			return os.ErrNotExist
		}
		_, err := io.Copy(out, bytes.NewReader(b))
		return err
	}
}

func TestCaptureReopenReuseAndRelease(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	password := "native-content-test-password"
	s, err := Open(ctx, dir, password, true)
	if err != nil {
		t.Fatal(err)
	}
	firstData := []byte("exact opaque bytes\x00\xff")
	first := fileEntry("assets/a.bin", firstData)
	root1, captured, err := s.Capture(ctx, "operation-one", []Entry{first}, source(map[string][]byte{"assets/a.bin": firstData}))
	if err != nil {
		t.Fatal(err)
	}
	if captured[0].Object == "" {
		t.Fatal("capture returned no native object reference")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(t.TempDir(), "moved-store")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("original path is unavailable"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir = moved
	s, err = Open(ctx, dir, password, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	cfg, err := repo.LoadConfigFromFile(filepath.Join(dir, "kopia.config"))
	if err != nil {
		t.Fatal(err)
	}
	storageCfg, ok := cfg.Storage.Config.(*filesystemConfig)
	if !ok || storageCfg.Path != filepath.Join(dir, "repository") {
		t.Fatalf("reopened repository path = %#v", cfg.Storage.Config)
	}
	cacheRel, err := filepath.Rel(dir, cfg.Caching.CacheDirectory)
	if err != nil || cacheRel == ".." || filepath.IsAbs(cacheRel) {
		t.Fatalf("cache path escaped store: %q", cfg.Caching.CacheDirectory)
	}
	if cfg.Caching.ContentCacheSizeBytes <= 0 || cfg.Caching.ContentCacheSizeLimitBytes < cfg.Caching.ContentCacheSizeBytes || cfg.Caching.MetadataCacheSizeBytes <= 0 || cfg.Caching.MetadataCacheSizeLimitBytes < cfg.Caching.MetadataCacheSizeBytes {
		t.Fatalf("unbounded or invalid cache sizes: %+v", cfg.Caching)
	}
	r, err := s.Read(ctx, captured[0].Object)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, firstData) {
		t.Fatalf("read bytes = %x, want %x", got, firstData)
	}

	secondData := []byte("second")
	second := fileEntry("data/b.bin", secondData)
	emptyData := []byte{}
	empty := fileEntry("empty.bin", emptyData)
	root2, combined, err := s.Capture(ctx, "operation-two", []Entry{captured[0], second, empty}, source(map[string][]byte{"data/b.bin": secondData, "empty.bin": emptyData}))
	if err != nil {
		t.Fatal(err)
	}
	if combined[0].Object != captured[0].Object {
		t.Fatal("unchanged native reference was not reused")
	}
	if combined[1].Object == "" {
		t.Fatal("changed file was not captured")
	}
	if root1 == root2 {
		t.Fatal("different complete trees got the same snapshot manifest")
	}
	root3, _, err := s.Capture(ctx, "operation-three", combined, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstManifest, err := snapshot.LoadSnapshot(ctx, s.repository, manifest.ID(root2))
	if err != nil {
		t.Fatal(err)
	}
	thirdManifest, err := snapshot.LoadSnapshot(ctx, s.repository, manifest.ID(root3))
	if err != nil {
		t.Fatal(err)
	}
	if firstManifest.RootObjectID() != thirdManifest.RootObjectID() {
		t.Fatalf("identical trees use different root directory objects: %s vs %s", firstManifest.RootObjectID(), thirdManifest.RootObjectID())
	}
	gotTree, err := readSnapshotTree(ctx, s, root2)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := map[string][]byte{"assets/a.bin": firstData, "data/b.bin": secondData, "empty.bin": emptyData}
	if len(gotTree) != len(wantTree) {
		t.Fatalf("native snapshot paths = %#v", gotTree)
	}
	for path, want := range wantTree {
		if !bytes.Equal(gotTree[path], want) {
			t.Errorf("native snapshot %s = %x, want %x", path, gotTree[path], want)
		}
	}

	roots, err := s.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots["operation-one"]) != 1 || roots["operation-one"][0] != root1 {
		t.Fatalf("roots for operation one: %#v", roots["operation-one"])
	}
	if err := s.Release(ctx, root1); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, root1); err != nil {
		t.Fatalf("release should be idempotent: %v", err)
	}
	roots, err = s.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots["operation-one"]) != 0 || len(roots["operation-two"]) != 1 || len(roots["operation-three"]) != 1 {
		t.Fatalf("roots after release: %#v", roots)
	}
}

func readSnapshotTree(ctx context.Context, s *Store, rootID string) (map[string][]byte, error) {
	man, err := snapshot.LoadSnapshot(ctx, s.repository, manifest.ID(rootID))
	if err != nil {
		return nil, err
	}
	entry := snapshotfs.EntryFromDirEntry(s.repository, man.RootEntry)
	dir, ok := entry.(fs.Directory)
	if !ok {
		return nil, fmt.Errorf("snapshot root is %T, not directory", entry)
	}
	contents := map[string][]byte{}
	var visit func(fs.Directory, string) error
	visit = func(d fs.Directory, prefix string) error {
		return fs.IterateEntries(ctx, d, func(ctx context.Context, e fs.Entry) error {
			defer e.Close()
			p := e.Name()
			if prefix != "" {
				p = prefix + "/" + p
			}
			switch v := e.(type) {
			case fs.Directory:
				return visit(v, p)
			case fs.File:
				r, err := v.Open(ctx)
				if err != nil {
					return err
				}
				data, readErr := io.ReadAll(r)
				closeErr := r.Close()
				if readErr != nil {
					return readErr
				}
				if closeErr != nil {
					return closeErr
				}
				contents[p] = data
				return nil
			default:
				return fmt.Errorf("unsupported native entry %T at %s", e, p)
			}
		})
	}
	return contents, visit(dir, "")
}

func TestCaptureRejectsIncorrectContentIdentity(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir(), "native-content-test-password", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	entry := fileEntry("file.bin", []byte("expected"))
	if _, _, err := s.Capture(ctx, "bad-operation", []Entry{entry}, source(map[string][]byte{"file.bin": []byte("wrong")})); err == nil {
		t.Fatal("capture accepted content with wrong size or hash")
	}
	roots, err := s.Roots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots["bad-operation"]) != 0 {
		t.Fatalf("mismatched capture created root: %#v", roots["bad-operation"])
	}
}

func TestOpenCreatesBaseDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s, err := Open(context.Background(), path, "native-content-test-password", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(path, "kopia.config")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("created config stat = %v, %v", st, err)
	}
}

func TestOpenWrongPasswordPreservesConfig(t *testing.T) {
	path := t.TempDir()
	s, err := Open(context.Background(), path, "correct-password", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), path, "incorrect-password", false); err == nil {
		t.Fatal("opened repository with incorrect password")
	}
	s, err = Open(context.Background(), path, "correct-password", false)
	if err != nil {
		t.Fatalf("correct password failed after rejected open: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenReconstructsMissingConfig(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	s, err := Open(ctx, path, "reconstruction-password", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "kopia.config")); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, "reconstruction-password", false)
	if err != nil {
		t.Fatalf("reconstruct config from existing repository: %v", err)
	}
	defer s.Close(ctx)
	if _, err := os.Stat(filepath.Join(path, "kopia.config")); err != nil {
		t.Fatalf("reconstructed config missing: %v", err)
	}
}

type byteBlob []byte

type readSeekCloser struct{ *bytes.Reader }

func (r readSeekCloser) Close() error { return nil }

func (b byteBlob) Length() int                        { return len(b) }
func (b byteBlob) Reader() io.ReadSeekCloser          { return readSeekCloser{bytes.NewReader(b)} }
func (b byteBlob) WriteTo(w io.Writer) (int64, error) { n, err := w.Write(b); return int64(n), err }

func TestFilesystemPutPropagatesDirectorySyncFailure(t *testing.T) {
	ctx := context.Background()
	store, err := newStorage(ctx, filepath.Join(t.TempDir(), "repository"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(ctx)
	durable := store.(*durableFilesystem)
	want := errors.New("injected directory sync failure")
	durable.syncDir = func(string) error { return want }
	err = durable.PutBlob(ctx, blob.ID("lode-test-blob"), byteBlob("data"), blob.PutOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("PutBlob error = %v, want sync failure", err)
	}
}
