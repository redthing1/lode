package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/engine/kopia"
)

const (
	retentionCrashDirectoryEnv = "LODE_TEST_RETENTION_CRASH_DIRECTORY"
	retentionCrashCommitEnv    = "LODE_TEST_RETENTION_CRASH_COMMIT"
	retentionCrashPhaseEnv     = "LODE_TEST_RETENTION_CRASH_PHASE"
	retentionCrashBeforeExit   = 95
	retentionCrashAfterExit    = 96
)

type processExitReleaseRoots struct {
	ContentRoots
	phase string
}

func (r processExitReleaseRoots) Release(ctx context.Context, key string) error {
	if r.phase == "before" {
		os.Exit(retentionCrashBeforeExit)
	}
	if err := r.ContentRoots.Release(ctx, key); err != nil {
		return err
	}
	if r.phase == "after" {
		os.Exit(retentionCrashAfterExit)
	}
	return nil
}

// TestExpiredRootReleaseCrashHelper is called only by the subprocess harness.
// Its os.Exit points bracket the actual native Release and the following SQLite
// markPayloadReleased update without adding a production fault hook.
func TestExpiredRootReleaseCrashHelper(t *testing.T) {
	directory := os.Getenv(retentionCrashDirectoryEnv)
	commitID := os.Getenv(retentionCrashCommitEnv)
	phase := os.Getenv(retentionCrashPhaseEnv)
	if directory == "" && commitID == "" && phase == "" {
		return
	}
	if directory == "" || commitID == "" || (phase != "before" && phase != "after") {
		t.Fatalf("invalid crash helper environment: directory=%q commit=%q phase=%q", directory, commitID, phase)
	}
	local, err := OpenLocal(context.Background(), directory)
	if err != nil {
		t.Fatalf("open child project: %v", err)
	}
	local.Service.roots = processExitReleaseRoots{ContentRoots: local.Service.roots, phase: phase}
	if _, err := local.Service.Expire(context.Background(), []string{commitID}); err != nil {
		t.Fatalf("child expiration did not reach release crash point: %v", err)
	}
	t.Fatal("requested native release crash point was not reached")
}

func TestExpiredRootReleaseProcessCrashRecoversBothSidesOfNativeBoundary(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			directory := t.TempDir() + "/project"
			if err := InitializeLocal(ctx, directory); err != nil {
				t.Fatal(err)
			}
			local := openInitializedLocal(t, directory)
			oldClock := time.Now().UTC().Add(-72 * time.Hour)
			local.Service.now = func() time.Time { return oldClock }
			oldData := []byte("old payload retained in immutable history")
			old := mergeApply(t, local.Service, "crash-old", "main", "", fileChanges(map[string][]byte{"file.bin": oldData}), map[string][]byte{"file.bin": oldData})
			currentData := []byte("current branch tip survives cleanup")
			current := mergeApply(t, local.Service, "crash-current", "main", old.ID, fileChanges(map[string][]byte{"file.bin": currentData}), map[string][]byte{"file.bin": currentData})
			if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
				t.Fatal(err)
			}
			if err := local.Close(); err != nil {
				t.Fatal(err)
			}

			crashRetentionReleaseChild(t, directory, old.ID, phase)

			reopened := openInitializedLocal(t, directory)
			reopened.Service.now = time.Now
			statuses, err := reopened.Service.PayloadStatuses(ctx, []string{old.ID})
			if err != nil || len(statuses) != 1 || statuses[0].State != "expired" || !statuses[0].ReleasePending {
				t.Fatalf("payload state after child exit = %+v, %v; want expired and release-pending", statuses, err)
			}
			oldCommit, err := reopened.Service.Commit(ctx, old.ID)
			if err != nil || oldCommit.ID != old.ID {
				t.Fatalf("immutable old commit after child exit = %+v, %v", oldCommit, err)
			}
			oldArchive, err := reopened.Service.archive(ctx, oldCommit.Tree)
			if err != nil {
				t.Fatalf("archived old metadata after child exit: %v", err)
			}
			nativeRoots, ok := reopened.Service.roots.(*kopia.Roots)
			if !ok {
				t.Fatalf("native roots type = %T", reopened.Service.roots)
			}
			oldOperation, err := reopened.metadata.publication(ctx, "crash-old")
			if err != nil || oldOperation.State != OperationAccepted || oldOperation.ReleaseDone {
				t.Fatalf("accepted publication after child exit = %+v, %v", oldOperation, err)
			}
			rootKey := reopened.Service.protectionKey(oldOperation.ID)
			rootErr := nativeRoots.Check(ctx, rootKey, oldArchive)
			if phase == "before" && rootErr != nil {
				t.Fatalf("root should survive crash before native release: %v", rootErr)
			}
			if phase == "after" && !errors.Is(rootErr, kopia.ErrRootNotFound) {
				t.Fatalf("root should be absent after native release: %v", rootErr)
			}
			if err := reopened.Service.roots.Validate(ctx, oldArchive); err != nil {
				t.Fatalf("payload bytes should remain physically available before collection: %v", err)
			}

			if err := reopened.Service.Recover(ctx); err != nil {
				t.Fatalf("recover release after %s crash: %v", phase, err)
			}
			if err := reopened.Service.Recover(ctx); err != nil {
				t.Fatalf("repeat recovery after %s crash: %v", phase, err)
			}
			statuses, err = reopened.Service.PayloadStatuses(ctx, []string{old.ID})
			if err != nil || len(statuses) != 1 || statuses[0].State != "expired" || statuses[0].ReleasePending {
				t.Fatalf("payload status after recovery = %+v, %v", statuses, err)
			}
			oldOperation, err = reopened.metadata.publication(ctx, "crash-old")
			if err != nil || oldOperation.State != OperationAccepted || oldOperation.ReleaseDone {
				t.Fatalf("recovery changed accepted operation = %+v, %v", oldOperation, err)
			}
			if err := nativeRoots.Check(ctx, rootKey, oldArchive); !errors.Is(err, kopia.ErrRootNotFound) {
				t.Fatalf("root after retrying native release = %v, want ErrRootNotFound", err)
			}
			currentReader, _, err := reopened.Service.OpenFile(ctx, current.ID, "file.bin")
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(currentReader)
			closeErr := currentReader.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(got, currentData) {
				t.Fatalf("current branch payload=%q, readErr=%v closeErr=%v", got, readErr, closeErr)
			}
			if _, _, err := reopened.Service.Entries(ctx, old.ID, []SelectionRule{{Included: true}}, "", 20); err != nil {
				t.Fatalf("old comparison metadata after recovery: %v", err)
			}
		})
	}
}

func crashRetentionReleaseChild(t *testing.T, directory, commitID, phase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExpiredRootReleaseCrashHelper$")
	env := make([]string, 0, len(os.Environ())+3)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, retentionCrashDirectoryEnv+"=") ||
			strings.HasPrefix(value, retentionCrashCommitEnv+"=") ||
			strings.HasPrefix(value, retentionCrashPhaseEnv+"=") {
			continue
		}
		env = append(env, value)
	}
	command.Env = append(env,
		retentionCrashDirectoryEnv+"="+directory,
		retentionCrashCommitEnv+"="+commitID,
		retentionCrashPhaseEnv+"="+phase,
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	want := retentionCrashBeforeExit
	if phase == "after" {
		want = retentionCrashAfterExit
	}
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != want {
		t.Fatalf("retention crash child exit=%v output=%q, want exit %d", err, output, want)
	}
}
