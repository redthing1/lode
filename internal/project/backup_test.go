package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBackupLocalCopiesIndependentProjectAndRestores(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	local := openTestLocal(t)
	source := filepath.Dir(local.metadata.path)
	oldData := []byte("expired ancestor payload")
	old := mergeApply(t, local.Service, "backup-old", "main", "", fileChanges(map[string][]byte{"folder/file.bin": oldData}), map[string][]byte{"folder/file.bin": oldData})
	branchData := []byte("branch descendant payload")
	branchTip := mergeApply(t, local.Service, "backup-branch-tip", "main", old.ID, fileChanges(map[string][]byte{"folder/file.bin": branchData}), map[string][]byte{"folder/file.bin": branchData})
	mainData := []byte("tagged main descendant payload")
	mainTip := mergeApply(t, local.Service, "backup-main-tip", "main", branchTip.ID, fileChanges(map[string][]byte{"folder/file.bin": mainData}), map[string][]byte{"folder/file.bin": mainData})
	if err := local.Service.CreateBranch(ctx, "stable", branchTip.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.CreateTag(ctx, "release/backup", mainTip.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	local.Service.now = func() time.Time { return mainTip.CreatedAt.Add(48 * time.Hour) }
	expired, err := local.Service.Expire(ctx, []string{old.ID})
	if err != nil || !reflect.DeepEqual(expired.Expired, []string{old.ID}) {
		t.Fatalf("expire old ancestor before backup = %+v, err=%v", expired, err)
	}
	if _, _, err := local.Service.Entries(ctx, old.ID, []SelectionRule{{Included: true}}, "", 20); err != nil {
		t.Fatalf("expired ancestor comparison metadata missing before backup: %v", err)
	}
	if _, _, err := local.Service.OpenFile(ctx, old.ID, "folder/file.bin"); !errors.Is(err, ErrPayloadExpired) {
		t.Fatalf("expired ancestor content before backup = %v, want ErrPayloadExpired", err)
	}
	projectID, token := local.Service.ID(), local.Token
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	contentTimes := backupFileTimes(t, filepath.Join(source, "content"))
	if len(contentTimes) == 0 {
		t.Fatal("source content store has no regular files")
	}

	backup := filepath.Join(filepath.Dir(source), "backup")
	if err := BackupLocal(ctx, source, backup); err != nil {
		t.Fatal(err)
	}
	assertMode(t, backup, 0700)
	assertMode(t, filepath.Join(backup, "state.json"), 0600)
	assertMode(t, filepath.Join(backup, "token"), 0600)
	assertMode(t, filepath.Join(backup, "metadata.sqlite"), 0600)
	if got := backupFileTimes(t, filepath.Join(backup, "content")); !reflect.DeepEqual(got, contentTimes) {
		t.Fatalf("native blob modification times changed across backup\n got: %#v\nwant: %#v", got, contentTimes)
	}

	// Moving the closed source away proves the backup's connection points to its
	// own copied native store and metadata database.
	movedSource := filepath.Join(filepath.Dir(source), "source-offline")
	if err := os.Rename(source, movedSource); err != nil {
		t.Fatal(err)
	}
	backupLocal, err := OpenLocal(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	if backupLocal.Service.ID() != projectID || backupLocal.Token != token {
		t.Fatalf("backup identity/token changed: project=%q token-match=%v", backupLocal.Service.ID(), backupLocal.Token == token)
	}
	if head, err := backupLocal.Service.Head(ctx, "main"); err != nil || head != mainTip.ID {
		t.Fatalf("backup main head=%q, err=%v; want %q", head, err, mainTip.ID)
	}
	if tag, err := backupLocal.Service.Tag(ctx, "release/backup"); err != nil || tag.CommitID != mainTip.ID {
		t.Fatalf("backup tag=%+v, err=%v", tag, err)
	}
	stable, err := backupLocal.Service.Branch(ctx, "stable")
	if err != nil || stable.Tip != branchTip.ID {
		t.Fatalf("backup stable branch=%+v, err=%v", stable, err)
	}
	policy, err := backupLocal.Service.RetentionPolicy(ctx)
	if err != nil || policy != (RetentionPolicy{}) {
		t.Fatalf("backup retention policy=%+v, err=%v", policy, err)
	}
	history, next, err := backupLocal.Service.History(ctx, mainTip.ID, 10)
	if err != nil || next != "" || len(history) != 3 || history[0].ID != mainTip.ID || history[1].ID != branchTip.ID || history[2].ID != old.ID {
		t.Fatalf("backup history=%+v next=%q err=%v", history, next, err)
	}
	if _, _, err := backupLocal.Service.Entries(ctx, old.ID, []SelectionRule{{Included: true}}, "", 20); err != nil {
		t.Fatalf("backup expired ancestor history metadata missing: %v", err)
	}
	if _, _, err := backupLocal.Service.OpenFile(ctx, old.ID, "folder/file.bin"); !errors.Is(err, ErrPayloadExpired) {
		t.Fatalf("backup expired ancestor content = %v, want ErrPayloadExpired", err)
	}
	reader, node, err := backupLocal.Service.OpenFile(ctx, mainTip.ID, "folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, mainData) || node.Size != int64(len(mainData)) {
		t.Fatalf("backup payload node=%+v bytes=%q read=%v close=%v", node, got, readErr, closeErr)
	}
	branchReader, _, err := backupLocal.Service.OpenFile(ctx, branchTip.ID, "folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	branchBytes, readErr := io.ReadAll(branchReader)
	closeErr = branchReader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(branchBytes, branchData) {
		t.Fatalf("backup branch payload=%q, read=%v, close=%v", branchBytes, readErr, closeErr)
	}
	if err := backupLocal.Close(); err != nil {
		t.Fatal(err)
	}

	// The same complete-copy operation restores a backup to another fresh path.
	restored := filepath.Join(filepath.Dir(source), "restored")
	if err := BackupLocal(ctx, backup, restored); err != nil {
		t.Fatalf("restore from backup: %v", err)
	}
	restoredLocal, err := OpenLocal(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredLocal.Close()
	if restoredLocal.Service.ID() != projectID || restoredLocal.Token != token {
		t.Fatal("restored project identity or token changed")
	}
	if head, err := restoredLocal.Service.Head(ctx, "main"); err != nil || head != mainTip.ID {
		t.Fatalf("restored main head=%q, err=%v; want %q", head, err, mainTip.ID)
	}
	stable, err = restoredLocal.Service.Branch(ctx, "stable")
	if err != nil || stable.Tip != branchTip.ID {
		t.Fatalf("restored stable branch=%+v, err=%v", stable, err)
	}
	restoredTag, err := restoredLocal.Service.Tag(ctx, "release/backup")
	if err != nil || restoredTag.CommitID != mainTip.ID {
		t.Fatalf("restored tag=%+v, err=%v", restoredTag, err)
	}
	policy, err = restoredLocal.Service.RetentionPolicy(ctx)
	if err != nil || policy != (RetentionPolicy{}) {
		t.Fatalf("restored retention policy=%+v, err=%v", policy, err)
	}
	if _, _, err := restoredLocal.Service.Entries(ctx, old.ID, []SelectionRule{{Included: true}}, "", 20); err != nil {
		t.Fatalf("restored expired ancestor metadata missing: %v", err)
	}
	if _, _, err := restoredLocal.Service.OpenFile(ctx, old.ID, "folder/file.bin"); !errors.Is(err, ErrPayloadExpired) {
		t.Fatalf("restored expired ancestor content = %v, want ErrPayloadExpired", err)
	}
	restoredReader, _, err := restoredLocal.Service.OpenFile(ctx, mainTip.ID, "folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	restoredData, readErr := io.ReadAll(restoredReader)
	closeErr = restoredReader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(restoredData, mainData) {
		t.Fatalf("restored payload=%q, read=%v, close=%v", restoredData, readErr, closeErr)
	}
	branchReader, _, err = restoredLocal.Service.OpenFile(ctx, branchTip.ID, "folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	branchBytes, readErr = io.ReadAll(branchReader)
	closeErr = branchReader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(branchBytes, branchData) {
		t.Fatalf("restored branch payload=%q, read=%v, close=%v", branchBytes, readErr, closeErr)
	}
}

func TestBackupLocalRejectsBusyOverlapAndExistingDestination(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	local := openTestLocal(t)
	source := filepath.Dir(local.metadata.path)
	parent := filepath.Dir(source)

	if err := BackupLocal(ctx, source, filepath.Join(parent, "busy-copy")); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("backup while service owns source = %v, want ErrProjectBusy", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := BackupLocal(ctx, source, filepath.Join(source, "nested-copy")); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("nested destination error = %v, want overlap", err)
	}
	if err := os.Mkdir(filepath.Join(parent, "existing"), 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(parent, "existing", "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := BackupLocal(ctx, source, filepath.Join(parent, "existing")); err == nil {
		t.Fatal("backup overwrote existing destination")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("existing destination changed: %q, %v", data, err)
	}

	linkParent := filepath.Join(parent, "source-link")
	if err := os.Symlink(source, linkParent); err != nil {
		t.Fatal(err)
	}
	if err := BackupLocal(ctx, source, filepath.Join(linkParent, "copy")); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("symlink-resolved overlap error = %v, want overlap", err)
	}
	if err := BackupLocal(ctx, linkParent, filepath.Join(parent, "source-link-copy")); err == nil {
		t.Fatal("backup accepted a symlink as the source directory")
	}
}

func TestBackupLocalRejectsSymlinkAndCleansFailedStagingCopy(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	local := openTestLocal(t)
	source := filepath.Dir(local.metadata.path)
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a-copy-first"), []byte("partial stage"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("state.json", filepath.Join(source, "zz-forbidden-link")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(filepath.Dir(source), "failed-copy")
	if err := BackupLocal(ctx, source, destination); err == nil || !strings.Contains(err.Error(), "symlink or special") {
		t.Fatalf("backup with source symlink = %v, want symlink/special rejection", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed backup published destination: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(source), ".lode-backup-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("failed backup left staging paths %#v, err=%v", stages, err)
	}
}

func TestBackupLocalRejectsSymlinkedLockBeforeOpeningIt(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	local := openTestLocal(t)
	source := filepath.Dir(local.metadata.path)
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(source, "metadata.sqlite.service-lock")
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	if err := BackupLocal(ctx, source, filepath.Join(filepath.Dir(source), "bad-lock-copy")); err == nil || !strings.Contains(err.Error(), "owner lock") {
		t.Fatalf("backup with symlinked lock = %v, want rejection", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "untouched" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
}

func TestBackupLocalHoldsSourceOwnerUntilCopyCompletes(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	local := openTestLocal(t)
	source := filepath.Dir(local.metadata.path)
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	largePath := filepath.Join(source, "zz-large-copy-check")
	large, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	if err := large.Truncate(size); err != nil {
		_ = large.Close()
		t.Fatal(err)
	}
	if err := errors.Join(large.Sync(), large.Close()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(filepath.Dir(source), "owner-held-copy")
	result := make(chan error, 1)
	go func() { result <- BackupLocal(ctx, source, destination) }()
	deadline := time.Now().Add(10 * time.Second)
	checkedOwner := false
	for time.Now().Before(deadline) {
		stages, err := filepath.Glob(filepath.Join(filepath.Dir(source), ".lode-backup-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(stages) != 0 {
			opened, openErr := OpenLocal(ctx, source)
			if errors.Is(openErr, ErrProjectBusy) {
				checkedOwner = true
				break
			}
			if opened != nil {
				_ = opened.Close()
			}
			if openErr != nil {
				t.Fatalf("OpenLocal during backup = %v, want ErrProjectBusy", openErr)
			}
			if _, err := os.Stat(destination); err == nil {
				break
			}
		}
		select {
		case err := <-result:
			t.Fatalf("backup completed before owner check: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if !checkedOwner {
		t.Fatal("could not observe source lock during backup copy")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestBackupLocalCrashHelper(t *testing.T) {
	source := os.Getenv("LODE_BACKUP_CRASH_SOURCE")
	if source == "" {
		return
	}
	if err := BackupLocal(context.Background(), source, os.Getenv("LODE_BACKUP_CRASH_DESTINATION")); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestBackupLocalProcessExitDoesNotPublishPartialCopy(t *testing.T) {
	requireLinuxLocalProject(t)
	ctx := context.Background()
	local := openTestLocal(t)
	source := filepath.Dir(local.metadata.path)
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	const size = 128 << 20
	largePath := filepath.Join(source, "zz-large-crash-boundary")
	large, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(size); err != nil {
		_ = large.Close()
		t.Fatal(err)
	}
	if err := errors.Join(large.Sync(), large.Close()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(filepath.Dir(source), "crashed-copy")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestBackupLocalCrashHelper$")
	command.Env = append(os.Environ(), "LODE_BACKUP_CRASH_SOURCE="+source, "LODE_BACKUP_CRASH_DESTINATION="+destination)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	deadline := time.Now().Add(20 * time.Second)
	var interruptedStage string
	for time.Now().Before(deadline) {
		stages, globErr := filepath.Glob(filepath.Join(filepath.Dir(source), ".lode-backup-*"))
		if globErr != nil {
			_ = command.Process.Kill()
			<-finished
			t.Fatal(globErr)
		}
		for _, stage := range stages {
			info, statErr := os.Stat(filepath.Join(stage, filepath.Base(largePath)))
			if statErr == nil && info.Size() > 0 && info.Size() < size {
				interruptedStage = stage
				if err := command.Process.Kill(); err != nil {
					t.Fatalf("kill backup process at partial copy: %v", err)
				}
				goto waitForCrash
			}
		}
		select {
		case childErr := <-finished:
			t.Fatalf("backup process finished before interruption, error=%v", childErr)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	_ = command.Process.Kill()
	<-finished
	t.Fatal("did not observe a partially copied file before backup publication")

waitForCrash:
	if err := <-finished; err == nil {
		t.Fatal("interrupted backup process exited successfully")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted backup published destination: %v", err)
	}
	if interruptedStage == "" {
		t.Fatal("backup stage path was not observed")
	}
	if err := os.RemoveAll(interruptedStage); err != nil {
		t.Fatalf("remove interrupted private stage: %v", err)
	}
	reopened, err := OpenLocal(ctx, source)
	if err != nil {
		t.Fatalf("source lock was not released when backup process exited: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func backupFileTimes(t *testing.T, root string) map[string]int64 {
	t.Helper()
	result := map[string]int64{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = info.ModTime().UnixNano()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
