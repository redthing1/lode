//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/project"
)

const (
	mergeCrashWorkspaceEnv = "LODE_TEST_MERGE_CRASH_WORKSPACE"
	mergeCrashOtherEnv     = "LODE_TEST_MERGE_CRASH_OTHER"
	mergeCrashExitCode     = 94
)

func TestMergeProcessCrashAfterInstallRecoversAcceptedCommit(t *testing.T) {
	ctx := context.Background()
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "merge-crash-base", "", []project.Change{makeFileChange("file.bin", "base bytes")}, map[string]string{"file.bin": "base bytes"})
	if err := remote.client.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatal(err)
	}
	other := applyTestChanges(t, remote, "merge-crash-other", "feature", base.ID,
		[]project.Change{makeFileChange("file.bin", "merged bytes")}, map[string]string{"file.bin": "merged bytes"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	if w.Base() != base.ID {
		t.Fatalf("workspace base=%q, want %q", w.Base(), base.ID)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	crashMergeChild(t, directory, other.ID)

	committedHead, err := remote.client.Head(ctx, "main")
	if err != nil || committedHead == base.ID || committedHead == "" {
		t.Fatalf("main head after child crash=%q err=%v", committedHead, err)
	}
	accepted, err := remote.client.Commit(ctx, committedHead)
	if err != nil || len(accepted.Parents) != 2 || accepted.Parents[0] != base.ID || accepted.Parents[1] != other.ID {
		t.Fatalf("accepted merge=%+v err=%v", accepted, err)
	}
	firstHistory, firstNext, err := remote.client.History(ctx, committedHead, 10)
	if err != nil || len(firstHistory) != 2 || firstNext != "" || firstHistory[0].ID != committedHead {
		t.Fatalf("history after child crash=%+v next=%q err=%v", firstHistory, firstNext, err)
	}

	w, err = Open(directory)
	if err != nil {
		t.Fatalf("Open after child exit: %v", err)
	}
	defer w.Close()
	if w.Pending() != "transition" || w.Base() != base.ID {
		t.Fatalf("after crash base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "merged bytes")
	// os.WriteFile truncates the installed file in place, leaving the staged
	// transition hard link as evidence that the target was already installed.
	writeTestFile(t, filepath.Join(directory, "file.bin"), "newer local edit")
	if _, err = w.Resume(ctx); err != nil {
		t.Fatalf("Resume after merge install: %v", err)
	}
	if w.Base() != committedHead || w.Pending() != "" || w.Branch() != "main" {
		t.Fatalf("after recovery base=%q pending=%q branch=%q", w.Base(), w.Pending(), w.Branch())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "newer local edit")
	changes, err := w.Status(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "file.bin" {
		t.Fatalf("Status after recovery=%+v err=%v", changes, err)
	}
	finalHead, err := remote.client.Head(ctx, "main")
	if err != nil || finalHead != committedHead {
		t.Fatalf("main head after recovery=%q err=%v; want stable accepted head %q", finalHead, err, committedHead)
	}
	finalHistory, finalNext, err := remote.client.History(ctx, finalHead, 10)
	if err != nil || len(finalHistory) != len(firstHistory) || finalNext != "" || finalHistory[0].ID != committedHead {
		t.Fatalf("history after recovery=%+v next=%q err=%v; before=%+v", finalHistory, finalNext, err, firstHistory)
	}
}

// TestWorkspaceMergeCrashHelper runs in a child test process. It exits only
// after the actual target hard link is installed, before transition progress
// can be checkpointed.
func TestWorkspaceMergeCrashHelper(t *testing.T) {
	directory := os.Getenv(mergeCrashWorkspaceEnv)
	other := os.Getenv(mergeCrashOtherEnv)
	if directory == "" || other == "" {
		return
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatalf("open child workspace: %v", err)
	}
	w.linkEntry = func(from, to string) error {
		if err := linkNoReplace(w.root, from, to); err != nil {
			return err
		}
		os.Exit(mergeCrashExitCode)
		return errors.New("unreachable after crash")
	}
	if _, err := w.Merge(context.Background(), other, "crash-test", "merge before process exit", nil); err != nil {
		t.Fatalf("child Merge: %v", err)
	}
	t.Fatal("requested post-install crash point was not reached")
}

func crashMergeChild(t *testing.T, directory, other string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkspaceMergeCrashHelper$")
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, mergeCrashWorkspaceEnv+"=") || strings.HasPrefix(value, mergeCrashOtherEnv+"=") {
			continue
		}
		env = append(env, value)
	}
	command.Env = append(env, mergeCrashWorkspaceEnv+"="+directory, mergeCrashOtherEnv+"="+other)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != mergeCrashExitCode {
		t.Fatalf("merge child exit=%v output=%q, want exit %d", err, output, mergeCrashExitCode)
	}
}
