package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestSelectedBranchSwitchAndRestore(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "assets/a", "first")
	writeFile(t, dir, "references/keep", "omitted")
	owner := attach(t, f, dir)
	first := save(t, f, owner, "initial")
	w := cloneWorkspace(t, f, model.Selection{{Path: "assets", Include: true}})
	if _, err := f.client.CreateBranch(ctx, "alice", "project", model.Reference{Name: "experiment", Version: first.ID}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, w.Directory(), "assets/a", "experiment")
	writeFile(t, w.Directory(), "private/note", "unselected local work")
	if err := w.Switch(ctx, f.token, "experiment"); err != nil {
		t.Fatal(err)
	}
	if readFile(t, w.Directory(), "assets/a") != "experiment" {
		t.Fatal("switch at same version lost unsaved work")
	}
	second := save(t, f, w, "experiment")
	if got := remoteFiles(t, f, second.ID); got["references/keep"] != "omitted" {
		t.Fatal("branch lost omitted contents")
	}
	f.dropFile.Store(true)
	if err := w.Switch(ctx, f.token, "main"); err == nil {
		t.Fatal("interrupted switch succeeded")
	}
	if err := w.Cancel(ctx, f.token); err != nil {
		t.Fatal(err)
	}
	if w.State().Branch != "experiment" || readFile(t, w.Directory(), "assets/a") != "experiment" {
		t.Fatal("cancelled switch changed branch or contents")
	}
	if err := w.Switch(ctx, f.token, "main"); err != nil {
		t.Fatal(err)
	}
	if w.State().Branch != "main" || w.State().Version != first.ID || readFile(t, w.Directory(), "assets/a") != "first" {
		t.Fatal("switch did not install main")
	}
	writeFile(t, w.Directory(), "assets/a", "dirty")
	if err := w.Switch(ctx, f.token, "experiment"); err == nil || !strings.Contains(err.Error(), "save selected changes") {
		t.Fatalf("dirty switch: %v", err)
	}
	writeFile(t, w.Directory(), "assets/a", "first")
	writeFile(t, w.Directory(), "assets/unrelated", "unrelated local edit")
	if err := w.Restore(ctx, f.token, model.ShortVersion(second.ID), []string{"assets/a"}); err != nil {
		t.Fatal(err)
	}
	if w.State().Branch != "main" || w.State().Version != first.ID || readFile(t, w.Directory(), "assets/a") != "experiment" {
		t.Fatal("restore changed branch/baseline or wrong contents")
	}
	if readFile(t, w.Directory(), "assets/unrelated") != "unrelated local edit" || readFile(t, w.Directory(), "private/note") != "unselected local work" {
		t.Fatal("restore changed unrelated work")
	}
	if err := w.Restore(ctx, f.token, first.ID, []string{"assets/a"}); err == nil || !strings.Contains(err.Error(), "changed path") {
		t.Fatalf("dirty restore: %v", err)
	}
	if err := w.Restore(ctx, f.token, first.ID, []string{"references"}); err == nil {
		t.Fatal("restore broadened selection")
	}
	third := save(t, f, w, "restore into main")
	if got := remoteFiles(t, f, third.ID); got["assets/a"] != "experiment" || got["references/keep"] != "omitted" || got["assets/unrelated"] != "unrelated local edit" {
		t.Fatalf("restored save: %v", got)
	}
	branchClone, err := Clone(ctx, f.url, f.client, filepath.Join(t.TempDir(), "experiment"), ConnectOptions{Branch: "experiment", Selection: model.Selection{{Path: "assets", Include: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer branchClone.Close()
	if branchClone.State().Version != second.ID || readFile(t, branchClone.Directory(), "assets/a") != "experiment" {
		t.Fatal("cloned wrong branch")
	}
	if _, err := os.Stat(filepath.Join(branchClone.Directory(), "references")); !os.IsNotExist(err) {
		t.Fatalf("branch clone loaded omitted files: %v", err)
	}
}

func TestRestoreRecoveryUsesSourceWithoutMovingBaseline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "a", "old")
	w := attach(t, f, dir)
	first := save(t, f, w, "first")
	writeFile(t, dir, "a", "new")
	writeFile(t, dir, "later", "added later")
	second := save(t, f, w, "second")
	f.dropFile.Store(true)
	if err := w.Restore(ctx, f.token, first.ID, []string{"a"}); err == nil {
		t.Fatal("interrupted restore succeeded")
	}
	w.Close()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Resume(ctx, f.token); err != nil {
		t.Fatal(err)
	}
	if w.State().Version != second.ID || readFile(t, dir, "a") != "old" {
		t.Fatal("restore recovery used baseline as download source")
	}
	if err := w.Restore(ctx, f.token, first.ID, []string{"later"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "later")); !os.IsNotExist(err) {
		t.Fatalf("restore did not remove later file: %v", err)
	}
	changes, err := w.Status(ctx)
	if err != nil || len(changes) != 2 {
		t.Fatalf("restored contents are not unsaved changes: %v %v", changes, err)
	}
}
