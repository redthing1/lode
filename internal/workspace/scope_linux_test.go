package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/progress"
)

func TestScopedWorkspaceSharesWorkingTree(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	scope := model.Selection{{Path: "art", Include: true}, {Path: "levels/forest.blend", Include: true}}
	seed := t.TempDir()
	writeFile(t, seed, ".git/HEAD", "source index")
	writeFile(t, seed, "art/nested/.git", "linked worktree")
	writeFile(t, seed, "art/palette", "palette")
	writeFile(t, seed, "levels/forest.blend", "first forest")
	writeFile(t, seed, "levels/forest.go", "local source")
	owner, err := Attach(ctx, f.url, f.client, seed, ConnectOptions{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	v := save(t, f, owner, "initial assets")
	if files := remoteFiles(t, f, v.ID); len(files) != 2 || files["art/palette"] != "palette" || files["levels/forest.blend"] != "first forest" {
		t.Fatal(files)
	}
	full := cloneWorkspace(t, f, nil)
	writeFile(t, full.Directory(), "levels/forest.go", "separately versioned on server")
	save(t, f, full, "additional project file")

	dir := t.TempDir()
	writeFile(t, dir, ".git", "gitdir: elsewhere")
	writeFile(t, dir, "levels/forest.go", "unsaved Git edit")
	if _, err := Clone(ctx, f.url, f.client, dir, ConnectOptions{Selection: scope}); err == nil || !strings.Contains(err.Error(), "clone destination must be empty") {
		t.Fatalf("ordinary clone into populated directory: %v", err)
	}
	peer, err := Clone(ctx, f.url, f.client, dir, ConnectOptions{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if got := readFile(t, dir, "levels/forest.blend"); got != "first forest" {
		t.Fatal(got)
	}
	art := model.Selection{{Path: "art", Include: true}}
	if err := peer.Select(ctx, f.token, art, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "levels/forest.blend")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("asset not unloaded: %v", err)
	}
	if err := peer.Select(ctx, f.token, model.Selection{{Path: "levels/forest.go", Include: true}}, nil); err == nil || !strings.Contains(err.Error(), "outside the workspace scope") {
		t.Fatalf("unrelated include: %v", err)
	}
	if err := peer.Select(ctx, f.token, model.Selection{{Include: true}}, nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "levels/forest.blend", "changed forest")
	v = save(t, f, peer, "asset edit")
	if files := remoteFiles(t, f, v.ID); len(files) != 3 || files["levels/forest.go"] != "separately versioned on server" || files["levels/forest.blend"] != "changed forest" {
		t.Fatal(files)
	}
	if err := owner.Update(ctx, f.token, nil); err != nil {
		t.Fatal(err)
	}
	if readFile(t, seed, "levels/forest.blend") != "changed forest" || readFile(t, seed, "levels/forest.go") != "local source" ||
		readFile(t, dir, "levels/forest.go") != "unsaved Git edit" || readFile(t, dir, ".git") != "gitdir: elsewhere" {
		t.Fatal("update or select touched an unrelated local file")
	}
}

func TestSetScopeKeepsFilesAndRejectsLegacyReader(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "art/scene", "saved")
	w := attach(t, f, dir)
	save(t, f, w, "before scope")
	writeFile(t, dir, "src/main.go", "source")
	if err := w.SetScope(model.Selection{{Path: "art/other", Include: true}}); err == nil || !strings.Contains(err.Error(), `saved path "art/scene"`) {
		t.Fatalf("excluded saved baseline: %v", err)
	}
	if err := w.SetScope(model.Selection{{Path: "art/scene", Include: true}}); err != nil {
		t.Fatal(err)
	}
	// Released clients decode state at the top level and validate its identity.
	var legacy State
	if err := w.readJSON("state.json", &legacy); err != nil || legacy.validate() == nil {
		t.Fatalf("legacy read accepted scoped state: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if changes, err := reopened.Status(ctx); err != nil || len(changes) != 0 || reopened.State().Scope.Includes("art") || !reopened.State().Scope.Includes("art/scene") {
		t.Fatalf("scoped status after reopen: %v %v", changes, err)
	}
	if readFile(t, dir, "art/scene") != "saved" || readFile(t, dir, "src/main.go") != "source" {
		t.Fatal("setting scope changed working files")
	}
}

func TestScopedClonePreflightAndRecovery(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := attach(t, f, t.TempDir())
	writeFile(t, owner.Directory(), "levels/a", "first")
	writeFile(t, owner.Directory(), "levels/b", "second")
	save(t, f, owner, "initial assets")
	options := ConnectOptions{Scope: model.Selection{{Path: "levels/a", Include: true}, {Path: "levels/b", Include: true}}}
	for _, mode := range []string{"existing collision", "symlink collision", "late collision", "resume", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "levels/source", "keep")
			cloneCtx := ctx
			switch mode {
			case "existing collision":
				writeFile(t, dir, "levels/b", "differ")
			case "symlink collision":
				writeFile(t, dir, "copy", "second")
				if err := os.Symlink("../copy", filepath.Join(dir, "levels/b")); err != nil {
					t.Fatal(err)
				}
			case "late collision":
				cloneCtx = progress.WithReporter(ctx, func(e progress.Event) {
					if e.Phase == "Updating" && e.Path == "levels/b" {
						writeFile(t, dir, "levels/b", "late local contents")
					}
				})
			default:
				f.dropFile.Store(true)
			}
			if _, err := Clone(cloneCtx, f.url, f.client, dir, options); err == nil {
				t.Fatal("clone unexpectedly succeeded")
			} else if strings.Contains(mode, "collision") && !strings.Contains(err.Error(), "levels/b") {
				t.Fatal(err)
			}
			if mode == "existing collision" || mode == "symlink collision" {
				for _, name := range []string{".lode", "levels/a"} {
					if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("preflight created %s: %v", name, err)
					}
				}
				return
			}
			w, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if mode != "late collision" {
				w = interruptAction(t, f, w, 0)
			}
			if readFile(t, dir, "levels/a") != "first" {
				t.Fatal("fixture did not interrupt after installing the first file")
			}
			if mode == "resume" {
				if err := w.Resume(ctx, f.token); err != nil {
					t.Fatal(err)
				}
				if readFile(t, dir, "levels/b") != "second" {
					t.Fatal("resume did not complete clone")
				}
			} else {
				if err := w.Cancel(ctx, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(dir, "levels/a")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cancel kept installed file: %v", err)
				}
				if mode == "late collision" && readFile(t, dir, "levels/b") != "late local contents" {
					t.Fatal("cancel changed the colliding file")
				}
			}
			if readFile(t, dir, "levels/source") != "keep" {
				t.Fatal("clone or recovery changed the pre-existing parent")
			}
		})
	}
}

func TestScopedCloneReusesMatchingFiles(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := attach(t, f, t.TempDir())
	writeFile(t, owner.Directory(), "assets/a", "already here")
	writeFile(t, owner.Directory(), "assets/b", "second file")
	saved := save(t, f, owner, "assets")
	options := ConnectOptions{Scope: model.Selection{{Path: "assets", Include: true}}}
	for _, mode := range []string{"all present", "partially present"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "assets/a", "already here")
			if mode == "all present" {
				writeFile(t, dir, "assets/b", "second file")
			}
			name := filepath.Join(dir, "assets/a")
			original, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			transport := f.client.HTTP.Transport
			counts := &batchTransport{RoundTripper: transport}
			f.client.HTTP.Transport = counts
			t.Cleanup(func() { f.client.HTTP.Transport = transport })
			w, err := Clone(ctx, f.url, f.client, dir, options)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if now, err := os.Stat(name); err != nil || !os.SameFile(original, now) {
				t.Fatalf("matching file was replaced: %v", err)
			}
			if mode == "all present" && len(counts.requests) != 0 ||
				mode == "partially present" && (len(counts.requests) != 1 || len(counts.requests[0]) != 1 || counts.requests[0][0].Path != "assets/b") ||
				counts.singles != 0 {
				t.Fatalf("unexpected downloads: batches=%v singles=%d", counts.requests, counts.singles)
			}
			if changes, err := w.Status(ctx); err != nil || len(changes) != 0 || w.State().Version != saved.ID {
				t.Fatalf("clone did not establish a clean baseline: %v %v", changes, err)
			}
		})
	}
}

func TestScopedCloneRechecksReusedFilesOnResume(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	owner := attach(t, f, t.TempDir())
	writeFile(t, owner.Directory(), "assets/a", "original")
	writeFile(t, owner.Directory(), "assets/b", "download")
	saved := save(t, f, owner, "assets")
	options := ConnectOptions{Scope: model.Selection{{Path: "assets", Include: true}}}
	for _, mode := range []string{"resume", "cancel", "cancel with all files present"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "assets/a", "original")
			allPresent := mode == "cancel with all files present"
			if allPresent {
				writeFile(t, dir, "assets/b", "download")
			}
			cloneCtx, interrupt := context.WithCancel(ctx)
			defer interrupt()
			cloneCtx = progress.WithReporter(cloneCtx, func(e progress.Event) {
				if e.Phase == "Checking" && e.Path == "assets/a" {
					if _, err := os.Stat(filepath.Join(dir, ".lode", "transition.json")); err == nil {
						interrupt()
					}
				}
			})
			if _, err := Clone(cloneCtx, f.url, f.client, dir, options); !errors.Is(err, context.Canceled) {
				t.Fatalf("clone should stop before committing: %v", err)
			}
			if readFile(t, dir, "assets/b") != "download" {
				t.Fatal("clone stopped before installing the missing file")
			}
			writeFile(t, dir, "assets/a", "modified")
			w, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if err := w.Resume(ctx, f.token); err == nil || !strings.Contains(err.Error(), `changed file "assets/a"`) || w.State().Version != "" {
				t.Fatalf("resume accepted the changed file: %v", err)
			}
			if mode == "resume" {
				writeFile(t, dir, "assets/a", "original")
				if err := w.Resume(ctx, f.token); err != nil || w.State().Version != saved.ID {
					t.Fatalf("resume failed with the matching file restored: %v", err)
				}
				return
			}
			if err := w.Cancel(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if allPresent {
				if readFile(t, dir, "assets/b") != "download" {
					t.Fatal("cancel changed the second pre-existing file")
				}
			} else if _, err := os.Stat(filepath.Join(dir, "assets/b")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cancel kept the installed file: %v", err)
			}
			if readFile(t, dir, "assets/a") != "modified" || w.State().Version != "" {
				t.Fatal("cancel affected the pre-existing file or committed the clone")
			}
		})
	}
	t.Run("committed before baseline write", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "assets/a", "original")
		const blocker = ".lode/state.json.next/keep"
		cloneCtx := progress.WithReporter(ctx, func(e progress.Event) {
			if e.Phase == "Finishing workspace" {
				writeFile(t, dir, blocker, "block the baseline write")
			}
		})
		if _, err := Clone(cloneCtx, f.url, f.client, dir, options); err == nil || !strings.Contains(err.Error(), "state.json.next") {
			t.Fatalf("clone did not stop after commitment: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, blocker)); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, "assets/a", "modified")
		w, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if w.State().Version != "" {
			t.Fatal("baseline was written before recovery")
		}
		if err := w.Cancel(ctx, ""); err != nil || w.State().Version != saved.ID {
			t.Fatalf("cancel should finish the committed clone: %v", err)
		}
		changes, err := w.Status(ctx)
		if err != nil || len(changes) != 1 || changes[0].Path != "assets/a" || readFile(t, dir, "assets/a") != "modified" {
			t.Fatalf("recovery did not preserve the later edit: %v %v", changes, err)
		}
	})
}

func TestLegacyGitEntriesStayOnServer(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seed := attach(t, f, t.TempDir())
	writeFile(t, seed.Directory(), "asset", "asset")
	base := save(t, f, seed, "asset")
	w := cloneWorkspace(t, f, nil)
	writeFile(t, w.Directory(), ".git/HEAD", "working Git repository")
	if changes, err := w.Status(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("unscoped status included Git metadata: %v %v", changes, err)
	}
	const gitData = "gitdir: historic"
	gitEntry := model.Entry{Path: ".git", Kind: model.File, Size: int64(len(gitData)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(gitData)))}
	request := model.Save{ID: uuid.Must(uuid.NewV7()).String(), Branch: "main", Base: base.ID, Changes: []model.Entry{gitEntry}}
	if _, err := f.client.Save(ctx, "alice", "project", request, nil, func(model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(gitData)), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Update(ctx, f.token, nil); err == nil || !strings.Contains(err.Error(), `Git metadata path ".git"`) {
		t.Fatalf("protected update: %v", err)
	}
	if readFile(t, w.Directory(), ".git/HEAD") != "working Git repository" {
		t.Fatal("update changed live metadata")
	}
	if _, err := Clone(ctx, f.url, f.client, t.TempDir(), ConnectOptions{}); err == nil || !strings.Contains(err.Error(), `Git metadata path ".git"`) {
		t.Fatalf("protected clone: %v", err)
	}
	peer, err := Clone(ctx, f.url, f.client, t.TempDir(), ConnectOptions{Scope: model.Selection{{Path: "asset", Include: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if files := remoteFiles(t, f, peer.State().Version); files[".git"] != gitData || readFile(t, peer.Directory(), "asset") != "asset" {
		t.Fatal(files)
	}
}
