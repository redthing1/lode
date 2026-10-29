package project

import (
	"context"
	"database/sql"
	"errors"

	"github.com/redthing1/lode/internal/model"
)

var ErrTagExists = errors.New("tag already exists")
var ErrTagChanged = errors.New("tag changed; inspect it before deleting")

func (s *Store) Tags(ctx context.Context, after string) ([]model.Reference, string, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.references(ctx, "SELECT name,version FROM tags WHERE project=? AND name>? ORDER BY name LIMIT 101", after)
}

// Tags protect one complete version. They are immutable until explicitly deleted.
func (s *Store) CreateTag(ctx context.Context, name, version string) (model.Reference, error) {
	if !model.ValidReference(name) || version == "" {
		return model.Reference{}, ErrRequest
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	var existing string
	err := s.db.QueryRowContext(ctx, "SELECT version FROM tags WHERE project=? AND name=?", s.id, name).Scan(&existing)
	if err == nil {
		if existing != version {
			return model.Reference{}, ErrTagExists
		}
		return model.Reference{Name: name, Version: version}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Reference{}, err
	}
	v, err := s.version(ctx, version)
	if err != nil {
		return model.Reference{}, err
	}
	if !v.Retained {
		return model.Reference{}, ErrExpired
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO tags VALUES(?,?,?)", s.id, name, version)
	return model.Reference{Name: name, Version: version}, err
}

func (s *Store) DeleteTag(ctx context.Context, name, expected string) error {
	if !model.ValidReference(name) || expected == "" {
		return ErrRequest
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	var version string
	err := s.db.QueryRowContext(ctx, "SELECT version FROM tags WHERE project=? AND name=?", s.id, name).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if version != expected {
		return ErrTagChanged
	}
	_, err = s.db.ExecContext(ctx, "DELETE FROM tags WHERE project=? AND name=? AND version=?", s.id, name, expected)
	return err
}
