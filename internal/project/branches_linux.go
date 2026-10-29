package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/redthing1/lode/internal/model"
)

var ErrBranchExists = errors.New("branch already exists")
var ErrBranchChanged = errors.New("branch changed; inspect it before deleting")
var ErrAmbiguous = errors.New("reference is ambiguous; use branch:<name>, tag:<name> or version:<full-id>")

func (s *Store) Branches(ctx context.Context, after string) ([]model.Reference, string, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.references(ctx, "SELECT name,tip FROM branches WHERE project=? AND name>? ORDER BY name LIMIT 101", after)
}

func (s *Store) references(ctx context.Context, query, after string) ([]model.Reference, string, error) {
	rows, err := s.db.QueryContext(ctx, query, s.id, after)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var refs []model.Reference
	for rows.Next() {
		var b model.Reference
		if err := rows.Scan(&b.Name, &b.Version); err != nil {
			return nil, "", err
		}
		refs = append(refs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(refs) > 100 {
		return refs[:100], refs[99].Name, nil
	}
	return refs, "", nil
}

// Reference resolves a branch, tag, complete version ID or its displayed suffix.
// Collisions are refused rather than silently preferring one kind of reference.
func (s *Store) Reference(ctx context.Context, ref string) (model.Version, error) {
	if ref == "" || len(ref) > 136 {
		return model.Version{}, ErrRequest
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	var rows *sql.Rows
	var err error
	if kind, name, qualified := strings.Cut(ref, ":"); qualified {
		if !model.ValidReference(name) {
			return model.Version{}, ErrRequest
		}
		switch kind {
		case "branch":
			rows, err = s.db.QueryContext(ctx, "SELECT tip FROM branches WHERE project=? AND name=?", s.id, name)
		case "tag":
			rows, err = s.db.QueryContext(ctx, "SELECT version FROM tags WHERE project=? AND name=?", s.id, name)
		case "version":
			rows, err = s.db.QueryContext(ctx, "SELECT id FROM versions WHERE project=? AND id=?", s.id, name)
		default:
			return model.Version{}, ErrRequest
		}
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT tip FROM branches WHERE project=? AND name=?
 UNION SELECT version FROM tags WHERE project=? AND name=?
 UNION SELECT id FROM versions WHERE project=? AND (id=? OR (length(?) BETWEEN 8 AND 36 AND substr(id,-length(?))=?)) LIMIT 2`, s.id, ref, s.id, ref, s.id, ref, ref, ref, ref)
	}
	if err != nil {
		return model.Version{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return model.Version{}, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return model.Version{}, err
	}
	switch len(ids) {
	case 0:
		return model.Version{}, sql.ErrNoRows
	case 1:
		if ids[0] == "" {
			return model.Version{}, nil
		}
		v, err := s.version(ctx, ids[0])
		return v.Version, err
	default:
		return model.Version{}, ErrAmbiguous
	}
}

// CreateBranch takes an immutable version ID. Repeating the same creation is
// harmless, but it never moves an existing branch back after another save.
func (s *Store) CreateBranch(ctx context.Context, name, version string) (model.Reference, error) {
	if !model.ValidReference(name) {
		return model.Reference{}, fmt.Errorf("%w: invalid branch name", ErrRequest)
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	if head, err := s.head(ctx, name); err == nil {
		if head == version {
			return model.Reference{Name: name, Version: version}, nil
		}
		return model.Reference{}, ErrBranchExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return model.Reference{}, err
	}
	if version != "" {
		v, err := s.version(ctx, version)
		if err != nil {
			return model.Reference{}, err
		}
		if !v.Retained {
			return model.Reference{}, ErrExpired
		}
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO branches VALUES(?,?,?)", s.id, name, version)
	return model.Reference{Name: name, Version: version}, err
}

func (s *Store) DeleteBranch(ctx context.Context, name, expected string) error {
	if !model.ValidReference(name) || name == "main" {
		return fmt.Errorf("%w: cannot delete this branch", ErrRequest)
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	head, err := s.head(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if head != expected {
		return ErrBranchChanged
	}
	_, err = s.db.ExecContext(ctx, "DELETE FROM branches WHERE project=? AND name=? AND tip=?", s.id, name, expected)
	return err
}
