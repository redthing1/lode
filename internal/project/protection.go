package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CreateTag adds an immutable name for one retained version. Tags are
// metadata-only protection reasons; the accepted operation root remains the
// sole native root for the commit.
func (s *Service) CreateTag(ctx context.Context, name, commitID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateBranchName(name); err != nil {
		return fmt.Errorf("%w: invalid tag name", ErrInvalidRequest)
	}
	if err := validateCommitID(commitID); err != nil {
		return fmt.Errorf("%w: invalid commit ID", ErrInvalidRequest)
	}
	if existing, err := s.metadata.tag(ctx, name); err == nil {
		if existing.CommitID == commitID {
			return nil
		}
		return ErrTagExists
	} else if !errors.Is(err, ErrTagNotFound) {
		return err
	}
	if err := s.requireRetained(ctx, commitID); err != nil {
		return err
	}
	commit, err := s.metadata.commit(ctx, commitID)
	if err != nil {
		return err
	}
	archive, err := s.archive(ctx, commit.Tree)
	if err != nil {
		return err
	}
	if err := s.roots.Validate(ctx, archive); err != nil {
		return fmt.Errorf("validate tagged version contents: %w", err)
	}

	tx, err := s.metadata.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tag creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT commit_id FROM tags WHERE name = ?`, name).Scan(&existingID)
	if err == nil {
		if existingID != commitID {
			return ErrTagExists
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing tag: %w", err)
	}
	var expiredAt string
	if err := tx.QueryRowContext(ctx, `SELECT expired_at FROM payload_retention WHERE commit_id = ?`, commitID).Scan(&expiredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("commit %q has no payload retention row", commitID)
		}
		return fmt.Errorf("recheck tagged version availability: %w", err)
	}
	if expiredAt != "" {
		return ErrPayloadExpired
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tags (name, commit_id) VALUES (?, ?)`, name, commitID); err != nil {
		return fmt.Errorf("insert tag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tag creation: %w", err)
	}
	return nil
}

// DeleteTag removes a tag only if it still refers to expectedCommitID. An
// absent tag is an idempotent success.
func (s *Service) DeleteTag(ctx context.Context, name, expectedCommitID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateBranchName(name); err != nil {
		return fmt.Errorf("%w: invalid tag name", ErrInvalidRequest)
	}
	if err := validateCommitID(expectedCommitID); err != nil {
		return fmt.Errorf("%w: invalid commit ID", ErrInvalidRequest)
	}
	tx, err := s.metadata.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tag deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentID string
	err = tx.QueryRowContext(ctx, `SELECT commit_id FROM tags WHERE name = ?`, name).Scan(&currentID)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("read tag before deletion: %w", err)
	}
	if currentID != expectedCommitID {
		return ErrStaleHead
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE name = ? AND commit_id = ?`, name, expectedCommitID)
	if err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check tag deletion: %w", err)
	}
	if deleted == 0 {
		return ErrStaleHead
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tag deletion: %w", err)
	}
	return nil
}

// Tags returns a bounded page of tag names in lexical order.
func (s *Service) Tags(ctx context.Context, after string, limit int) ([]Tag, string, error) {
	if s.closed.Load() {
		return nil, "", ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("%w: tag page limit must be 1–1000", ErrInvalidRequest)
	}
	if after != "" {
		if err := validateBranchName(after); err != nil {
			return nil, "", fmt.Errorf("%w: invalid tag cursor", ErrInvalidRequest)
		}
	}
	rows, err := s.metadata.db.QueryContext(ctx,
		`SELECT name, commit_id FROM tags WHERE name > ? ORDER BY name LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	result := make([]Tag, 0, limit)
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.Name, &tag.CommitID); err != nil {
			return nil, "", fmt.Errorf("scan tag: %w", err)
		}
		result = append(result, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate tags: %w", err)
	}
	var next string
	if len(result) > limit {
		result = result[:limit]
		next = result[len(result)-1].Name
	}
	return result, next, nil
}

// Tag returns one exact tag name.
func (s *Service) Tag(ctx context.Context, name string) (Tag, error) {
	if s.closed.Load() {
		return Tag{}, ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return Tag{}, err
	}
	if err := validateBranchName(name); err != nil {
		return Tag{}, fmt.Errorf("%w: invalid tag name", ErrInvalidRequest)
	}
	return s.metadata.tag(ctx, name)
}

// HoldRead creates or renews a bounded read hold. The first grant verifies the
// native contents; subsequent renewals trust the existing lease and retained
// metadata state, without creating another native root.
func (s *Service) HoldRead(ctx context.Context, id, commitID string) (ReadLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ReadLease{}, ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return ReadLease{}, err
	}
	if err := validateOperationID(id); err != nil {
		return ReadLease{}, fmt.Errorf("%w: invalid read hold ID", ErrInvalidRequest)
	}
	if err := validateCommitID(commitID); err != nil {
		return ReadLease{}, fmt.Errorf("%w: invalid commit ID", ErrInvalidRequest)
	}
	existing, err := s.metadata.readLease(ctx, id)
	if err == nil {
		if existing.CommitID != commitID {
			return ReadLease{}, ErrReadLeaseConflict
		}
		if err := s.requireRetained(ctx, commitID); err != nil {
			return ReadLease{}, err
		}
		return s.renewReadLease(ctx, id, commitID)
	}
	if !errors.Is(err, ErrMetadataNotFound) {
		return ReadLease{}, err
	}
	if err := s.requireRetained(ctx, commitID); err != nil {
		return ReadLease{}, err
	}
	commit, err := s.metadata.commit(ctx, commitID)
	if err != nil {
		return ReadLease{}, err
	}
	archive, err := s.archive(ctx, commit.Tree)
	if err != nil {
		return ReadLease{}, err
	}
	if err := s.roots.Validate(ctx, archive); err != nil {
		return ReadLease{}, fmt.Errorf("validate read-held contents: %w", err)
	}
	return s.renewReadLease(ctx, id, commitID)
}

func (s *Service) renewReadLease(ctx context.Context, id, commitID string) (ReadLease, error) {
	now := s.now().UTC()
	expiresAt := now.Add(s.readTTL)
	tx, err := s.metadata.db.BeginTx(ctx, nil)
	if err != nil {
		return ReadLease{}, fmt.Errorf("begin read hold renewal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var expiredAt string
	if err := tx.QueryRowContext(ctx, `SELECT expired_at FROM payload_retention WHERE commit_id = ?`, commitID).Scan(&expiredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReadLease{}, fmt.Errorf("commit %q has no payload retention row", commitID)
		}
		return ReadLease{}, fmt.Errorf("recheck read-held version: %w", err)
	}
	if expiredAt != "" {
		return ReadLease{}, ErrPayloadExpired
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO read_leases (id, commit_id, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET expires_at = excluded.expires_at WHERE read_leases.commit_id = excluded.commit_id`,
		id, commitID, formatMetadataTime(expiresAt)); err != nil {
		return ReadLease{}, fmt.Errorf("persist read hold: %w", err)
	}
	var storedCommit string
	if err := tx.QueryRowContext(ctx, `SELECT commit_id FROM read_leases WHERE id = ?`, id).Scan(&storedCommit); err != nil {
		return ReadLease{}, fmt.Errorf("verify read hold binding: %w", err)
	}
	if storedCommit != commitID {
		return ReadLease{}, ErrReadLeaseConflict
	}
	if err := tx.Commit(); err != nil {
		return ReadLease{}, fmt.Errorf("commit read hold renewal: %w", err)
	}
	if err := s.pruneReadLeases(ctx); err != nil {
		return ReadLease{}, err
	}
	return ReadLease{ID: id, CommitID: commitID, ExpiresAt: expiresAt, Lifetime: s.readTTL}, nil
}

// ReleaseRead removes a hold immediately. Holds are bounded even if release
// cannot reach the service, so release IDs do not reserve tombstones.
func (s *Service) ReleaseRead(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOperationID(id); err != nil {
		return fmt.Errorf("%w: invalid read hold ID", ErrInvalidRequest)
	}
	if _, err := s.metadata.db.ExecContext(ctx, `DELETE FROM read_leases WHERE id = ?`, id); err != nil {
		return fmt.Errorf("release read hold: %w", err)
	}
	return nil
}

// pruneReadLeases removes expired rows. Callers hold Service.mu so pruning
// cannot race a service-mediated renewal. Exact stored timestamps are used in
// deletes as an additional compare-and-swap guard.
func (s *Service) pruneReadLeases(ctx context.Context) error {
	now := s.now().UTC()
	rows, err := s.metadata.db.QueryContext(ctx, `SELECT id, expires_at FROM read_leases ORDER BY id`)
	if err != nil {
		return fmt.Errorf("query read holds to prune: %w", err)
	}
	type expiredHold struct{ id, storedExpiry string }
	var expired []expiredHold
	for rows.Next() {
		var item expiredHold
		var expiresText string
		if err := rows.Scan(&item.id, &expiresText); err != nil {
			rows.Close()
			return fmt.Errorf("scan read hold to prune: %w", err)
		}
		expiresAt, err := parseMetadataTime(expiresText)
		if err != nil {
			rows.Close()
			return fmt.Errorf("parse read hold expiry: %w", err)
		}
		if !expiresAt.After(now) {
			item.storedExpiry = expiresText
			expired = append(expired, item)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate read holds to prune: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close read holds to prune: %w", err)
	}
	if len(expired) == 0 {
		return nil
	}
	tx, err := s.metadata.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin read hold pruning: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range expired {
		if _, err := tx.ExecContext(ctx, `DELETE FROM read_leases WHERE id = ? AND expires_at = ?`, item.id, item.storedExpiry); err != nil {
			return fmt.Errorf("prune read hold %q: %w", item.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit read hold pruning: %w", err)
	}
	return nil
}

func (m *Metadata) tag(ctx context.Context, name string) (Tag, error) {
	if m == nil || m.db == nil {
		return Tag{}, errors.New("read tag: database is not open")
	}
	var tag Tag
	err := m.db.QueryRowContext(ctx, `SELECT name, commit_id FROM tags WHERE name = ?`, name).Scan(&tag.Name, &tag.CommitID)
	if errors.Is(err, sql.ErrNoRows) {
		return Tag{}, ErrTagNotFound
	}
	if err != nil {
		return Tag{}, fmt.Errorf("read tag: %w", err)
	}
	return tag, nil
}

func (m *Metadata) readLease(ctx context.Context, id string) (ReadLease, error) {
	if m == nil || m.db == nil {
		return ReadLease{}, errors.New("read hold: database is not open")
	}
	var lease ReadLease
	var expiresAt string
	err := m.db.QueryRowContext(ctx, `SELECT id, commit_id, expires_at FROM read_leases WHERE id = ?`, id).Scan(&lease.ID, &lease.CommitID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReadLease{}, ErrMetadataNotFound
	}
	if err != nil {
		return ReadLease{}, fmt.Errorf("read hold: %w", err)
	}
	lease.ExpiresAt, err = parseMetadataTime(expiresAt)
	if err != nil {
		return ReadLease{}, fmt.Errorf("parse read hold expiry: %w", err)
	}
	return lease, nil
}
