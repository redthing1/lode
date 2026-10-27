package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func reviewFile(path string, data []byte) model.Entry {
	hash := sha256.Sum256(data)
	return model.Entry{Path: path, Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
}

func TestCleanupPreservesChangedOldBackupDespiteExternalHardlink(t *testing.T) {
	_, _, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("before")}, nil)
	if err := w.root.Mkdir(".lode/transition", 0700); err != nil {
		t.Fatal(err)
	}
	changed := []byte("unexpected backup contents")
	oldPath := filepath.Join(localDir, ".lode", "transition", "0.old")
	if err := os.WriteFile(oldPath, changed, 0600); err != nil {
		t.Fatal(err)
	}
	// Link count alone does not establish that cleanup has a safe copy outside
	// the recovery files it will itself remove.
	if err := os.Link(oldPath, filepath.Join(localDir, "saved-copy")); err != nil {
		t.Fatal(err)
	}
	p := transition{Changes: []model.Change{{Before: w.state.Baseline[0], After: model.Entry{Path: "a", Kind: model.Absent}}}}
	err := w.cleanTransition(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), "0.old") {
		t.Fatalf("cleanTransition() error = %v, want preserved old-backup conflict", err)
	}
	if got, err := os.ReadFile(oldPath); err != nil || string(got) != string(changed) {
		t.Fatalf("old backup after cleanup = %q, %v", got, err)
	}
}

func TestCleanupKeepsLastUnexpectedCopyAcrossPrivateHardlinkAliases(t *testing.T) {
	_, _, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("before")}, nil)
	if err := w.root.Mkdir(".lode/transition", 0700); err != nil {
		t.Fatal(err)
	}
	changed := []byte("unexpected staged contents")
	newPath := filepath.Join(localDir, ".lode", "transition", "0.new")
	undoPath := filepath.Join(localDir, ".lode", "transition", "0.undo")
	if err := os.WriteFile(newPath, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(newPath, undoPath); err != nil {
		t.Fatal(err)
	}
	p := transition{Changes: []model.Change{{Before: model.Entry{Path: "a", Kind: model.Absent}, After: reviewFile("a", []byte("expected"))}}}
	err := w.cleanTransition(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), "0.undo") {
		t.Fatalf("cleanTransition() error = %v, want conflict at last private alias", err)
	}
	if _, err := os.Lstat(newPath); !os.IsNotExist(err) {
		t.Fatalf("first private alias still exists or lstat failed unexpectedly: %v", err)
	}
	if got, err := os.ReadFile(undoPath); err != nil || string(got) != string(changed) {
		t.Fatalf("last unexpected copy after cleanup = %q, %v", got, err)
	}
}
