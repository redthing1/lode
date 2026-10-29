package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// Backup publishes a verified cold copy at a new destination. Restoring uses
// the same operation. The source is locked but never opened by SQLite or Kopia.
func Backup(ctx context.Context, source, destination string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return err
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	relative, err := filepath.Rel(source, destination)
	if err != nil {
		return err
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("backup destination must be outside the source instance")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("backup destination already exists")
		}
		return err
	}
	in, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if info, err := in.Lstat("instance.lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err == nil && !info.Mode().IsRegular() {
		return errors.New("instance lock is not a regular file")
	}
	lock := flock.New(filepath.Join(source, "instance.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("instance is in use; stop the server before backing up")
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	stage, err := os.MkdirTemp(parent, ".lode-backup-")
	if err != nil {
		return err
	}
	defer func() {
		if stage != "" {
			err = errors.Join(err, os.RemoveAll(stage))
		}
	}()
	out, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, name := range []string{"config.json", "instance.sqlite", "instance.sqlite-journal", "instance.sqlite-wal"} {
		if err := copyBackupFile(ctx, in, out, name); err != nil {
			if strings.HasPrefix(name, "instance.sqlite-") && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
	}
	ids, err := backupProjects(ctx, stage)
	if err != nil {
		return err
	}
	if info, err := in.Lstat("projects"); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New("projects is not a directory")
	}
	entries, err := fs.ReadDir(in.FS(), "projects")
	if err != nil {
		return err
	}
	registered := make(map[string]bool, len(ids))
	for _, id := range ids {
		registered[id] = true
	}
	for _, entry := range entries {
		if !registered[entry.Name()] || !entry.IsDir() {
			return fmt.Errorf("unregistered or non-directory project storage %q", entry.Name())
		}
	}
	if err := out.Mkdir("projects", 0700); err != nil {
		return err
	}
	for _, id := range ids {
		if err := out.Mkdir(filepath.Join("projects", id), 0700); err != nil {
			return err
		}
		if err := copyBackupTree(ctx, in, out, filepath.Join("projects", id, "repository")); err != nil {
			return fmt.Errorf("copy project %s: %w", id, err)
		}
	}
	s, err := Open(ctx, stage)
	if err != nil {
		return err
	}
	for _, id := range ids {
		p, release, verifyErr := s.acquire(ctx, id)
		if verifyErr == nil {
			verifyErr = p.Verify(ctx)
			release()
		}
		if verifyErr != nil {
			return errors.Join(fmt.Errorf("verify project %s: %w", id, verifyErr), s.Close(context.Background()))
		}
	}
	if err := s.Close(context.Background()); err != nil {
		return err
	}
	// Opening the stage rebuilds path-specific connection files and caches.
	// They are not part of the backup and must not refer to its temporary name.
	for _, id := range ids {
		for _, name := range []string{"cache", "kopia.config", "kopia.connect"} {
			if err := out.RemoveAll(filepath.Join("projects", id, name)); err != nil {
				return err
			}
		}
		if err := syncPath(filepath.Join(stage, "projects", id)); err != nil {
			return err
		}
	}
	if err := out.Remove("instance.lock"); err != nil {
		return err
	}
	for _, name := range []string{"instance.sqlite", "projects", "."} {
		if err := syncPath(filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	stage = ""
	if err := syncPath(parent); err != nil {
		return fmt.Errorf("backup published at %s, but destination parent synchronization failed: %w", destination, err)
	}
	return nil
}

// Inspect only the staged database, including recovery of a copied journal/WAL.
func backupProjects(ctx context.Context, stage string) (_ []string, err error) {
	db, err := database(stage)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	for _, check := range []string{"integrity_check", "foreign_key_check"} {
		rows, err := db.QueryContext(ctx, "PRAGMA "+check)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			count++
			var result string
			if check != "integrity_check" || rows.Scan(&result) != nil || result != "ok" || count != 1 {
				rows.Close()
				return nil, fmt.Errorf("instance database failed %s", check)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		if check == "integrity_check" && count != 1 {
			return nil, errors.New("instance database integrity check returned no result")
		}
	}
	var format, invalid int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&format); err != nil {
		return nil, err
	}
	if format != 7 {
		return nil, errors.New("unsupported instance format; create a new instance")
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM tokens t LEFT JOIN projects p ON p.id=t.project WHERE t.project<>'' AND p.id IS NULL").Scan(&invalid); err != nil {
		return nil, err
	}
	if invalid != 0 {
		return nil, errors.New("access token refers to a missing project")
	}
	rows, err := db.QueryContext(ctx, "SELECT id,state FROM projects ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return nil, err
		}
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id {
			return nil, errors.New("invalid stored project ID")
		}
		if state != "ready" {
			return nil, fmt.Errorf("project %s has unfinished %s state; start the source and finish that operation before backup", id, state)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func copyBackupTree(ctx context.Context, in, out *os.Root, name string) error {
	return fs.WalkDir(in.FS(), filepath.ToSlash(name), func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if err := out.Mkdir(name, 0700); err != nil {
				return err
			}
			return syncBackupParent(out, name)
		}
		return copyBackupFile(ctx, in, out, name)
	})
}

func copyBackupFile(ctx context.Context, in, out *os.Root, name string) error {
	// NONBLOCK prevents a substituted FIFO from hanging before f.Stat rejects it.
	f, err := in.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup source %q is not a regular file", name)
	}
	g, err := out.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(g, backupReader{ctx, f})
	if copyErr == nil && n != info.Size() {
		copyErr = fmt.Errorf("backup source %q changed size", name)
	}
	// Native blob timestamps participate in maintenance's age decisions.
	err = errors.Join(copyErr, out.Chtimes(name, info.ModTime(), info.ModTime()), g.Sync(), g.Close())
	if err != nil {
		return err
	}
	return syncBackupParent(out, name)
}

func syncBackupParent(root *os.Root, name string) error {
	f, err := root.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

type backupReader struct {
	ctx context.Context
	in  io.Reader
}

func (r backupReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}
