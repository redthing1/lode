package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The CLI discovers roots; the workspace service still opens an exact root.
// Any .lode entry stops the search, so a broken workspace cannot select a parent.
func findWorkspace(directory string) (string, error) {
	current, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(current)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", directory)
	}
	for {
		if _, err := os.Lstat(filepath.Join(current, ".lode")); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("not in a Lode workspace; use lode clone or lode attach, or choose a workspace with -C DIR")
		}
		current = parent
	}
}
