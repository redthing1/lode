//go:build linux

package workspace

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/redthing1/lode/internal/model"
)

// moveNoReplace atomically moves one workspace entry without replacing an
// existing destination. Parent directories are opened relative to root with
// symlink following disabled, then synced after the rename.
func moveNoReplace(root *os.Root, from, to string) error {
	if root == nil {
		return errors.New("move workspace entry: nil root")
	}
	source, err := openRelativeParent(root, from)
	if err != nil {
		return fmt.Errorf("open move source parent: %w", err)
	}
	defer source.close()
	destination, err := openRelativeParent(root, to)
	if err != nil {
		return fmt.Errorf("open move destination parent: %w", err)
	}
	defer destination.close()
	if err := requireNonSymlink(source.fd, source.name); err != nil {
		return fmt.Errorf("inspect move source: %w", err)
	}
	if err := unix.Renameat2(source.fd, source.name, destination.fd, destination.name, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("rename %q to %q without replacement: %w", from, to, err)
	}
	return errors.Join(syncDirFD(source.fd), syncDirFD(destination.fd))
}

// linkNoReplace creates a hard link to an existing regular file without
// replacing a destination. It does not follow the final source symlink.
func linkNoReplace(root *os.Root, from, to string) error {
	if root == nil {
		return errors.New("link workspace file: nil root")
	}
	source, err := openRelativeParent(root, from)
	if err != nil {
		return fmt.Errorf("open link source parent: %w", err)
	}
	defer source.close()
	destination, err := openRelativeParent(root, to)
	if err != nil {
		return fmt.Errorf("open link destination parent: %w", err)
	}
	defer destination.close()
	before, err := statAtNoFollow(source.fd, source.name)
	if err != nil {
		return fmt.Errorf("inspect link source: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("link source %q is not a regular file", from)
	}
	if err := unix.Linkat(source.fd, source.name, destination.fd, destination.name, 0); err != nil {
		return fmt.Errorf("link %q to %q without replacement: %w", from, to, err)
	}
	after, err := statAtNoFollow(destination.fd, destination.name)
	if err != nil {
		return fmt.Errorf("inspect linked destination %q: %w", to, err)
	}
	if after.Mode&unix.S_IFMT != unix.S_IFREG || after.Dev != before.Dev || after.Ino != before.Ino {
		// The caller's journal must inspect an uncertain destination. Never
		// unlink it here because another process may have replaced our link.
		return fmt.Errorf("source %q changed while linking; destination %q needs inspection", from, to)
	}
	if err := syncDirFD(destination.fd); err != nil {
		return fmt.Errorf("sync link destination directory: %w", err)
	}
	return nil
}

type relativeParent struct {
	fd   int
	name string
}

func (p relativeParent) close() {
	if p.fd >= 0 {
		_ = unix.Close(p.fd)
	}
}

func openRelativeParent(root *os.Root, value string) (relativeParent, error) {
	logical, err := model.ParsePath(value)
	if err != nil || logical.IsRoot() {
		return relativeParent{fd: -1}, fmt.Errorf("invalid workspace path %q", value)
	}
	parts := strings.Split(value, "/")
	rootFile, err := root.Open(".")
	if err != nil {
		return relativeParent{fd: -1}, fmt.Errorf("open workspace root: %w", err)
	}
	currentFD, err := unix.Dup(int(rootFile.Fd()))
	if err != nil {
		_ = rootFile.Close()
		return relativeParent{fd: -1}, fmt.Errorf("duplicate workspace root descriptor: %w", err)
	}
	unix.CloseOnExec(currentFD)
	owned := true
	defer func() {
		if owned {
			_ = unix.Close(currentFD)
		}
		_ = rootFile.Close()
	}()
	for _, component := range parts[:len(parts)-1] {
		nextFD, err := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return relativeParent{fd: -1}, err
		}
		if owned {
			_ = unix.Close(currentFD)
		}
		currentFD = nextFD
		owned = true
	}
	result := relativeParent{fd: currentFD, name: parts[len(parts)-1]}
	owned = false
	return result, nil
}

func requireNonSymlink(parentFD int, name string) error {
	stat, err := statAtNoFollow(parentFD, name)
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("%q is a symbolic link", name)
	}
	return nil
}

func statAtNoFollow(parentFD int, name string) (*unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	return &stat, nil
}

func syncDirFD(fd int) error { return unix.Fsync(fd) }
