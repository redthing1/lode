//go:build linux

package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestAbortTransitionRestoresBackupAndArchivesRemainingStage(t *testing.T) {
	_, directory, base, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "move")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	id := w.state.Pending.Transition.ID
	if err := w.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Pending() != "" || w.Base() != base.ID {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "base contents")
	recovery := w.RecoveryDirectory()
	if recovery != recoveryDirectory(id) {
		t.Fatalf("recovery directory=%q, want %q", recovery, recoveryDirectory(id))
	}
	assertFileContents(t, filepath.Join(directory, recovery, "new-0"), "target contents")
	var journal recoveryJournal
	data, err := os.ReadFile(filepath.Join(directory, recovery, "journal.json"))
	if err != nil || json.Unmarshal(data, &journal) != nil {
		t.Fatalf("read recovery journal: %v", err)
	}
	if journal.Pending.Kind != "transition" || len(journal.Actions) != 1 || journal.Actions[0].Path != "file.bin" || journal.Actions[0].Old != "old-0" || journal.Actions[0].Fresh != "new-0" {
		t.Fatalf("recovery journal mapping=%+v", journal)
	}
	if _, err := os.Stat(filepath.Join(directory, transitionDirectory(&transition{ID: id}))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transition stage remains after archive: %v", err)
	}
}

func TestAbortTransitionPreservesInstalledEditAndUnexpectedCurrentPath(t *testing.T) {
	_, directory, base, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "link")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	id := w.state.Pending.Transition.ID
	writeTestFile(t, filepath.Join(directory, "file.bin"), "newer user edit")
	if err := w.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "newer user edit")
	assertFileContents(t, filepath.Join(directory, recoveryDirectory(id), "old-0"), "base contents")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"file.bin"}) {
		t.Fatalf("Status after abandon=%+v err=%v", changes, err)
	}

	_, directory2, base2, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory2, "move")
	w2, err := Open(directory2)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	id2 := w2.state.Pending.Transition.ID
	writeTestFile(t, filepath.Join(directory2, "file.bin"), "unmanaged racing file")
	if err := w2.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w2.Base() != base2.ID || w2.Pending() != "" {
		t.Fatalf("unexpected-path abandon base=%q pending=%q", w2.Base(), w2.Pending())
	}
	assertFileContents(t, filepath.Join(directory2, "file.bin"), "unmanaged racing file")
	assertFileContents(t, filepath.Join(directory2, recoveryDirectory(id2), "old-0"), "base contents")
}

func TestAbortArchivesModifiedBackupWithoutRestoringIt(t *testing.T) {
	_, directory, base, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "move")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	id := w.state.Pending.Transition.ID
	writeTestFile(t, filepath.Join(directory, transitionDirectory(w.state.Pending.Transition), "old-0"), "modified backup")
	if err := w.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	if _, err := os.Stat(filepath.Join(directory, "file.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("modified backup unexpectedly restored: %v", err)
	}
	assertFileContents(t, filepath.Join(directory, recoveryDirectory(id), "old-0"), "modified backup")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"file.bin"}) {
		t.Fatalf("Status with absent original=%+v err=%v", changes, err)
	}
}

func TestAbortPreservesUnmanagedChildrenAtOriginalPath(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "abandon-hidden-base", "", []project.Change{makeFileChange("folder", "base file")}, map[string]string{"folder": "base file"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	updated := seed(t, remote, "abandon-hidden-target", base.ID, []project.Change{
		{Path: "folder", Kind: model.Directory},
		makeFileChange("folder/child.bin", "target child"),
	}, map[string]string{"folder/child.bin": "target child"})
	w.moveEntry = func(from, to string) error {
		if err := moveNoReplace(w.root, from, to); err != nil {
			return err
		}
		if from == "folder" && strings.Contains(to, "/old-") {
			return errors.New("interrupted after moving original file")
		}
		return nil
	}
	if err := w.Update(context.Background()); err == nil || w.Pending() != "transition" {
		t.Fatalf("Update err=%v pending=%q", err, w.Pending())
	}
	if err := os.Mkdir(filepath.Join(directory, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "folder", "unmanaged.txt"), "preserve me")
	id := w.state.Pending.Transition.ID
	if err := w.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != base.ID || w.Pending() != "" || w.Base() == updated.ID {
		t.Fatalf("abandon base=%q pending=%q target=%q", w.Base(), w.Pending(), updated.ID)
	}
	assertFileContents(t, filepath.Join(directory, "folder", "unmanaged.txt"), "preserve me")
	assertFileContents(t, filepath.Join(directory, recoveryDirectory(id), "old-0"), "base file")
}

func TestAbortRestoresParentDirectoryButKeepsCompletedChildDeletion(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "abandon-parent-base", "", []project.Change{makeFileChange("folder/child.bin", "child")}, map[string]string{"folder/child.bin": "child"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	target := seed(t, remote, "abandon-parent-target", base.ID, []project.Change{
		{Path: "folder/child.bin", Kind: model.Absent},
		{Path: "folder", Kind: model.Absent},
	}, nil)
	w.moveEntry = func(from, to string) error {
		if err := moveNoReplace(w.root, from, to); err != nil {
			return err
		}
		if from == "folder" && strings.Contains(to, "/old-") {
			return errors.New("interrupted after moving parent directory")
		}
		return nil
	}
	if err := w.Update(context.Background()); err == nil || w.Pending() != "transition" {
		t.Fatalf("Update err=%v pending=%q", err, w.Pending())
	}
	id := w.state.Pending.Transition.ID
	if err := w.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != base.ID || w.Base() == target.ID || w.Pending() != "" {
		t.Fatalf("abandon base=%q target=%q pending=%q", w.Base(), target.ID, w.Pending())
	}
	if info, err := os.Stat(filepath.Join(directory, "folder")); err != nil || !info.IsDir() {
		t.Fatalf("parent directory not restored: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "folder", "child.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("already completed child deletion was undone: %v", err)
	}
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"folder/child.bin"}) {
		t.Fatalf("Status after abandon=%+v err=%v", changes, err)
	}
	if _, err := os.Stat(filepath.Join(directory, recoveryDirectory(id), "journal.json")); err != nil {
		t.Fatalf("recovery archive missing: %v", err)
	}
}

func TestAbortResumeAfterRecoveryRenameIsIdempotent(t *testing.T) {
	_, directory, base, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "move")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	id := w.state.Pending.Transition.ID
	w.moveEntry = func(from, to string) error {
		if err := moveNoReplace(w.root, from, to); err != nil {
			return err
		}
		if from == transitionDirectory(w.state.Pending.Transition) && to == recoveryDirectory(id) {
			return errors.New("interrupted after recovery archive rename")
		}
		return nil
	}
	if err := w.Abort(context.Background()); err == nil {
		t.Fatal("Abort unexpectedly completed after injected post-rename failure")
	}
	if w.Pending() != "abandon" || w.Base() != base.ID {
		t.Fatalf("after injected interruption base=%q pending=%q", w.Base(), w.Pending())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Pending() != "" || w.Base() != base.ID || w.RecoveryDirectory() != recoveryDirectory(id) {
		t.Fatalf("after recovery base=%q pending=%q archive=%q", w.Base(), w.Pending(), w.RecoveryDirectory())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "base contents")
}

func TestAbortResumeAfterDurableJournalTempBeforePublish(t *testing.T) {
	_, directory, base, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "move")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	id := w.state.Pending.Transition.ID
	w.moveEntry = func(from, to string) error {
		if strings.HasSuffix(to, "/journal.json") {
			return errors.New("interrupted before recovery journal publish")
		}
		return moveNoReplace(w.root, from, to)
	}
	if err := w.Abort(context.Background()); err == nil {
		t.Fatal("Abort unexpectedly published the recovery journal")
	}
	if w.Pending() != "abandon" {
		t.Fatalf("pending after journal interruption=%q", w.Pending())
	}
	stage := filepath.Join(directory, transitionDirectory(w.state.Pending.Transition))
	if matches, err := filepath.Glob(filepath.Join(stage, "journal-*.tmp")); err != nil || len(matches) != 1 {
		t.Fatalf("durable journal temp files=%v err=%v", matches, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Pending() != "" || w.Base() != base.ID || w.RecoveryDirectory() != recoveryDirectory(id) {
		t.Fatalf("after resume base=%q pending=%q recovery=%q", w.Base(), w.Pending(), w.RecoveryDirectory())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "base contents")
	if _, err := os.Stat(filepath.Join(directory, recoveryDirectory(id), "journal.json")); err != nil {
		t.Fatalf("published recovery journal: %v", err)
	}
}

func TestAbortRejectsForeignRecoveryDirectory(t *testing.T) {
	_, directory, base, _ := prepareTransitionCrash(t)
	crashTransitionChild(t, directory, "move")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	id := w.state.Pending.Transition.ID
	foreign := filepath.Join(directory, recoveryDirectory(id))
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "journal.json"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Abort()=%v, want conflict with foreign recovery directory", err)
	}
	if w.Pending() != "abandon" || w.Base() != base.ID {
		t.Fatalf("foreign archive changed state: base=%q pending=%q", w.Base(), w.Pending())
	}
	if got, err := os.ReadFile(filepath.Join(foreign, "journal.json")); err != nil || string(got) != "foreign" {
		t.Fatalf("foreign journal changed: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "file.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abort overwrote path while rejecting foreign archive: %v", err)
	}
}

func TestExpiredPendingTransitionCanBeAbandonedThenUpdatedToNewTip(t *testing.T) {
	var failFile, expireOldHold atomic.Bool
	var expiredID atomic.Value
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/read/hold" && expireOldHold.Load() {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read hold request: %v", err)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var request struct {
					CommitID string `json:"commitID"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Errorf("decode hold request: %v", err)
					return
				}
				if request.CommitID == expiredID.Load() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusGone)
					_, _ = w.Write([]byte(`{"code":"payload_expired","message":"version contents have expired"}`))
					return
				}
			}
			if r.URL.Path == "/v1/file" && failFile.CompareAndSwap(true, false) {
				http.Error(w, "interrupted file read", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "abandon-expired-base", "", []project.Change{makeFileChange("file.bin", "base")}, map[string]string{"file.bin": "base"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	target := seed(t, remote, "abandon-expired-target", base.ID, []project.Change{makeFileChange("file.bin", "target")}, map[string]string{"file.bin": "target"})
	failFile.Store(true)
	if err := w.Update(context.Background()); err == nil || w.Pending() != "transition" {
		t.Fatalf("interrupted Update err=%v pending=%q", err, w.Pending())
	}
	expiredID.Store(target.ID)
	expireOldHold.Store(true)
	if _, err := w.Resume(context.Background()); !errors.Is(err, project.ErrPayloadExpired) {
		t.Fatalf("Resume()=%v, want expired source", err)
	}
	if w.Pending() != "transition" || w.Base() != base.ID {
		t.Fatalf("expired resume changed state: base=%q pending=%q", w.Base(), w.Pending())
	}
	if err := w.Abort(context.Background()); err != nil {
		t.Fatalf("Abort expired transition: %v", err)
	}
	if w.Pending() != "" || w.Base() != base.ID {
		t.Fatalf("after abandon base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "base")
	newer := seed(t, remote, "abandon-expired-newer", target.ID, []project.Change{makeFileChange("file.bin", "newer retained")}, map[string]string{"file.bin": "newer retained"})
	if err := w.Update(context.Background()); err != nil {
		t.Fatalf("Update to newer retained tip: %v", err)
	}
	if w.Base() != newer.ID || w.Pending() != "" {
		t.Fatalf("after later update base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "newer retained")
}

func TestAbortCheckoutKeepsInstalledFilesAsAdditions(t *testing.T) {
	var failSecond atomic.Bool
	failSecond.Store(true)
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/file" && r.URL.Query().Get("path") == "b.bin" && failSecond.CompareAndSwap(true, false) {
				http.Error(w, "interrupted checkout", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	commit := seed(t, remote, "abandon-checkout-base", "", []project.Change{makeFileChange("a.bin", "a"), makeFileChange("b.bin", "b")}, map[string]string{"a.bin": "a", "b.bin": "b"})
	directory := filepath.Join(t.TempDir(), "workspace")
	if _, err := Create(context.Background(), Options{Directory: directory, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main"}); err == nil {
		t.Fatal("checkout unexpectedly completed")
	}
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	id := w.state.Pending.ReadID
	if err := w.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Base() != "" || w.Pending() != "" || w.RecoveryDirectory() != recoveryDirectory(id) {
		t.Fatalf("checkout abandon base=%q pending=%q archive=%q", w.Base(), w.Pending(), w.RecoveryDirectory())
	}
	assertFileContents(t, filepath.Join(directory, "a.bin"), "a")
	changes, err := w.Status(context.Background())
	if err != nil || !reflect.DeepEqual(changePaths(changes), []string{"a.bin"}) {
		t.Fatalf("Status after checkout abandon=%+v err=%v", changes, err)
	}
	if err := w.Update(context.Background()); err != nil {
		t.Fatalf("update abandoned checkout to retained tip: %v", err)
	}
	if w.Base() != commit.ID || w.Pending() != "" {
		t.Fatalf("after update base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "b.bin"), "b")
}
