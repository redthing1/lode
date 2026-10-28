//go:build linux

package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/redthing1/lode/internal/project"
)

const projectDeletionAuthority = `SELECT count(*) FROM projects p JOIN users u ON u.id=?
	WHERE p.id=? AND NOT u.disabled AND NOT u.force_password_change AND u.auth_version=? AND (u.admin OR p.owner_id=u.id)`

func (i *Instance) CanDeleteProject(ctx context.Context, actor Principal, projectID string) (bool, error) {
	if actor.TokenID != "" {
		return false, nil
	}
	var allowed int
	err := i.db.QueryRowContext(ctx, projectDeletionAuthority, actor.UserID, projectID, actor.AuthVersion).Scan(&allowed)
	return allowed == 1, err
}

// DeleteProject refuses active requests, then records deletion before touching
// files. Once accepted, interruption leaves an intent that Open completes.
// Scoped tokens and memberships disappear with the final registry row.
func (i *Instance) DeleteProject(ctx context.Context, actor Principal, projectID string) error {
	if actor.TokenID != "" {
		return ErrUnauthorized
	}
	if !project.ValidID(projectID) {
		return ErrInvalid
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.Store == nil {
		return errors.New("instance is closed")
	}
	err := i.tx(ctx, func(tx *sql.Tx) error {
		var allowed int
		if err := tx.QueryRowContext(ctx, projectDeletionAuthority, actor.UserID, projectID, actor.AuthVersion).Scan(&allowed); err != nil {
			return err
		}
		if allowed != 1 {
			return ErrUnauthorized
		}
		if slot := i.open[projectID]; slot != nil {
			if slot.users != 0 {
				return fmt.Errorf("project has active requests; retry when they finish: %w", ErrConflict)
			}
			if err := slot.p.Close(); err != nil {
				return err
			}
			delete(i.open, projectID)
		}
		_, err := tx.ExecContext(ctx, "UPDATE projects SET deleting=1 WHERE id=?", projectID)
		return err
	})
	if err != nil {
		return err
	}
	return i.finishProjectDeletion(ctx, projectID)
}

func (i *Instance) finishProjectDeletion(ctx context.Context, id string) error {
	if !project.ValidID(id) {
		return fmt.Errorf("invalid project deletion ID %q", id)
	}
	if err := os.RemoveAll(filepath.Join(i.directory, "projects", id)); err != nil {
		return fmt.Errorf("project deletion is pending: %w", err)
	}
	if err := syncDirectory(filepath.Join(i.directory, "projects")); err != nil {
		return err
	}
	_, err := i.db.ExecContext(ctx, "DELETE FROM projects WHERE id=? AND deleting", id)
	return err
}

func (i *Instance) finishProjectDeletions(ctx context.Context) error {
	rows, err := i.db.QueryContext(ctx, "SELECT id FROM projects WHERE deleting ORDER BY id")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := i.finishProjectDeletion(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
