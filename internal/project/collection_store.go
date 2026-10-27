package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

type compactionRecord struct {
	SourcePack  string
	StartedAt   time.Time
	RewrittenAt time.Time
}

func (m *Metadata) capacityPolicy(ctx context.Context) (CapacityPolicy, error) {
	if m == nil || m.db == nil {
		return CapacityPolicy{}, errors.New("read capacity policy: database is not open")
	}
	var reserve int64
	err := m.db.QueryRowContext(ctx, `SELECT reserve_bytes FROM capacity_policy WHERE singleton = 1`).Scan(&reserve)
	if err != nil {
		return CapacityPolicy{}, fmt.Errorf("read capacity policy: %w", err)
	}
	if reserve < 0 {
		return CapacityPolicy{}, errors.New("stored capacity reserve is negative")
	}
	return CapacityPolicy{ReserveBytes: uint64(reserve)}, nil
}

func (m *Metadata) setCapacityPolicy(ctx context.Context, policy CapacityPolicy) error {
	if m == nil || m.db == nil {
		return errors.New("write capacity policy: database is not open")
	}
	if policy.ReserveBytes > math.MaxInt64 {
		return errors.New("capacity reserve exceeds SQLite integer range")
	}
	_, err := m.db.ExecContext(ctx, `INSERT INTO capacity_policy (singleton, reserve_bytes) VALUES (1, ?)
		ON CONFLICT(singleton) DO UPDATE SET reserve_bytes = excluded.reserve_bytes`, int64(policy.ReserveBytes))
	if err != nil {
		return fmt.Errorf("write capacity policy: %w", err)
	}
	return nil
}

// compaction returns the one current targeted-rewrite checkpoint. An absent row
// means no targeted rewrite is in flight.
func (m *Metadata) compaction(ctx context.Context) (compactionRecord, bool, error) {
	if m == nil || m.db == nil {
		return compactionRecord{}, false, errors.New("read compaction state: database is not open")
	}
	var record compactionRecord
	var started, rewritten string
	err := m.db.QueryRowContext(ctx, `SELECT source_pack_id, started_at, rewritten_at
		FROM compaction_state WHERE singleton = 1`).Scan(&record.SourcePack, &started, &rewritten)
	if errors.Is(err, sql.ErrNoRows) {
		return compactionRecord{}, false, nil
	}
	if err != nil {
		return compactionRecord{}, false, fmt.Errorf("read compaction state: %w", err)
	}
	record.StartedAt, err = parseMetadataTime(started)
	if err != nil {
		return compactionRecord{}, false, fmt.Errorf("parse compaction start time: %w", err)
	}
	if rewritten != "" {
		record.RewrittenAt, err = parseMetadataTime(rewritten)
		if err != nil {
			return compactionRecord{}, false, fmt.Errorf("parse compaction completion time: %w", err)
		}
	}
	return record, true, nil
}

func (m *Metadata) beginCompaction(ctx context.Context, source string, at time.Time) error {
	if m == nil || m.db == nil {
		return errors.New("begin compaction: database is not open")
	}
	if err := validateSourcePack(source); err != nil {
		return err
	}
	if at.IsZero() {
		return errors.New("begin compaction: zero start time")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compaction checkpoint: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing, started string
	err = tx.QueryRowContext(ctx, `SELECT source_pack_id, started_at FROM compaction_state WHERE singleton = 1`).Scan(&existing, &started)
	if err == nil {
		if existing != source {
			return ErrMetadataConflict
		}
		if _, err := parseMetadataTime(started); err != nil {
			return fmt.Errorf("parse existing compaction start time: %w", err)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO compaction_state (singleton, source_pack_id, started_at, rewritten_at) VALUES (1, ?, ?, '')`, source, formatMetadataTime(at)); err != nil {
			return fmt.Errorf("insert compaction checkpoint: %w", err)
		}
	} else {
		return fmt.Errorf("read existing compaction checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit compaction checkpoint: %w", err)
	}
	return nil
}

func (m *Metadata) finishCompactionRewrite(ctx context.Context, source string, at time.Time) error {
	if m == nil || m.db == nil {
		return errors.New("finish compaction rewrite: database is not open")
	}
	if err := validateSourcePack(source); err != nil {
		return err
	}
	if at.IsZero() {
		return errors.New("finish compaction rewrite: zero completion time")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compaction completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing, rewritten string
	err = tx.QueryRowContext(ctx, `SELECT source_pack_id, rewritten_at FROM compaction_state WHERE singleton = 1`).Scan(&existing, &rewritten)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMetadataNotFound
	}
	if err != nil {
		return fmt.Errorf("read compaction checkpoint: %w", err)
	}
	if existing != source {
		return ErrMetadataConflict
	}
	if rewritten == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE compaction_state SET rewritten_at = ? WHERE singleton = 1 AND source_pack_id = ? AND rewritten_at = ''`, formatMetadataTime(at), source); err != nil {
			return fmt.Errorf("mark compaction rewritten: %w", err)
		}
	} else if _, err := parseMetadataTime(rewritten); err != nil {
		return fmt.Errorf("parse existing compaction completion time: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit compaction completion: %w", err)
	}
	return nil
}

func (m *Metadata) clearCompaction(ctx context.Context, source string) error {
	if m == nil || m.db == nil {
		return errors.New("clear compaction: database is not open")
	}
	if err := validateSourcePack(source); err != nil {
		return err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compaction clear: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT source_pack_id FROM compaction_state WHERE singleton = 1`).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read compaction checkpoint: %w", err)
	}
	if err == nil {
		if existing != source {
			return ErrMetadataConflict
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM compaction_state WHERE singleton = 1 AND source_pack_id = ?`, source); err != nil {
			return fmt.Errorf("clear compaction checkpoint: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit compaction clear: %w", err)
	}
	return nil
}

func validateSourcePack(source string) error {
	if source == "" || len(source) > 1024 {
		return errors.New("invalid source pack identifier")
	}
	return nil
}
