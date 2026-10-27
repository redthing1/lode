//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/project"
)

func TestProgressProcessCrashesPreserveInstalledEdits(t *testing.T) {
	for _, phase := range []string{"progress-before", "progress-after", "finish-before", "finish-after"} {
		t.Run(phase, func(t *testing.T) {
			_, directory, base, target := prepareTransitionCrash(t)
			crashTransitionChild(t, directory, phase)
			writeTestFile(t, filepath.Join(directory, "file.bin"), "newer edit after checkpoint crash")
			w, err := Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if phase == "finish-after" {
				if w.Pending() != "" || w.Base() != target.ID {
					t.Fatalf("completed state: pending=%q base=%q", w.Pending(), w.Base())
				}
			} else {
				if w.Pending() != "transition" || w.Base() != base.ID {
					t.Fatalf("pending state: pending=%q base=%q", w.Pending(), w.Base())
				}
				if _, err := w.Resume(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if w.Pending() != "" || w.Base() != target.ID {
				t.Fatalf("recovered state: pending=%q base=%q", w.Pending(), w.Base())
			}
			assertFileContents(t, filepath.Join(directory, "file.bin"), "newer edit after checkpoint crash")
			changes, err := w.Status(context.Background())
			if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"file.bin"}) {
				t.Fatalf("recovered edits=%+v err=%v", changes, err)
			}
			left, err := filepath.Glob(filepath.Join(directory, ".lode", "progress-*.json"))
			if err != nil || len(left) != 0 {
				t.Fatalf("completed cursors=%v err=%v", left, err)
			}
		})
	}
}

func TestProgressRejectsMissingOrMismatchedCursorBeforeMutation(t *testing.T) {
	for _, mutation := range []string{"missing", "plan", "id", "index", "oversize", "permissions"} {
		t.Run(mutation, func(t *testing.T) {
			_, directory, _, _ := prepareTransitionCrash(t)
			w, err := Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			stop := errors.New("stop before replacing the old file")
			w.moveEntry = func(string, string) error { return stop }
			if err := w.Update(context.Background()); !errors.Is(err, stop) {
				t.Fatalf("Update=%v", err)
			}
			id := w.state.Pending.Transition.ID
			progress, err := w.readProgress(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(directory, ".lode", progressName(id))
			switch mutation {
			case "missing":
				err = os.Remove(filename)
			case "permissions":
				err = os.Chmod(filename, 0644)
			case "oversize":
				err = os.WriteFile(filename, []byte(strings.Repeat(" ", 1025)), 0600)
			default:
				switch mutation {
				case "plan":
					progress.Plan = strings.Repeat("0", 64)
				case "id":
					progress.ID = randomID()
				case "index":
					progress.Index = 2
				}
				data, marshalErr := json.Marshal(progress)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = os.WriteFile(filename, data, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			opened, err := Open(directory)
			if err == nil {
				_ = opened.Close()
				t.Fatal("Open accepted invalid progress")
			}
			assertFileContents(t, filepath.Join(directory, "file.bin"), "base contents")
		})
	}
}

func TestProgressUpgradesLegacyCompletedActionsBeforeResume(t *testing.T) {
	_, directory, _, target := prepareTransitionCrash(t)
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop before final state publication")
	w.replaceMetadata = func(from, to string) error {
		if to == "state.json" {
			data, err := w.private.ReadFile(from)
			if err != nil {
				return err
			}
			var next state
			if err := json.Unmarshal(data, &next); err != nil {
				return err
			}
			if next.Pending == nil {
				return stop
			}
		}
		return w.private.Rename(from, to)
	}
	if err := w.Update(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("Update=%v", err)
	}
	legacy := w.state
	legacy.Version = 1
	legacy.Pending.Transition.ProgressVersion = 0
	if legacy.Pending.Transition.Index != 1 {
		t.Fatalf("legacy cursor=%d", legacy.Pending.Transition.Index)
	}
	id := legacy.Pending.Transition.ID
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".lode", "state.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, ".lode", progressName(id))); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "file.bin"), "new edit after legacy checkpoint")
	w, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	upgraded := false
	w.replaceMetadata = func(from, to string) error {
		if to == "state.json" {
			data, err := w.private.ReadFile(from)
			if err != nil {
				return err
			}
			var next state
			if err := json.Unmarshal(data, &next); err != nil {
				return err
			}
			if next.Pending != nil {
				cursor, err := w.readProgress(id)
				if err != nil || cursor.Index != 1 || next.Version != 2 || next.Pending.Transition.ProgressVersion != 1 {
					t.Fatalf("upgrade published without matching progress: %+v %v", cursor, err)
				}
				upgraded = true
			} else if !upgraded {
				t.Fatal("completed without upgrading legacy progress")
			}
		}
		return w.private.Rename(from, to)
	}
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !upgraded || w.Base() != target.ID || w.Pending() != "" {
		t.Fatalf("legacy recovery: upgraded=%v base=%q pending=%q", upgraded, w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "new edit after legacy checkpoint")
}

func TestProgressCheckpointWritesStaySmallAcrossManyActions(t *testing.T) {
	remote := newTestRemote(t, nil)
	const count = 64
	changes := make([]project.Change, 0, count)
	files := make(map[string]string, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("file-%03d", i)
		changes = append(changes, makeFileChange(name, "old"))
		files[name] = "old"
	}
	base := seed(t, remote, "progress-many-base", "", changes, files)
	w := createTestWorkspace(t, remote, filepath.Join(t.TempDir(), "workspace"), "main", nil)
	for i := range changes {
		changes[i] = makeFileChange(changes[i].Path, "new")
		files[changes[i].Path] = "new"
	}
	seed(t, remote, "progress-many-target", base.ID, changes, files)
	var stateWrites, progressWrites int
	w.replaceMetadata = func(from, to string) error {
		data, err := w.private.ReadFile(from)
		if err != nil {
			return err
		}
		if to == "state.json" {
			stateWrites++
		} else if strings.HasPrefix(to, "progress-") {
			progressWrites++
			if len(data) > 256 {
				t.Fatalf("progress checkpoint grew to %d bytes", len(data))
			}
		}
		return w.private.Rename(from, to)
	}
	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stateWrites != 2 || progressWrites != count+1 {
		t.Fatalf("state writes=%d progress writes=%d", stateWrites, progressWrites)
	}
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("updated tree=%+v err=%v", changes, err)
	}
}

func TestProgressBeforePlanPublicationIsDisposableAfterReopen(t *testing.T) {
	_, directory, base, target := prepareTransitionCrash(t)
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop before publishing transition plan")
	w.replaceMetadata = func(from, to string) error {
		if to == "state.json" {
			return stop
		}
		return w.private.Rename(from, to)
	}
	if err := w.Update(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("Update=%v", err)
	}
	if w.Pending() != "" || w.Base() != base.ID {
		t.Fatalf("failed publication changed state: pending=%q base=%q", w.Pending(), w.Base())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "base contents")
	left, err := filepath.Glob(filepath.Join(directory, ".lode", "progress-*.json"))
	if err != nil || len(left) != 1 {
		t.Fatalf("unpublished progress=%v err=%v", left, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Files that do not parse as generated progress remain untouched.
	unknown := filepath.Join(directory, ".lode", progressName(randomID()))
	if err := os.WriteFile(unknown, []byte("user recovery notes"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := os.Stat(left[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan progress remains: %v", err)
	}
	assertFileContents(t, unknown, "user recovery notes")
	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != target.ID || w.Pending() != "" {
		t.Fatalf("retry: base=%q pending=%q", w.Base(), w.Pending())
	}
}

func TestProgressReopenThenAbandonKeepsCompletedEdits(t *testing.T) {
	_, directory, base, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "progress-after")
	writeTestFile(t, filepath.Join(directory, "file.bin"), "newer edit before abandonment")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	id := w.state.Pending.Transition.ID
	if err := w.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("abandon: base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "newer edit before abandonment")
	if _, err := os.Stat(filepath.Join(directory, ".lode", progressName(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned progress remains: %v", err)
	}
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"file.bin"}) {
		t.Fatalf("abandoned edits=%+v err=%v", changes, err)
	}
}
