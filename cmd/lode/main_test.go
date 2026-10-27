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
