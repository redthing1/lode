package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/remote"
)

func TestUpdateReconcilesExpiredPendingSaveWithoutDiscardingLocalWork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "a", "base a")
	writeFile(t, dir, "b", "base b")
	w := attach(t, f, dir)
	base := save(t, f, w, "base")
	other, err := Clone(ctx, f.url, f.client, filepath.Join(t.TempDir(), "other"), ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	writeFile(t, dir, "a", "pending a")
	f.refuseSave.Store(true)
	if _, err := w.Save(ctx, f.token, "interrupted"); err == nil {
		t.Fatal("save was not interrupted")
	}
	var pending pendingSave
	if err := w.readJSON("save.json", &pending); err != nil {
		t.Fatal(err)
	}
	// Model a workspace returning after its unaccepted request has expired.
	stamp := time.Now().Add(-9 * 24 * time.Hour).UnixMilli()
	pending.Request.ID = fmt.Sprintf("%08x-%04x-%s", stamp>>16, stamp&0xffff, pending.Request.ID[14:])
	if err := w.writeJSON("save.json", pending); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a", "later local work")
	writeFile(t, other.Directory(), "b", "incoming b")
	tip := save(t, f, other, "independent incoming work")
	for _, settle := range []func(context.Context, string) error{w.Resume, w.Cancel} {
		err := settle(ctx, f.token)
		var failure *remote.Error
		if !errors.As(err, &failure) || failure.Code != "operation_expired" || !strings.Contains(err.Error(), "lode update") {
			t.Fatalf("expiry guidance: %v", err)
		}
		if exists, err := w.pending("save.json"); err != nil || !exists || w.State().Version != base.ID {
			t.Fatalf("expiry silently settled unknown outcome: %v %v", exists, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Update(ctx, f.token, nil); err != nil {
		t.Fatal(err)
	}
	if w.State().Version != tip.ID || readFile(t, dir, "a") != "later local work" || readFile(t, dir, "b") != "incoming b" {
		t.Fatal("update lost local work or failed to reconcile incoming contents")
	}
	if exists, err := w.pending("save.json"); err != nil || exists {
		t.Fatalf("expired pending request remains: %v %v", exists, err)
	}
	changes, err := w.Status(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "a" {
		t.Fatalf("local work status: %+v %v", changes, err)
	}
	if got := remoteFiles(t, f, save(t, f, w, "reconciled").ID); got["a"] != "later local work" || got["b"] != "incoming b" {
		t.Fatalf("reconciled publication: %v", got)
	}
}
