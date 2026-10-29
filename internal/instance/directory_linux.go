package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"golang.org/x/sys/unix"
)

func lockDirectory(root string) (*flock.Flock, error) {
	name := filepath.Join(root, "instance.lock")
	if info, err := os.Lstat(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("instance lock is not a regular file")
	}
	lock := flock.New(name)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("instance is already in use; stop the server before maintenance")
	}
	return lock, nil
}

// prepareDirectory permits empty bind mounts, but never adopts existing data.
// The caller holds the returned lock until state has been published.
func prepareDirectory(root string) (_ *flock.Flock, err error) {
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if info, err := os.Lstat(root); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, errors.New("destination is not a directory")
	}
	lock, err := lockDirectory(root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Unlock()
		}
	}()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() != "instance.lock" && (entry.Name() != "projects" || !entry.IsDir()) {
			return nil, fmt.Errorf("destination is not empty: %s", entry.Name())
		}
	}
	projects := filepath.Join(root, "projects")
	if err := os.Mkdir(projects, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	entries, err = os.ReadDir(projects)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, errors.New("destination project storage is not empty; use an empty directory")
	}
	for _, name := range []string{root, projects} {
		if err := os.Chmod(name, 0700); err != nil {
			return nil, err
		}
	}
	if err := syncPath(filepath.Dir(root)); err != nil {
		return nil, err
	}
	return lock, nil
}

// State is the publication point. Its staging directory stays on the metadata
// filesystem; projects can be a separate mounted filesystem.
func publishState(stage, root string) error {
	for _, name := range []string{filepath.Join(root, "projects"), stage} {
		if err := syncPath(name); err != nil {
			return err
		}
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, filepath.Join(root, "state"), unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := syncPath(root); err != nil {
		return fmt.Errorf("instance published at %s, but directory synchronization failed: %w", root, err)
	}
	return nil
}
