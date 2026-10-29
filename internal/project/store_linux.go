// Package project owns publication, logical version trees and content availability.
package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

var (
	ErrStale            = errors.New("branch changed; update before saving")
	ErrIntent           = errors.New("operation ID belongs to another request")
	ErrExpired          = errors.New("version contents have expired")
	ErrOperationExpired = errors.New("save outcome expired; use lode update to reconcile before saving")
	ErrMetadata         = errors.New("incomplete project metadata")
	ErrRequest          = errors.New("invalid request")
	ErrUnaccepted       = errors.New("save has not been accepted")
	ErrCancelled        = errors.New("save was cancelled")
)

type Store struct {
	id      string
	db      *sql.DB
	content *content.Store
	// Mutations serialize independently of reads of immutable published trees.
	// Cleanup and collection take mutation, then gate exclusively, before
	// removing metadata or contents. Readers hold gate through their last use.
	mutation sync.Mutex
	gate     sync.RWMutex
}

func (s *Store) ID() string { return s.id }

// Initialize creates project tables in the instance-owned database.
func Initialize(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE directories(project TEXT NOT NULL,id TEXT NOT NULL,body BLOB NOT NULL,PRIMARY KEY(project,id));
CREATE TABLE groups(project TEXT NOT NULL,id TEXT NOT NULL,body BLOB NOT NULL,PRIMARY KEY(project,id));
CREATE TABLE versions(project TEXT NOT NULL,id TEXT NOT NULL,parent TEXT NOT NULL,merge_parent TEXT NOT NULL,actor TEXT NOT NULL,message TEXT NOT NULL,created INTEGER NOT NULL,root TEXT NOT NULL,protection TEXT NOT NULL,intent TEXT NOT NULL,retained INTEGER NOT NULL,PRIMARY KEY(project,id));
CREATE INDEX version_history ON versions(project,created DESC,id);
CREATE TABLE cancelled(project TEXT NOT NULL,id TEXT NOT NULL,actor TEXT NOT NULL,intent TEXT NOT NULL,PRIMARY KEY(project,id,actor));
CREATE TABLE branches(project TEXT NOT NULL,name TEXT NOT NULL,tip TEXT NOT NULL,PRIMARY KEY(project,name));
CREATE TABLE tags(project TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,PRIMARY KEY(project,name));
CREATE TABLE retention(project TEXT PRIMARY KEY,versions INTEGER NOT NULL,history_days INTEGER NOT NULL,cutoff TEXT NOT NULL);`)
	return err
}

func Create(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO retention VALUES(?,20,90,'')", id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO branches VALUES(?,'main','')", id)
	return err
}

// Delete removes project metadata after the instance has excluded all use and
// durably removed the project's native repository. The caller commits the tx.
func Delete(ctx context.Context, tx *sql.Tx, id string) error {
	for _, table := range []string{"directories", "groups", "versions", "cancelled", "branches", "tags", "retention"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE project=?", id); err != nil {
			return err
		}
	}
	return nil
}

// Open requires exclusive instance ownership of the repository directory.
// The instance owns both dependencies and their lifetime.
func Open(ctx context.Context, db *sql.DB, id string, contents *content.Store) (*Store, error) {
	s := &Store{id: id, db: db, content: contents}
	if _, err := s.Head(ctx, "main"); err != nil {
		return nil, err
	}
	if err := s.reconcile(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Head(ctx context.Context, branch string) (string, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	return s.head(ctx, branch)
}
func (s *Store) head(ctx context.Context, branch string) (string, error) {
	var tip string
	err := s.db.QueryRowContext(ctx, "SELECT tip FROM branches WHERE project=? AND name=?", s.id, branch).Scan(&tip)
	return tip, err
}

type savedVersion struct {
	model.Version
	root, protection, intent string
}

func (s *Store) version(ctx context.Context, id string) (savedVersion, error) {
	var v savedVersion
	var created int64
	err := s.db.QueryRowContext(ctx, "SELECT id,parent,merge_parent,actor,message,created,root,protection,intent,retained FROM versions WHERE project=? AND id=?", s.id, id).Scan(&v.ID, &v.Parent, &v.MergeParent, &v.Actor, &v.Message, &created, &v.root, &v.protection, &v.intent, &v.Retained)
	v.Created = fromMillis(created)
	return v, err
}

func (s *Store) reconcile(ctx context.Context) error {
	roots, err := s.content.Roots(ctx)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,root,protection FROM versions WHERE project=? AND retained=1", s.id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, root, protection string
		if err := rows.Scan(&id, &root, &protection); err != nil {
			rows.Close()
			return err
		}
		p, ok := roots[protection]
		if !ok || p.Operation != id || p.Object != root {
			rows.Close()
			return fmt.Errorf("version %s is missing its content protection", id)
		}
		delete(roots, protection)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	orphans := make([]string, 0, len(roots))
	for id := range roots {
		orphans = append(orphans, id)
	}
	return s.content.Release(ctx, orphans)
}
