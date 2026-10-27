package project

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

var (
	ErrBranchNotFound = errors.New("branch not found")
	ErrBranchExists   = errors.New("branch already exists at another tip")
)

// Branch identifies a named pointer into immutable project history.
type Branch struct {
	Name string
	Tip  string
}

// CreateBranch creates a branch at an existing commit. Repeating the exact
// name and tip is idempotent; a branch name is never silently moved.
func (s *Service) CreateBranch(ctx context.Context, name, fromCommitID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateBranchName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := validateCommitID(fromCommitID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// An existing branch determines whether this is an idempotent retry or an
	// attempted move. No root validation or metadata write is needed either way.
	tx, err := s.metadata.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch lookup: %w", err)
	}
	var currentTip string
	err = tx.QueryRowContext(ctx, `SELECT tip FROM branches WHERE name = ?`, name).Scan(&currentTip)
	if err == nil {
		_ = tx.Rollback()
		if currentTip != fromCommitID {
			return ErrBranchExists
		}
		return nil
	}
	_ = tx.Rollback()
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing branch: %w", err)
	}
	if err := s.requireRetained(ctx, fromCommitID); err != nil {
		return err
	}

	commit, err := s.metadata.commit(ctx, fromCommitID)
	if err != nil {
		return err
	}
	archive, err := s.archive(ctx, commit.Tree)
	if err != nil {
		return fmt.Errorf("load branch source tree: %w", err)
	}
	if err := s.roots.Validate(ctx, archive); err != nil {
		return fmt.Errorf("validate branch source tree: %w", err)
	}

	tx, err = s.metadata.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO branches (name, tip) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`, name, fromCommitID)
	if err != nil {
		return fmt.Errorf("create branch %q: %w", name, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check branch creation: %w", err)
	}
	if inserted == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT tip FROM branches WHERE name = ?`, name).Scan(&currentTip); err != nil {
			return fmt.Errorf("read concurrently created branch: %w", err)
		}
		if currentTip != fromCommitID {
			return ErrBranchExists
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch creation: %w", err)
	}
	return nil
}

// DeleteBranch removes exactly the named branch at expectedTip. It never
// removes commits or native content, and a stale delete cannot remove a move.
func (s *Service) DeleteBranch(ctx context.Context, name, expectedTip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateBranchName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := validateCommitID(expectedTip); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	tx, err := s.metadata.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin branch deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentTip string
	err = tx.QueryRowContext(ctx, `SELECT tip FROM branches WHERE name = ?`, name).Scan(&currentTip)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read branch before deletion: %w", err)
	}
	if currentTip != expectedTip {
		return ErrStaleHead
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM branches WHERE name = ? AND tip = ?`, name, expectedTip)
	if err != nil {
		return fmt.Errorf("delete branch %q: %w", name, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check branch deletion: %w", err)
	}
	if deleted == 0 {
		return ErrStaleHead
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit branch deletion: %w", err)
	}
	return nil
}

// Branch returns one exact branch name.
func (s *Service) Branch(ctx context.Context, name string) (Branch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Branch{}, ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return Branch{}, err
	}
	if err := validateBranchName(name); err != nil {
		return Branch{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	var result Branch
	err := s.metadata.db.QueryRowContext(ctx, `SELECT name, tip FROM branches WHERE name = ?`, name).Scan(&result.Name, &result.Tip)
	if errors.Is(err, sql.ErrNoRows) {
		return Branch{}, ErrBranchNotFound
	}
	if err != nil {
		return Branch{}, fmt.Errorf("read branch %q: %w", name, err)
	}
	return result, nil
}

// Branches returns a bounded page in lexical branch-name order. Next is the
// last returned name when another page remains and is used as an exclusive
// cursor for the following request.
func (s *Service) Branches(ctx context.Context, after string, limit int) ([]Branch, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, "", ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("%w: branch page limit must be 1–1000", ErrInvalidRequest)
	}
	if after != "" {
		if err := validateBranchName(after); err != nil {
			return nil, "", fmt.Errorf("%w: invalid branch cursor", ErrInvalidRequest)
		}
	}
	rows, err := s.metadata.db.QueryContext(ctx,
		`SELECT name, tip FROM branches WHERE name > ? ORDER BY name LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()
	result := make([]Branch, 0, limit)
	for rows.Next() {
		var branch Branch
		if err := rows.Scan(&branch.Name, &branch.Tip); err != nil {
			return nil, "", fmt.Errorf("scan branch: %w", err)
		}
		result = append(result, branch)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate branches: %w", err)
	}
	var next string
	if len(result) > limit {
		result = result[:limit]
		next = result[len(result)-1].Name
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, next, nil
}

func validateCommitID(id string) error {
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != id {
		return errors.New("invalid commit ID")
	}
	return nil
}
