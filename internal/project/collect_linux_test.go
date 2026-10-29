package project

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
)

func TestCollectionWaitsForReadersAndPreservesPromisedVersions(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	first := publish(t, f.store, "", []model.Entry{file("a", "first")}, map[string]string{"a": "first"})
	tip := publish(t, f.store, first.ID, []model.Entry{file("a", "latest")}, map[string]string{"a": "latest"})
	if err := f.store.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.PreviewCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Cleanup(ctx, p.Token); err != nil {
		t.Fatal(err)
	}
	_, reader, err := f.store.File(ctx, tip.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	started, done := make(chan struct{}), make(chan error, 1)
	waitCtx, cancel := context.WithCancel(ctx)
	go func() { close(started); _, err := f.store.Collect(waitCtx); done <- err }()
	<-started
	select {
	case err := <-done:
		t.Fatalf("collection ran during read: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collection remained blocked")
	}
	if _, err := f.store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if read(t, f.store, tip.ID, "a") != "latest" {
		t.Fatal("live contents lost")
	}
	if _, _, err := f.store.File(ctx, first.ID, "a"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired read: %v", err)
	}
	f.close(t)
	f = openFixture(t, f.directory, false)
	if read(t, f.store, tip.ID, "a") != "latest" {
		t.Fatal("reopened contents lost")
	}
	// Missing promised protection must stop collection before other roots are
	// released or native maintenance can make the inconsistency irreversible.
	v, err := f.store.version(ctx, tip.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.content.Release(ctx, []string{v.protection}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Collect(ctx); err == nil {
		t.Fatal("collected with missing promised protection")
	}
}
