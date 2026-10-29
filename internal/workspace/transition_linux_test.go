package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func cloneWorkspace(t *testing.T, f *fixture, selection model.Selection) *Workspace {
	t.Helper()
	w, err := Clone(context.Background(), f.url, f.client, filepath.Join(t.TempDir(), "work"), ConnectOptions{Selection: selection})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestUpdatePreservesIndependentEditsAndResolvesConflicts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "a", "initial a")
	writeFile(t, dir, "b", "initial b")
	writeFile(t, dir, "omitted/file", "untouched")
	a := attach(t, f, dir)
	save(t, f, a, "initial")
	b := cloneWorkspace(t, f, model.Selection{{Include: true}, {Path: "omitted", Include: false}})
	writeFile(t, dir, "a", "Alice's edit")
	save(t, f, a, "Alice")
	writeFile(t, b.Directory(), "b", "Bob's edit")
	if _, err := b.Save(ctx, f.token, "stale Bob"); err == nil {
		t.Fatal("stale save accepted")
	}
	if err := b.Update(ctx, f.token, nil); err != nil {
		t.Fatal(err)
	}
	if readFile(t, b.Directory(), "a") != "Alice's edit" || readFile(t, b.Directory(), "b") != "Bob's edit" {
		t.Fatal("independent work lost")
	}
	changes, err := b.Status(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "b" {
		t.Fatalf("remaining local changes: %v %v", changes, err)
	}
	v := save(t, f, b, "Bob")
	if got := remoteFiles(t, f, v.ID); got["omitted/file"] != "untouched" || got["b"] != "Bob's edit" {
		t.Fatal(got)
	}
	if err := a.Update(ctx, f.token, nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a", "incoming conflict")
	save(t, f, a, "conflict")
	writeFile(t, b.Directory(), "a", "local conflict")
	baseline := stateDigest(b.State())
	var conflicts *ConflictsError
	if err := b.Update(ctx, f.token, nil); !errors.As(err, &conflicts) || len(conflicts.Conflicts) != 1 || conflicts.Conflicts[0].Path != "a" {
		t.Fatalf("conflict: %v", err)
	}
	if stateDigest(b.State()) != baseline || readFile(t, b.Directory(), "a") != "local conflict" {
		t.Fatal("conflict changed workspace")
	}
	if err := b.Update(ctx, f.token, map[string]model.Side{"a": model.Local}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, b.Directory(), "a") != "local conflict" {
		t.Fatal("local choice lost")
	}
	v = save(t, f, b, "resolved locally")
	if got := remoteFiles(t, f, v.ID)["a"]; got != "local conflict" {
		t.Fatal(got)
	}
}

func TestSelectionPreservesDirtyAndUnselectedFiles(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "assets/a", "asset")
	writeFile(t, dir, "references/b", "reference")
	a := attach(t, f, dir)
	save(t, f, a, "initial")
	b := cloneWorkspace(t, f, model.Selection{{Path: "assets", Include: true}, {Path: "assets/private", Include: false}})
	writeFile(t, b.Directory(), "assets/private", "local unselected work")
	writeFile(t, b.Directory(), "assets/a", "dirty asset")
	references := model.Selection{{Path: "references", Include: true}}
	if err := b.Select(ctx, f.token, references, nil); err == nil {
		t.Fatal("unloaded dirty file")
	}
	if readFile(t, b.Directory(), "assets/a") != "dirty asset" {
		t.Fatal("lost dirty file")
	}
	save(t, f, b, "asset edit")
	if err := b.Select(ctx, f.token, references, nil); err != nil {
		t.Fatal(err)
	}
	if readFile(t, b.Directory(), "references/b") != "reference" || readFile(t, b.Directory(), "assets/private") != "local unselected work" {
		t.Fatal("wrong selection contents")
	}
	if _, err := os.Stat(filepath.Join(b.Directory(), "assets/a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean unloaded asset: %v", err)
	}
	if err := b.Select(ctx, f.token, model.Selection{{Path: "typo", Include: true}}, nil); err == nil {
		t.Fatal("empty accidental selection accepted")
	}
	writeFile(t, b.Directory(), "assets/a", "new local collision")
	var conflict *ConflictsError
	if err := b.Select(ctx, f.token, model.Selection{{Include: true}}, nil); !errors.As(err, &conflict) {
		t.Fatalf("load collision: %v", err)
	}
	if err := b.Select(ctx, f.token, model.Selection{{Include: true}}, map[string]model.Side{"assets/a": model.Local}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, b.Directory(), "assets/a") != "new local collision" {
		t.Fatal("lost collision choice")
	}
	if err := b.Select(ctx, f.token, model.Selection{}, nil); err == nil {
		t.Fatal("unloaded changed/new files")
	}
	save(t, f, b, "local additions")
	if err := b.Select(ctx, f.token, model.Selection{}, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(b.Directory())
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lode" {
		t.Fatalf("unload all: %v %v", entries, err)
	}
}

// This subprocess exits after an actual filesystem action, before advancing its
// durable cursor. No cleanup/deferred close runs in the interrupted process.
func TestTransitionProcessExit(t *testing.T) {
	directory := os.Getenv("LODE_TEST_TRANSITION")
	if directory == "" {
		return
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	var plan transition
	if err := w.readJSON("transition.json", &plan); err != nil {
		t.Fatal(err)
	}
	boundary, err := strconv.Atoi(os.Getenv("LODE_TEST_BOUNDARY"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := w.Client(os.Getenv("LODE_TEST_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LODE_TEST_PREFETCH") == "1" {
		if err := w.prepareFiles(context.Background(), c, plan, 0, len(plan.Changes)); err != nil {
			t.Fatal(err)
		}
		os.Exit(73)
	}
	if os.Getenv("LODE_TEST_REVERSE") == "1" {
		var progress transitionProgress
		if err := w.readJSON("progress.json", &progress); err != nil {
			t.Fatal(err)
		}
		progress.Reverse = true
		if err := w.writeJSON("progress.json", progress); err != nil {
			t.Fatal(err)
		}
		for progress.Cursor >= boundary {
			if err := w.reverseAction(context.Background(), plan.Changes[progress.Cursor], progress.Cursor); err != nil {
				t.Fatal(err)
			}
			if progress.Cursor == boundary {
				os.Exit(73)
			}
			progress.Cursor--
			if err := w.writeJSON("progress.json", progress); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < len(plan.Changes); i++ {
		if i == boundary && os.Getenv("LODE_TEST_DETACHED") == "1" {
			a := plan.Changes[i]
			if err := w.prepareEntry(context.Background(), c, plan.Target.Version, a, i); err != nil {
				t.Fatal(err)
			}
			if err := w.preserveEntry(context.Background(), a.Before, i); err != nil {
				t.Fatal(err)
			}
			os.Exit(73)
		}
		if err := w.applyAction(context.Background(), c, plan.Target.Version, plan.Changes[i], i); err != nil {
			t.Fatal(err)
		}
		if i == boundary {
			os.Exit(73)
		}
		if err := w.writeJSON("progress.json", transitionProgress{ID: plan.ID, Cursor: i + 1}); err != nil {
			t.Fatal(err)
		}
	}
	if boundary == len(plan.Changes) {
		os.Exit(73)
	}
	t.Fatal("exit boundary not reached")
}

func interruptAction(t *testing.T, f *fixture, w *Workspace, index int) *Workspace {
	return interruptDirection(t, f, w, index, false)
}

func interruptDirection(t *testing.T, f *fixture, w *Workspace, index int, reverse bool) *Workspace {
	t.Helper()
	directory := w.Directory()
	w.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTransitionProcessExit$")
	cmd.Env = append(os.Environ(), "LODE_TEST_TRANSITION="+directory, "LODE_TEST_BOUNDARY="+strconv.Itoa(index), "LODE_TEST_TOKEN="+f.token)
	if reverse {
		cmd.Env = append(cmd.Env, "LODE_TEST_REVERSE=1")
	}
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("interruption: %v\n%s", err, output)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

// Prepare a real transition with cancellation already requested, so the plan is
// durable but none of its filesystem actions have executed yet.
func planUpdate(t *testing.T, f *fixture, w *Workspace) transition {
	t.Helper()
	ctx := context.Background()
	target := w.State()
	var err error
	target.Version, err = f.client.Head(ctx, target.Owner, target.Project, target.Branch)
	if err != nil {
		t.Fatal(err)
	}
	target.Baseline, err = f.client.Inventory(ctx, target.Owner, target.Project, target.Version, target.Selection)
	if err != nil {
		t.Fatal(err)
	}
	before, err := w.observeSelection(ctx, w.state.Selection)
	if err != nil {
		t.Fatal(err)
	}
	after, conflicts, err := model.Reconcile(w.state.Baseline, before, target.Baseline, nil)
	if err != nil || len(conflicts) > 0 {
		t.Fatalf("plan: %v %v", conflicts, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := w.beginTransition(cancelled, f.client, target, target.Version, before, after); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare: %v", err)
	}
	var plan transition
	if err := w.readJSON("transition.json", &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func transitionFixture(t *testing.T) (*fixture, string, []model.Entry) {
	t.Helper()
	f := newFixture(t)
	dir := t.TempDir()
	for name, data := range map[string]string{"d/a": "old child", "f": "old file", "keep": "old contents", "remove": "removed"} {
		writeFile(t, dir, name, data)
	}
	w := attach(t, f, dir)
	save(t, f, w, "initial")
	seed := cloneWorkspace(t, f, nil)
	original := seed.State().Baseline
	seed.Close()
	if err := os.RemoveAll(filepath.Join(dir, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "f")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "remove")); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"d": "new file", "f/b": "new child", "keep": "new contents", "new/c": "new directory"} {
		writeFile(t, dir, name, data)
	}
	save(t, f, w, "type replacements")
	return f, seed.Directory(), original
}

func copyWorkspace(t *testing.T, source string) *Workspace {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "work")
	if err := os.CopyFS(directory, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestTransitionRecoversEveryActionBoundary(t *testing.T) {
	f, seed, original := transitionFixture(t)
	first := copyWorkspace(t, seed)
	plan := planUpdate(t, f, first)
	first.Close()
	for _, reverse := range []bool{false, true} {
		for index := range plan.Changes {
			t.Run(fmtCase(reverse, index), func(t *testing.T) {
				w := copyWorkspace(t, seed)
				actual := planUpdate(t, f, w)
				w = interruptAction(t, f, w, index)
				var err error
				want := actual.Target.Baseline
				if reverse {
					w = interruptDirection(t, f, w, index, true)
					err = w.Cancel(context.Background(), "")
					want = original
				} else {
					err = w.Resume(context.Background(), f.token)
				}
				if err != nil {
					t.Fatal(err)
				}
				observed, err := w.observe(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				materialized, err := model.Materialized(want)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(observed, materialized) {
					t.Fatalf("contents: %v want %v", observed, materialized)
				}
				changes, err := w.Status(context.Background())
				if err != nil || len(changes) != 0 {
					t.Fatalf("baseline: %v %v", changes, err)
				}
				if exists, err := w.pending("transition.json"); err != nil || exists {
					t.Fatalf("pending: %v %v", exists, err)
				}
				if _, err := os.Stat(filepath.Join(w.Directory(), ".lode/staging")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("staging: %v", err)
				}
			})
		}
	}
	t.Run("committed-before-baseline", func(t *testing.T) {
		w := copyWorkspace(t, seed)
		p := planUpdate(t, f, w)
		w = interruptAction(t, f, w, len(p.Changes))
		if err := w.Cancel(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if w.State().Version != p.Target.Version {
			t.Fatal("reversed committed operation")
		}
		if readFile(t, w.Directory(), "keep") != "new contents" {
			t.Fatal("committed contents lost")
		}
	})
}
func fmtCase(reverse bool, index int) string {
	direction := "resume"
	if reverse {
		direction = "cancel"
	}
	return direction + "/" + strconv.Itoa(index)
}

func TestInterruptedTransitionPreservesLaterEdits(t *testing.T) {
	f, seed, _ := transitionFixture(t)
	for _, atomicReplace := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(strconv.FormatBool(atomicReplace)+"/"+strconv.FormatBool(reverse), func(t *testing.T) {
				w := copyWorkspace(t, seed)
				plan := planUpdate(t, f, w)
				index := -1
				for i, a := range plan.Changes {
					if a.After.Path == "keep" && a.After.Kind == model.File {
						index = i
						break
					}
				}
				w = interruptAction(t, f, w, index)
				if atomicReplace {
					writeFile(t, w.Directory(), "replacement", "later edit")
					if err := os.Rename(filepath.Join(w.Directory(), "replacement"), filepath.Join(w.Directory(), "keep")); err != nil {
						t.Fatal(err)
					}
				} else {
					writeFile(t, w.Directory(), "keep", "later edit")
				}
				var err error
				if reverse {
					err = w.Cancel(context.Background(), "")
				} else {
					err = w.Resume(context.Background(), f.token)
				}
				if atomicReplace || reverse {
					if err == nil {
						t.Fatal("replaced a later edit")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if readFile(t, w.Directory(), "keep") != "later edit" {
					t.Fatal("later work changed")
				}
				if !atomicReplace && !reverse {
					changes, err := w.Status(context.Background())
					if err != nil || len(changes) != 1 || changes[0].Path != "keep" {
						t.Fatalf("later edit status: %v %v", changes, err)
					}
				} else if readFile(t, w.Directory(), stagePath(index, "old")) != "old contents" {
					t.Fatal("original not preserved")
				}
			})
		}
	}
}

func TestClonePublishesRecoverableIntentWithMetadata(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "a", "contents")
	a := attach(t, f, dir)
	save(t, f, a, "initial")
	target := a.State()
	initial := target
	initial.Version = ""
	initial.Selection = model.Selection{}
	initial.Baseline = nil
	entries, err := model.Materialized(target.Baseline)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := model.PlanChanges(nil, entries)
	if err != nil {
		t.Fatal(err)
	}
	plan := transition{ID: "initialization-test", Base: stateDigest(initial), Target: target, Source: target.Version, Changes: changes}
	directory := t.TempDir()
	w, err := initialize(directory, initial, &plan)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Resume(context.Background(), f.token); err != nil {
		t.Fatal(err)
	}
	if readFile(t, directory, "a") != "contents" {
		t.Fatal("clone intent lost")
	}
}

func TestRecoveryBetweenRemovingAndInstallingFile(t *testing.T) {
	f, seed, _ := transitionFixture(t)
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			w := copyWorkspace(t, seed)
			plan := planUpdate(t, f, w)
			index := -1
			for i, a := range plan.Changes {
				if a.After.Path == "keep" {
					index = i
					break
				}
			}
			t.Setenv("LODE_TEST_DETACHED", "1")
			w = interruptAction(t, f, w, index)
			if _, err := os.Stat(filepath.Join(w.Directory(), "keep")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("original still installed: %v", err)
			}
			want := "new contents"
			var err error
			if reverse {
				err = w.Cancel(context.Background(), "")
				want = "old contents"
			} else {
				err = w.Resume(context.Background(), f.token)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, w.Directory(), "keep"); got != want {
				t.Fatalf("recovered %q want %q", got, want)
			}
		})
	}
}

func TestRecoveryPreservesReplacedDirectory(t *testing.T) {
	f, seed, _ := transitionFixture(t)
	for _, populated := range []bool{false, true} {
		t.Run(strconv.FormatBool(populated), func(t *testing.T) {
			w := copyWorkspace(t, seed)
			plan := planUpdate(t, f, w)
			index := -1
			for i, a := range plan.Changes {
				if a.After.Path == "f" && a.After.Kind == model.Directory {
					index = i
					break
				}
			}
			w = interruptAction(t, f, w, index)
			if err := os.Rename(filepath.Join(w.Directory(), "f"), filepath.Join(w.Directory(), "saved-directory")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(w.Directory(), "f"), 0755); err != nil {
				t.Fatal(err)
			}
			if populated {
				writeFile(t, w.Directory(), "f/local", "local work")
			}
			if err := w.Resume(context.Background(), f.token); err == nil {
				t.Fatal("adopted replacement directory")
			}
			if err := w.Cancel(context.Background(), ""); err == nil {
				t.Fatal("overwrote replacement directory")
			}
			if populated && readFile(t, w.Directory(), "f/local") != "local work" {
				t.Fatal("lost local work")
			}
			if info, err := os.Stat(filepath.Join(w.Directory(), "f")); err != nil || !info.IsDir() {
				t.Fatalf("replacement removed: %v", err)
			}
		})
	}
}

// Exit without closing the workspace, either after a real cleanup failure or
// after cleanup's directory sync but before the transition plan is removed.
func TestCleanupProcessExit(t *testing.T) {
	directory := os.Getenv("LODE_TEST_CLEANUP_DIRECTORY")
	if directory == "" {
		return
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv("LODE_TEST_CLEANUP_MODE")
	if mode == "complete" {
		var plan transition
		if err := w.readJSON("transition.json", &plan); err != nil {
			t.Fatal(err)
		}
		if err := w.writeJSON("state.json", plan.Target); err != nil {
			t.Fatal(err)
		}
		if err := w.cleanTransition(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	} else {
		err := w.resumeTransition(context.Background(), nil, mode == "cancel")
		if !errors.Is(err, syscall.ENOTEMPTY) {
			t.Fatalf("expected cleanup removal failure, got %v", err)
		}
	}
	os.Exit(73)
}

func TestCleanupRecoversAfterProcessExit(t *testing.T) {
	f, seed, original := transitionFixture(t)
	for _, mode := range []string{"resume", "cancel", "complete"} {
		t.Run(mode, func(t *testing.T) {
			w := copyWorkspace(t, seed)
			plan := planUpdate(t, f, w)
			boundary := len(plan.Changes)
			want := plan.Target.Baseline
			if mode == "cancel" {
				// The last action ran, but its progress update has not committed.
				boundary--
				want = original
			}
			w = interruptAction(t, f, w, boundary)
			blocker := stageName(len(plan.Changes)/2, "new") + ".id.next"
			if mode != "complete" {
				// A nonempty unexpected directory makes cleanup stop after it
				// has removed a prefix of the real recovery entries.
				writeFile(t, w.Directory(), ".lode/"+blocker+"/preserved", "do not discard")
			}
			directory := w.Directory()
			w.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCleanupProcessExit$")
			cmd.Env = append(os.Environ(), "LODE_TEST_CLEANUP_DIRECTORY="+directory, "LODE_TEST_CLEANUP_MODE="+mode)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("cleanup interruption: %v\n%s", err, output)
			}
			w, err = Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if exists, err := w.pending("transition.json"); err != nil || !exists {
				t.Fatalf("lost recovery plan: %v %v", exists, err)
			}
			for _, side := range []string{"old", "new"} {
				if exists, err := w.pending(stageName(0, side)); err != nil || exists {
					t.Fatalf("cleanup did not remove first %s entry: %v %v", side, exists, err)
				}
			}
			observed, err := w.observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			materialized, err := model.Materialized(want)
			if err != nil || !reflect.DeepEqual(observed, materialized) {
				t.Fatalf("workspace contents: %v want %v (%v)", observed, materialized, err)
			}
			if mode != "complete" {
				if got := readFile(t, directory, ".lode/"+blocker+"/preserved"); got != "do not discard" {
					t.Fatal("unexpected recovery contents lost")
				}
				if err := w.meta.Remove(blocker + "/preserved"); err != nil {
					t.Fatal(err)
				}
				if err := w.meta.Remove(blocker); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, directory, "keep", "edit after cleanup stopped")
			// Cancel also finishes a committed transition; it cannot roll it back.
			if err := w.Cancel(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			changes, err := w.Status(context.Background())
			if err != nil || len(changes) != 1 || changes[0].Path != "keep" || readFile(t, directory, "keep") != "edit after cleanup stopped" {
				t.Fatalf("later edit: %v %v", changes, err)
			}
			if _, err := w.meta.Lstat("staging"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging remains: %v", err)
			}
		})
	}
}

func TestPrefetchedFilesRecoverBeforeFirstAction(t *testing.T) {
	f, seed, original := transitionFixture(t)
	for _, mode := range []string{"resume", "cancel", "changed"} {
		t.Run(mode, func(t *testing.T) {
			w := copyWorkspace(t, seed)
			plan := planUpdate(t, f, w)
			t.Setenv("LODE_TEST_PREFETCH", "1")
			w = interruptAction(t, f, w, 0)
			before, err := w.observe(context.Background())
			materialized, materializeErr := model.Materialized(original)
			if err != nil || materializeErr != nil || !reflect.DeepEqual(before, materialized) {
				t.Fatalf("prefetch changed working files: %v %v", err, materializeErr)
			}
			want := original
			switch mode {
			case "resume":
				err = w.Resume(context.Background(), f.token)
				want = plan.Target.Baseline
			case "cancel":
				err = w.Cancel(context.Background(), "")
			case "changed":
				index := -1
				for i, a := range plan.Changes {
					if a.After.Path == "keep" {
						index = i
					}
				}
				if index < 0 {
					t.Fatal("fixture lacks changed file")
				}
				name := stagePath(index, "new")
				writeFile(t, w.Directory(), name, "later recovery edit")
				if err := w.Resume(context.Background(), f.token); err == nil {
					t.Fatal("installed changed staged contents")
				}
				if err := w.Cancel(context.Background(), ""); err == nil {
					t.Fatal("discarded changed staged contents")
				}
				rescued := filepath.Join(t.TempDir(), "recovered")
				if err := os.Rename(filepath.Join(w.Directory(), name), rescued); err != nil {
					t.Fatal(err)
				}
				if b, err := os.ReadFile(rescued); err != nil || string(b) != "later recovery edit" {
					t.Fatalf("recovery edit lost: %q %v", b, err)
				}
				err = w.Cancel(context.Background(), "")
			}
			if err != nil {
				t.Fatal(err)
			}
			observed, err := w.observe(context.Background())
			materialized, materializeErr = model.Materialized(want)
			if err != nil || materializeErr != nil || !reflect.DeepEqual(observed, materialized) {
				t.Fatalf("recovered files differ: %v %v", err, materializeErr)
			}
			if exists, err := w.pending("transition.json"); err != nil || exists {
				t.Fatalf("operation remains pending: %v %v", exists, err)
			}
		})
	}
}
