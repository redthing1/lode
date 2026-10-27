package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/workspace"
)

func TestBranchAndSwitchCLIWorkflow(t *testing.T) {
	ctx := context.Background()
	p, err := project.Create(ctx, filepath.Join(t.TempDir(), "server"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	srv := httptest.NewServer(remote.Handler(p))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scene.bin"), []byte("scene"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workspace.Attach(ctx, srv.URL, p.Token(), dir, model.Selection{{Include: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Save(ctx, "tester", "initial"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	invoke := func(args ...string) string {
		t.Helper()
		var out, diagnostics bytes.Buffer
		if err := run(ctx, append([]string{"-C", dir}, args...), &out, &diagnostics); err != nil {
			t.Fatalf("run %v: %v (diagnostics %s)", args, err, diagnostics.String())
		}
		return out.String()
	}
	if got := invoke("branch", "feature"); !strings.Contains(got, "Created branch feature") {
		t.Fatalf("branch output: %q", got)
	}
	if got := invoke("branch"); !strings.Contains(got, "feature") {
		t.Fatalf("branch list output: %q", got)
	}
	if got := invoke("switch", "feature"); !strings.Contains(got, "Switched to branch feature") {
		t.Fatalf("switch output: %q", got)
	}

	var out, diagnostics bytes.Buffer
	if err := run(ctx, []string{"-C", dir, "branch", "-delete", "feature"}, &out, &diagnostics); err == nil || !strings.Contains(err.Error(), "current workspace branch") {
		t.Fatalf("delete current branch error = %v", err)
	}
}

func TestBackupCLIIsAvailableOutsideAWorkspace(t *testing.T) {
	ctx := context.Background()
	// The -C directory need not exist: backup is a project-level operation.
	dir := filepath.Join(t.TempDir(), "not-a-workspace")
	var out, diagnostics bytes.Buffer
	destination := filepath.Join(t.TempDir(), "backup")
	source := filepath.Join(t.TempDir(), "source")
	p, err := project.Create(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"-C", dir, "backup", source, destination}, &out, &diagnostics); err != nil {
		t.Fatalf("backup CLI: %v (diagnostics %s)", err, diagnostics.String())
	}
	if !strings.Contains(out.String(), "Backup created:") {
		t.Fatalf("backup output = %q", out.String())
	}
}

func TestTagAndCleanupPreviewCLI(t *testing.T) {
	ctx := context.Background()
	p, err := project.Create(ctx, filepath.Join(t.TempDir(), "server"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	srv := httptest.NewServer(remote.Handler(p))
	defer srv.Close()
	dir := t.TempDir()
	file := filepath.Join(dir, "scene.bin")
	if err := os.WriteFile(file, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workspace.Attach(ctx, srv.URL, p.Token(), dir, model.Selection{{Include: true}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Save(ctx, "tester", "first")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := w.Save(ctx, "tester", "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (string, error) {
		t.Helper()
		var out, diagnostics bytes.Buffer
		err := run(ctx, append([]string{"-C", dir}, args...), &out, &diagnostics)
		return out.String(), err
	}
	if out, err := invoke("tag", "release", first.ID); err != nil || !strings.Contains(out, "Created tag release") {
		t.Fatalf("tag output %q: %v", out, err)
	}
	if out, err := invoke("tag"); err != nil || !strings.Contains(out, "release "+first.ID) {
		t.Fatalf("tag list %q: %v", out, err)
	}
	if out, err := invoke("cleanup", "-keep", "0"); err != nil || !strings.Contains(out, "keep "+second.ID) || !strings.Contains(out, "keep "+first.ID) {
		t.Fatalf("cleanup preview %q: %v", out, err)
	}
	if out, err := invoke("tag", "-delete", "release"); err != nil || !strings.Contains(out, "Deleted tag release") {
		t.Fatalf("tag delete %q: %v", out, err)
	}
	if out, err := invoke("tag", "-delete", "release"); err != nil || !strings.Contains(out, "already absent") {
		t.Fatalf("repeat tag delete %q: %v", out, err)
	}
	if out, err := invoke("cleanup", "-keep", "0"); err != nil || !strings.Contains(out, "release "+first.ID) || !strings.Contains(out, "keep "+second.ID) {
		t.Fatalf("cleanup release preview %q: %v", out, err)
	}
	if _, err := invoke("cleanup"); err == nil || !strings.Contains(err.Error(), "explicit -keep") {
		t.Fatalf("missing keep error %v", err)
	}
}
