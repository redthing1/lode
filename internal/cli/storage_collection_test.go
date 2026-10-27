package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transport"
	"github.com/redthing1/lode/internal/workspace"
)

func TestStorageCapacityAndCollectCommands(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := filepath.Join(root, "store")
	if err := project.InitializeLocal(ctx, store); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = local.Close() }()
	handler, err := transport.NewHandler(local.Service, local.Token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	workspaceDir := filepath.Join(root, "workspace")
	w, err := workspace.Create(ctx, workspace.Options{Directory: workspaceDir, Remote: server.URL, Token: local.Token, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	invoke := func(args ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr, "test")
		return stdout.String(), stderr.String(), code
	}
	stdout, stderr, code := invoke("storage", "capacity", "-C", workspaceDir)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "reserve-bytes: 1073741824") {
		t.Fatalf("capacity show: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = invoke("storage", "capacity", "-C", workspaceDir, "--reserve-bytes", "2147483648")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "reserve-bytes: 2147483648") {
		t.Fatalf("capacity set: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = invoke("storage", "collect", "-C", workspaceDir, "--max-rewrite-bytes", "1048576")
	if code != 0 || stderr != "" {
		t.Fatalf("storage collect: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, field := range []string{"collection-executed:", "blob-bytes-before:", "blob-bytes-after:", "rewritten-bytes:", "source-pack-pending:", "source-pack-reclaimed:"} {
		if !strings.Contains(stdout, field) {
			t.Errorf("collection output missing %q: %q", field, stdout)
		}
	}
	if strings.Contains(stdout, "SourcePack") || strings.Contains(stdout, "source-pack:") {
		t.Fatalf("collection output exposed source pack identifier: %q", stdout)
	}
	for _, args := range [][]string{
		{"storage", "capacity", "-C", workspaceDir, "--reserve-bytes", "0"},
		{"storage", "capacity", "-C", workspaceDir, "--reserve-bytes", "18446744073709551615"},
		{"storage", "collect", "-C", workspaceDir, "--max-rewrite-bytes", "0"},
		{"storage", "collect", "-C", workspaceDir, "--max-rewrite-bytes", "1073741825"},
	} {
		_, stderr, code := invoke(args...)
		if code != 1 || stderr == "" {
			t.Errorf("Run(%q) = code %d stderr=%q; want invalid input", args, code, stderr)
		}
	}
}
