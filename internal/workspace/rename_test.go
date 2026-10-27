package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(root, "file", ".lode/moved"); err == nil {
		t.Fatal("rename into missing parent unexpectedly succeeded")
	}
	if err := os.Mkdir(filepath.Join(dir, ".lode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(root, "file", ".lode/moved"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".lode/moved"))
	if err != nil || string(got) != "contents" {
		t.Fatalf("moved file = %q, %v", got, err)
	}

	if err := os.Mkdir(filepath.Join(dir, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(root, "empty", "empty-moved"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "empty-moved"))
	if err != nil || !info.IsDir() {
		t.Fatalf("moved empty directory: info=%v err=%v", info, err)
	}
	if err := os.WriteFile(filepath.Join(dir, `literal\name`), []byte("backslash"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(root, `literal\name`, `moved\name`); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dir, `moved\name`))
	if err != nil || string(got) != "backslash" {
		t.Fatalf("moved backslash filename = %q, %v", got, err)
	}
}

func TestRenameNoReplaceCollisionPreservesBoth(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for name, contents := range map[string]string{"old": "source", "new": "destination"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := renameNoReplace(root, "old", "new"); err == nil {
		t.Fatal("collision unexpectedly succeeded")
	}
	for name, want := range map[string]string{"old": "source", "new": "destination"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestRenameNoReplaceRejectsSymlinkParentAndEscapes(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(root, "source", "link/moved"); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "moved")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside destination exists: %v", err)
	}
	for _, name := range []string{"../outside", "real/../../outside", "/tmp/outside", "a//b"} {
		if err := renameNoReplace(root, "source", name); err == nil {
			t.Errorf("accepted escaping or noncanonical path %q", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "source")); err != nil {
		t.Fatalf("source changed: %v", err)
	}
}
