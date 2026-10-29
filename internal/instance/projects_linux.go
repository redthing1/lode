package instance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/project"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var ErrNotFound = errors.New("project not found")
var ErrName = errors.New("project names use lowercase letters, numbers, hyphens and underscores")
var ErrExists = errors.New("project already exists")

type Project struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
	Role  Role   `json:"role"`
	State string `json:"-"`
}

func (s *Store) CreateProject(ctx context.Context, actor Principal, name string) (Project, error) {
	if !validName(name) {
		return Project{}, ErrName
	}
	if (actor.Token && !actor.Role.Allows(Writer)) || actor.Project != "" {
		return Project{}, ErrAccess
	}
	id, err := newID()
	if err != nil {
		return Project{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	user, err := currentUser(ctx, tx, actor.User)
	if err != nil {
		return Project{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO projects(id,owner,name,created) VALUES(?,?,?,?)", id, user.ID, name, time.Now().UnixMilli()); err != nil {
		var constraint *sqlite.Error
		if errors.As(err, &constraint) && constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
			return Project{}, ErrExists
		}
		return Project{}, err
	}
	if err := project.Create(ctx, tx, id); err != nil {
		return Project{}, err
	}
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	// A recorded project whose creation is interrupted is initialized on next use.
	_, release, err := s.acquire(ctx, id)
	if err != nil {
		return Project{}, err
	}
	release()
	p := Project{ID: id, Owner: user.Name, Name: name, Role: Maintainer, State: "ready"}
	p.limit(actor)
	return p, nil
}

func (s *Store) Project(ctx context.Context, actor Principal, owner, name string, required Role) (Project, error) {
	return projectFor(ctx, s.db, actor, owner, name, required)
}

func projectFor(ctx context.Context, q userQuery, actor Principal, owner, name string, required Role) (Project, error) {
	user, err := currentUser(ctx, q, actor.User)
	if err != nil {
		return Project{}, err
	}
	var p Project
	err = q.QueryRowContext(ctx, projectAccess+" WHERE u.name=? AND p.name=? AND p.state<>'deleting'", user.ID, user.Admin && !actor.Token, user.ID, owner, name).Scan(&p.ID, &p.Owner, &p.Name, &p.State, &p.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	p.limit(actor)
	if !p.Role.Allows(required) || (actor.Project != "" && actor.Project != p.ID) {
		return Project{}, ErrAccess
	}
	return p, nil
}

func (s *Store) Projects(ctx context.Context, actor Principal) ([]Project, error) {
	user, err := currentUser(ctx, s.db, actor.User)
	if err != nil {
		return nil, err
	}
	admin := user.Admin && !actor.Token
	rows, err := s.db.QueryContext(ctx, projectAccess+" WHERE p.state<>'deleting' AND (p.owner=? OR ? OR m.user_id IS NOT NULL) AND (?='' OR p.id=?) ORDER BY u.name,p.name", user.ID, admin, user.ID, user.ID, admin, actor.Project, actor.Project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Owner, &p.Name, &p.State, &p.Role); err != nil {
			return nil, err
		}
		p.limit(actor)
		if p.Role.Allows(Reader) {
			projects = append(projects, p)
		}
	}
	return projects, rows.Err()
}

// WithProject supplies the authorized metadata and keeps that same project's
// repository open for the entire operation.
func (s *Store) WithProject(ctx context.Context, actor Principal, owner, name string, required Role, fn func(Project, *project.Store) error) error {
	p, err := s.Project(ctx, actor, owner, name, required)
	if err != nil {
		return err
	}
	store, release, err := s.acquire(ctx, p.ID)
	if err != nil {
		return err
	}
	defer release()
	return fn(p, store)
}

type openedProject struct {
	store      *project.Store
	content    *content.Store
	references int
	lastUsed   time.Time
	closed     bool
	closeErr   error
}

// Native Close is not retryable after an error. Keep the failed handle closed
// until the instance stops; a later nil Close would not prove cleanup succeeded.
func (p *openedProject) close(ctx context.Context) error {
	if !p.closed {
		p.closed = true
		p.closeErr = p.content.Close(context.WithoutCancel(ctx))
	}
	return p.closeErr
}

const maxOpenProjects = 8

func (s *Store) acquire(ctx context.Context, id string) (*project.Store, func(), error) {
	// Already-open projects need only the short registry lock. Native IO for
	// another project must not hold up their reads or reference releases.
	s.mu.Lock()
	if p := s.projects[id]; p != nil {
		store, release, err := s.reference(ctx, id, p)
		s.mu.Unlock()
		return store, release, err
	}
	s.mu.Unlock()

	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.projectState(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	p := s.projects[id]
	if p == nil {
		if len(s.projects) >= maxOpenProjects {
			var oldest string
			var used time.Time
			for key, p := range s.projects {
				if p.references == 0 && (oldest == "" || p.lastUsed.Before(used)) {
					oldest, used = key, p.lastUsed
				}
			}
			if oldest == "" {
				return nil, nil, errors.New("server is busy opening projects; retry shortly")
			}
			evicted := s.projects[oldest]
			delete(s.projects, oldest)
			s.mu.Unlock()
			err := evicted.close(ctx)
			s.mu.Lock()
			if err != nil {
				s.projects[oldest] = evicted
				return nil, nil, err
			}
		}
		s.mu.Unlock()
		p, err = s.openProject(ctx, id, state == "creating")
		s.mu.Lock()
		if err != nil {
			return nil, nil, err
		}
		s.projects[id] = p
	}
	return s.reference(ctx, id, p)
}

// projectState and reference require mu; native lifecycle changes also hold
// lifecycle, always acquired before mu. A deleting project cannot gain a lease.
func (s *Store) projectState(ctx context.Context, id string) (string, error) {
	if s.db == nil {
		return "", errors.New("instance is closed")
	}
	var state string
	err := s.db.QueryRowContext(ctx, "SELECT state FROM projects WHERE id=?", id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || err == nil && state == "deleting" {
		return "", ErrNotFound
	}
	return state, err
}

func (s *Store) reference(ctx context.Context, id string, p *openedProject) (*project.Store, func(), error) {
	if _, err := s.projectState(ctx, id); err != nil {
		return nil, nil, err
	}
	if p.closed {
		return nil, nil, errors.New("project repository is closed; restart the server")
	}
	p.references++
	release := func() { s.mu.Lock(); defer s.mu.Unlock(); p.references--; p.lastUsed = time.Now() }
	return p.store, release, nil
}

// openProject holds lifecycle but leaves the registry available to readers.
func (s *Store) openProject(ctx context.Context, id string, create bool) (*openedProject, error) {
	h := hmac.New(sha256.New, s.key)
	_, _ = h.Write([]byte("project:" + id))
	c, err := content.Open(ctx, filepath.Join(s.directory, "projects", id), hex.EncodeToString(h.Sum(nil)), create)
	if err != nil {
		return nil, err
	}
	store, err := project.Open(ctx, s.db, id, c)
	if err != nil {
		c.Close(ctx)
		return nil, err
	}
	if create {
		if _, err := s.db.ExecContext(ctx, "UPDATE projects SET state='ready' WHERE id=?", id); err != nil {
			c.Close(ctx)
			return nil, err
		}
	}
	return &openedProject{store: store, content: c}, nil
}
