package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/project"
)

func TestBackupCommandRequiresStoppedStoreAndFreshDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source"), filepath.Join(dir, "backup")
	if err := project.InitializeLocal(ctx, source); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	projectID, token := local.Service.ID(), local.Token
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"backup", source, destination}, &stdout, &stderr, "test"); code != 1 || stdout.Len() != 0 {
		t.Fatalf("busy backup: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"backup", source, destination}, &stdout, &stderr, "test"); code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), destination) {
		t.Fatalf("backup: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := os.Rename(source, filepath.Join(dir, "original-offline")); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"backup", destination, restored}, &stdout, &stderr, "test"); code != 0 || stderr.Len() != 0 {
		t.Fatalf("restore: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	copy, err := project.OpenLocal(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if copy.Service.ID() != projectID || copy.Token != token {
		t.Fatal("restored identity or credentials changed")
	}
	for _, args := range [][]string{{"backup"}, {"backup", destination}, {"backup", destination, restored}, {"backup", destination, restored, "extra"}} {
		stdout.Reset()
		stderr.Reset()
		if code := Run(args, &stdout, &stderr, "test"); code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("invalid backup %q: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}
