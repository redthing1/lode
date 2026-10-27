package workspace

import (
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

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
	if err := unix.Renameat2(int(oldParent.Fd()), oldBase, int(newParent.Fd()), newBase, unix.RENAME_NOREPLACE); err != nil {
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
