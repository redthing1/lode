//go:build linux

package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestStatusCancellationStopsBeforeSaveJournal(t *testing.T) {
	_, _, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("contents")}, nil)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Status(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled status error = %v", err)
	}
	if _, err := w.Save(cancelled, "tester", "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled save error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, ".lode", "save.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled save journal = %v", err)
	}
}

type cancelOnFirstRead struct {
	cancel context.CancelFunc
	data   []byte
	done   bool
	sent   int
}

func (r *cancelOnFirstRead) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	r.sent = n
	r.cancel()
	return n, nil
}

func TestContextReaderStopsHashAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	data := bytes.Repeat([]byte{0x42}, 64<<10)
	hash := sha256.New()
	r := &cancelOnFirstRead{cancel: cancel, data: data}
	_, err := io.Copy(hash, contextReader{ctx: ctx, reader: r})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("hash copy error = %v", err)
	}
	if !bytes.Equal(hash.Sum(nil), sha256Bytes(data[:r.sent])) {
		t.Fatal("hash boundary did not consume the completed read before cancellation")
	}
}

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func TestResumeFinishesCommittedCleanupAfterCancellation(t *testing.T) {
	oldBytes := bytes.Repeat([]byte{0x19}, 128<<10)
	s, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"a": oldBytes}, nil)
	newBytes := bytes.Repeat([]byte{0x20}, 128<<10)
	write(t, sourceDir, "a", newBytes)
	newBase := publishWorkspace(t, s, sourceDir)
	client, err := w.client()
	if err != nil {
		t.Fatal(err)
	}
	target := w.State()
	target.Base = newBase
	target.Baseline, err = client.Entries(context.Background(), newBase, target.Selection)
	if err != nil {
		t.Fatal(err)
	}
	before, after := w.State().Baseline[0], target.Baseline[0]
	write(t, localDir, "a", newBytes)
	if err := os.MkdirAll(filepath.Join(localDir, ".lode", "transition"), 0700); err != nil {
		t.Fatal(err)
	}
	oldName, newName := recoveryName(0, "old"), recoveryName(0, "new")
	if err := os.WriteFile(filepath.Join(localDir, filepath.FromSlash(oldName)), oldBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(localDir, "a"), filepath.Join(localDir, filepath.FromSlash(newName))); err != nil {
		t.Fatal(err)
	}
	if err := w.store("state.json", target); err != nil {
		t.Fatal(err)
	}
	w.state = target
	plan := transition{Target: target, Changes: []model.Change{{Before: before, After: after}}, Next: 1}
	if err := w.storeTransitionProgress(&plan); err != nil {
		t.Fatal(err)
	}
	if err := w.store("transition.json", plan); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.cleanTransition(ctx, &plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup error = %v", err)
	}
	if w.State().Base != newBase || !bytes.Equal(read(t, localDir, "a"), newBytes) {
		t.Fatal("cancelled cleanup changed committed target or baseline")
	}
	if _, err := os.Stat(filepath.Join(localDir, filepath.FromSlash(oldName))); err != nil {
		t.Fatalf("cancelled cleanup removed old backup: %v", err)
	}
	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("resume committed cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, filepath.FromSlash(oldName))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old backup after cleanup = %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, ".lode", "transition.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition plan after cleanup = %v", err)
	}
}
