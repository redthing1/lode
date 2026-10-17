package kopia_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	engine "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestRemotePartialCheckpoint(t *testing.T) {
	f := newFixture(t)
	oldA, omittedB, newA := randomBytes(512<<10, 21), randomBytes(8<<20, 22), randomBytes(256<<10, 23)
	var base, a, b engine.Entry
	var first, second manifest.ID
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		a, err = engine.CaptureFile(ctx, w, bytes.NewReader(oldA), false)
		if err != nil {
			return err
		}
		b, err = engine.CaptureFile(ctx, w, bytes.NewReader(omittedB), false)
		if err != nil {
			return err
		}
		base, err = engine.Apply(ctx, w, engine.Entry{}, fullSelection(), []engine.Change{
			{Path: path(t, "a.bin"), After: a}, {Path: path(t, "large/b.bin"), After: b},
		})
		if err != nil {
			return err
		}
		first, err = engine.Protect(ctx, w, base, "remote-base")
		return err
	})
	bID, err := object.ParseID(strings.TrimPrefix(b.State().ContentID, "kopia:"))
	if err != nil {
		t.Fatal(err)
	}
	bContents, err := f.rep.VerifyObject(f.ctx, bID)
	if err != nil {
		t.Fatal(err)
	}
	r := newRemote(t, f)
	client := r.open(t, "partial", "proof-append-token", false)
	beforeDownload := r.written.Load()
	// The future application API supplies accepted entry descriptors. This test
	// supplies one directly; native manifest access is deliberately unavailable.
	verifyFile(t, f.ctx, client, base, "a.bin", oldA)
	downloadBytes := r.written.Load() - beforeDownload
	if downloadBytes >= 2<<20 {
		t.Fatalf("partial materialization downloaded too much: %d", downloadBytes)
	}
	beforeUpload := r.read.Load()
	var changed engine.Entry
	err = repo.WriteSession(f.ctx, client, repo.WriteSessionOptions{Purpose: "capture selected file"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		changed, err = engine.CaptureFile(ctx, w, bytes.NewReader(newA), false)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	uploadBytes := r.read.Load() - beforeUpload
	if uploadBytes >= 1<<20 {
		t.Fatalf("partial capture uploaded too much: %d", uploadBytes)
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	for _, id := range bContents {
		if r.gets[id.String()] {
			r.mu.Unlock()
			t.Fatalf("partial client requested omitted content %s", id)
		}
	}
	r.mu.Unlock()
	var current engine.Entry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		current, err = engine.Apply(ctx, w, base, (model.Selection{}).Include(path(t, "a.bin")),
			[]engine.Change{{Path: path(t, "a.bin"), Before: a, After: changed}})
		if err != nil {
			return err
		}
		second, err = engine.Protect(ctx, w, current, "remote-result")
		return err
	})
	beforeDuplicate := r.read.Load()
	if err := repo.WriteSession(f.ctx, client, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		duplicate, err := engine.CaptureFile(ctx, w, bytes.NewReader(newA), false)
		if err == nil && duplicate.State() != changed.State() {
			return errors.New("duplicate bytes changed identity")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	duplicateBytes := r.read.Load() - beforeDuplicate
	if duplicateBytes >= 64<<10 {
		t.Fatalf("duplicate capture uploaded payload again: %d", duplicateBytes)
	}
	if _, err := engine.LoadRoot(f.ctx, client, first); err == nil {
		t.Fatal("native manifest reads were exposed")
	}
	if err := repo.WriteSession(f.ctx, client, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		if _, err := w.PutManifest(ctx, map[string]string{"type": snapshot.ManifestType}, map[string]string{}); err == nil {
			return errors.New("client created a root")
		}
		if err := w.DeleteManifest(ctx, first); err == nil {
			return errors.New("client deleted a root")
		}
		if _, err := w.(repo.RemoteRetentionPolicy).ApplyRetentionPolicy(ctx, "/projects", true); err == nil {
			return errors.New("client applied retention")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []manifest.ID{first, second} {
		if _, err := engine.LoadRoot(f.ctx, f.rep, id); err != nil {
			t.Fatalf("client affected server root %s: %v", id, err)
		}
	}
	cold := r.open(t, "independent", "proof-read-token", true)
	verifyFile(t, f.ctx, cold, current, "a.bin", newA)
	verifyFile(t, f.ctx, cold, current, "large/b.bin", omittedB)
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	if err := r.activity.Collect(ctx, f.rep); err != nil {
		t.Fatalf("collection did not drain native sessions: %v", err)
	}
	report, _ := json.Marshal(struct{ DownloadTLSBytes, UploadTLSBytes, DuplicateTLSBytes int64 }{downloadBytes, uploadBytes, duplicateBytes})
	t.Logf("REMOTE_PROOF %s", report)
}

func TestRemoteRejectedConnectionCloses(t *testing.T) {
	f := newFixture(t)
	r := newRemote(t, f)
	err := repo.ConnectAPIServer(f.ctx, filepath.Join(f.dir, "rejected.config"), &r.info, "invalid-token", &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Username: "lode", Hostname: "rejected"},
	})
	if err == nil {
		t.Fatal("invalid token opened a native repository")
	}
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	for r.connected.Load() != 0 {
		select {
		case <-r.ended:
		case <-ctx.Done():
			t.Fatal("rejected connection remained open")
		}
	}
}

func TestRemoteWriterSessionsRelease(t *testing.T) {
	f := newFixture(t)
	r := newRemote(t, f)
	client := r.open(t, "repeated-writer", "proof-append-token", false)
	for i := range 20 {
		if err := repo.WriteSession(f.ctx, client, repo.WriteSessionOptions{}, func(context.Context, repo.RepositoryWriter) error { return nil }); err != nil {
			t.Fatalf("writer %d leaked admission from earlier writers: %v", i, err)
		}
	}
}

func TestRemoteInterruptedTransferCanRecapture(t *testing.T) {
	f := newFixture(t)
	r := newRemote(t, f)
	client := r.open(t, "interrupted", "proof-append-token", false)
	payload := randomBytes(2<<20, 24)
	r.cutWrite.Store(true)
	err := repo.WriteSession(f.ctx, client, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		_, err := engine.CaptureFile(ctx, w, bytes.NewReader(payload), false)
		return err
	})
	if err == nil || r.cutWrite.Load() {
		t.Fatalf("connection interruption was not observed: %v", err)
	}
	roots, err := f.rep.FindManifests(f.ctx, map[string]string{"type": snapshot.ManifestType})
	if err != nil || len(roots) != 0 {
		t.Fatalf("interrupted transfer published a root: %v", err)
	}
	retry := r.open(t, "recapture", "proof-append-token", false)
	var file engine.Entry
	if err := repo.WriteSession(f.ctx, retry, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		file, err = engine.CaptureFile(ctx, w, bytes.NewReader(payload), false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var root engine.Entry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		root, err = engine.Apply(ctx, w, engine.Entry{}, fullSelection(), []engine.Change{{Path: path(t, "retry.bin"), After: file}})
		if err != nil {
			return err
		}
		_, err = engine.Protect(ctx, w, root, "recaptured")
		return err
	})
	verifyFile(t, f.ctx, f.rep, root, "retry.bin", payload)
}

func TestRemoteRecaptureAfterUnrootedContentIsCollected(t *testing.T) {
	f := newFixture(t)
	r := newRemote(t, f)
	client := r.open(t, "cached-upload", "proof-append-token", false)
	payload := randomBytes(256<<10, 25)
	capture := func() engine.Entry {
		t.Helper()
		var entry engine.Entry
		if err := repo.WriteSession(f.ctx, client, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
			var err error
			entry, err = engine.CaptureFile(ctx, w, bytes.NewReader(payload), false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	original := capture()
	// Fill the client's disposable content cache without protecting a root.
	reader, err := engine.OpenFile(f.ctx, client, original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	id, err := object.ParseID(strings.TrimPrefix(original.State().ContentID, "kopia:"))
	if err != nil {
		t.Fatal(err)
	}
	for cycle := range 7 {
		if cycle > 0 {
			f.clock.advance(30 * time.Hour)
		}
		if err := r.activity.Collect(f.ctx, f.rep); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rep.VerifyObject(f.ctx, id); err == nil {
		t.Fatal("unrooted content was not collected")
	}
	before := r.read.Load()
	recaptured := capture()
	transferred := r.read.Load() - before
	if recaptured.State() != original.State() || transferred < int64(len(payload))/2 {
		t.Fatalf("cached presence bypassed reupload: %d bytes", transferred)
	}
	if err := f.rep.Refresh(f.ctx); err != nil {
		t.Fatal(err)
	}
	reader, err = engine.OpenFile(f.ctx, f.rep, recaptured)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("recaptured contents did not reconstruct: read=%v close=%v", err, closeErr)
	}
	t.Logf("RECAPTURE_TLS_BYTES %d", transferred)
}
