//go:build linux

package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/redthing1/lode/internal/project"
)

// Backup creates a cold, self-contained copy of a stopped instance. The
// destination is atomically published and must not already exist.
func Backup(ctx context.Context, source, destination string) (retErr error) {
	source, err := existingDirectory(source)
	if err != nil {
		return err
	}
	destination, parent, name, err := targetPaths(destination)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(source, destination); err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("instance backup destination must be outside its source")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("instance backup destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	instance, err := Open(ctx, source)
	if err != nil {
		return fmt.Errorf("open backup source (stop its server first): %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, instance.Close())
		}
	}()
	projectIDs, err := registeredProjectIDs(ctx, instance.db)
	if err != nil {
		return err
	}
	if err := rejectUnregisteredProjectData(source, projectIDs); err != nil {
		return err
	}
	userIDs, err := accountIDs(ctx, instance.db)
	if err != nil {
		return err
	}
	var busy, frames, checkpointed int
	if err := instance.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint instance database: %w", err)
	}
	if busy != 0 || frames != checkpointed {
		return fmt.Errorf("checkpoint instance database is busy (frames=%d checkpointed=%d)", frames, checkpointed)
	}
	owner := instance.owner
	instance.owner = nil
	if err := instance.Close(); err != nil {
		closed = true
		return fmt.Errorf("close cold backup source: %w", errors.Join(err, owner.Close()))
	}
	closed = true
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()

	stage, err := os.MkdirTemp(parent, "."+name+".partial-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			retErr = errors.Join(retErr, os.RemoveAll(stage))
		}
	}()
	if err := os.Chmod(stage, 0700); err != nil {
		return err
	}
	projectsDir := filepath.Join(stage, "projects")
	if err := os.Mkdir(projectsDir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"instance.json", "instance.sqlite"} {
		if err := copyRegularFile(ctx, filepath.Join(source, name), filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	for _, id := range projectIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := project.Backup(ctx, filepath.Join(source, "projects", id), filepath.Join(projectsDir, id)); err != nil {
			return fmt.Errorf("backup registered project %s: %w", id, err)
		}
	}
	if err := syncFile(filepath.Join(stage, "instance.sqlite")); err != nil {
		return err
	}
	if err := syncDirectory(projectsDir); err != nil {
		return err
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}

	verified, err := Open(ctx, stage)
	if err != nil {
		return fmt.Errorf("reopen instance backup: %w", err)
	}
	var integrity string
	checkErr := verified.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity)
	if checkErr == nil && integrity != "ok" {
		checkErr = fmt.Errorf("backup instance database check: %s", integrity)
	}
	if checkErr == nil && verified.Config != instance.Config {
		checkErr = errors.New("instance backup configuration mismatch")
	}
	if checkErr == nil {
		var gotIDs []string
		gotIDs, checkErr = registeredProjectIDs(ctx, verified.db)
		if checkErr == nil && !slices.Equal(projectIDs, gotIDs) {
			checkErr = errors.New("instance backup project registry mismatch")
		}
	}
	if checkErr == nil {
		var gotUsers []string
		gotUsers, checkErr = accountIDs(ctx, verified.db)
		if checkErr == nil && !slices.Equal(userIDs, gotUsers) {
			checkErr = errors.New("instance backup account registry mismatch")
		}
	}
	closeErr := verified.Close()
	if err := errors.Join(checkErr, closeErr); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(stage, "lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := rejectUnregisteredProjectData(stage, projectIDs); err != nil {
		return fmt.Errorf("verify instance backup projects: %w", err)
	}
	if err := syncDirectory(stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := renameNoReplace(stage, destination); err != nil {
		return fmt.Errorf("publish instance backup: %w", err)
	}
	published = true
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("instance backup was published at %s but parent sync failed: %w", destination, err)
	}
	return nil
}

func registeredProjectIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT id FROM projects ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !project.ValidID(id) {
			return nil, fmt.Errorf("invalid registered project ID %q", id)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func accountIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT id FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func rejectUnregisteredProjectData(source string, registered []string) error {
	known := make(map[string]bool, len(registered))
	for _, id := range registered {
		known[id] = true
	}
	entries, err := os.ReadDir(filepath.Join(source, "projects"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !known[entry.Name()] {
			return fmt.Errorf("unregistered project data at %s; backup did not omit it; inspect it, then move a valid project outside projects and use import to recover", filepath.Join(source, "projects", entry.Name()))
		}
	}
	return nil
}

func copyRegularFile(ctx context.Context, source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-regular instance backup file %s", source)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.Join(err, in.Close())
	}
	_, copyErr := copyContext(ctx, out, in)
	return errors.Join(copyErr, in.Close(), out.Sync(), out.Close())
}

func copyContext(ctx context.Context, out io.Writer, in io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := in.Read(buffer)
		if count > 0 {
			written, err := out.Write(buffer[:count])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}
