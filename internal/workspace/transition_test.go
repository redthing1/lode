package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func makeTransitionPair(t *testing.T, files map[string][]byte, selection model.Selection) (*workspaceServer, string, string, *Workspace) {
	t.Helper()
	s := newWorkspaceServer(t, nil)
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		write(t, sourceDir, name, data)
	}
	source := s.attach(t, sourceDir)
	save(t, source)
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, selection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeWorkspace(t, w) })
	return s, sourceDir, localDir, w
}

func publishWorkspace(t *testing.T, dir string) string {
	t.Helper()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	return save(t, w).ID
}

func transitionJournal(t *testing.T, dir string) transition {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".lode", "transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p transition
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	progress, err := os.ReadFile(filepath.Join(dir, ".lode", "transition-progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cursor transitionProgress
	if err := json.Unmarshal(progress, &cursor); err != nil {
		t.Fatal(err)
	}
	p.Next, p.Cancelled = cursor.Next, cursor.Cancelled
	return p
}

func TestUpdatePreservesLocalEditAndDoesNotFetchUnloadedFile(t *testing.T) {
	var readB atomic.Int32
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b" {
				readB.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("a one"))
	write(t, sourceDir, "b", []byte("b one"))
	source := s.attach(t, sourceDir)
	save(t, source)
	closeWorkspace(t, source)
	selection := model.Selection{{Include: true}, {Path: "b", Include: false}}
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	localEdit := []byte("local dirty a")
	write(t, localDir, "a", localEdit)
	write(t, sourceDir, "b", []byte("b two"))
	publishWorkspace(t, sourceDir)
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := read(t, localDir, "a"); !bytes.Equal(got, localEdit) {
		t.Fatalf("local edit = %q", got)
	}
	if got := readB.Load(); got != 0 {
		t.Fatalf("update fetched unloaded b %d times", got)
	}
	if _, err := os.Stat(filepath.Join(localDir, "b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unloaded b appeared: %v", err)
	}
}

func TestPartialStaleSaveCanUpdateThenSaveWithoutLosingOmittedChange(t *testing.T) {
	s := newWorkspaceServer(t, nil)
	fullDir := filepath.Join(t.TempDir(), "full")
	if err := os.Mkdir(fullDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, fullDir, "a", []byte("a one"))
	write(t, fullDir, "b", []byte("b one"))
	full := s.attach(t, fullDir)
	save(t, full)
	closeWorkspace(t, full)
	selection := model.Selection{{Include: true}, {Path: "b", Include: false}}
	partialDir := filepath.Join(t.TempDir(), "partial")
	partial, err := Clone(context.Background(), s.srv.URL, s.p.Token(), partialDir, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, partial)
	write(t, partialDir, "a", []byte("a local"))
	full, err = Open(fullDir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, fullDir, "b", []byte("b intervening"))
	save(t, full)
	closeWorkspace(t, full)
	if _, err := partial.Save(context.Background(), "tester", "stale"); err == nil {
		t.Fatal("stale save unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(partialDir, ".lode", "save.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale save journal remains: %v", err)
	}
	if err := partial.Update(context.Background(), nil); err != nil {
		t.Fatalf("update after stale save: %v", err)
	}
	commit := save(t, partial)
	if got := string(readProjectFile(t, s.p, commit.ID, "a")); got != "a local" {
		t.Fatalf("saved selected file = %q", got)
	}
	if got := string(readProjectFile(t, s.p, commit.ID, "b")); got != "b intervening" {
		t.Fatalf("omitted intervening file = %q", got)
	}
}

func TestUpdateConflictRequiresChoiceAndChoicesKeepWholeFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		ours bool
		want string
	}{{"ours", true, "local"}, {"theirs", false, "remote"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("base")}, nil)
			local := []byte("local")
			remote := []byte("remote")
			write(t, localDir, "a", local)
			write(t, sourceDir, "a", remote)
			publishWorkspace(t, sourceDir)
			base := w.State().Base
			if err := w.Update(context.Background(), nil); err == nil {
				t.Fatal("conflicting update unexpectedly succeeded")
			}
			if got := read(t, localDir, "a"); !bytes.Equal(got, local) {
				t.Fatalf("conflict mutated file: %q", got)
			}
			if w.State().Base != base {
				t.Fatal("conflict advanced baseline")
			}
			if _, err := os.Stat(filepath.Join(localDir, ".lode", "transition.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("conflict wrote journal: %v", err)
			}
			if err := w.Update(context.Background(), []model.Choice{{Path: "a", Ours: tc.ours}}); err != nil {
				t.Fatal(err)
			}
			if got := read(t, localDir, "a"); string(got) != tc.want {
				t.Fatalf("chosen bytes = %q, want %q", got, tc.want)
			}
			changes, err := w.Status()
			if err != nil {
				t.Fatal(err)
			}
			if tc.ours && (len(changes) != 1 || changes[0].Path != "a") {
				t.Fatalf("ours status = %#v", changes)
			}
			if !tc.ours && len(changes) != 0 {
				t.Fatalf("theirs status = %#v", changes)
			}
		})
	}
}

func TestSelectLoadsAndUnloadsSelectedPathsAndEmptyDirectories(t *testing.T) {
	var reads atomic.Int32
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" {
				reads.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("a"))
	write(t, sourceDir, "b", []byte("b"))
	if err := os.Mkdir(filepath.Join(sourceDir, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	source := s.attach(t, sourceDir)
	save(t, source)
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	initial := model.Selection{{Include: true}, {Path: "b", Include: false}, {Path: "empty", Include: false}}
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, initial)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	if reads.Load() != 1 {
		t.Fatalf("initial reads = %d", reads.Load())
	}
	all := model.Selection{{Include: true}}
	if err := w.Select(context.Background(), all); err != nil {
		t.Fatal(err)
	}
	if string(read(t, localDir, "b")) != "b" {
		t.Fatal("selected b was not installed")
	}
	if info, err := os.Stat(filepath.Join(localDir, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty dir: %v, %v", info, err)
	}
	if reads.Load() != 2 {
		t.Fatalf("selection read %d contents, want only newly selected b", reads.Load()-1)
	}
	if err := w.Select(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unloaded b remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "empty")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unloaded empty dir remains: %v", err)
	}
	if reads.Load() != 2 {
		t.Fatalf("unload fetched content: reads=%d", reads.Load())
	}
}

func TestSelectUnloadKeepsContainersForPhysicalUnselectedChild(t *testing.T) {
	var reads atomic.Int32
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" {
				reads.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	selected := []byte("selected payload")
	hidden := []byte("unselected payload")
	write(t, sourceDir, "one/two/selected/data.bin", selected)
	write(t, sourceDir, "one/two/hidden/deep/data.bin", hidden)
	source := s.attach(t, sourceDir)
	save(t, source)
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	selection := model.Selection{{Include: true}, {Path: "one/two/hidden", Include: false}}
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	if got := reads.Load(); got != 1 {
		t.Fatalf("initial selected content reads = %d, want 1", got)
	}
	// The excluded entry is locally present from before it became excluded.
	write(t, localDir, "one/two/hidden/deep/data.bin", hidden)
	if err := w.Select(context.Background(), model.Selection{}); err != nil {
		t.Fatalf("unload selected paths while preserving hidden child: %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("unload fetched content, reads=%d", got)
	}
	if _, err := os.Stat(filepath.Join(localDir, "one/two/selected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected directory remains: %v", err)
	}
	if got := read(t, localDir, "one/two/hidden/deep/data.bin"); !bytes.Equal(got, hidden) {
		t.Fatalf("hidden payload changed: %q", got)
	}
	for _, name := range []string{"one", "one/two", "one/two/hidden", "one/two/hidden/deep"} {
		info, err := os.Stat(filepath.Join(localDir, filepath.FromSlash(name)))
		if err != nil || !info.IsDir() {
			t.Errorf("needed container %s: info=%v err=%v", name, info, err)
		}
	}
	for _, name := range []string{"transition.json", "transition"} {
		if _, err := os.Stat(filepath.Join(localDir, ".lode", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("recovery artifact %s remains: %v", name, err)
		}
	}
}

func TestSelectRefusesDirtyDeletionUntrackedAndNewChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"dirty", func(t *testing.T, dir string) { write(t, dir, "a", []byte("dirty")) }},
		{"deletion", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "a")); err != nil {
				t.Fatal(err)
			}
		}},
		{"untracked", func(t *testing.T, dir string) { write(t, dir, "extra", []byte("extra")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, dir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("base")}, nil)
			tc.mutate(t, dir)
			if err := w.Select(context.Background(), model.Selection{}); err == nil {
				t.Fatal("selection discarded local change")
			}
			if _, err := os.Stat(filepath.Join(dir, ".lode", "transition.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal wrote journal: %v", err)
			}
		})
	}
}

func TestUpdateReplacesFileAndDirectoryAndPreflightsUnselectedChild(t *testing.T) {
	t.Run("file to directory", func(t *testing.T) {
		_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"node": []byte("file")}, nil)
		if err := os.Remove(filepath.Join(sourceDir, "node")); err != nil {
			t.Fatal(err)
		}
		write(t, sourceDir, "node/child", []byte("child"))
		publishWorkspace(t, sourceDir)
		if err := w.Update(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if string(read(t, localDir, "node/child")) != "child" {
			t.Fatal("child missing")
		}
	})
	t.Run("directory to file", func(t *testing.T) {
		_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"node/child": []byte("child")}, nil)
		if err := os.Remove(filepath.Join(sourceDir, "node/child")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(sourceDir, "node")); err != nil {
			t.Fatal(err)
		}
		write(t, sourceDir, "node", []byte("file"))
		publishWorkspace(t, sourceDir)
		if err := w.Update(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if string(read(t, localDir, "node")) != "file" {
			t.Fatal("file missing")
		}
	})
	t.Run("unselected child blocks", func(t *testing.T) {
		_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"node/selected": []byte("selected"), "node/hidden": []byte("hidden")}, model.Selection{{Path: "node", Include: true}, {Path: "node/hidden", Include: false}})
		if err := os.Remove(filepath.Join(sourceDir, "node/selected")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(sourceDir, "node/hidden")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(sourceDir, "node")); err != nil {
			t.Fatal(err)
		}
		write(t, sourceDir, "node", []byte("file"))
		publishWorkspace(t, sourceDir)
		write(t, localDir, "node/hidden", []byte("unselected local child"))
		if err := w.Update(context.Background(), nil); err == nil {
			t.Fatal("update replaced directory with unselected child")
		}
		if _, err := os.Stat(filepath.Join(localDir, ".lode", "transition.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preflight wrote journal: %v", err)
		}
		if got := string(read(t, localDir, "node/hidden")); got != "unselected local child" {
			t.Fatalf("obstruction changed: %q", got)
		}
	})
	t.Run("unselected parent container remains", func(t *testing.T) {
		var nodeReads atomic.Int32
		s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "node" {
					nodeReads.Add(1)
				}
				next.ServeHTTP(w, r)
			})
		})
		sourceDir := filepath.Join(t.TempDir(), "source")
		if err := os.Mkdir(sourceDir, 0700); err != nil {
			t.Fatal(err)
		}
		write(t, sourceDir, "node/selected", []byte("selected"))
		write(t, sourceDir, "node/hidden", []byte("hidden"))
		source := s.attach(t, sourceDir)
		save(t, source)
		closeWorkspace(t, source)
		selection := model.Selection{{Path: "node/selected", Include: true}}
		localDir := filepath.Join(t.TempDir(), "local")
		w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, selection)
		if err != nil {
			t.Fatal(err)
		}
		defer closeWorkspace(t, w)
		write(t, localDir, "node/hidden", []byte("unselected local child"))
		if err := os.Remove(filepath.Join(sourceDir, "node/selected")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(sourceDir, "node/hidden")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(sourceDir, "node")); err != nil {
			t.Fatal(err)
		}
		write(t, sourceDir, "node", []byte("remote file outside selection"))
		publishWorkspace(t, sourceDir)
		if err := w.Update(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(localDir, "node"))
		if err != nil || !info.IsDir() {
			t.Fatalf("unselected parent container: info=%v err=%v", info, err)
		}
		if got := string(read(t, localDir, "node/hidden")); got != "unselected local child" {
			t.Fatalf("unselected child changed: %q", got)
		}
		if _, err := os.Stat(filepath.Join(localDir, "node/selected")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("selected removed child remains: %v", err)
		}
		if got := nodeReads.Load(); got != 0 {
			t.Fatalf("update read unselected replacement file %d times", got)
		}
	})
}

func TestTransitionResumeCompletesAndCancelRestores(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel"}[cancel], func(t *testing.T) {
			var failB atomic.Bool
			s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b" && failB.Swap(false) {
						http.Error(w, "injected", http.StatusBadGateway)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			sourceDir := filepath.Join(t.TempDir(), "source")
			if err := os.Mkdir(sourceDir, 0700); err != nil {
				t.Fatal(err)
			}
			write(t, sourceDir, "a", []byte("old a"))
			write(t, sourceDir, "b", []byte("old b"))
			source := s.attach(t, sourceDir)
			base := save(t, source).ID
			closeWorkspace(t, source)
			localDir := filepath.Join(t.TempDir(), "local")
			w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			write(t, sourceDir, "a", []byte("new a"))
			write(t, sourceDir, "b", []byte("new b"))
			publishWorkspace(t, sourceDir)
			failB.Store(true)
			if err := w.Update(context.Background(), nil); err == nil {
				closeWorkspace(t, w)
				t.Fatal("expected second-file failure")
			}
			if got := string(read(t, localDir, "a")); got != "new a" {
				closeWorkspace(t, w)
				t.Fatalf("first change not installed: %q", got)
			}
			closeWorkspace(t, w)
			w, err = Open(localDir)
			if err != nil {
				t.Fatal(err)
			}
			defer closeWorkspace(t, w)
			if cancel {
				if err := w.Cancel(context.Background()); err != nil {
					t.Fatal(err)
				}
				if string(read(t, localDir, "a")) != "old a" || string(read(t, localDir, "b")) != "old b" {
					t.Fatal("cancel did not restore originals")
				}
				if w.State().Base != base {
					t.Fatal("cancel advanced baseline")
				}
			} else {
				if err := w.Resume(context.Background()); err != nil {
					t.Fatal(err)
				}
				if string(read(t, localDir, "a")) != "new a" || string(read(t, localDir, "b")) != "new b" {
					t.Fatal("resume did not finish update")
				}
				if w.State().Base == base {
					t.Fatal("resume did not advance baseline")
				}
			}
		})
	}
}

func TestTransitionPlanWithoutProgressRefusesResume(t *testing.T) {
	_, _, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("original")}, nil)
	plan := transition{Target: w.state}
	if err := w.store("transition.json", plan); err != nil {
		t.Fatal(err)
	}
	if err := w.Resume(context.Background()); err == nil {
		t.Fatal("resume accepted a plan with missing progress")
	}
	if got := string(read(t, localDir, "a")); got != "original" {
		t.Fatalf("resume mutated file: %q", got)
	}
	if _, err := os.Stat(filepath.Join(localDir, ".lode", "transition.json")); err != nil {
		t.Fatalf("plan was removed: %v", err)
	}
}

func TestOrphanTransitionProgressIsOverwrittenByNextOperation(t *testing.T) {
	_, sourceDir, _, w := makeTransitionPair(t, map[string][]byte{"a": []byte("one")}, nil)
	write(t, sourceDir, "a", []byte("two"))
	publishWorkspace(t, sourceDir)
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	orphan := transitionProgress{Next: 999, Cancelled: true}
	if err := w.store("transition-progress.json", orphan); err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("three"))
	publishWorkspace(t, sourceDir)
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatalf("next transition was affected by orphan progress: %v", err)
	}
}

func TestResumeUsesCommittedCursorToFinishCleanup(t *testing.T) {
	_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("old")}, nil)
	write(t, sourceDir, "a", []byte("new"))
	publishWorkspace(t, sourceDir)
	client, err := w.client()
	if err != nil {
		t.Fatal(err)
	}
	target := w.state
	target.Base, err = client.Head(context.Background(), target.Branch)
	if err != nil {
		t.Fatal(err)
	}
	target.Baseline, err = client.Entries(context.Background(), target.Base, target.Selection)
	if err != nil {
		t.Fatal(err)
	}
	changes := model.Diff(w.state.Baseline, target.Baseline)
	if err := w.root.Mkdir(".lode/transition", 0700); err != nil {
		t.Fatal(err)
	}
	if err := w.root.Rename("a", recoveryName(0, "old")); err != nil {
		t.Fatal(err)
	}
	write(t, localDir, "a", []byte("new"))
	plan := transition{Target: target, Changes: changes, Next: len(changes)}
	if err := w.storeTransitionProgress(&plan); err != nil {
		t.Fatal(err)
	}
	if err := w.store("transition.json", plan); err != nil {
		t.Fatal(err)
	}
	if err := w.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.state.Base != target.Base {
		t.Fatal("resume did not apply committed target state")
	}
	if got := string(read(t, localDir, "a")); got != "new" {
		t.Fatalf("installed contents = %q", got)
	}
	for _, name := range []string{"transition.json", "transition-progress.json", "transition"} {
		if _, err := os.Stat(filepath.Join(localDir, ".lode", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery artifact %s remains: %v", name, err)
		}
	}
}

func TestCancelPreservesLaterEditUntilMovedAside(t *testing.T) {
	var failB atomic.Bool
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b" && failB.Swap(false) {
				http.Error(w, "injected", http.StatusBadGateway)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("old a"))
	write(t, sourceDir, "b", []byte("old b"))
	source := s.attach(t, sourceDir)
	save(t, source)
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("new a"))
	write(t, sourceDir, "b", []byte("new b"))
	publishWorkspace(t, sourceDir)
	failB.Store(true)
	if err := w.Update(context.Background(), nil); err == nil {
		closeWorkspace(t, w)
		t.Fatal("expected interrupted update")
	}
	later := []byte("user edit")
	write(t, localDir, "a", later)
	if err := w.Cancel(context.Background()); err == nil {
		closeWorkspace(t, w)
		t.Fatal("cancel overwrote later edit")
	}
	if got := read(t, localDir, "a"); !bytes.Equal(got, later) {
		closeWorkspace(t, w)
		t.Fatalf("later edit changed: %q", got)
	}
	if err := os.Rename(filepath.Join(localDir, "a"), filepath.Join(localDir, "a.aside")); err != nil {
		closeWorkspace(t, w)
		t.Fatal(err)
	}
	if err := w.Cancel(context.Background()); err != nil {
		closeWorkspace(t, w)
		t.Fatalf("cancel after moving edit aside: %v", err)
	}
	if got := string(read(t, localDir, "a")); got != "old a" {
		closeWorkspace(t, w)
		t.Fatalf("restored a = %q", got)
	}
	if got := read(t, localDir, "a.aside"); !bytes.Equal(got, later) {
		closeWorkspace(t, w)
		t.Fatalf("aside edit = %q", got)
	}
	closeWorkspace(t, w)
}

func TestTransitionResumePreservesEditMadeAfterInstallBeforeProgress(t *testing.T) {
	_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("old")}, nil)
	write(t, sourceDir, "a", []byte("new"))
	publishWorkspace(t, sourceDir)
	client, err := w.client()
	if err != nil {
		t.Fatal(err)
	}
	target := w.state
	target.Base, err = client.Head(context.Background(), target.Branch)
	if err != nil {
		t.Fatal(err)
	}
	target.Baseline, err = client.Entries(context.Background(), target.Base, target.Selection)
	if err != nil {
		t.Fatal(err)
	}
	changes := model.Diff(w.state.Baseline, target.Baseline)
	if len(changes) != 1 {
		t.Fatalf("changes = %#v", changes)
	}
	if err := w.root.Mkdir(".lode/transition", 0700); err != nil {
		t.Fatal(err)
	}
	if err := w.applyChange(context.Background(), target.Base, changes[0], 0); err != nil {
		t.Fatal(err)
	}
	plan := transition{Target: target, Changes: changes, Next: 0}
	if err := w.storeTransitionProgress(&plan); err != nil {
		t.Fatal(err)
	}
	if err := w.store("transition.json", plan); err != nil {
		t.Fatal(err)
	}
	later := []byte("edit after install before progress")
	f, err := os.OpenFile(filepath.Join(localDir, "a"), os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(later); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, localDir, "a"); !bytes.Equal(got, later) {
		t.Fatalf("later edit lost: %q", got)
	}
	changesNow, err := w.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(changesNow) != 1 || changesNow[0].Path != "a" {
		t.Fatalf("status = %#v", changesNow)
	}
}
