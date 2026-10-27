//go:build linux

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/redthing1/lode/internal/project"
)

func TestCloneUnknownBranchDoesNotCreateEmptyWorkspace(t *testing.T) {
	remote := newTestRemote(t, nil)
	seed(t, remote, "existing-main", "", []project.Change{makeFileChange("file", "contents")}, map[string]string{"file": "contents"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w, err := Create(context.Background(), Options{Directory: directory, Remote: remote.server.URL, Token: remote.local.Token, Branch: "misspelled"})
	if w != nil {
		_ = w.Close()
	}
	if !errors.Is(err, project.ErrBranchNotFound) {
		t.Fatalf("unknown branch: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed clone changed destination: %v", err)
	}
}
