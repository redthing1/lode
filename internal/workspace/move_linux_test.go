//go:build linux

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openMoveTestRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

func TestMoveAndLinkNoReplace(t *testing.T) {
	root, dir := openMoveTestRoot(t)
	if err := os.Mkdir(filepath.Join(dir, ".lode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "source-dir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("source bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source-dir", "child"), []byte("child"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := linkNoReplace(root, "source", ".lode/linked"); err != nil {
		t.Fatalf("linkNoReplace: %v", err)
	}
	originalInfo, err := os.Stat(filepath.Join(dir, "source"))
	if err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Stat(filepath.Join(dir, ".lode", "linked"))
	if err != nil || !os.SameFile(originalInfo, linkInfo) {
		t.Fatalf("hard-link identity: source=%v linked=%v err=%v", originalInfo, linkInfo, err)
	}

	if err := moveNoReplace(root, "source", ".lode/moved"); err != nil {
		t.Fatalf("moveNoReplace file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "source")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source after move: %v, want absent", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, ".lode", "moved")); err != nil || string(got) != "source bytes" {
		t.Fatalf("moved contents=%q err=%v", got, err)
	}

	if err := moveNoReplace(root, "source-dir", ".lode/backup-dir"); err != nil {
		t.Fatalf("moveNoReplace directory: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, ".lode", "backup-dir", "child")); err != nil || string(got) != "child" {
		t.Fatalf("moved directory child=%q err=%v", got, err)
	}
}

func TestNoReplacePreservesExistingEntries(t *testing.T) {
	root, dir := openMoveTestRoot(t)
	if err := os.Mkdir(filepath.Join(dir, ".lode"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"from": "source", "to": "destination"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := moveNoReplace(root, "from", "to"); err == nil {
		t.Fatal("moveNoReplace replaced existing destination")
	}
	if err := linkNoReplace(root, "from", "to"); err == nil {
		t.Fatal("linkNoReplace replaced existing destination")
	}
	for name, want := range map[string]string{"from": "source", "to": "destination"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q err=%v; want %q", name, got, err, want)
		}
	}
}

func TestNoReplaceRejectsSymlinkParentsAndSources(t *testing.T) {
	root, dir := openMoveTestRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".lode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "file"), filepath.Join(dir, "source-link")); err != nil {
		t.Fatal(err)
	}

	if err := moveNoReplace(root, "escape/file", ".lode/moved"); err == nil {
		t.Fatal("move followed symlink source parent")
	}
	if err := linkNoReplace(root, "escape/file", ".lode/linked"); err == nil {
		t.Fatal("link followed symlink source parent")
	}
	if err := moveNoReplace(root, "source", "escape/moved"); err == nil {
		t.Fatal("move followed symlink destination parent")
	}
	if err := linkNoReplace(root, "source", "escape/linked"); err == nil {
		t.Fatal("link followed symlink destination parent")
	}
	if err := moveNoReplace(root, "source-link", ".lode/moved-link"); err == nil {
		t.Fatal("move accepted final symlink source")
	}
	if err := linkNoReplace(root, "source-link", ".lode/linked-link"); err == nil {
		t.Fatal("link accepted final symlink source")
	}
	if got, err := os.ReadFile(filepath.Join(outside, "file")); err != nil || string(got) != "outside" {
		t.Fatalf("outside file changed: %q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "moved")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside move target stat=%v", err)
	}
}

func TestNoReplaceRejectsInvalidLogicalPaths(t *testing.T) {
	root, _ := openMoveTestRoot(t)
	for _, path := range []string{"", ".", "..", "../escape", "/absolute", "a//b", "a/../b", `a\\b`} {
		if err := moveNoReplace(root, path, "target"); err == nil {
			t.Errorf("moveNoReplace accepted invalid source %q", path)
		}
		if err := linkNoReplace(root, path, "target"); err == nil {
			t.Errorf("linkNoReplace accepted invalid source %q", path)
		}
		if err := moveNoReplace(root, "source", path); err == nil {
			t.Errorf("moveNoReplace accepted invalid destination %q", path)
		}
		if err := linkNoReplace(root, "source", path); err == nil {
			t.Errorf("linkNoReplace accepted invalid destination %q", path)
		}
	}
}
