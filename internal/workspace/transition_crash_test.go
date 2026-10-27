//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/project"
)

const (
	transitionCrashWorkspaceEnv = "LODE_TEST_TRANSITION_CRASH_WORKSPACE"
	transitionCrashPhaseEnv     = "LODE_TEST_TRANSITION_CRASH_PHASE"
	transitionCrashExitCode     = 93
)

func TestTransitionProcessCrashAfterOldMoveRecovers(t *testing.T) {
	remote, directory, base, target := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "move")

	w, err := Open(directory)
	if err != nil {
		t.Fatalf("Open after child exit: %v", err)
	}
	defer w.Close()
	if w.Pending() != "transition" || w.Base() != base.ID {
		t.Fatalf("after crash base=%q pending=%q", w.Base(), w.Pending())
	}
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume after old move: %v", err)
	}
	if w.Base() != target.ID || w.Pending() != "" {
		t.Fatalf("after recovery base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "target contents")
	if changes, err := w.Status(context.Background()); err != nil || len(changes) != 0 {
		t.Fatalf("Status after recovery=%+v err=%v", changes, err)
	}
	if head, err := remote.client.Head(context.Background(), "main"); err != nil || head != target.ID {
		t.Fatalf("remote head=%q err=%v", head, err)
	}
}

func TestTransitionProcessCrashAfterInstallPreservesNewerEdit(t *testing.T) {
	_, directory, base, target := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "link")

	w, err := Open(directory)
	if err != nil {
		t.Fatalf("Open after child exit: %v", err)
	}
	defer w.Close()
	if w.Pending() != "transition" || w.Base() != base.ID {
		t.Fatalf("after crash base=%q pending=%q", w.Base(), w.Pending())
	}
	writeTestFile(t, filepath.Join(directory, "file.bin"), "newer edit after crash")
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatalf("Resume after installed file edit: %v", err)
	}
	if w.Base() != target.ID || w.Pending() != "" {
		t.Fatalf("after recovery base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "newer edit after crash")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"file.bin"}) {
		t.Fatalf("Status after recovery=%+v err=%v", changes, err)
	}
}

// TestWorkspaceTransitionCrashHelper is run as a separate test process. It
// exits only after the requested real filesystem mutation has completed.
func TestWorkspaceTransitionCrashHelper(t *testing.T) {
	directory := os.Getenv(transitionCrashWorkspaceEnv)
	phase := os.Getenv(transitionCrashPhaseEnv)
	if directory == "" || phase == "" {
		return
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatalf("open child workspace: %v", err)
	}
	crash := func() { os.Exit(transitionCrashExitCode) }
	switch phase {
	case "move":
		w.moveEntry = func(from, to string) error {
			if err := moveNoReplace(w.root, from, to); err != nil {
				return err
			}
			crash()
			return nil
		}
	case "link":
		w.linkEntry = func(from, to string) error {
			if err := linkNoReplace(w.root, from, to); err != nil {
				return err
			}
			crash()
			return nil
		}
	case "progress-before", "progress-after", "finish-before", "finish-after":
		w.replaceMetadata = func(from, to string) error {
			data, err := w.private.ReadFile(from)
			if err != nil {
				return err
			}
			trigger := false
			if strings.HasPrefix(to, "progress-") {
				var progress transitionProgress
				if err := json.Unmarshal(data, &progress); err != nil {
					return err
				}
				trigger = strings.HasPrefix(phase, "progress-") && progress.Index == 1
			} else if to == "state.json" {
				var next state
				if err := json.Unmarshal(data, &next); err != nil {
					return err
				}
				trigger = strings.HasPrefix(phase, "finish-") && next.Pending == nil
			}
			if trigger && strings.HasSuffix(phase, "-before") {
				crash()
			}
			if err := w.private.Rename(from, to); err != nil {
				return err
			}
			if trigger {
				crash()
			}
			return nil
		}
	default:
		t.Fatalf("unknown crash phase %q", phase)
	}
	if err := w.Update(context.Background()); err != nil {
		t.Fatalf("child Update: %v", err)
	}
	t.Fatal("requested crash mutation was not reached")
}

func prepareTransitionCrash(t *testing.T) (*testRemote, string, project.Commit, project.Commit) {
	t.Helper()
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "process-crash-base", "", []project.Change{
		makeFileChange("file.bin", "base contents"),
	}, map[string]string{"file.bin": "base contents"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	target := seed(t, remote, "process-crash-target", base.ID, []project.Change{
		makeFileChange("file.bin", "target contents"),
	}, map[string]string{"file.bin": "target contents"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return remote, directory, base, target
}

func crashTransitionChild(t *testing.T, directory, phase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkspaceTransitionCrashHelper$")
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, transitionCrashWorkspaceEnv+"=") || strings.HasPrefix(value, transitionCrashPhaseEnv+"=") {
			continue
		}
		env = append(env, value)
	}
	command.Env = append(env,
		transitionCrashWorkspaceEnv+"="+directory,
		transitionCrashPhaseEnv+"="+phase,
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != transitionCrashExitCode {
		t.Fatalf("child phase %q exit=%v output=%q, want exit %d", phase, err, output, transitionCrashExitCode)
	}
}
