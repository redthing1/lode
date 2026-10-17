package kopia_test

import (
	"context"
	"errors"
	"testing"
	"time"

	engine "github.com/redthing1/lode/internal/engine/kopia"
)

func TestActivityDrainsBeforeCollectionAndRecoversFromCancellation(t *testing.T) {
	a, err := engine.NewActivity(1)
	if err != nil {
		t.Fatal(err)
	}
	active, release, err := a.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, _, err := a.Enter(t.Context()); !errors.Is(err, engine.ErrSessionLimit) {
		t.Fatalf("unbounded activity: %v", err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if err := a.Collect(canceled, nil); !errors.Is(err, context.Canceled) || active.Err() != nil {
		t.Fatalf("canceled collection disturbed active work: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	// A nil repository makes accidental maintenance before release fail loudly.
	go func() { finished <- a.Collect(ctx, nil) }()
	select {
	case <-active.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("collection did not cancel active work")
	}
	if _, _, err := a.Enter(t.Context()); !errors.Is(err, engine.ErrMaintenance) {
		t.Fatalf("admission continued during drain: %v", err)
	}
	if err := a.Collect(t.Context(), nil); !errors.Is(err, engine.ErrMaintenance) {
		t.Fatalf("concurrent collection was allowed: %v", err)
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("collection ignored canceled drain: %v", err)
	}
	release()
	release()
	_, end, err := a.Enter(t.Context())
	if err != nil {
		t.Fatalf("admission did not recover: %v", err)
	}
	end()
}
