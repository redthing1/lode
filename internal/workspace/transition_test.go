//go:build linux

package workspace

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestUpdateMaterializesFilesDeletionsAndEmptyDirectories(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "update-base", "", []project.Change{
		makeFileChange("keep.txt", "old"),
		makeFileChange("remove.txt", "remove me"),
		{Path: "old-empty", Kind: model.Directory},
	}, map[string]string{"keep.txt": "old", "remove.txt": "remove me"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)

	updated := seed(t, remote, "update-target", base.ID, []project.Change{
		makeFileChange("keep.txt", "updated"),
		{Path: "remove.txt", Kind: model.Absent},
		makeFileChange("new/deep/file.txt", "new file"),
		{Path: "new-empty", Kind: model.Directory},
		{Path: "old-empty", Kind: model.Absent},
	}, map[string]string{"keep.txt": "updated", "new/deep/file.txt": "new file"})
	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q, want %q and no pending operation", w.Base(), w.Pending(), updated.ID)
	}
	assertFileContents(t, filepath.Join(directory, "keep.txt"), "updated")
	assertFileContents(t, filepath.Join(directory, "new/deep/file.txt"), "new file")
	for _, path := range []string{"remove.txt", "old-empty"} {
		if _, err := os.Lstat(filepath.Join(directory, path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("deleted path %q remains or has unexpected error: %v", path, err)
		}
	}
	for _, path := range []string{"new", "new/deep", "new-empty"} {
		info, err := os.Stat(filepath.Join(directory, path))
		if err != nil || !info.IsDir() {
			t.Errorf("directory %q: info=%v err=%v", path, info, err)
		}
	}
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("Status after clean update=%+v err=%v", changes, err)
	}
}

func TestUpdatePreservesDisjointLocalEditAgainstNewBase(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "disjoint-base", "", []project.Change{
		makeFileChange("local.txt", "base local"), makeFileChange("remote.txt", "base remote"),
	}, map[string]string{"local.txt": "base local", "remote.txt": "base remote"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	writeTestFile(t, filepath.Join(directory, "local.txt"), "local edit")
	updated := seed(t, remote, "disjoint-target", base.ID, []project.Change{
		makeFileChange("remote.txt", "remote update"),
	}, map[string]string{"remote.txt": "remote update"})

	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "local.txt"), "local edit")
	assertFileContents(t, filepath.Join(directory, "remote.txt"), "remote update")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"local.txt"}) {
		t.Fatalf("Status after update=%+v err=%v", changes, err)
	}
}

func TestUpdateDivergentEditConflictsBeforeWriting(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "divergent-base", "", []project.Change{makeFileChange("file.bin", "base")}, map[string]string{"file.bin": "base"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	writeTestFile(t, filepath.Join(directory, "file.bin"), "local edit")
	updated := seed(t, remote, "divergent-target", base.ID, []project.Change{makeFileChange("file.bin", "remote edit")}, map[string]string{"file.bin": "remote edit"})

	err := w.Update(context.Background())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update()=%v, want ErrConflict", err)
	}
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("conflict changed workspace state: base=%q target=%q pending=%q", w.Base(), updated.ID, w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "local edit")
}

func TestUpdateLocalChildConflictsWithRemoteDirectoryDeletion(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "delete-dir-base", "", []project.Change{
		makeFileChange("folder/original.txt", "original"),
	}, map[string]string{"folder/original.txt": "original"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	writeTestFile(t, filepath.Join(directory, "folder/local.txt"), "local child")
	updated := seed(t, remote, "delete-dir-target", base.ID, []project.Change{
		{Path: "folder/original.txt", Kind: model.Absent}, {Path: "folder", Kind: model.Absent},
	}, nil)

	err := w.Update(context.Background())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update()=%v, want ErrConflict", err)
	}
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("conflict changed workspace state: base=%q target=%q pending=%q", w.Base(), updated.ID, w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "folder/original.txt"), "original")
	assertFileContents(t, filepath.Join(directory, "folder/local.txt"), "local child")
}

func TestUpdateOrdersFileDirectoryReplacements(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "type-base", "", []project.Change{
		makeFileChange("item", "file contents"), makeFileChange("folder/child", "child contents"),
	}, map[string]string{"item": "file contents", "folder/child": "child contents"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	updated := seed(t, remote, "type-target", base.ID, []project.Change{
		{Path: "item", Kind: model.Directory}, makeFileChange("item/inside", "inside"),
		{Path: "folder/child", Kind: model.Absent}, makeFileChange("folder", "folder now a file"),
	}, map[string]string{"item/inside": "inside", "folder": "folder now a file"})

	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	if info, err := os.Stat(filepath.Join(directory, "item")); err != nil || !info.IsDir() {
		t.Fatalf("item should be a directory: info=%v err=%v", info, err)
	}
	assertFileContents(t, filepath.Join(directory, "item/inside"), "inside")
	assertFileContents(t, filepath.Join(directory, "folder"), "folder now a file")
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("Status after type replacements=%+v err=%v", changes, err)
	}
}

func TestSwitchToExistingForkedBranchUsesItsExactMetadata(t *testing.T) {
	remote := newTestRemote(t, nil)
	fork := seed(t, remote, "fork-base", "", []project.Change{
		makeFileChange("shared.txt", "fork point"), makeFileChange("main-only.txt", "base-only"),
	}, map[string]string{"shared.txt": "fork point", "main-only.txt": "base-only"})
	if err := remote.client.CreateBranch(context.Background(), "feature", fork.ID); err != nil {
		t.Fatal(err)
	}
	mainTip := seed(t, remote, "main-tip", fork.ID, []project.Change{
		makeFileChange("shared.txt", "main version"), {Path: "main-only.txt", Kind: model.Absent},
	}, map[string]string{"shared.txt": "main version"})
	featureTip := applyTestChanges(t, remote, "feature-tip", "feature", fork.ID, []project.Change{
		makeFileChange("shared.txt", "feature version"), makeFileChange("feature-only.txt", "feature-only"),
	}, map[string]string{"shared.txt": "feature version", "feature-only.txt": "feature-only"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	if w.Base() != mainTip.ID {
		t.Fatalf("workspace base=%q, want main tip %q", w.Base(), mainTip.ID)
	}

	if err := w.Switch(context.Background(), "feature"); err != nil {
		t.Fatal(err)
	}
	if w.Branch() != "feature" || w.Base() != featureTip.ID || w.Pending() != "" {
		t.Fatalf("branch=%q base=%q pending=%q", w.Branch(), w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "shared.txt"), "feature version")
	assertFileContents(t, filepath.Join(directory, "main-only.txt"), "base-only")
	assertFileContents(t, filepath.Join(directory, "feature-only.txt"), "feature-only")
	entries, _, err := remote.client.Entries(context.Background(), featureTip.ID, []project.SelectionRule{{Included: true}}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.state.Baseline, entries) {
		t.Fatalf("workspace baseline differs from feature tree:\n%#v\n%#v", w.state.Baseline, entries)
	}
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("Status after switch=%+v err=%v", changes, err)
	}
}

func TestSparseUpdateDoesNotDownloadUnselectedPayloads(t *testing.T) {
	var mu sync.Mutex
	downloads := map[string]int{}
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/file" {
				mu.Lock()
				downloads[r.URL.Query().Get("path")]++
				mu.Unlock()
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "sparse-update-base", "", []project.Change{
		makeFileChange("selected/keep.bin", "keep"), makeFileChange("unselected/secret.bin", "secret"),
	}, map[string]string{"selected/keep.bin": "keep", "unselected/secret.bin": "secret"})
	selection := []project.SelectionRule{{Path: "selected", Included: true}}
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", selection)
	updated := seed(t, remote, "sparse-update-target", base.ID, []project.Change{
		makeFileChange("selected/new.bin", "new"), makeFileChange("unselected/secret.bin", "secret update"),
	}, map[string]string{"selected/new.bin": "new", "unselected/secret.bin": "secret update"})
	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID {
		t.Fatalf("base=%q, want %q", w.Base(), updated.ID)
	}
	assertFileContents(t, filepath.Join(directory, "selected/new.bin"), "new")
	if _, err := os.Lstat(filepath.Join(directory, "unselected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unselected directory unexpectedly materialized: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if downloads["unselected/secret.bin"] != 0 || downloads["selected/new.bin"] != 1 {
		t.Fatalf("file download counts=%#v", downloads)
	}
}

func TestRemoteDirectoryDeletionPreservesUnmanagedExcludedChild(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "unmanaged-delete-base", "", []project.Change{makeFileChange("folder/owned.txt", "owned")}, map[string]string{"folder/owned.txt": "owned"})
	selection := []project.SelectionRule{{Path: "folder", Included: true}, {Path: "folder/unmanaged.txt", Included: false}}
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", selection)
	writeTestFile(t, filepath.Join(directory, "folder/unmanaged.txt"), "user data")
	updated := seed(t, remote, "unmanaged-delete-target", base.ID, []project.Change{
		{Path: "folder/owned.txt", Kind: model.Absent}, {Path: "folder", Kind: model.Absent},
	}, nil)
	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != updated.ID {
		t.Fatalf("base=%q, want %q", w.Base(), updated.ID)
	}
	if info, err := os.Stat(filepath.Join(directory, "folder")); err != nil || !info.IsDir() {
		t.Fatalf("folder with unmanaged child should remain: info=%v err=%v", info, err)
	}
	assertFileContents(t, filepath.Join(directory, "folder/unmanaged.txt"), "user data")
	if _, err := os.Lstat(filepath.Join(directory, "folder/owned.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracked child was not removed: %v", err)
	}
}

func TestRemoteDirectoryToFileRejectsUnmanagedChild(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "unmanaged-replace-base", "", []project.Change{makeFileChange("folder/owned.txt", "owned")}, map[string]string{"folder/owned.txt": "owned"})
	selection := []project.SelectionRule{{Path: "folder", Included: true}, {Path: "folder/unmanaged.txt", Included: false}}
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", selection)
	writeTestFile(t, filepath.Join(directory, "folder/unmanaged.txt"), "user data")
	updated := seed(t, remote, "unmanaged-replace-target", base.ID, []project.Change{
		{Path: "folder/owned.txt", Kind: model.Absent}, makeFileChange("folder", "replacement file"),
	}, map[string]string{"folder": "replacement file"})

	err := w.Update(context.Background())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update()=%v, want ErrConflict", err)
	}
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("rejected replacement changed state: base=%q target=%q pending=%q", w.Base(), updated.ID, w.Pending())
	}
	if info, err := os.Stat(filepath.Join(directory, "folder")); err != nil || !info.IsDir() {
		t.Fatalf("folder changed despite conflict: info=%v err=%v", info, err)
	}
	assertFileContents(t, filepath.Join(directory, "folder/owned.txt"), "owned")
	assertFileContents(t, filepath.Join(directory, "folder/unmanaged.txt"), "user data")
}

func TestTransitionMutationBoundaryFailuresResumeAfterReopen(t *testing.T) {
	for _, operation := range []string{"move", "link"} {
		t.Run(operation, func(t *testing.T) {
			remote := newTestRemote(t, nil)
			base := seed(t, remote, "boundary-base-"+operation, "", []project.Change{makeFileChange("file.bin", "old")}, map[string]string{"file.bin": "old"})
			directory := filepath.Join(t.TempDir(), "workspace")
			w := createTestWorkspace(t, remote, directory, "main", nil)
			updated := seed(t, remote, "boundary-target-"+operation, base.ID, []project.Change{makeFileChange("file.bin", "new")}, map[string]string{"file.bin": "new"})
			injectTransitionFailure(t, w, operation)
			if err := w.Update(context.Background()); err == nil {
				t.Fatal("Update succeeded despite injected post-mutation failure")
			}
			if w.Pending() != "transition" {
				t.Fatalf("pending=%q, want transition", w.Pending())
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			w, err := Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Close() })
			if _, err := w.Resume(context.Background()); err != nil {
				t.Fatalf("Resume after reopen: %v", err)
			}
			if w.Base() != updated.ID || w.Pending() != "" {
				t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
			}
			assertFileContents(t, filepath.Join(directory, "file.bin"), "new")
		})
	}
}

func TestTransitionInstallThenLocalEditSurvivesResume(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "install-edit-base", "", []project.Change{makeFileChange("file.bin", "old")}, map[string]string{"file.bin": "old"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	updated := seed(t, remote, "install-edit-target", base.ID, []project.Change{makeFileChange("file.bin", "new")}, map[string]string{"file.bin": "new"})
	injected := false
	w.linkEntry = func(from, to string) error {
		if err := linkNoReplace(w.root, from, to); err != nil {
			return err
		}
		if !injected {
			injected = true
			return errors.New("injected after install")
		}
		return nil
	}
	if err := w.Update(context.Background()); err == nil {
		t.Fatal("Update succeeded despite injected after-install failure")
	}
	if !injected || w.Pending() != "transition" {
		t.Fatalf("injected=%t pending=%q", injected, w.Pending())
	}
	writeTestFile(t, filepath.Join(directory, "file.bin"), "newer local edit")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume after local edit: %v", err)
	}
	if w.Base() != updated.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "newer local edit")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"file.bin"}) {
		t.Fatalf("Status after local edit recovery=%+v err=%v", changes, err)
	}
}

func TestUnexpectedFileAfterOldMoveIsPreservedAndResumeCanContinue(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "unexpected-base", "", []project.Change{makeFileChange("file.bin", "old")}, map[string]string{"file.bin": "old"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	updated := seed(t, remote, "unexpected-target", base.ID, []project.Change{makeFileChange("file.bin", "new")}, map[string]string{"file.bin": "new"})
	injectTransitionFailure(t, w, "move")
	if err := w.Update(context.Background()); err == nil {
		t.Fatal("Update succeeded despite injected post-move failure")
	}
	writeTestFile(t, filepath.Join(directory, "file.bin"), "unexpected user file")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.Resume(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Resume with unexpected file=%v, want ErrConflict", err)
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "unexpected user file")
	if w.Base() != base.ID || w.Pending() != "transition" {
		t.Fatalf("unexpected path changed state: base=%q pending=%q", w.Base(), w.Pending())
	}
	if err := os.Remove(filepath.Join(directory, "file.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume after moving unexpected file aside: %v", err)
	}
	if w.Base() != updated.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "new")
}

func TestModifiedTransitionBackupSurvivesConflict(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "backup-base", "", []project.Change{makeFileChange("file.bin", "old")}, map[string]string{"file.bin": "old"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	seed(t, remote, "backup-target", base.ID, []project.Change{makeFileChange("file.bin", "new")}, map[string]string{"file.bin": "new"})
	injectTransitionFailure(t, w, "move")
	if err := w.Update(context.Background()); err == nil {
		t.Fatal("Update succeeded despite injected post-move failure")
	}
	oldPath, _ := actionFiles(w.state.Pending.Transition, 0)
	backup := filepath.Join(directory, filepath.FromSlash(oldPath))
	writeTestFile(t, backup, "modified backup")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.Resume(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Resume with modified backup=%v, want ErrConflict", err)
	}
	assertFileContents(t, backup, "modified backup")
	if _, err := os.Lstat(filepath.Join(directory, "file.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target path changed unexpectedly: %v", err)
	}
	if w.Base() != base.ID || w.Pending() != "transition" {
		t.Fatalf("backup conflict changed state: base=%q pending=%q", w.Base(), w.Pending())
	}
}

func createTestWorkspace(t *testing.T, remote *testRemote, directory, branch string, selection []project.SelectionRule) *Workspace {
	t.Helper()
	w, err := Create(context.Background(), Options{
		Directory: directory, Remote: remote.server.URL, Token: remote.local.Token,
		Branch: branch, Selection: selection,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func applyTestChanges(t *testing.T, remote *testRemote, operation, branch, base string, changes []project.Change, files map[string]string) project.Commit {
	t.Helper()
	commit, err := remote.client.Apply(context.Background(), project.ChangeRequest{
		OperationID: operation, Branch: branch, Base: base, Author: "test", Message: operation, Changes: changes,
	}, func(path model.Path) (io.ReadCloser, error) {
		data, ok := files[path.String()]
		if !ok {
			return nil, errors.New("missing test payload")
		}
		return io.NopCloser(strings.NewReader(data)), nil
	})
	if err != nil {
		t.Fatalf("apply test changes: %v", err)
	}
	return commit
}

func injectTransitionFailure(t *testing.T, w *Workspace, operation string) {
	t.Helper()
	injected := false
	if operation == "move" {
		w.moveEntry = func(from, to string) error {
			if err := moveNoReplace(w.root, from, to); err != nil {
				return err
			}
			if !injected {
				injected = true
				return errors.New("injected after move")
			}
			return nil
		}
		return
	}
	w.linkEntry = func(from, to string) error {
		if err := linkNoReplace(w.root, from, to); err != nil {
			return err
		}
		if !injected {
			injected = true
			return errors.New("injected after link")
		}
		return nil
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != expected {
		t.Fatalf("read %q = %q, %v; want %q", path, got, err, expected)
	}
}

func changePaths(changes []project.Change) []string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	return paths
}
