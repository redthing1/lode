//go:build linux

package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// BackupLocal makes a complete, independently openable copy of a local
// project. Passing a backup as source restores it to another fresh destination.
// The copy is offline with respect to Lode's service lock; callers must also
// ensure that no external Kopia writer or maintenance process uses the source.
//
// The destination must not exist. Files are copied into a private sibling
// staging directory, validated by opening that copy, then published with a
// no-replace rename. A failure after the rename (for example, parent-directory
// sync failure) may be returned even though the destination is already visible.
func BackupLocal(ctx context.Context, source, destination string) (resultErr error) {
	if runtime.GOOS != "linux" {
		return errLocalUnsupported
	}
	if source == "" || destination == "" {
		return errors.New("backup local project: empty source or destination")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sourcePath, sourceInfo, err := resolveBackupSource(source)
	if err != nil {
		return err
	}
	destinationPath, destinationParent, err := resolveBackupDestination(destination)
	if err != nil {
		return err
	}
	if pathsOverlap(sourcePath, destinationPath) {
		return errors.New("backup local project: source and destination overlap")
	}
	if sourceInfo.Mode().Perm()&0077 != 0 || sourceInfo.Mode().Perm()&0700 != 0700 {
		return errors.New("backup local project: source directory must be private and owner-accessible")
	}

	metadataPath := filepath.Join(sourcePath, "metadata.sqlite")
	metadataInfo, err := os.Lstat(metadataPath)
	if err != nil {
		return fmt.Errorf("inspect source metadata: %w", err)
	}
	if !metadataInfo.Mode().IsRegular() {
		return errors.New("backup local project: metadata must be a regular file")
	}
	if _, err := os.Lstat(metadataPath + ".service-lock"); err != nil {
		return fmt.Errorf("inspect source service lock: %w", err)
	}
	owner, err := acquireProjectOwner(metadataPath)
	if err != nil {
		return fmt.Errorf("lock source project for backup: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, owner.Close())
	}()

	if err := ctx.Err(); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(destinationParent, ".lode-backup-")
	if err != nil {
		return fmt.Errorf("create backup staging directory: %w", err)
	}
	if err := os.Chmod(stage, 0700); err != nil {
		_ = removeBackupStage(stage)
		return fmt.Errorf("secure backup staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = removeBackupStage(stage)
		}
	}()

	rootFD, err := unix.Open(sourcePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NOATIME, 0)
	if err != nil {
		return fmt.Errorf("open source project directory: %w", err)
	}
	sourceRoot := os.NewFile(uintptr(rootFD), sourcePath)
	var openedRootInfo unix.Stat_t
	if err := unix.Fstat(rootFD, &openedRootInfo); err != nil {
		_ = sourceRoot.Close()
		return fmt.Errorf("inspect opened source project directory: %w", err)
	}
	sourceStat, ok := sourceInfo.Sys().(*syscall.Stat_t)
	if !ok || sourceStat.Dev != openedRootInfo.Dev || sourceStat.Ino != openedRootInfo.Ino {
		_ = sourceRoot.Close()
		return errors.New("backup source directory changed before copying")
	}
	copyErr := copyBackupDirectory(ctx, sourceRoot, stage, 0, nil)
	closeErr := sourceRoot.Close()
	if copyErr != nil {
		return fmt.Errorf("copy local project: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close source project directory: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	stagedLocal, err := OpenLocal(ctx, stage)
	if err != nil {
		return fmt.Errorf("validate staged project: %w", err)
	}
	if err := stagedLocal.Close(); err != nil {
		return fmt.Errorf("close validated staged project: %w", err)
	}
	if err := syncBackupFile(filepath.Join(stage, "metadata.sqlite")); err != nil {
		return fmt.Errorf("sync validated metadata: %w", err)
	}
	if err := syncLocalProjectDir(stage); err != nil {
		return fmt.Errorf("sync validated project directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameNoReplace(stage, destinationPath); err != nil {
		return fmt.Errorf("publish local project backup: %w", err)
	}
	published = true
	if err := syncLocalProjectDir(destinationParent); err != nil {
		return fmt.Errorf("sync backup destination parent: %w", err)
	}
	return nil
}

func resolveBackupSource(path string) (string, os.FileInfo, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve backup source: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("inspect backup source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, errors.New("backup local project: source must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("resolve backup source symlinks: %w", err)
	}
	canonicalInfo, err := os.Lstat(canonical)
	if err != nil || !canonicalInfo.IsDir() || canonicalInfo.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return "", nil, fmt.Errorf("inspect resolved backup source: %w", err)
		}
		return "", nil, errors.New("backup local project: resolved source is not a real directory")
	}
	return canonical, canonicalInfo, nil
}

func resolveBackupDestination(path string) (destination, parent string, err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve backup destination: %w", err)
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", "", fmt.Errorf("backup destination already exists: %q", absolute)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("inspect backup destination: %w", err)
	}
	name := filepath.Base(absolute)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", "", errors.New("backup local project: invalid destination name")
	}
	parent, err = filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", "", fmt.Errorf("resolve backup destination parent: %w", err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		return "", "", fmt.Errorf("inspect backup destination parent: %w", err)
	}
	if !info.IsDir() {
		return "", "", errors.New("backup destination parent is not a directory")
	}
	return filepath.Join(parent, name), parent, nil
}

func pathsOverlap(a, b string) bool {
	return pathContains(a, b) || pathContains(b, a)
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func copyBackupDirectory(ctx context.Context, source *os.File, destination string, depth int, expected *unix.Stat_t) error {
	if depth > 4096 {
		return errors.New("source directory depth exceeds backup limit")
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &before); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("source entry changed type during backup")
	}
	if expected != nil && !sameBackupDirectoryVersion(*expected, before) {
		return errors.New("source directory changed before backup")
	}
	for {
		entries, readErr := source.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			var childInfo unix.Stat_t
			if err := unix.Fstatat(int(source.Fd()), name, &childInfo, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return fmt.Errorf("inspect source entry %q: %w", name, err)
			}
			typeBits := childInfo.Mode & unix.S_IFMT
			if typeBits != unix.S_IFREG && typeBits != unix.S_IFDIR {
				return fmt.Errorf("source entry %q is a symlink or special file", name)
			}
			flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NOATIME
			if typeBits == unix.S_IFDIR {
				flags |= unix.O_DIRECTORY
			} else {
				flags |= unix.O_NONBLOCK
			}
			childFD, err := unix.Openat(int(source.Fd()), name, flags, 0)
			if err != nil {
				return fmt.Errorf("open source entry %q without following links: %w", name, err)
			}
			var openedInfo unix.Stat_t
			statErr := unix.Fstat(childFD, &openedInfo)
			if statErr != nil || openedInfo.Dev != childInfo.Dev || openedInfo.Ino != childInfo.Ino || openedInfo.Mode&unix.S_IFMT != typeBits {
				_ = unix.Close(childFD)
				if statErr != nil {
					return fmt.Errorf("inspect opened source entry %q: %w", name, statErr)
				}
				return fmt.Errorf("source entry %q changed during backup", name)
			}
			childSource := os.NewFile(uintptr(childFD), name)
			childDestination := filepath.Join(destination, name)
			if typeBits == unix.S_IFDIR {
				if err := os.Mkdir(childDestination, 0700); err != nil {
					_ = childSource.Close()
					return fmt.Errorf("create destination directory %q: %w", name, err)
				}
				copyErr := copyBackupDirectory(ctx, childSource, childDestination, depth+1, &openedInfo)
				closeErr := childSource.Close()
				if copyErr != nil {
					return fmt.Errorf("copy source directory %q: %w", name, copyErr)
				}
				if closeErr != nil {
					return fmt.Errorf("close source directory %q: %w", name, closeErr)
				}
			} else {
				copyErr := copyBackupFile(ctx, childSource, childDestination, openedInfo)
				closeErr := childSource.Close()
				if copyErr != nil {
					return fmt.Errorf("copy source file %q: %w", name, copyErr)
				}
				if closeErr != nil {
					return fmt.Errorf("close source file %q: %w", name, closeErr)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
		if len(entries) == 0 {
			return io.ErrNoProgress
		}
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &after); err != nil {
		return err
	}
	if !sameBackupDirectoryVersion(before, after) {
		return errors.New("source directory changed during backup")
	}
	if err := finishBackupMetadata(destination, before); err != nil {
		return err
	}
	return nil
}

func copyBackupFile(ctx context.Context, source *os.File, destination string, info unix.Stat_t) error {
	mode := os.FileMode(info.Mode & 0777)
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	copyErr := copyBackupBytes(ctx, target, source)
	if copyErr == nil {
		var after unix.Stat_t
		if err := unix.Fstat(int(source.Fd()), &after); err != nil {
			copyErr = err
		} else if !sameBackupFileVersion(info, after) {
			copyErr = errors.New("source file changed during backup")
		}
	}
	if copyErr == nil {
		copyErr = target.Chmod(mode)
	}
	if copyErr == nil {
		copyErr = os.Chtimes(destination, backupTime(info.Atim), backupTime(info.Mtim))
	}
	if copyErr == nil {
		copyErr = target.Sync()
	}
	return errors.Join(copyErr, target.Close())
}

func copyBackupBytes(ctx context.Context, target io.Writer, source io.Reader) error {
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := source.Read(buffer)
		for written := 0; written < read; {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, err := target.Write(buffer[written:read])
			written += n
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func finishBackupMetadata(path string, info unix.Stat_t) error {
	if err := os.Chmod(path, os.FileMode(info.Mode&0777)); err != nil {
		return err
	}
	if err := os.Chtimes(path, backupTime(info.Atim), backupTime(info.Mtim)); err != nil {
		return err
	}
	return syncLocalProjectDir(path)
}

func backupTime(value unix.Timespec) time.Time {
	return time.Unix(value.Sec, value.Nsec)
}

func sameBackupFileVersion(before, after unix.Stat_t) bool {
	return before.Dev == after.Dev && before.Ino == after.Ino && before.Size == after.Size && before.Mtim == after.Mtim && before.Ctim == after.Ctim
}

func syncBackupFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func removeBackupStage(path string) error {
	if err := os.Chmod(path, 0700); err != nil {
		return errors.Join(err, os.RemoveAll(path))
	}
	var makeWritable func(string) error
	makeWritable = func(directory string) error {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			child := filepath.Join(directory, entry.Name())
			info, err := os.Lstat(child)
			if err != nil {
				return err
			}
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				if err := os.Chmod(child, 0700); err != nil {
					return err
				}
				if err := makeWritable(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return errors.Join(makeWritable(path), os.RemoveAll(path))
}

func sameBackupDirectoryVersion(before, after unix.Stat_t) bool {
	return before.Dev == after.Dev && before.Ino == after.Ino && before.Size == after.Size && before.Mtim == after.Mtim && before.Ctim == after.Ctim
}
