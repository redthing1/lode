package project

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type expiredPayloadRoot struct {
	CommitID    string
	OperationID string
}

func migrateRetentionSchema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read retention schema version: %w", err)
	}
	if version == metadataSchemaVersion {
		return nil
	}
	if version != 3 {
		return fmt.Errorf("cannot migrate retention schema from version %d", version)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retention schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`CREATE INDEX publication_ops_by_candidate_state ON publication_ops(candidate_commit_id, state)`,
		`CREATE TABLE payload_retention (
			commit_id TEXT PRIMARY KEY,
			expired_at TEXT NOT NULL DEFAULT '',
			released INTEGER NOT NULL DEFAULT 0 CHECK (released IN (0, 1)),
			protect_until TEXT NOT NULL DEFAULT ''
		) WITHOUT ROWID`,
		`INSERT INTO payload_retention (commit_id, expired_at, released, protect_until)
		 SELECT c.id, '', 0,
			COALESCE((SELECT p.expires_at FROM publication_ops p
			 WHERE p.candidate_commit_id = c.id AND p.state = 'accepted'
			 ORDER BY p.id LIMIT 1), '')
		 FROM commits c`,
		`CREATE INDEX payload_retention_pending_release ON payload_retention(commit_id) WHERE expired_at <> '' AND released = 0`,
		`CREATE TABLE tags (
			name TEXT PRIMARY KEY,
			commit_id TEXT NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE read_leases (
			id TEXT PRIMARY KEY,
			commit_id TEXT NOT NULL,
			expires_at TEXT NOT NULL
		) WITHOUT ROWID`,
		`CREATE INDEX read_leases_by_commit_expiry ON read_leases(commit_id, expires_at)`,
		`CREATE TABLE retention_policy (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			keep_last INTEGER NOT NULL CHECK (keep_last >= 0),
			keep_days INTEGER NOT NULL CHECK (keep_days >= 0)
		) WITHOUT ROWID`,
		`INSERT INTO retention_policy (singleton, keep_last, keep_days) VALUES (1, 10, 7)`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate retention schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retention schema migration: %w", err)
	}
	return nil
}

// payload returns the deliberate availability state for one immutable commit.
// A missing commit is distinct from a commit whose retention row is corruptly
// absent.
func (m *Metadata) payload(ctx context.Context, commitID string) (payloadRecord, error) {
	if m == nil || m.db == nil {
		return payloadRecord{}, errors.New("read payload retention: database is not open")
	}
	var record payloadRecord
	var expiredAt, protectUntil string
	var released int
	err := m.db.QueryRowContext(ctx,
		`SELECT expired_at, released, protect_until FROM payload_retention WHERE commit_id = ?`, commitID,
	).Scan(&expiredAt, &released, &protectUntil)
	if errors.Is(err, sql.ErrNoRows) {
		if _, commitErr := loadCommit(ctx, m.db, commitID); errors.Is(commitErr, ErrCommitNotFound) {
			return payloadRecord{}, ErrCommitNotFound
		} else if commitErr != nil {
			return payloadRecord{}, commitErr
		}
		return payloadRecord{}, fmt.Errorf("commit %q has no payload retention row", commitID)
	}
	if err != nil {
		return payloadRecord{}, fmt.Errorf("read payload retention: %w", err)
	}
	record.CommitID = commitID
	record.Released = released != 0
	if expiredAt != "" {
		record.ExpiredAt, err = parseMetadataTime(expiredAt)
		if err != nil {
			return payloadRecord{}, fmt.Errorf("parse payload expiration: %w", err)
		}
	}
	if protectUntil != "" {
		record.ProtectUntil, err = parseMetadataTime(protectUntil)
		if err != nil {
			return payloadRecord{}, fmt.Errorf("parse payload grace deadline: %w", err)
		}
	}
	return record, nil
}

// retentionSnapshot reads the metadata required to decide payload protection
// from one SQLite snapshot. Rows are closed before another query is started.
func (m *Metadata) retentionSnapshot(ctx context.Context) (retentionSnapshot, error) {
	if m == nil || m.db == nil {
		return retentionSnapshot{}, errors.New("read retention snapshot: database is not open")
	}
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return retentionSnapshot{}, fmt.Errorf("begin retention snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := retentionSnapshot{
		Commits: make(map[string]Commit), Payloads: make(map[string]payloadRecord),
		Branches: []Branch{}, Tags: []Tag{}, Leases: []ReadLease{}, Operations: []publicationRecord{},
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, data FROM commits ORDER BY id`)
	if err != nil {
		return retentionSnapshot{}, fmt.Errorf("query retention commits: %w", err)
	}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("scan retention commit: %w", err)
		}
		var commit Commit
		if err := json.Unmarshal(data, &commit); err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("decode retention commit %q: %w", id, err)
		}
		snapshot.Commits[id] = commit
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return retentionSnapshot{}, fmt.Errorf("iterate retention commits: %w", err)
	}
	if err := rows.Close(); err != nil {
		return retentionSnapshot{}, fmt.Errorf("close retention commits: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `SELECT commit_id, expired_at, released, protect_until FROM payload_retention ORDER BY commit_id`)
	if err != nil {
		return retentionSnapshot{}, fmt.Errorf("query payload states: %w", err)
	}
	for rows.Next() {
		var record payloadRecord
		var expiredAt, protectUntil string
		var released int
		if err := rows.Scan(&record.CommitID, &expiredAt, &released, &protectUntil); err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("scan payload state: %w", err)
		}
		record.Released = released != 0
		if expiredAt != "" {
			record.ExpiredAt, err = parseMetadataTime(expiredAt)
			if err != nil {
				rows.Close()
				return retentionSnapshot{}, fmt.Errorf("parse payload expiration: %w", err)
			}
		}
		if protectUntil != "" {
			record.ProtectUntil, err = parseMetadataTime(protectUntil)
			if err != nil {
				rows.Close()
				return retentionSnapshot{}, fmt.Errorf("parse payload grace deadline: %w", err)
			}
		}
		snapshot.Payloads[record.CommitID] = record
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return retentionSnapshot{}, fmt.Errorf("iterate payload states: %w", err)
	}
	if err := rows.Close(); err != nil {
		return retentionSnapshot{}, fmt.Errorf("close payload states: %w", err)
	}
	if len(snapshot.Payloads) != len(snapshot.Commits) {
		return retentionSnapshot{}, fmt.Errorf("retention snapshot has %d payload rows for %d commits", len(snapshot.Payloads), len(snapshot.Commits))
	}
	for id := range snapshot.Commits {
		if _, ok := snapshot.Payloads[id]; !ok {
			return retentionSnapshot{}, fmt.Errorf("commit %q has no payload retention row", id)
		}
	}
	for id := range snapshot.Payloads {
		if _, ok := snapshot.Commits[id]; !ok {
			return retentionSnapshot{}, fmt.Errorf("payload retention row references missing commit %q", id)
		}
	}

	rows, err = tx.QueryContext(ctx, `SELECT name, tip FROM branches ORDER BY name`)
	if err != nil {
		return retentionSnapshot{}, fmt.Errorf("query retention branches: %w", err)
	}
	for rows.Next() {
		var branch Branch
		if err := rows.Scan(&branch.Name, &branch.Tip); err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("scan retention branch: %w", err)
		}
		snapshot.Branches = append(snapshot.Branches, branch)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return retentionSnapshot{}, fmt.Errorf("iterate retention branches: %w", err)
	}
	if err := rows.Close(); err != nil {
		return retentionSnapshot{}, fmt.Errorf("close retention branches: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `SELECT name, commit_id FROM tags ORDER BY name`)
	if err != nil {
		return retentionSnapshot{}, fmt.Errorf("query retention tags: %w", err)
	}
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.Name, &tag.CommitID); err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("scan retention tag: %w", err)
		}
		snapshot.Tags = append(snapshot.Tags, tag)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return retentionSnapshot{}, fmt.Errorf("iterate retention tags: %w", err)
	}
	if err := rows.Close(); err != nil {
		return retentionSnapshot{}, fmt.Errorf("close retention tags: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `SELECT id, commit_id, expires_at FROM read_leases ORDER BY id`)
	if err != nil {
		return retentionSnapshot{}, fmt.Errorf("query read leases: %w", err)
	}
	for rows.Next() {
		var lease ReadLease
		var expiresAt string
		if err := rows.Scan(&lease.ID, &lease.CommitID, &expiresAt); err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("scan read lease: %w", err)
		}
		lease.ExpiresAt, err = parseMetadataTime(expiresAt)
		if err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("parse read lease expiry: %w", err)
		}
		snapshot.Leases = append(snapshot.Leases, lease)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return retentionSnapshot{}, fmt.Errorf("iterate read leases: %w", err)
	}
	if err := rows.Close(); err != nil {
		return retentionSnapshot{}, fmt.Errorf("close read leases: %w", err)
	}

	rows, err = tx.QueryContext(ctx, publicationSelect+` WHERE state IN ('capturing', 'pending') ORDER BY id`)
	if err != nil {
		return retentionSnapshot{}, fmt.Errorf("query active retention operations: %w", err)
	}
	for rows.Next() {
		op, err := scanPublication(rows)
		if err != nil {
			rows.Close()
			return retentionSnapshot{}, fmt.Errorf("scan active retention operation: %w", err)
		}
		snapshot.Operations = append(snapshot.Operations, op)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return retentionSnapshot{}, fmt.Errorf("iterate active retention operations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return retentionSnapshot{}, fmt.Errorf("close active retention operations: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT keep_last, keep_days FROM retention_policy WHERE singleton = 1`).Scan(&snapshot.Policy.KeepLast, &snapshot.Policy.KeepDays); err != nil {
		return retentionSnapshot{}, fmt.Errorf("read retention policy snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return retentionSnapshot{}, fmt.Errorf("finish retention snapshot: %w", err)
	}
	return snapshot, nil
}

func (m *Metadata) retentionPolicy(ctx context.Context) (RetentionPolicy, error) {
	if m == nil || m.db == nil {
		return RetentionPolicy{}, errors.New("read retention policy: database is not open")
	}
	var policy RetentionPolicy
	if err := m.db.QueryRowContext(ctx, `SELECT keep_last, keep_days FROM retention_policy WHERE singleton = 1`).Scan(&policy.KeepLast, &policy.KeepDays); err != nil {
		return RetentionPolicy{}, fmt.Errorf("read retention policy: %w", err)
	}
	return policy, nil
}

func (m *Metadata) setRetentionPolicy(ctx context.Context, policy RetentionPolicy) error {
	if m == nil || m.db == nil {
		return errors.New("set retention policy: database is not open")
	}
	if policy.KeepLast < 0 || policy.KeepDays < 0 {
		return errors.New("set retention policy: keep_last and keep_days must be nonnegative")
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE retention_policy SET keep_last = ?, keep_days = ? WHERE singleton = 1`, policy.KeepLast, policy.KeepDays); err != nil {
		return fmt.Errorf("update retention policy: %w", err)
	}
	return nil
}

// expirePayloads durably declares payloads unavailable. Native release is a
// separate idempotent step recorded by markPayloadReleased.
func (m *Metadata) expirePayloads(ctx context.Context, commitIDs []string, now time.Time) error {
	if m == nil || m.db == nil {
		return errors.New("expire payloads: database is not open")
	}
	ordered := append([]string(nil), commitIDs...)
	sort.Strings(ordered)
	for i, id := range ordered {
		if id == "" || i > 0 && ordered[i-1] == id {
			return errors.New("expire payloads: commit IDs must be nonempty and unique")
		}
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin payload expiration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ordered {
		var expiredAt, protectUntil string
		err := tx.QueryRowContext(ctx,
			`SELECT expired_at, protect_until FROM payload_retention WHERE commit_id = ?`, id,
		).Scan(&expiredAt, &protectUntil)
		if errors.Is(err, sql.ErrNoRows) {
			if _, commitErr := loadCommitTx(ctx, tx, id); errors.Is(commitErr, ErrCommitNotFound) {
				return ErrCommitNotFound
			} else if commitErr != nil {
				return commitErr
			}
			return fmt.Errorf("commit %q has no payload retention row", id)
		}
		if err != nil {
			return fmt.Errorf("read payload before expiration: %w", err)
		}
		if expiredAt != "" {
			continue
		}
		if protectUntil != "" {
			deadline, err := parseMetadataTime(protectUntil)
			if err != nil {
				return fmt.Errorf("parse payload grace deadline: %w", err)
			}
			if now.Before(deadline) {
				return fmt.Errorf("commit %q is within its initial payload grace period", id)
			}
		}
		var acceptedCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM publication_ops WHERE candidate_commit_id = ? AND state = 'accepted'`, id,
		).Scan(&acceptedCount); err != nil {
			return fmt.Errorf("validate accepted operation mapping: %w", err)
		}
		if acceptedCount != 1 {
			return fmt.Errorf("commit %q has %d accepted operation mappings, want exactly one", id, acceptedCount)
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE payload_retention SET expired_at = ?, released = 0 WHERE commit_id = ? AND expired_at = ''`,
			formatMetadataTime(now), id)
		if err != nil {
			return fmt.Errorf("mark payload expired: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check payload expiration: %w", err)
		}
		if updated != 1 {
			return fmt.Errorf("payload expiration for commit %q did not update one row", id)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit payload expiration: %w", err)
	}
	return nil
}

func (m *Metadata) expiredPayloadRoots(ctx context.Context, after string, limit int) ([]expiredPayloadRoot, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("list expired payload roots: database is not open")
	}
	if limit < 1 {
		return nil, errors.New("list expired payload roots: limit must be positive")
	}
	if limit > maxRecoverablePublicationBatch {
		limit = maxRecoverablePublicationBatch
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.commit_id, p.id
		FROM payload_retention r
		JOIN publication_ops p ON p.candidate_commit_id = r.commit_id AND p.state = 'accepted'
		WHERE r.expired_at <> '' AND r.released = 0 AND r.commit_id > ?
		ORDER BY r.commit_id, p.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query expired payload roots: %w", err)
	}
	defer rows.Close()
	var result []expiredPayloadRoot
	for rows.Next() {
		var root expiredPayloadRoot
		if err := rows.Scan(&root.CommitID, &root.OperationID); err != nil {
			return nil, fmt.Errorf("scan expired payload root: %w", err)
		}
		result = append(result, root)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired payload roots: %w", err)
	}
	return result, nil
}

func (m *Metadata) markPayloadReleased(ctx context.Context, commitID string) error {
	if m == nil || m.db == nil {
		return errors.New("mark payload released: database is not open")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin payload release completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var expiredAt string
	var released int
	err = tx.QueryRowContext(ctx, `SELECT expired_at, released FROM payload_retention WHERE commit_id = ?`, commitID).Scan(&expiredAt, &released)
	if errors.Is(err, sql.ErrNoRows) {
		if _, commitErr := loadCommitTx(ctx, tx, commitID); errors.Is(commitErr, ErrCommitNotFound) {
			return ErrCommitNotFound
		} else if commitErr != nil {
			return commitErr
		}
		return fmt.Errorf("commit %q has no payload retention row", commitID)
	}
	if err != nil {
		return fmt.Errorf("read payload release state: %w", err)
	}
	if expiredAt == "" {
		return fmt.Errorf("commit %q payload has not been marked expired", commitID)
	}
	if released == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE payload_retention SET released = 1 WHERE commit_id = ? AND expired_at <> ''`, commitID); err != nil {
			return fmt.Errorf("mark payload released: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit payload release completion: %w", err)
	}
	return nil
}
