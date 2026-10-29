package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestIgnoreSaveAndTrackedChanges(t *testing.T) {
	f := newFixture(t)
	w := attach(t, f, t.TempDir())
	for _, name := range []string{".DS_Store", "assets/.DS_Store", "Thumbs.db", "assets/desktop.ini", "assets/keep", "tracked.tmp"} {
		writeFile(t, w.Directory(), name, name)
	}
	v := save(t, f, w, "first")
	files := remoteFiles(t, f, v.ID)
	if len(files) != 2 || files["assets/keep"] == "" || files["tracked.tmp"] == "" {
		t.Fatalf("unexpected files: %v", files)
	}
	writeFile(t, w.Directory(), ".lodeignore", "*.tmp\ncache/\n!.DS_Store\n")
	writeFile(t, w.Directory(), ".lode/ignore", ".DS_Store\nlocal-only\n")
	for _, name := range []string{"new.tmp", "cache/data", "local-only", "tracked.tmp"} {
		writeFile(t, w.Directory(), name, "changed")
	}
	// Ignored directories are not inspected, even with unsupported local entries.
	if err := os.Symlink("/outside", filepath.Join(w.Directory(), "cache/link")); err != nil {
		t.Fatal(err)
	}
	v = save(t, f, w, "rules and tracked edit")
	files = remoteFiles(t, f, v.ID)
	if len(files) != 3 || files["tracked.tmp"] != "changed" || files[".lodeignore"] == "" {
		t.Fatalf("ignored or tracked files: %v", files)
	}
	if err := os.Remove(filepath.Join(w.Directory(), "tracked.tmp")); err != nil {
		t.Fatal(err)
	}
	v = save(t, f, w, "tracked removal")
	if _, exists := remoteFiles(t, f, v.ID)["tracked.tmp"]; exists {
		t.Fatal("ignore hid tracked deletion")
	}
	writeFile(t, w.Directory(), ".lodeignore", "bad[")
	if _, err := w.Status(context.Background()); err == nil || !strings.Contains(err.Error(), ".lodeignore: line 1") {
		t.Fatalf("bad rules were not diagnosed: %v", err)
	}
}

func TestIgnoredFilesStayLocalAcrossWorkspaceChanges(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := attach(t, f, t.TempDir())
	writeFile(t, owner.Directory(), ".lodeignore", "cache/\n*.tmp\n")
	writeFile(t, owner.Directory(), "assets/file", "old")
	first := save(t, f, owner, "first")
	local := cloneWorkspace(t, f, nil)
	writeFile(t, local.Directory(), "cache/scratch", "private")
	writeFile(t, local.Directory(), "assets/.DS_Store", "Finder")
	if err := local.Select(ctx, f.token, model.Selection{{Path: ".lodeignore", Include: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, local.Directory(), "assets/.DS_Store"); got != "Finder" {
		t.Fatal("ignored file removed")
	}
	if err := local.Select(ctx, f.token, model.Selection{{Include: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := local.Restore(ctx, f.token, first.ID, []string{"assets"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, local.Directory(), "cache/scratch"); got != "private" {
		t.Fatal("ignored subtree removed")
	}
	// A peer deliberately versions an ignored name. Local data must cause a conflict.
	writeFile(t, owner.Directory(), ".lodeignore", "")
	writeFile(t, owner.Directory(), "new.tmp", "shared")
	save(t, f, owner, "incoming ignored name")
	writeFile(t, local.Directory(), "new.tmp", "private")
	var conflict *ConflictsError
	if err := local.Update(ctx, f.token, nil); !errors.As(err, &conflict) {
		t.Fatalf("ignored collision: %v", err)
	}
	if got := readFile(t, local.Directory(), "new.tmp"); got != "private" {
		t.Fatal("ignored collision overwritten")
	}
	if err := local.Update(ctx, f.token, map[string]model.Side{"new.tmp": model.Incoming}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, local.Directory(), "new.tmp"); got != "shared" {
		t.Fatal("explicit conflict choice not applied")
	}
}
