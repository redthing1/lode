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
	for _, mode := range []string{"existing collision", "late collision", "resume", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "levels/source", "keep")
			cloneCtx := ctx
			switch mode {
			case "existing collision":
				writeFile(t, dir, "levels/b", "second")
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
			if mode == "existing collision" {
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
