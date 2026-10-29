package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/project"
)

var ErrProjectBusy = errors.New("project has active requests; retry deletion when they finish")
var ErrProjectChanged = errors.New("project identity changed; review the project again")
var ErrDeletionPending = errors.New("project deletion is incomplete; retry or restart the server")

// OwnedProject includes pending deletion, which ordinary reads cannot access.
// Ownership and site administration are browser authorities, never token roles.
func (s *Store) OwnedProject(ctx context.Context, actor Principal, owner, name string) (Project, error) {
	return ownedProject(ctx, s.db, actor, owner, name)
}

func ownedProject(ctx context.Context, q userQuery, actor Principal, owner, name string) (Project, error) {
	if actor.Token {
		return Project{}, ErrAccess
	}
	user, err := currentUser(ctx, q, actor.User)
	if err != nil {
		return Project{}, err
	}
	if !user.Admin && user.Name != owner {
		return Project{}, ErrAccess
	}
	p := Project{Role: Maintainer}
	err = q.QueryRowContext(ctx, `SELECT p.id,u.name,p.name,p.state FROM projects p JOIN users u ON u.id=p.owner WHERE u.name=? AND p.name=?`, owner, name).Scan(&p.ID, &p.Owner, &p.Name, &p.State)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *Store) AdminProjects(ctx context.Context, actor Principal, after string) ([]Project, string, error) {
	if err := administrator(ctx, s.db, actor); err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,u.name,p.name,p.state FROM projects p JOIN users u ON u.id=p.owner WHERE u.name||'/'||p.name>? ORDER BY u.name||'/'||p.name LIMIT 51`, after)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		p := Project{Role: Maintainer}
		if err := rows.Scan(&p.ID, &p.Owner, &p.Name, &p.State); err != nil {
			return nil, "", err
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(projects) > 50 {
		return projects[:50], projects[49].Owner + "/" + projects[49].Name, nil
	}
	return projects, "", nil
}

func (s *Store) DeleteProject(ctx context.Context, actor Principal, owner, name, expectedID string) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if err := s.beginDeletion(ctx, actor, owner, name, expectedID); err != nil {
		return err
	}
	if err := s.finishDeletion(ctx, expectedID); err != nil {
		return errors.Join(ErrDeletionPending, err)
	}
	return nil
}

func (s *Store) beginDeletion(ctx context.Context, actor Principal, owner, name, expectedID string) error {
	// Acquisition checks state under the same mutex, including requests whose
	// authorization happened before this operation. References cover full reads.
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := ownedProject(ctx, tx, actor, owner, name)
	if err != nil {
		return err
	}
	if p.ID != expectedID {
		return ErrProjectChanged
	}
	if opened := s.projects[p.ID]; opened != nil && opened.references != 0 {
		return ErrProjectBusy
	}
	if _, err := tx.ExecContext(ctx, "UPDATE projects SET state='deleting' WHERE id=?", p.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tokens WHERE project=?", p.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// finishDeletion requires a recorded deletion and exclusive lifecycle access.
// Storage removal precedes forgetting its identity; either step can be resumed.
func (s *Store) finishDeletion(ctx context.Context, id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return errors.New("invalid stored project ID")
	}
	s.mu.Lock()
	p := s.projects[id]
	delete(s.projects, id)
	s.mu.Unlock()
	if p != nil {
		if err := p.close(ctx); err != nil {
			s.mu.Lock()
			s.projects[id] = p
			s.mu.Unlock()
			return fmt.Errorf("close project repository; restart the server: %w", err)
		}
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, err := root.Open("projects")
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := root.RemoveAll("projects/" + id); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := project.Delete(ctx, tx, id); err != nil {
		return err
	}
	for _, statement := range []string{"DELETE FROM members WHERE project=?", "DELETE FROM tokens WHERE project=?", "DELETE FROM projects WHERE id=?"} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) resumeDeletions(ctx context.Context) error {
	for {
		var id string
		err := s.db.QueryRowContext(ctx, "SELECT id FROM projects WHERE state='deleting' LIMIT 1").Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.finishDeletion(ctx, id); err != nil {
			return fmt.Errorf("finish deleting project %s: %w", id, err)
		}
	}
}
