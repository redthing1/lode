//go:build linux || darwin

package workspace

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func openWorkspaceLock(root *os.Root) (*os.File, error) {
	f, err := openNoFollow(root, ".lode/lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func openNoFollow(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
	return root.OpenFile(name, flags|syscall.O_NOFOLLOW, perm)
}

func openRegular(root *os.Root, name string) (*os.File, error) {
	parent, base, err := openRenameParent(root, name)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func hasMultipleLinks(info os.FileInfo) bool {
	return info.Sys().(*syscall.Stat_t).Nlink > 1
}

func syncDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// renameNoReplace atomically moves one workspace entry without replacing a
// destination. Both names are slash-separated paths relative to root.
func renameNoReplace(root *os.Root, oldname, newname string) error {
	oldParent, oldBase, err := openRenameParent(root, oldname)
	if err != nil {
		return fmt.Errorf("open source parent: %w", err)
	}
	defer oldParent.Close()
	newParent, newBase, err := openRenameParent(root, newname)
	if err != nil {
		return fmt.Errorf("open destination parent: %w", err)
	}
	defer newParent.Close()
	if err := renameAtNoReplace(oldParent, oldBase, newParent, newBase); err != nil {
		return fmt.Errorf("rename %s to %s: %w", oldname, newname, err)
	}
	if err := oldParent.Sync(); err != nil {
		return fmt.Errorf("sync source parent after rename: %w", err)
	}
	if err := newParent.Sync(); err != nil {
		return fmt.Errorf("sync destination parent after rename: %w", err)
	}
	return nil
}

func openRenameParent(root *os.Root, name string) (*os.File, string, error) {
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == "." || name == ".." {
		return nil, "", fmt.Errorf("invalid workspace path %q", name)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, "", fmt.Errorf("invalid workspace path %q", name)
		}
	}
	current, err := root.Open(".")
	if err != nil {
		return nil, "", err
	}
	// Each child is opened relative to the already-validated parent and rejects
	// symlinks, including intermediate ones.
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		closeErr := current.Close()
		if openErr != nil {
			return nil, "", openErr
		}
		if closeErr != nil {
			unix.Close(next)
			return nil, "", closeErr
		}
		current = os.NewFile(uintptr(next), part)
	}
	return current, parts[len(parts)-1], nil
}
