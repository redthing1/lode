//go:build linux

package workspace

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestLoadFetchesOnlyRequestedPathAndKeepsBase(t *testing.T) {
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
	base := seed(t, remote, "load-base", "", []project.Change{
		makeFileChange("selected.txt", "selected"), makeFileChange("requested.txt", "load this"), makeFileChange("still-hidden.txt", "omit"),
	}, map[string]string{"selected.txt": "selected", "requested.txt": "load this", "still-hidden.txt": "omit"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", []project.SelectionRule{{Path: "selected.txt", Included: true}})
	mu.Lock()
	clear(downloads)
	mu.Unlock()
	headBefore, err := remote.client.Head(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Load(context.Background(), []string{"requested.txt"}); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(dir, "requested.txt"), "load this")
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	if head, err := remote.client.Head(context.Background(), "main"); err != nil || head != headBefore {
		t.Fatalf("server head moved: %q err=%v", head, err)
	}
	mu.Lock()
	if downloads["requested.txt"] != 1 || downloads["still-hidden.txt"] != 0 || downloads["selected.txt"] != 0 {
		t.Fatalf("payload downloads=%#v", downloads)
	}
	mu.Unlock()
	if got := baselinePaths(w.state.Baseline); !reflect.DeepEqual(got, []string{"requested.txt", "selected.txt"}) {
		t.Fatalf("baseline paths=%v", got)
	}
	if got := w.state.Selection; !selectionIncludes(t, got, "requested.txt") || selectionIncludes(t, got, "still-hidden.txt") {
		t.Fatalf("selection after load=%+v", got)
	}
}

func TestLoadAdoptsMatchingUnmanagedFileAndRejectsDivergentFile(t *testing.T) {
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
	base := seed(t, remote, "load-adopt-base", "", []project.Change{
		makeFileChange("selected.txt", "selected"), makeFileChange("matching.txt", "remote match"), makeFileChange("divergent.txt", "remote value"),
	}, map[string]string{"selected.txt": "selected", "matching.txt": "remote match", "divergent.txt": "remote value"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", []project.SelectionRule{{Path: "selected.txt", Included: true}})
	mu.Lock()
	clear(downloads)
	mu.Unlock()
	writeTestFile(t, filepath.Join(dir, "matching.txt"), "remote match")
	if err := w.Load(context.Background(), []string{"matching.txt"}); err != nil {
		t.Fatalf("Load matching local file: %v", err)
	}
	mu.Lock()
	if downloads["matching.txt"] != 0 {
		t.Fatalf("matching unmanaged file was redundantly downloaded: %#v", downloads)
	}
	mu.Unlock()
	assertFileContents(t, filepath.Join(dir, "matching.txt"), "remote match")
	oldBase := w.Base()
	oldSelection := append([]project.SelectionRule(nil), w.state.Selection...)
	oldBaseline := append([]project.Node(nil), w.state.Baseline...)
	writeTestFile(t, filepath.Join(dir, "divergent.txt"), "local divergent")
	if err := w.Load(context.Background(), []string{"divergent.txt"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Load divergent unmanaged file=%v, want conflict", err)
	}
	assertFileContents(t, filepath.Join(dir, "divergent.txt"), "local divergent")
	if w.Base() != oldBase || !reflect.DeepEqual(w.state.Selection, oldSelection) || !reflect.DeepEqual(w.state.Baseline, oldBaseline) || w.Pending() != "" {
		t.Fatalf("failed load changed workspace state: base=%q selection=%+v baseline=%+v pending=%q", w.Base(), w.state.Selection, w.state.Baseline, w.Pending())
	}
	if w.Base() != base.ID {
		t.Fatalf("base=%q, want %q", w.Base(), base.ID)
	}
}

func TestUnloadCleanPathDoesNotMoveBaseAndPreservesOtherFiles(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "unload-base", "", []project.Change{
		makeFileChange("remove.txt", "remove"), makeFileChange("keep.txt", "keep"),
	}, map[string]string{"remove.txt": "remove", "keep.txt": "keep"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", nil)
	if err := w.Unload(context.Background(), []string{"remove.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unloaded file stat=%v, want absent", err)
	}
	assertFileContents(t, filepath.Join(dir, "keep.txt"), "keep")
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("unload changed history: base=%q pending=%q", w.Base(), w.Pending())
	}
	if head, err := remote.client.Head(context.Background(), "main"); err != nil || head != base.ID {
		t.Fatalf("remote head=%q err=%v", head, err)
	}
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("Status after clean unload=%+v err=%v", changes, err)
	}
	if selectionIncludes(t, w.state.Selection, "remove.txt") {
		t.Fatalf("selection still includes unloaded path: %+v", w.state.Selection)
	}
}

func TestUnloadRejectsModifiedDeletedAndNewLocalFilesBeforeChanges(t *testing.T) {
	for _, scenario := range []string{"modified", "deleted", "new"} {
		t.Run(scenario, func(t *testing.T) {
			remote := newTestRemote(t, nil)
			seed(t, remote, "unload-conflict-"+scenario, "", []project.Change{makeFileChange("tracked.txt", "remote")}, map[string]string{"tracked.txt": "remote"})
			dir := filepath.Join(t.TempDir(), "workspace")
			w := createTestWorkspace(t, remote, dir, "main", nil)
			path := filepath.Join(dir, "tracked.txt")
			if scenario == "modified" {
				writeTestFile(t, path, "local")
			}
			if scenario == "deleted" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "new" {
				writeTestFile(t, filepath.Join(dir, "new-local.txt"), "new")
			}
			base := w.Base()
			selection := append([]project.SelectionRule(nil), w.state.Selection...)
			baseline := append([]project.Node(nil), w.state.Baseline...)
			target := "tracked.txt"
			if scenario == "new" {
				target = "new-local.txt"
			}
			if err := w.Unload(context.Background(), []string{target}); !errors.Is(err, ErrConflict) {
				t.Fatalf("Unload()=%v, want conflict", err)
			}
			if w.Base() != base || !reflect.DeepEqual(w.state.Selection, selection) || !reflect.DeepEqual(w.state.Baseline, baseline) || w.Pending() != "" {
				t.Fatal("failed unload changed selection, baseline, base, or journal")
			}
			switch scenario {
			case "modified":
				assertFileContents(t, path, "local")
			case "deleted":
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("deleted path changed: %v", err)
				}
			case "new":
				assertFileContents(t, filepath.Join(dir, "new-local.txt"), "new")
			}
		})
	}
}

func TestUnloadPreservesExcludedUnmanagedChild(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "unload-unmanaged-base", "", []project.Change{makeFileChange("folder/owned.txt", "owned")}, map[string]string{"folder/owned.txt": "owned"})
	dir := filepath.Join(t.TempDir(), "workspace")
	selection := []project.SelectionRule{{Path: "folder", Included: true}, {Path: "folder/unmanaged.txt", Included: false}}
	w := createTestWorkspace(t, remote, dir, "main", selection)
	writeTestFile(t, filepath.Join(dir, "folder", "unmanaged.txt"), "keep user file")
	if err := w.Unload(context.Background(), []string{"folder"}); err != nil {
		t.Fatal(err)
	}
	if w.Base() != base.ID {
		t.Fatalf("base changed during unload: %q", w.Base())
	}
	assertFileContents(t, filepath.Join(dir, "folder", "unmanaged.txt"), "keep user file")
	if _, err := os.Stat(filepath.Join(dir, "folder", "owned.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned path remains: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "folder")); err != nil || !info.IsDir() {
		t.Fatalf("unmanaged directory info=%v err=%v", info, err)
	}
}

func TestRestoreHistoricalPathKeepsCurrentBaseAndMakesNewCommitParent(t *testing.T) {
	remote := newTestRemote(t, nil)
	old := seed(t, remote, "restore-old", "", []project.Change{makeFileChange("target.txt", "old target"), makeFileChange("keep.txt", "old keep")}, map[string]string{"target.txt": "old target", "keep.txt": "old keep"})
	current := seed(t, remote, "restore-current", old.ID, []project.Change{makeFileChange("target.txt", "current target"), makeFileChange("keep.txt", "current keep"), makeFileChange("other.txt", "other")}, map[string]string{"target.txt": "current target", "keep.txt": "current keep", "other.txt": "other"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", nil)
	if w.Base() != current.ID {
		t.Fatalf("initial base=%q, want %q", w.Base(), current.ID)
	}
	if err := w.Restore(context.Background(), old.ID, []string{"target.txt"}); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(dir, "target.txt"), "old target")
	assertFileContents(t, filepath.Join(dir, "keep.txt"), "current keep")
	assertFileContents(t, filepath.Join(dir, "other.txt"), "other")
	if w.Base() != current.ID || w.Pending() != "" {
		t.Fatalf("restore changed base=%q or left pending=%q", w.Base(), w.Pending())
	}
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"target.txt"}) {
		t.Fatalf("Status after historical restore=%+v err=%v", changes, err)
	}
	commit, err := w.Commit(context.Background(), "author", "restore target from history")
	if err != nil {
		t.Fatal(err)
	}
	if len(commit.Parents) != 1 || commit.Parents[0] != current.ID {
		t.Fatalf("restoration commit parents=%v, want current base %s", commit.Parents, current.ID)
	}
	if w.Base() != commit.ID {
		t.Fatalf("base after commit=%q, want %q", w.Base(), commit.ID)
	}
}

func TestRestoreModifiedPathConflictsEvenWhenTargetEqualsCurrentBase(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "restore-current-base", "", []project.Change{makeFileChange("target.txt", "base"), makeFileChange("keep.txt", "keep")}, map[string]string{"target.txt": "base", "keep.txt": "keep"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", nil)
	writeTestFile(t, filepath.Join(dir, "target.txt"), "local divergent edit")
	if err := w.Restore(context.Background(), base.ID, []string{"target.txt"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Restore with local edit=%v, want conflict", err)
	}
	assertFileContents(t, filepath.Join(dir, "target.txt"), "local divergent edit")
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("failed restore changed base or created journal: base=%q pending=%q", w.Base(), w.Pending())
	}
}

func TestRestoreRootScopeDoesNotExpandHiddenPaths(t *testing.T) {
	remote := newTestRemote(t, nil)
	old := seed(t, remote, "restore-scope-old", "", []project.Change{makeFileChange("visible.txt", "old visible"), makeFileChange("hidden.txt", "old hidden")}, map[string]string{"visible.txt": "old visible", "hidden.txt": "old hidden"})
	current := seed(t, remote, "restore-scope-current", old.ID, []project.Change{makeFileChange("visible.txt", "current visible"), makeFileChange("hidden.txt", "current hidden")}, map[string]string{"visible.txt": "current visible", "hidden.txt": "current hidden"})
	dir := filepath.Join(t.TempDir(), "workspace")
	selection := []project.SelectionRule{{Included: true}, {Path: "hidden.txt", Included: false}}
	w := createTestWorkspace(t, remote, dir, "main", selection)
	if err := w.Restore(context.Background(), old.ID, []string{""}); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(dir, "visible.txt"), "old visible")
	if _, err := os.Stat(filepath.Join(dir, "hidden.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden path materialized by root restore: %v", err)
	}
	if w.Base() != current.ID {
		t.Fatalf("restore changed current base: %q", w.Base())
	}
	for _, node := range w.state.Baseline {
		if node.Path == "hidden.txt" {
			t.Fatal("restore expanded baseline into hidden selection")
		}
	}
}

func TestRestoreSourceFileAncestorTreatsRequestedChildAsAbsent(t *testing.T) {
	remote := newTestRemote(t, nil)
	source := seed(t, remote, "restore-parent-source", "", []project.Change{makeFileChange("folder", "parent file")}, map[string]string{"folder": "parent file"})
	current := seed(t, remote, "restore-parent-current", source.ID, []project.Change{{Path: "folder", Kind: model.Directory}, makeFileChange("folder/child.txt", "child")}, map[string]string{"folder/child.txt": "child"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", nil)
	if err := w.Restore(context.Background(), source.ID, []string{"folder/child.txt"}); err != nil {
		t.Fatalf("Restore child absent below historical file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "folder", "child.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historically absent child remains: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "folder")); err != nil || !info.IsDir() {
		t.Fatalf("current directory parent should remain: info=%v err=%v", info, err)
	}
	if w.Base() != current.ID || w.Pending() != "" {
		t.Fatalf("restore changed base/pending: %q %q", w.Base(), w.Pending())
	}
}

func TestRestoreHistoricalChildConflictsWithCurrentFileParent(t *testing.T) {
	remote := newTestRemote(t, nil)
	source := seed(t, remote, "restore-child-source", "", []project.Change{{Path: "folder", Kind: model.Directory}, makeFileChange("folder/child.txt", "historical child")}, map[string]string{"folder/child.txt": "historical child"})
	current := seed(t, remote, "restore-child-current", source.ID, []project.Change{{Path: "folder/child.txt", Kind: model.Absent}, makeFileChange("folder", "current file")}, map[string]string{"folder": "current file"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", nil)
	if err := w.Restore(context.Background(), source.ID, []string{"folder/child.txt"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Restore child beneath current file parent=%v, want conflict", err)
	}
	assertFileContents(t, filepath.Join(dir, "folder"), "current file")
	if w.Base() != current.ID || w.Pending() != "" {
		t.Fatalf("conflicting restore changed base/pending: %q %q", w.Base(), w.Pending())
	}
}

func TestInterruptedLoadResumesFromTransitionJournal(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "load-interrupted-base", "", []project.Change{makeFileChange("selected.txt", "selected"), makeFileChange("hidden.txt", "hidden")}, map[string]string{"selected.txt": "selected", "hidden.txt": "hidden"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, dir, "main", []project.SelectionRule{{Path: "selected.txt", Included: true}})
	injectTransitionFailure(t, w, "link")
	if err := w.Load(context.Background(), []string{"hidden.txt"}); err == nil {
		t.Fatal("Load succeeded despite injected post-install failure")
	}
	if w.Pending() != "transition" || w.Base() != base.ID {
		t.Fatalf("pending=%q base=%q", w.Pending(), w.Base())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume interrupted Load: %v", err)
	}
	if w.Pending() != "" || w.Base() != base.ID || !selectionIncludes(t, w.state.Selection, "hidden.txt") {
		t.Fatalf("resumed load state: base=%q pending=%q selection=%+v", w.Base(), w.Pending(), w.state.Selection)
	}
	assertFileContents(t, filepath.Join(dir, "hidden.txt"), "hidden")
}

func baselinePaths(nodes []project.Node) []string {
	result := make([]string, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node.Path)
	}
	return result
}

func selectionIncludes(t *testing.T, rules []project.SelectionRule, path string) bool {
	t.Helper()
	selection, err := project.Selection(rules)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := model.ParsePath(path)
	if err != nil {
		t.Fatal(err)
	}
	return selection.Includes(logical)
}
