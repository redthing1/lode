package kopia_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	engine "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

type guardedReader struct {
	repo.Repository
	denied string
}

func (r guardedReader) OpenObject(ctx context.Context, id object.ID) (object.Reader, error) {
	if "kopia:"+id.String() == r.denied {
		return nil, errors.New("omitted file payload was opened")
	}
	return r.Repository.OpenObject(ctx, id)
}

type guardedWriter struct {
	repo.RepositoryWriter
	denied string
}

func (w guardedWriter) OpenObject(ctx context.Context, id object.ID) (object.Reader, error) {
	return guardedReader{Repository: w.RepositoryWriter, denied: w.denied}.OpenObject(ctx, id)
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	_, _ = rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func blobBytes(t *testing.T, f *fixture) int64 {
	t.Helper()
	blobs, err := blob.ListAllBlobs(f.ctx, f.rep.BlobReader(), "")
	if err != nil {
		t.Fatal(err)
	}
	return blob.TotalLength(blobs)
}

func verifyFile(t *testing.T, ctx context.Context, r repo.Repository, root engine.Entry, name string, expected []byte) {
	t.Helper()
	e, err := engine.Lookup(ctx, r, root, path(t, name))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := engine.OpenFile(ctx, r, e)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read %s: %v; close: %v", name, err, closeErr)
	}
	want := sha256.Sum256(expected)
	if n != int64(len(expected)) || !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatalf("%s did not reconstruct exactly", name)
	}
}

func TestPartialCheckpointAndSafeReclamation(t *testing.T) {
	f := newFixture(t)
	oldA, originalB, newA := randomBytes(12<<20, 1), randomBytes(8<<20, 2), randomBytes(256<<10, 3)
	var base, a, b engine.Entry
	var first, second manifest.ID
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		a, err = engine.CaptureFile(ctx, w, bytes.NewReader(oldA), false)
		if err != nil {
			return err
		}
		b, err = engine.CaptureFile(ctx, w, bytes.NewReader(originalB), false)
		if err != nil {
			return err
		}
		base, err = engine.Apply(ctx, w, engine.Entry{}, fullSelection(), []engine.Change{
			{Path: path(t, "a.bin"), After: a}, {Path: path(t, "large/b.bin"), After: b},
		})
		if err != nil {
			return err
		}
		first, err = engine.Protect(ctx, w, base, "first")
		return err
	})
	initialBytes := blobBytes(t, f)
	client := f.reader("partial-client")
	guard := guardedReader{Repository: client, denied: b.State().ContentID}
	loadedRoot, err := engine.LoadRoot(f.ctx, guard, first)
	if err != nil {
		t.Fatal(err)
	}
	loadedA, err := engine.Lookup(f.ctx, guard, loadedRoot, path(t, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := engine.OpenFile(f.ctx, guard, loadedA)
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(materialized, oldA) {
		t.Fatal("partial materialization failed")
	}
	workspace := filepath.Join(f.dir, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	localFile := filepath.Join(workspace, "a.bin")
	if err := os.WriteFile(localFile, materialized, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localFile, newA, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(workspace)
	if err != nil || len(files) != 1 || files[0].Name() != "a.bin" {
		t.Fatal("workspace unexpectedly contains omitted files")
	}
	beforeUpload := f.clock.written.Load()
	var changedA engine.Entry
	err = repo.WriteSession(f.ctx, client, repo.WriteSessionOptions{Purpose: "partial capture"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		source, err := os.Open(localFile)
		if err != nil {
			return err
		}
		defer source.Close()
		changedA, err = engine.CaptureFile(ctx, w, source, false)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	uploadBytes := f.clock.written.Load() - beforeUpload
	if uploadBytes >= 1<<20 {
		t.Fatalf("partial capture wrote too much: %d bytes", uploadBytes)
	}
	if err := client.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	var current engine.Entry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		current, err = engine.Apply(ctx, guardedWriter{RepositoryWriter: w, denied: b.State().ContentID}, base,
			(model.Selection{}).Include(path(t, "a.bin")), []engine.Change{{Path: path(t, "a.bin"), Before: a, After: changedA}})
		if err != nil {
			return err
		}
		second, err = engine.Protect(ctx, w, current, "second")
		return err
	})
	if lookup(t, f, current, "large/b.bin").State() != b.State() {
		t.Fatal("omitted file identity changed")
	}
	verifyFile(t, f.ctx, f.rep, current, "a.bin", newA)
	verifyFile(t, f.ctx, f.rep, current, "large/b.bin", originalB)
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		if err := engine.Release(ctx, w, first); err != nil {
			return err
		}
		return engine.Release(ctx, w, first)
	})
	if _, err := engine.LoadRoot(f.ctx, f.rep, first); !errors.Is(err, snapshot.ErrSnapshotNotFound) {
		t.Fatalf("released root still available: %v", err)
	}
	beforeCollection := blobBytes(t, f)
	var sizes []int64
	for cycle := range 7 {
		if cycle > 0 {
			f.clock.advance(30 * time.Hour)
		}
		if err := engine.Collect(f.ctx, f.rep); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		sizes = append(sizes, blobBytes(t, f))
		cold := f.reader(fmt.Sprintf("verify-%d", cycle))
		retained, err := engine.LoadRoot(f.ctx, cold, second)
		if err != nil {
			t.Fatal(err)
		}
		verifyFile(t, f.ctx, cold, retained, "a.bin", newA)
		verifyFile(t, f.ctx, cold, retained, "large/b.bin", originalB)
		if err := cold.Close(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	finalBytes := sizes[len(sizes)-1]
	report, _ := json.Marshal(struct {
		Initial, BeforeCollection, Final, PartialCaptureBytes int64
		CycleBlobBytes                                        []int64
		Safety                                                string
		SimulatedHours                                        int
	}{initialBytes, beforeCollection, finalBytes, uploadBytes, sizes, "full", 180})
	t.Logf("PROOF %s", report)
	if reclaimed := beforeCollection - finalBytes; reclaimed < int64(len(oldA))/2 {
		t.Fatalf("obsolete payload not physically reclaimed: before=%d after=%d", beforeCollection, finalBytes)
	}
}

func TestRebuiltTreeReusesOperationRootAndFileContents(t *testing.T) {
	f := newFixture(t)
	payload := randomBytes(256<<10, 5)
	var file engine.Entry
	var id manifest.ID
	changes := []engine.Change{
		{Path: path(t, "assets/model.bin")},
		{Path: path(t, "empty"), After: engine.Directory()},
	}
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		file, err = engine.CaptureFile(ctx, w, bytes.NewReader(payload), false)
		if err != nil {
			return err
		}
		changes[0].After = file
		root, err := engine.Apply(ctx, w, engine.Entry{}, fullSelection(), changes)
		if err != nil {
			return err
		}
		id, err = engine.Protect(ctx, w, root, "rebuild")
		return err
	})
	f.clock.advance(time.Minute)
	fresh := f.reader("retry")
	defer fresh.Close(context.WithoutCancel(f.ctx))
	before := f.clock.written.Load()
	err := repo.WriteSession(f.ctx, fresh, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		duplicate, err := engine.CaptureFile(ctx, w, bytes.NewReader(payload), false)
		if err != nil {
			return err
		}
		if duplicate.State() != file.State() {
			return errors.New("identical bytes did not share content identity")
		}
		executable, err := engine.CaptureFile(ctx, w, bytes.NewReader(payload), true)
		if err != nil {
			return err
		}
		if executable.State().ContentID != file.State().ContentID || !executable.State().Executable {
			return errors.New("executable attribute changed content identity or was lost")
		}
		// Reuse the finalized descriptors, as a publication retry must do. Order
		// and the time of tree construction must not change the protected tree.
		root, err := engine.Apply(ctx, w, engine.Entry{}, fullSelection(), []engine.Change{changes[1], changes[0]})
		if err != nil {
			return err
		}
		retried, err := engine.Protect(ctx, w, root, "rebuild")
		if err == nil && retried != id {
			return errors.New("rebuilding the same tree created a second root")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if written := f.clock.written.Load() - before; written >= int64(len(payload))/8 {
		t.Fatalf("duplicate capture rewrote payload: %d bytes", written)
	}
}

func TestRootRetryAndFailedCapture(t *testing.T) {
	f := newFixture(t)
	var root engine.Entry
	var id manifest.ID
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		root, err = engine.Apply(ctx, w, engine.Entry{}, model.Selection{}, nil)
		if err != nil {
			return err
		}
		id, err = engine.Protect(ctx, w, root, "same-operation")
		return err
	})
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		retried, err := engine.Protect(ctx, w, root, "same-operation")
		if err == nil && retried != id {
			return errors.New("retry created a second root")
		}
		return err
	})
	err := repo.WriteSession(f.ctx, f.rep, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		other, err := engine.Apply(ctx, w, root, fullSelection(), []engine.Change{{Path: path(t, "new"), After: engine.Directory()}})
		if err != nil {
			return err
		}
		_, err = engine.Protect(ctx, w, other, "same-operation")
		return err
	})
	if !errors.Is(err, model.ErrConflict) {
		t.Fatalf("operation accepted different contents: %v", err)
	}
	captureFailure := errors.New("source changed during capture")
	err = repo.WriteSession(f.ctx, f.rep, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		_, err := engine.CaptureFile(ctx, w, io.MultiReader(bytes.NewReader(randomBytes(1<<20, 4)), failingReader{captureFailure}), false)
		return err
	})
	if !errors.Is(err, captureFailure) {
		t.Fatalf("capture failure lost: %v", err)
	}
	roots, err := f.rep.FindManifests(f.ctx, map[string]string{"type": snapshot.ManifestType})
	if err != nil || len(roots) != 1 {
		t.Fatalf("failed operations changed protected roots: %v", err)
	}
	if _, err := engine.LoadRoot(f.ctx, f.rep, id); err != nil {
		t.Fatal(err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestCollectionStopsOnUnreadableRoot(t *testing.T) {
	f := newFixture(t)
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		writer := w.NewObjectWriter(ctx, object.WriterOptions{Prefix: "k"})
		defer writer.Close()
		if _, err := io.WriteString(writer, "not a directory"); err != nil {
			return err
		}
		id, err := writer.Result()
		if err != nil {
			return err
		}
		_, err = snapshot.SaveSnapshot(ctx, w, &snapshot.Manifest{
			Source:    snapshot.SourceInfo{Host: "lode", UserName: "lode", Path: "/broken"},
			RootEntry: &snapshot.DirEntry{Type: snapshot.EntryTypeDirectory, ObjectID: id},
		})
		return err
	})
	deletes := f.clock.deleted.Load()
	if err := engine.Collect(f.ctx, f.rep); err == nil {
		t.Fatal("collection accepted an unreadable retained tree")
	}
	if got := f.clock.deleted.Load(); got != deletes {
		t.Fatal("collection deleted blobs before completing root preflight")
	}
}
