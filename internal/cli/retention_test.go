package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transport"
	"github.com/redthing1/lode/internal/workspace"
)

func TestRetentionAndTagCommandsAgainstLocalServer(t *testing.T) {
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
	directory := filepath.Join(root, "workspace")
	w, err := workspace.Create(ctx, workspace.Options{Directory: directory, Remote: server.URL, Token: local.Token, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "file.bin")
	if err := os.WriteFile(file, []byte("base bytes"), 0644); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	base, err := w.Commit(ctx, "cli-retention", "base")
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("new bytes"), 0644); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, "cli-retention", "second"); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	invoke := func(args ...string) (string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr, "test")
		if code != 0 {
			return stdout.String(), errors.New(stderr.String())
		}
		return stdout.String(), nil
	}
	if out, err := invoke("tag", "create", "-C", directory, "--from", base.ID, "release/v1"); err != nil || !strings.Contains(out, base.ID) {
		t.Fatalf("tag create = %q, %v", out, err)
	}
	if out, err := invoke("tag", "list", "-C", directory); err != nil || !strings.Contains(out, "release/v1 "+base.ID) {
		t.Fatalf("tag list = %q, %v", out, err)
	}
	if out, err := invoke("retention", "show", "-C", directory); err != nil || !strings.Contains(out, "keep-last: 10") || !strings.Contains(out, "keep-days: 7") {
		t.Fatalf("retention show = %q, %v", out, err)
	}
	if out, err := invoke("retention", "set", "-C", directory, "--last", "0", "--days", "0"); err != nil || !strings.Contains(out, "keep-last 0") {
		t.Fatalf("retention set = %q, %v", out, err)
	}
	if out, err := invoke("retention", "why", "-C", directory, base.ID); err != nil || !strings.Contains(out, "state: retained") || !strings.Contains(out, "reason: tag:release/v1") {
		t.Fatalf("retention why = %q, %v", out, err)
	}
	if out, err := invoke("retention", "preview", "-C", directory); err != nil || !strings.Contains(out, "candidates: 0") {
		t.Fatalf("retention preview = %q, %v", out, err)
	}
	if out, err := invoke("retention", "apply", "-C", directory); err != nil || !strings.Contains(out, "no eligible payloads") {
		t.Fatalf("retention apply = %q, %v", out, err)
	}
	if out, err := invoke("storage", "status", "-C", directory); err != nil || !strings.Contains(out, "blob-bytes:") || !strings.Contains(out, "indexed-live-bytes:") {
		t.Fatalf("storage status = %q, %v", out, err)
	}
	if out, err := invoke("tag", "delete", "-C", directory, "release/v1"); err != nil || !strings.Contains(out, "deleted tag") {
		t.Fatalf("tag delete = %q, %v", out, err)
	}
}

func TestRetentionCLIArgumentValidationAndHelp(t *testing.T) {
	for _, args := range [][]string{
		{"tag"}, {"tag", "create"}, {"tag", "delete"}, {"tag", "list", "extra"},
		{"retention"}, {"retention", "set", "--last", "-1", "--days", "1"},
		{"retention", "why"}, {"retention", "apply", "--limit", "0"}, {"storage"}, {"storage", "status", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test"); code != 1 || stdout.Len() != 0 {
			t.Errorf("Run(%q) = %d, stdout=%q stderr=%q; want argument error", args, code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help"}, &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("help code=%d: %s", code, stderr.String())
	}
	for _, line := range []string{"tag create", "tag delete", "retention show|set|preview|apply|why", "storage status"} {
		if !strings.Contains(stdout.String(), line) {
			t.Errorf("help missing %q", line)
		}
	}
}

func TestGroupedRetentionHelpRequiresNoExtraArguments(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"tag", "--help"}, "Usage: lode tag"},
		{[]string{"tag", "help"}, "tag create|delete|list"},
		{[]string{"retention", "-h"}, "Usage: lode retention"},
		{[]string{"retention", "help"}, "retention show|set|preview|apply|why"},
		{[]string{"storage", "--help"}, "Usage: lode storage"},
		{[]string{"storage", "help"}, "storage status"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(test.args, &stdout, &stderr, "test"); code != 0 || !strings.Contains(stdout.String(), test.want) {
			t.Errorf("Run(%q) = %d, stdout=%q stderr=%q", test.args, code, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"tag", "help", "extra"}, {"retention", "--help", "extra"}, {"storage", "help", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr, "test"); code != 1 || stdout.Len() != 0 {
			t.Errorf("Run(%q) = %d, stdout=%q stderr=%q; want extra-argument error", args, code, stdout.String(), stderr.String())
		}
	}
}
