package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	kcontent "github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/object"
	"github.com/redthing1/lode/internal/model"
)

func protectTestFile(t *testing.T, s *Store, name string, data []byte) (Object, string) {
	t.Helper()
	var object Object
	var protection string
	err := s.Write(context.Background(), func(w *Writer) error {
		var err error
		object, protection, err = writeProtectedFile(w, name, data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return object, protection
}

func writeProtectedFile(w *Writer, name string, data []byte) (Object, string, error) {
	hash := sha256.Sum256(data)
	entry := model.Entry{Path: name, Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	o, err := w.File(entry, func(out io.Writer) error { _, err := out.Write(data); return err })
	if err != nil {
		return Object{}, "", err
	}
	dir, err := w.Directory([]Entry{{Entry: entry, Object: o}})
	if err != nil {
		return Object{}, "", err
	}
	protection, err := w.Protect(name, dir)
	return o, protection, err
}

// A filesystem timestamp can lag the wall clock used for content timestamps.
// Only the native maintenance marker is affected; stored contents stay intact.
type coarseMaintenanceRepository struct {
	repo.DirectRepository
	timestamp time.Time
}

func (r coarseMaintenanceRepository) NewDirectWriter(ctx context.Context, opt repo.WriteSessionOptions) (context.Context, repo.DirectRepositoryWriter, error) {
	ctx, w, err := r.DirectRepository.NewDirectWriter(ctx, opt)
	if err != nil {
		return ctx, nil, err
	}
	return ctx, coarseMaintenanceWriter{w, r.timestamp}, nil
}

type coarseMaintenanceWriter struct {
	repo.DirectRepositoryWriter
	timestamp time.Time
}

func (w coarseMaintenanceWriter) BlobReader() blob.Reader {
	return coarseMaintenanceReader{w.DirectRepositoryWriter.BlobReader(), w.timestamp}
}

type coarseMaintenanceReader struct {
	blob.Reader
	timestamp time.Time
}

func (r coarseMaintenanceReader) GetMetadata(ctx context.Context, id blob.ID) (blob.Metadata, error) {
	m, err := r.Reader.GetMetadata(ctx, id)
	if id == "kopia.maintenance" && err == nil {
		m.Timestamp = r.timestamp
	}
	return m, err
}

func TestCollectionReclaimsMixedPacksAndKeepsLiveHandle(t *testing.T) {
	for _, coarse := range []bool{false, true} {
		name := "native-clock"
		if coarse {
			name = "coarse-filesystem-clock"
		}
		t.Run(name, func(t *testing.T) { testCollectionMixedPacks(t, coarse) })
	}
}

func testCollectionMixedPacks(t *testing.T, coarse bool) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "content")
	s, err := Open(ctx, root, "test-only", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close(ctx)
		}
	}()
	// Force native timestamps ahead of real time, as rapid rewrites/deletions
	// can do. Collection must settle them instead of missing recent garbage.
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s.repository, err = repo.Open(ctx, filepath.Join(root, "kopia.config"), "test-only", &repo.Options{DisableRepositoryLog: true, TimeNowFunc: func() time.Time { return time.Now().Add(2 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	live, expired := make([]byte, 4<<20), make([]byte, 32<<20)
	random := rand.New(rand.NewSource(27))
	_, _ = random.Read(live)
	_, _ = random.Read(expired)
	var kept, old Object
	var protection, released string
	err = s.Write(ctx, func(w *Writer) error {
		var err error
		kept, protection, err = writeProtectedFile(w, "keep", live)
		if err != nil {
			return err
		}
		old, released, err = writeProtectedFile(w, "old", expired)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Establish that this fixture requires compaction of a shared pack, not
	// merely deletion of separate packs containing only obsolete bytes.
	packs := map[string]bool{}
	mixed := false
	for n, o := range []Object{kept, old} {
		id, err := object.ParseID(o.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := s.repository.VerifyObject(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			info, err := s.repository.(repo.DirectRepository).ContentInfo(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				packs[string(info.PackBlobID)] = true
			} else if packs[string(info.PackBlobID)] {
				mixed = true
			}
		}
	}
	if !mixed {
		t.Fatal("fixture has no pack shared by retained and obsolete files")
	}
	if err := s.Release(ctx, []string{released}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s.repository, err = repo.Open(ctx, filepath.Join(root, "kopia.config"), "test-only", &repo.Options{DisableRepositoryLog: true})
	if err != nil {
		t.Fatal(err)
	}
	if coarse {
		direct := s.repository.(repo.DirectRepository)
		var latest time.Time
		err := direct.ContentReader().IterateContents(ctx, kcontent.IterateOptions{IncludeDeleted: true}, func(i kcontent.Info) error {
			if i.Timestamp().After(latest) {
				latest = i.Timestamp()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		s.repository = coarseMaintenanceRepository{direct, latest.Add(-time.Millisecond)}
	}
	cancelled, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := s.Collect(cancelled); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("collection ignored cancelled timestamp wait: %v", err)
	}
	result, err := s.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("collection: %+v", result)
	if result.After.Allocated >= 8<<20 || result.Before.Allocated-result.After.Allocated < 24<<20 {
		t.Fatalf("obsolete payload not reclaimed: %+v", result)
	}
	verify := func() {
		t.Helper()
		r, err := s.Read(ctx, kept.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, live) {
			t.Fatalf("retained bytes: %v", err)
		}
		r, err = s.Read(ctx, old.ID)
		if err == nil {
			_, err = io.Copy(io.Discard, r)
			r.Close()
		}
		if err == nil {
			t.Fatal("expired-only native object remains readable")
		}
		roots, err := s.Roots(ctx)
		if err != nil || len(roots) != 1 || roots[protection].Operation != "keep" {
			t.Fatalf("roots: %v %v", roots, err)
		}
	}
	verify()
	if again, err := s.Collect(ctx); err != nil || again.After.Allocated > result.After.Allocated+(1<<20) {
		t.Fatalf("repeat collection: %+v %v", again, err)
	}
	verify()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, root, "test-only", false)
	if err != nil {
		t.Fatal(err)
	}
	verify()
	// The same connection must remain usable for publication after maintenance.
	_, _ = protectTestFile(t, s, "later", []byte("new saved contents"))
	if roots, err := s.Roots(ctx); err != nil || len(roots) != 2 {
		t.Fatalf("later write: %v %v", roots, err)
	}
}

func TestCollectionRecoversAfterPartialNativeMaintenance(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		name := "write failure"
		if cancelRequest {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "content")
			s, err := Open(ctx, root, "test-only", true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if s != nil {
					s.Close(ctx)
				}
			}()
			data := []byte("retained contents")
			kept, _ := protectTestFile(t, s, "keep", data)
			_, expired := protectTestFile(t, s, "old", []byte("obsolete contents"))
			if err := s.Release(ctx, []string{expired}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			operation, cancel := context.WithCancel(ctx)
			defer cancel()
			var deletedFlushed atomic.Bool
			injected := errors.New("injected native flush failure")
			s.repository, err = repo.Open(ctx, filepath.Join(root, "kopia.config"), "test-only", &repo.Options{DisableRepositoryLog: true, BeforeFlush: []repo.RepositoryWriterCallback{func(ctx context.Context, w repo.RepositoryWriter) error {
				// Let the first GC tombstones flush. Fail only at a later flush,
				// after some native maintenance has already become durable.
				if deletedFlushed.Load() {
					if cancelRequest {
						cancel()
						return operation.Err()
					}
					return injected
				}
				return w.(repo.DirectRepository).ContentReader().IterateContents(ctx, kcontent.IterateOptions{IncludeDeleted: true}, func(info kcontent.Info) error {
					if info.Deleted {
						deletedFlushed.Store(true)
					}
					return nil
				})
			}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Collect(operation)
			want := injected
			if cancelRequest {
				want = context.Canceled
			}
			if !deletedFlushed.Load() || !errors.Is(err, want) {
				t.Fatalf("partial native failure was not exercised: %v", err)
			}
			verify := func() {
				t.Helper()
				r, err := s.Read(ctx, kept.ID)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(r)
				r.Close()
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("retained contents after partial maintenance: %v", err)
				}
			}
			verify()
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, root, "test-only", false)
			if err != nil {
				t.Fatal(err)
			}
			verify()
			if _, err := s.Collect(ctx); err != nil {
				t.Fatal(err)
			}
			verify()
		})
	}
}
