//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestUpdateWithChoicesResolvesDeletionAndFileDirectoryConflicts(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "choice-update-base", "", []project.Change{
		makeFileChange("delete-local.bin", "base local delete"),
		makeFileChange("delete-target.bin", "base target delete"),
		makeFileChange("type.bin", "base type"),
	}, map[string]string{
		"delete-local.bin": "base local delete", "delete-target.bin": "base target delete", "type.bin": "base type",
	})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	writeTestFile(t, filepath.Join(directory, "delete-local.bin"), "local survives deletion")
	writeTestFile(t, filepath.Join(directory, "delete-target.bin"), "local deleted by choice")
	writeTestFile(t, filepath.Join(directory, "type.bin"), "local type edit")
	updated := seed(t, remote, "choice-update-target", base.ID, []project.Change{
		{Path: "delete-local.bin", Kind: model.Absent},
		{Path: "delete-target.bin", Kind: model.Absent},
		{Path: "type.bin", Kind: model.Directory},
		makeFileChange("type.bin/child.bin", "directory child"),
	}, map[string]string{"type.bin/child.bin": "directory child"})

	err := w.UpdateWithChoices(context.Background(), []project.Choice{
		{Path: "delete-local.bin", Side: model.LocalSide},
		{Path: "delete-target.bin", Side: model.TargetSide},
		{Path: "type.bin", Side: model.TargetSide},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "delete-local.bin"), "local survives deletion")
	if _, err := os.Stat(filepath.Join(directory, "delete-target.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target-side deletion left a file: %v", err)
	}
	if info, err := os.Stat(filepath.Join(directory, "type.bin")); err != nil || !info.IsDir() {
		t.Fatalf("target-side file/directory choice: info=%v err=%v", info, err)
	}
	assertFileContents(t, filepath.Join(directory, "type.bin/child.bin"), "directory child")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"delete-local.bin"}) {
		t.Fatalf("Status after choices=%+v err=%v", changes, err)
	}
}

func TestUpdateWithRootTargetChoiceSelectsCompleteSelectedTree(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "root-choice-workspace-base", "", []project.Change{
		makeFileChange("a.bin", "base a"), makeFileChange("b.bin", "base b"),
	}, map[string]string{"a.bin": "base a", "b.bin": "base b"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	writeTestFile(t, filepath.Join(directory, "a.bin"), "local a")
	updated := seed(t, remote, "root-choice-workspace-target", base.ID, []project.Change{
		makeFileChange("a.bin", "target a"), makeFileChange("b.bin", "target b"),
	}, map[string]string{"a.bin": "target a", "b.bin": "target b"})
	if err := w.UpdateWithChoices(context.Background(), []project.Choice{{Path: "", Side: model.TargetSide}}); err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID {
		t.Fatalf("base=%q, want %q", w.Base(), updated.ID)
	}
	assertFileContents(t, filepath.Join(directory, "a.bin"), "target a")
	assertFileContents(t, filepath.Join(directory, "b.bin"), "target b")
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("Status after root choice=%+v err=%v", changes, err)
	}
}

func TestSwitchWithChoicesResolvesBranchConflict(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "choice-switch-base", "", []project.Change{makeFileChange("file.bin", "fork")}, map[string]string{"file.bin": "fork"})
	if err := remote.client.CreateBranch(context.Background(), "feature", base.ID); err != nil {
		t.Fatal(err)
	}
	mainTip := seed(t, remote, "choice-switch-main", base.ID, []project.Change{makeFileChange("file.bin", "main")}, map[string]string{"file.bin": "main"})
	featureTip := applyTestChanges(t, remote, "choice-switch-feature", "feature", base.ID, []project.Change{makeFileChange("file.bin", "feature")}, map[string]string{"file.bin": "feature"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	if w.Base() != mainTip.ID {
		t.Fatalf("workspace base=%q, want %q", w.Base(), mainTip.ID)
	}
	writeTestFile(t, filepath.Join(directory, "file.bin"), "new local edit")
	if err := w.SwitchWithChoices(context.Background(), "feature", []project.Choice{{Path: "file.bin", Side: model.TargetSide}}); err != nil {
		t.Fatal(err)
	}
	if w.Branch() != "feature" || w.Base() != featureTip.ID || w.Pending() != "" {
		t.Fatalf("branch=%q base=%q pending=%q", w.Branch(), w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "feature")
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("Status after switch choice=%+v err=%v", changes, err)
	}
}

func TestRestoreRootChoiceIsRestrictedToRequestedScope(t *testing.T) {
	remote := newTestRemote(t, nil)
	old := seed(t, remote, "restore-choice-old", "", []project.Change{
		makeFileChange("a.bin", "old a"), makeFileChange("b.bin", "old b"),
	}, map[string]string{"a.bin": "old a", "b.bin": "old b"})
	current := seed(t, remote, "restore-choice-current", old.ID, []project.Change{
		makeFileChange("a.bin", "current a"), makeFileChange("b.bin", "current b"),
	}, map[string]string{"a.bin": "current a", "b.bin": "current b"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	if w.Base() != current.ID {
		t.Fatalf("workspace base=%q, want %q", w.Base(), current.ID)
	}
	writeTestFile(t, filepath.Join(directory, "a.bin"), "local a edit")
	writeTestFile(t, filepath.Join(directory, "b.bin"), "unrequested local b edit")
	if err := w.RestoreWithChoices(context.Background(), old.ID, []string{"a.bin"}, []project.Choice{{Path: "", Side: model.TargetSide}}); err != nil {
		t.Fatal(err)
	}
	if w.Base() != current.ID || w.Pending() != "" {
		t.Fatalf("restore changed base=%q or left pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "a.bin"), "old a")
	assertFileContents(t, filepath.Join(directory, "b.bin"), "unrequested local b edit")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"a.bin", "b.bin"}) {
		t.Fatalf("Status after scoped restore=%+v err=%v", changes, err)
	}
}

func TestUpdateWithChoicesRejectsOutOfScopeAndMissingFacts(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "choice-scope-base", "", []project.Change{makeFileChange("picked/file.bin", "base")}, map[string]string{"picked/file.bin": "base"})
	selection := []project.SelectionRule{{Path: "picked", Included: true}}
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", selection)
	updated := seed(t, remote, "choice-scope-target", base.ID, []project.Change{makeFileChange("picked/file.bin", "target")}, map[string]string{"picked/file.bin": "target"})
	for _, choice := range []project.Choice{
		{Path: "outside/file.bin", Side: model.TargetSide},
		{Path: "picked/missing.bin", Side: model.TargetSide},
	} {
		if err := w.UpdateWithChoices(context.Background(), []project.Choice{choice}); !errors.Is(err, model.ErrInvalidChoice) {
			t.Errorf("UpdateWithChoices(%q)=%v, want invalid choice", choice.Path, err)
		}
		if w.Base() != base.ID || w.Pending() != "" {
			t.Fatalf("invalid choice changed workspace state: base=%q pending=%q", w.Base(), w.Pending())
		}
	}
	if err := w.UpdateWithChoices(context.Background(), []project.Choice{{Path: "picked/file.bin", Side: model.TargetSide}}); err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID {
		t.Fatalf("valid choice did not advance base: %q", w.Base())
	}
}
