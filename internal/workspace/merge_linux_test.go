package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

func mergeWorkspaces(t *testing.T) (*fixture, *Workspace, *Workspace, model.Version) {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "assets/a", "original")
	writeFile(t, dir, "references/keep", "omitted")
	source := attach(t, f, dir)
	base := save(t, f, source, "initial")
	if _, err := f.client.CreateBranch(ctx, "alice", "project", model.Reference{Name: "experiment", Version: base.ID}); err != nil {
		t.Fatal(err)
	}
	if err := source.Switch(ctx, f.token, "experiment"); err != nil {
		t.Fatal(err)
	}
	target := cloneWorkspace(t, f, model.Selection{{Path: "assets", Include: true}})
	writeFile(t, dir, "assets/a", "experiment")
	writeFile(t, dir, "references/keep", "changed without download")
	v := save(t, f, source, "experiment")
	return f, source, target, v
}

func TestMergeUpdatesOnlySelectedWorkspace(t *testing.T) {
	ctx := context.Background()
	f, _, w, source := mergeWorkspaces(t)
	base := w.State().Version
	writeFile(t, w.Directory(), "private/note", "unselected local work")
	v, err := w.Merge(ctx, f.token, "experiment", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Parent != base || v.MergeParent != source.ID || w.State().Version != v.ID {
		t.Fatalf("merge receipt/baseline: %+v", v)
	}
	if readFile(t, w.Directory(), "assets/a") != "experiment" || readFile(t, w.Directory(), "private/note") != "unselected local work" {
		t.Fatal("local contents")
	}
	if _, err := os.Stat(filepath.Join(w.Directory(), "references")); !os.IsNotExist(err) {
		t.Fatalf("downloaded omitted paths: %v", err)
	}
	for _, e := range w.State().Baseline {
		if !strings.HasPrefix(e.Path, "assets") {
			t.Fatalf("unselected baseline: %+v", e)
		}
	}
	if got := remoteFiles(t, f, v.ID); got["references/keep"] != "changed without download" {
		t.Fatalf("incomplete remote tree: %v", got)
	}
	if changes, err := w.Status(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("merge left dirty workspace: %v %v", changes, err)
	}
	if _, err := w.Merge(ctx, f.token, "experiment", "", nil); err == nil {
		t.Fatal("already integrated source published again")
	}
	if pending, _ := w.pending("save.json"); pending {
		t.Fatal("already integrated response stranded a save")
	}
}

func TestMergeConflictRetryAndDirtyRefusal(t *testing.T) {
	ctx := context.Background()
	f, _, w, source := mergeWorkspaces(t)
	writeFile(t, w.Directory(), "assets/a", "target")
	if _, err := w.Merge(ctx, f.token, "experiment", "", nil); err == nil || !strings.Contains(err.Error(), "save selected changes") {
		t.Fatalf("dirty merge: %v", err)
	}
	target := save(t, f, w, "target")
	_, err := w.Merge(ctx, f.token, "experiment", "", nil)
	var conflict *remote.Error
	if !errors.As(err, &conflict) || conflict.Code != "conflicts" || len(conflict.Conflicts) != 1 || conflict.Conflicts[0].Path != "assets/a" {
		t.Fatalf("conflicts: %v", err)
	}
	if pending, _ := w.pending("save.json"); pending {
		t.Fatal("conflict request not settled")
	}
	if w.State().Version != target.ID || readFile(t, w.Directory(), "assets/a") != "target" {
		t.Fatal("conflict changed workspace")
	}
	v, err := w.Merge(ctx, f.token, "experiment", "choose target", map[string]model.Side{"assets/a": model.Local})
	if err != nil {
		t.Fatal(err)
	}
	if v.MergeParent != source.ID || readFile(t, w.Directory(), "assets/a") != "target" {
		t.Fatal("whole-file choice ignored")
	}
	if got := remoteFiles(t, f, v.ID); got["assets/a"] != "target" || got["references/keep"] != "changed without download" {
		t.Fatalf("choices lost independent contents: %v", got)
	}
}

func TestMergeLostResponsePreservesLaterEditsAndImmutableSource(t *testing.T) {
	ctx := context.Background()
	f, source, w, sourceVersion := mergeWorkspaces(t)
	base := w.State().Version
	f.dropSave.Store(true)
	if _, err := w.Merge(ctx, f.token, "experiment", "integrate", nil); err == nil {
		t.Fatal("lost response succeeded")
	}
	var pending pendingSave
	if err := w.readJSON("save.json", &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Request.Merge != sourceVersion.ID {
		t.Fatal("request did not pin source")
	}
	writeFile(t, source.Directory(), "assets/a", "advanced source")
	save(t, f, source, "later experiment")
	writeFile(t, w.Directory(), "assets/a", "later local edit")
	// Outcome resolution and selected reads need only reader permission.
	reader, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "reader", Role: instance.Reader})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w, err = Open(w.Directory())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	err = w.Resume(ctx, reader)
	var conflict *ConflictsError
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "accepted") {
		t.Fatalf("post-acceptance conflict: %v", err)
	}
	if w.State().Version != base || readFile(t, w.Directory(), "assets/a") != "later local edit" {
		t.Fatal("accepted merge discarded later work")
	}
	if exists, _ := w.pending("save.json"); exists {
		t.Fatal("accepted response left unresolved save")
	}
	if err := w.Update(ctx, reader, map[string]model.Side{"assets/a": model.Local}); err != nil {
		t.Fatal(err)
	}
	if w.State().Version != pending.Request.ID || readFile(t, w.Directory(), "assets/a") != "later local edit" {
		t.Fatal("local reconciliation failed")
	}
	if got := remoteFiles(t, f, pending.Request.ID); got["assets/a"] != "experiment" {
		t.Fatal("retry changed immutable source")
	}
	if changes, err := w.Status(ctx); err != nil || len(changes) != 1 {
		t.Fatalf("later edit not unsaved: %v %v", changes, err)
	}
	history, err := f.client.History(ctx, "alice", "project", "main", 100)
	if err != nil || len(history.Versions) != 2 {
		t.Fatalf("duplicate merge: %v %v", history, err)
	}
}

func TestMergeCancellationSettlesPublicationWithoutRewritingFiles(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unaccepted", true: "accepted"}[accepted], func(t *testing.T) {
			ctx := context.Background()
			f, _, w, _ := mergeWorkspaces(t)
			base := w.State().Version
			if accepted {
				f.dropSave.Store(true)
			} else {
				f.refuseSave.Store(true)
			}
			if _, err := w.Merge(ctx, f.token, "experiment", "", nil); err == nil {
				t.Fatal("interrupted request succeeded")
			}
			var pending pendingSave
			if err := w.readJSON("save.json", &pending); err != nil {
				t.Fatal(err)
			}
			if err := w.Cancel(ctx, f.token); err != nil {
				t.Fatal(err)
			}
			if w.State().Version != base || readFile(t, w.Directory(), "assets/a") != "original" {
				t.Fatal("cancellation rewrote local state")
			}
			v, err := f.client.Save(ctx, "alice", "project", pending.Request, nil, nil)
			if accepted {
				if err != nil || v.ID != pending.Request.ID {
					t.Fatalf("accepted retry: %v %v", v, err)
				}
				// This also exercises reopening after receipt settlement but before
				// any local update plan exists: the old baseline is sufficient.
				dir := w.Directory()
				w.Close()
				w, err = Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				if err := w.Update(ctx, f.token, nil); err != nil {
					t.Fatal(err)
				}
				if readFile(t, dir, "assets/a") != "experiment" {
					t.Fatal("settled merge could not update")
				}
			} else {
				var failure *remote.Error
				if !errors.As(err, &failure) || failure.Code != "cancelled" {
					t.Fatalf("late merge published: %v", err)
				}
			}
		})
	}
}

func TestMergeAcceptedThenInterruptedLocalUpdate(t *testing.T) {
	ctx := context.Background()
	f, _, w, _ := mergeWorkspaces(t)
	f.dropFile.Store(true)
	v, err := w.Merge(ctx, f.token, "experiment", "", nil)
	if err == nil || v.ID == "" || !strings.Contains(err.Error(), "accepted") {
		t.Fatalf("accepted interrupted update: %+v %v", v, err)
	}
	if exists, _ := w.pending("save.json"); exists {
		t.Fatal("publication still pending")
	}
	if exists, _ := w.pending("transition.json"); !exists {
		t.Fatal("local continuation lost")
	}
	if err := w.Resume(ctx, f.token); err != nil {
		t.Fatal(err)
	}
	if w.State().Version != v.ID || readFile(t, w.Directory(), "assets/a") != "experiment" {
		t.Fatal("resumed merge update")
	}
}
