package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestSelectNoMatchingIncludeRefusesUnloadButEmptyAndNewSelectionWork(t *testing.T) {
	s, _, localDir, w := makeTransitionPair(t, map[string][]byte{
		"a.bin": []byte("saved a"),
		"b.bin": []byte("saved b"),
	}, nil)
	before := w.State()
	if err := w.Select(context.Background(), model.Selection{{Path: "misspelled.bin", Include: true}}); err == nil {
		t.Fatal("no-match selection unloaded a populated workspace")
	}
	if !reflect.DeepEqual(w.State(), before) {
		t.Fatalf("refused selection changed state: before=%#v after=%#v", before, w.State())
	}
	for name, want := range map[string][]byte{"a.bin": []byte("saved a"), "b.bin": []byte("saved b")} {
		if got := read(t, localDir, name); !bytes.Equal(got, want) {
			t.Errorf("refused selection changed %s: %q", name, got)
		}
	}
	assertNoWorkspaceJournal(t, localDir)

	// A newly created local file is a valid match even before it exists remotely.
	write(t, localDir, "new-local.bin", []byte("new local content"))
	if err := w.Select(context.Background(), model.Selection{{Path: "new-local.bin", Include: true}}); err != nil {
		t.Fatalf("select newly created local file in populated workspace: %v", err)
	}
	if got := read(t, localDir, "new-local.bin"); !bytes.Equal(got, []byte("new local content")) {
		t.Fatalf("new local selected bytes = %q", got)
	}
	for _, name := range []string{"a.bin", "b.bin"} {
		if _, err := os.Stat(filepath.Join(localDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("replacement selection retained %s: %v", name, err)
		}
	}
	commit := save(t, w)
	if got := readProjectFile(t, s.p, commit.ID, "new-local.bin"); !bytes.Equal(got, []byte("new local content")) {
		t.Fatalf("new local selected file was not published: %q", got)
	}
	if err := w.Select(context.Background(), model.Selection{}); err != nil {
		t.Fatalf("explicit empty selection: %v", err)
	}
	if len(w.State().Baseline) != 0 || len(w.State().Selection) != 0 {
		t.Fatalf("empty selection state = %#v", w.State())
	}
	if _, err := os.Stat(filepath.Join(localDir, "new-local.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit empty selection retained new-local.bin: %v", err)
	}
	assertNoWorkspaceJournal(t, localDir)

	emptyServer := newWorkspaceServer(t, nil)
	emptyDir := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(emptyDir, 0700); err != nil {
		t.Fatal(err)
	}
	empty := emptyServer.attach(t, emptyDir)
	defer closeWorkspace(t, empty)
	futureSelection := model.Selection{{Path: "planned/new.bin", Include: true}}
	if err := empty.Select(context.Background(), futureSelection); err != nil {
		t.Fatalf("select a path before the empty project contains it: %v", err)
	}
	write(t, emptyDir, "planned/new.bin", []byte("created after selection"))
	changes, err := empty.Status(context.Background())
	if err != nil || len(changes) != 1 || changes[0].Path != "planned/new.bin" {
		t.Fatalf("new selected local file status = %#v, %v", changes, err)
	}
	emptyCommit := save(t, empty)
	if got := readProjectFile(t, emptyServer.p, emptyCommit.ID, "planned/new.bin"); !bytes.Equal(got, []byte("created after selection")) {
		t.Fatalf("newly selected file was not published: %q", got)
	}
}

func TestRestoreExplicitPathsMustBeKnownAndValidSetIsApplied(t *testing.T) {
	_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{
		"a.bin":   []byte("historical a"),
		"removed": []byte("removed in current version"),
	}, nil)
	historical := w.State().Base
	write(t, sourceDir, "a.bin", []byte("current a"))
	if err := os.Remove(filepath.Join(sourceDir, "removed")); err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "present-only.bin", []byte("added in current version"))
	write(t, sourceDir, "deleted-only.bin", []byte("added then locally deleted"))
	current := publishWorkspace(t, sourceDir)
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatalf("advance workspace to current version: %v", err)
	}
	if w.State().Base != current {
		t.Fatalf("base = %q, want current version %q", w.State().Base, current)
	}

	beforeState := w.State()
	beforeA := read(t, localDir, "a.bin")
	beforePresentOnly := read(t, localDir, "present-only.bin")
	beforeDeletedOnly := read(t, localDir, "deleted-only.bin")
	mixed := model.Selection{{Path: "a.bin", Include: true}, {Path: "a.typo", Include: true}}
	if err := w.Restore(context.Background(), historical, mixed, false); err == nil {
		t.Fatal("restore accepted an explicit path absent from desired, local, and baseline entries")
	}
	if !reflect.DeepEqual(w.State(), beforeState) {
		t.Fatalf("refused mixed restore changed state: before=%#v after=%#v", beforeState, w.State())
	}
	for name, want := range map[string][]byte{
		"a.bin": beforeA, "present-only.bin": beforePresentOnly, "deleted-only.bin": beforeDeletedOnly,
	} {
		if got := read(t, localDir, name); !bytes.Equal(got, want) {
			t.Errorf("refused mixed restore changed %s: %q", name, got)
		}
	}
	assertNoWorkspaceJournal(t, localDir)

	if err := w.Restore(context.Background(), historical, model.Selection{{Path: "a.bin", Include: true}}, false); err != nil {
		t.Fatalf("restore a valid historical path: %v", err)
	}
	if got := read(t, localDir, "a.bin"); !bytes.Equal(got, []byte("historical a")) {
		t.Fatalf("restored bytes = %q", got)
	}
	if !reflect.DeepEqual(w.State(), beforeState) {
		t.Fatalf("restore changed baseline state: before=%#v after=%#v", beforeState, w.State())
	}

	// A local file that was added after the historical version is known through
	// observation and must be removed when explicitly restored to its absence.
	if err := w.Restore(context.Background(), historical, model.Selection{{Path: "present-only.bin", Include: true}}, false); err != nil {
		t.Fatalf("restore historical absence of locally present file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "present-only.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locally present historical-absent file remains: %v", err)
	}

	// Baseline knowledge keeps an already locally deleted path valid even though
	// it is absent from both the desired version and the current observation.
	if err := os.Remove(filepath.Join(localDir, "deleted-only.bin")); err != nil {
		t.Fatal(err)
	}
	deletedPath := model.Selection{{Path: "deleted-only.bin", Include: true}}
	if err := w.Restore(context.Background(), historical, deletedPath, false); err == nil || err.Error() != "restore would discard local changes; save them first or use -force" {
		t.Fatalf("restore locally deleted path without force = %v, want existing dirty-change refusal", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "deleted-only.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused restore changed locally deleted file: %v", err)
	}
	if err := w.Restore(context.Background(), historical, deletedPath, true); err != nil {
		t.Fatalf("restore locally deleted baseline path with force: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "deleted-only.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historically absent locally deleted file reappeared: %v", err)
	}
	assertNoWorkspaceJournal(t, localDir)
}

func assertNoWorkspaceJournal(t *testing.T, directory string) {
	t.Helper()
	for _, name := range []string{"save.json", "transition.json", "transition-progress.json", "transition"} {
		if _, err := os.Stat(filepath.Join(directory, ".lode", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("transition recovery artifact %s exists or stat failed: %v", name, err)
		}
	}
}
