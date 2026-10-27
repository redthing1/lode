package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindWorkspaceFromNestedAndSymlinkedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".lode"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "assets", "models")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "models")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{root, nested, alias} {
		got, err := findWorkspace(directory)
		if err != nil || got != want {
			t.Fatalf("findWorkspace(%q) = %q, %v; want %q", directory, got, err, want)
		}
	}
	if _, err := findWorkspace(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nonexistent starting directory must not select its parent: %v", err)
	}
}

func TestFindWorkspaceStopsAtNearestMarkerEvenWhenBroken(t *testing.T) {
	for _, marker := range []string{"directory", "file", "broken symlink"} {
		t.Run(marker, func(t *testing.T) {
			outer := t.TempDir()
			inner := filepath.Join(outer, "nested")
			if err := os.MkdirAll(filepath.Join(inner, "child"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(outer, ".lode"), 0700); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(inner, ".lode")
			var err error
			switch marker {
			case "directory":
				err = os.Mkdir(name, 0700)
			case "file":
				err = os.WriteFile(name, []byte("broken"), 0600)
			case "broken symlink":
				err = os.Symlink("missing", name)
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := filepath.EvalSymlinks(inner)
			if err != nil {
				t.Fatal(err)
			}
			got, err := findWorkspace(filepath.Join(inner, "child"))
			if err != nil || got != want {
				t.Fatalf("nearest marker must win: got %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestFindWorkspaceOutsideReportsHowToStart(t *testing.T) {
	if _, err := findWorkspace(t.TempDir()); err == nil || !strings.Contains(err.Error(), "lode clone or lode attach") {
		t.Fatalf("missing-workspace diagnostic: %v", err)
	}
}
