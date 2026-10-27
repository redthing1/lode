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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestSparseMergeResolvesUnloadedConflictAndFetchesOnlySelectedFiles(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	downloads := make(map[string]int)
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
	base := seed(t, remote, "merge-base", "", []project.Change{
		makeFileChange("picked/file", "base"), makeFileChange("hidden/file", "old hidden"),
	}, map[string]string{"picked/file": "base", "hidden/file": "old hidden"})
	if err := remote.client.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatal(err)
	}
	ours := seed(t, remote, "merge-ours", base.ID, []project.Change{makeFileChange("hidden/file", "our hidden")}, map[string]string{"hidden/file": "our hidden"})
	theirs := applyTestChanges(t, remote, "merge-theirs", "feature", base.ID, []project.Change{
		makeFileChange("picked/file", "feature bytes\x00\xff"), makeFileChange("hidden/file", "their hidden"),
	}, map[string]string{"picked/file": "feature bytes\x00\xff", "hidden/file": "their hidden"})
	dir := filepath.Join(t.TempDir(), "work")
	w := createTestWorkspace(t, remote, dir, "main", []project.SelectionRule{{Path: "picked", Included: true}})
	preview, err := w.PreviewMerge(ctx, theirs.ID, nil, "", 1)
	if err != nil || preview.CommonBase != base.ID || !reflect.DeepEqual(preview.Conflicts, []string{"hidden/file"}) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if _, err := w.Merge(ctx, theirs.ID, "test", "unresolved", nil); !errors.Is(err, project.ErrMergeConflict) {
		t.Fatalf("unresolved merge=%v", err)
	}
	if w.Pending() != "" || w.Base() != ours.ID {
		t.Fatal("unresolved merge changed workspace")
	}
	commit, err := w.Merge(ctx, theirs.ID, "test", "selected merge", []project.Choice{{Path: "hidden/file", Side: model.TargetSide}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(commit.Parents, []string{ours.ID, theirs.ID}) || w.Base() != commit.ID || w.Pending() != "" {
		t.Fatalf("commit=%+v base=%s pending=%s", commit, w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(dir, "picked/file"), "feature bytes\x00\xff")
	if _, err := os.Lstat(filepath.Join(dir, "hidden")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hidden materialized: %v", err)
	}
	for _, node := range w.state.Baseline {
		if node.Path == "hidden" || node.Path == "hidden/file" {
			t.Fatal("unselected metadata in workspace baseline")
		}
	}
	mu.Lock()
	if downloads["hidden/file"] != 0 || downloads["picked/file"] != 2 {
		t.Errorf("downloads=%v", downloads)
	}
	mu.Unlock()
	if changes, err := w.Status(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("status=%v err=%v", changes, err)
	}
	reader, _, err := remote.client.OpenFile(ctx, commit.ID, "hidden/file")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err != nil || closeErr != nil || string(data) != "their hidden" {
		t.Fatalf("hidden contents=%q read=%v close=%v", data, err, closeErr)
	}
}

func TestMergeLostAcceptedReplyPreservesNewEditsAndNeedsSeparateLocalChoices(t *testing.T) {
	for _, side := range []model.Side{model.LocalSide, model.TargetSide} {
		t.Run(string(side), func(t *testing.T) {
			ctx := context.Background()
			var lose atomic.Bool
			remote := newTestRemote(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/merge" && lose.CompareAndSwap(true, false) {
						next.ServeHTTP(&lostResponseWriter{ResponseWriter: w, lost: true}, r)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			base := seed(t, remote, "lost-base", "", []project.Change{makeFileChange("file", "base")}, map[string]string{"file": "base"})
			if err := remote.client.CreateBranch(ctx, "feature", base.ID); err != nil {
				t.Fatal(err)
			}
			other := applyTestChanges(t, remote, "lost-other", "feature", base.ID, []project.Change{makeFileChange("file", "accepted bytes")}, map[string]string{"file": "accepted bytes"})
			dir := filepath.Join(t.TempDir(), "work")
			w := createTestWorkspace(t, remote, dir, "main", nil)
			lose.Store(true)
			if _, err := w.Merge(ctx, other.ID, "test", "lost reply", []project.Choice{{Side: model.TargetSide}}); err == nil {
				t.Fatal("expected lost response error")
			}
			if w.Pending() != "merge" || w.Base() != base.ID {
				t.Fatalf("pending=%s base=%s", w.Pending(), w.Base())
			}
			accepted, err := remote.client.Head(ctx, "main")
			if err != nil || accepted == base.ID {
				t.Fatalf("head=%s err=%v", accepted, err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			w, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			writeTestFile(t, filepath.Join(dir, "file"), "new local edit")
			writeTestFile(t, filepath.Join(dir, "new-file"), "new local addition")
			if _, err := w.Resume(ctx); !errors.Is(err, ErrConflict) {
				t.Fatalf("resume=%v, want local conflict", err)
			}
			if w.Pending() != "merge" || w.Base() != base.ID {
				t.Fatal("conflict lost accepted merge intent")
			}
			assertFileContents(t, filepath.Join(dir, "file"), "new local edit")
			commit, err := w.ResumeWithChoices(ctx, []project.Choice{{Path: "file", Side: side}})
			if err != nil || commit.ID != accepted || w.Base() != accepted || w.Pending() != "" {
				t.Fatalf("resume=%+v err=%v base=%s pending=%s", commit, err, w.Base(), w.Pending())
			}
			want := "accepted bytes"
			if side == model.LocalSide {
				want = "new local edit"
			}
			assertFileContents(t, filepath.Join(dir, "file"), want)
			assertFileContents(t, filepath.Join(dir, "new-file"), "new local addition")
			changes, err := w.Status(ctx)
			paths := []string{"new-file"}
			if side == model.LocalSide {
				paths = []string{"file", "new-file"}
			}
			if err != nil || !reflect.DeepEqual(changePaths(changes), paths) {
				t.Fatalf("changes=%v err=%v", changes, err)
			}
			history, _, err := remote.client.History(ctx, accepted, 10)
			if err != nil || len(history) != 2 || !reflect.DeepEqual(history[0].Parents, []string{base.ID, other.ID}) {
				t.Fatalf("history=%v err=%v", history, err)
			}
		})
	}
}

func TestMergeAbortBeforeArrivalBlocksDelayedPublication(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/merge" && fail.CompareAndSwap(true, false) {
				http.Error(w, "dropped before arrival", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "abort-merge-base", "", []project.Change{makeFileChange("file", "base")}, map[string]string{"file": "base"})
	if err := remote.client.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatal(err)
	}
	other := applyTestChanges(t, remote, "abort-merge-other", "feature", base.ID, []project.Change{makeFileChange("file", "other")}, map[string]string{"file": "other"})
	dir := filepath.Join(t.TempDir(), "work")
	w := createTestWorkspace(t, remote, dir, "main", nil)
	fail.Store(true)
	if _, err := w.Merge(ctx, other.ID, "test", "abort merge", nil); err == nil {
		t.Fatal("expected network error")
	}
	request := *w.state.Pending.Merge
	if err := w.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Pending() != "" || w.Base() != base.ID {
		t.Fatal("abort changed accepted state")
	}
	if _, err := remote.client.Merge(ctx, request); !errors.Is(err, project.ErrOperationAborted) {
		t.Fatalf("delayed request=%v", err)
	}
	assertFileContents(t, filepath.Join(dir, "file"), "base")
}

func TestMergeRequiresCleanWorkspaceAndResumesMaterialization(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/file" && r.URL.Query().Get("path") == "b" && fail.CompareAndSwap(true, false) {
				http.Error(w, "interrupted download", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "files-base", "", []project.Change{makeFileChange("a", "old a"), makeFileChange("b", "old b")}, map[string]string{"a": "old a", "b": "old b"})
	if err := remote.client.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatal(err)
	}
	other := applyTestChanges(t, remote, "files-other", "feature", base.ID, []project.Change{makeFileChange("a", "new a"), makeFileChange("b", "new b")}, map[string]string{"a": "new a", "b": "new b"})
	dir := filepath.Join(t.TempDir(), "work")
	w := createTestWorkspace(t, remote, dir, "main", nil)
	writeTestFile(t, filepath.Join(dir, "a"), "uncommitted")
	if _, err := w.Merge(ctx, other.ID, "test", "dirty merge", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("dirty merge=%v", err)
	}
	if w.Pending() != "" {
		t.Fatal("dirty merge reserved a journal")
	}
	writeTestFile(t, filepath.Join(dir, "a"), "old a")
	fail.Store(true)
	commit, err := w.Merge(ctx, other.ID, "test", "materialize merge", nil)
	if err == nil || commit.ID == "" || w.Pending() != "transition" || w.Base() != base.ID {
		t.Fatalf("merge=%+v err=%v base=%s pending=%s", commit, err, w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(dir, "a"), "new a")
	assertFileContents(t, filepath.Join(dir, "b"), "old b")
	writeTestFile(t, filepath.Join(dir, "a"), "newer local a")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.ResumeWithChoices(ctx, []project.Choice{{Side: model.TargetSide}}); err == nil {
		t.Fatal("choices changed a sealed transition")
	}
	if _, err := w.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if w.Base() != commit.ID || w.Pending() != "" {
		t.Fatal("merge did not finish")
	}
	assertFileContents(t, filepath.Join(dir, "a"), "newer local a")
	assertFileContents(t, filepath.Join(dir, "b"), "new b")
}
