//go:build linux

package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Backup copies a stopped project into a new, independently usable store.
// Restore uses the same operation with the backup as its source.
func Backup(ctx context.Context, source, destination string) (retErr error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	parentPath, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return err
	}
	name := filepath.Base(destination)
	if name == "." || name == string(filepath.Separator) {
		return errors.New("backup destination must name a new directory")
	}
	destination = filepath.Join(parentPath, name)
	if rel, err := filepath.Rel(source, destination); err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("backup destination must be outside the source project")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	if _, err := parent.Lstat(name); err == nil {
		return fmt.Errorf("backup destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Hold the ownership lock while copying a cold source. Opening it here
	// would reconcile native roots in place.
	owner, err := os.OpenFile(filepath.Join(source, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open backup source (stop its server first): %w", err)
	}
	if err := syscall.Flock(int(owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = owner.Close()
		return fmt.Errorf("open backup source (stop its server first): project is in use")
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer src.Close()
	stage := "." + name + ".partial"
	if err := parent.Mkdir(stage, 0700); err != nil {
		return fmt.Errorf("create backup staging directory %s: %w", filepath.Join(parentPath, stage), err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, parent.RemoveAll(stage))
		}
	}()
	dst, err := parent.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer dst.Close()

	for _, path := range []string{"project.json", "history.sqlite", "content/repository"} {
		if err := copyBackupPath(ctx, src, dst, path); err != nil {
			return err
		}
	}
	// SQLite normally removes these on close. If present, they belong to the
	// coherent cold database and must travel with its main file.
	for _, path := range []string{"history.sqlite-journal", "history.sqlite-wal", "history.sqlite-shm"} {
		if _, err := src.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := copyBackupPath(ctx, src, dst, path); err != nil {
			return err
		}
	}
	// Reopen the independent copy before making it visible at the final name.
	// This also proves derived native configuration is not a backup dependency.
	copy, err := Open(ctx, filepath.Join(parentPath, stage))
	if err != nil {
		return fmt.Errorf("verify backup: %w", err)
	}
	var integrity string
	checkErr := copy.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity)
	if checkErr == nil && integrity != "ok" {
		checkErr = fmt.Errorf("backup database check: %s", integrity)
	}
	if err := errors.Join(checkErr, copy.Close()); err != nil {
		return err
	}
	for _, path := range []string{"lock", "content/cache", "content/kopia.config"} {
		if err := dst.RemoveAll(path); err != nil {
			return err
		}
	}
	for _, path := range []string{"content", "."} {
		if err := syncBackupDirectory(dst, path); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parentFD, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer parentFD.Close()
	if err := unix.Renameat2(int(parentFD.Fd()), stage, int(parentFD.Fd()), name, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("publish backup: %w", err)
	}
	return parentFD.Sync()
}

func copyBackupPath(ctx context.Context, src, dst *os.Root, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := src.Lstat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := dst.MkdirAll(name, 0700); err != nil {
			return err
		}
		entries, err := fs.ReadDir(src.FS(), name)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyBackupPath(ctx, src, dst, name+"/"+entry.Name()); err != nil {
				return err
			}
		}
		return syncBackupDirectory(dst, name)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported backup entry: %s", name)
	}
	in, err := src.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dst.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, backupReader{ctx, in})
	stampErr := dst.Chtimes(name, info.ModTime(), info.ModTime())
	return errors.Join(copyErr, stampErr, out.Sync(), out.Close())
}

type backupReader struct {
	ctx context.Context
	r   io.Reader
}

func (r backupReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func syncBackupDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
