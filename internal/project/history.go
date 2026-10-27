package project

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/engine/kopia"
)

const maxRecoverablePublicationBatch = 256

// This fingerprint cannot be produced by either publication request path,
// both of which store SHA-256 hex digests. It marks an Abort-before-arrival
// row without adding a second operation ledger or schema version.
const abortedBeforeArrivalFingerprint = "lode-abort-before-arrival-v1"

func (m *Metadata) bindProject(ctx context.Context, namespace, proposedProjectID string) (string, error) {
	if m == nil || m.db == nil {
		return "", errors.New("bind project: database is not open")
	}
	if namespace == "" || proposedProjectID == "" {
		return "", errors.New("bind project: namespace and project ID must be nonempty")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin project binding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectID, existingNamespace string
	err = tx.QueryRowContext(ctx, `SELECT project_id, namespace FROM project_binding WHERE singleton = 1`).Scan(&projectID, &existingNamespace)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_binding (singleton, project_id, namespace) VALUES (1, ?, ?)`, proposedProjectID, namespace); err != nil {
			return "", fmt.Errorf("persist project binding: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit project binding: %w", err)
		}
		return proposedProjectID, nil
	}
	if err != nil {
		return "", fmt.Errorf("read project binding: %w", err)
	}
	if existingNamespace != namespace {
		return "", ErrRepositoryMismatch
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit project binding lookup: %w", err)
	}
	return projectID, nil
}

func (m *Metadata) preparePublication(ctx context.Context, proposed publicationRecord) (publicationRecord, error) {
	if m == nil || m.db == nil {
		return publicationRecord{}, errors.New("prepare publication: database is not open")
	}
	if proposed.ID == "" || proposed.Fingerprint == "" || proposed.CommitID == "" {
		return publicationRecord{}, errors.New("prepare publication: operation ID, fingerprint, and candidate commit ID must be nonempty")
	}
	if len(proposed.CaptureInput) != 0 {
		return publicationRecord{}, errors.New("prepare publication: capture input is only valid for sparse capture")
	}
	if err := validatePublicationParents(proposed.Input); err != nil {
		return publicationRecord{}, fmt.Errorf("prepare publication: %w", err)
	}
	if proposed.State != "" && proposed.State != OperationPending {
		return publicationRecord{}, errors.New("prepare publication: new operation must be pending")
	}
	sealedInput, err := json.Marshal(proposed.Input)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("encode publication input: %w", err)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("begin publication preparation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanPublication(tx.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, proposed.ID))
	if err == nil {
		if isAbortBeforeArrival(existing) {
			return publicationRecord{}, ErrOperationAborted
		}
		storedInput, marshalErr := json.Marshal(existing.Input)
		if marshalErr != nil {
			return publicationRecord{}, fmt.Errorf("encode stored publication input: %w", marshalErr)
		}
		if existing.Fingerprint != proposed.Fingerprint || len(existing.CaptureInput) != 0 || !bytes.Equal(storedInput, sealedInput) {
			return publicationRecord{}, ErrOperationConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return publicationRecord{}, fmt.Errorf("read existing publication: %w", err)
	}

	var boundNamespace string
	if err := tx.QueryRowContext(ctx, `SELECT namespace FROM project_binding WHERE singleton = 1`).Scan(&boundNamespace); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return publicationRecord{}, ErrRepositoryMismatch
		}
		return publicationRecord{}, fmt.Errorf("read project repository binding: %w", err)
	}
	var rootExists int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM archive_roots WHERE namespace = ? AND format = ? AND archive_id = ?`,
		boundNamespace, proposed.Input.Tree.Format, proposed.Input.Tree.ID).Scan(&rootExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return publicationRecord{}, ErrMetadataNotFound
		}
		return publicationRecord{}, fmt.Errorf("check publication root: %w", err)
	}
	proposed.State = OperationPending
	proposed.ObservedTip = ""
	proposed.ReleaseDone = false
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO publication_ops
			(id, fingerprint, sealed_input, capture_input, candidate_commit_id, created_at, expires_at, state, observed_tip, release_done)
		 VALUES (?, ?, ?, X'', ?, ?, ?, ?, ?, 0)`,
		proposed.ID, proposed.Fingerprint, sealedInput, proposed.CommitID,
		formatMetadataTime(proposed.CreatedAt), formatMetadataTime(proposed.ExpiresAt), string(OperationPending), ""); err != nil {
		return publicationRecord{}, fmt.Errorf("insert publication operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return publicationRecord{}, fmt.Errorf("commit publication preparation: %w", err)
	}
	return proposed, nil
}

// prepareCapture reserves an immutable sparse request before payload IO begins.
// CaptureInput is opaque canonical JSON produced by the project service.
func (m *Metadata) prepareCapture(ctx context.Context, proposed publicationRecord) (publicationRecord, error) {
	if m == nil || m.db == nil {
		return publicationRecord{}, errors.New("prepare capture: database is not open")
	}
	if proposed.ID == "" || proposed.Fingerprint == "" || proposed.CommitID == "" || len(proposed.CaptureInput) == 0 {
		return publicationRecord{}, errors.New("prepare capture: operation ID, fingerprint, candidate commit ID, and capture input must be nonempty")
	}
	if proposed.State != "" && proposed.State != OperationCapturing {
		return publicationRecord{}, errors.New("prepare capture: new operation must be capturing")
	}
	if err := validatePublicationParents(proposed.Input); err != nil {
		return publicationRecord{}, fmt.Errorf("prepare capture: %w", err)
	}
	sealedInput, err := json.Marshal(proposed.Input)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("encode capture input fields: %w", err)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("begin capture preparation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanPublication(tx.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, proposed.ID))
	if err == nil {
		if isAbortBeforeArrival(existing) {
			return publicationRecord{}, ErrOperationAborted
		}
		if len(existing.CaptureInput) == 0 || existing.Fingerprint != proposed.Fingerprint ||
			!bytes.Equal(existing.CaptureInput, proposed.CaptureInput) ||
			existing.Input.Branch != proposed.Input.Branch || existing.Input.Base != proposed.Input.Base ||
			existing.Input.MergeParent != proposed.Input.MergeParent ||
			existing.Input.Author != proposed.Input.Author || existing.Input.Message != proposed.Input.Message {
			return publicationRecord{}, ErrOperationConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return publicationRecord{}, fmt.Errorf("read existing capture: %w", err)
	}
	if proposed.Input.Tree != (TreeRef{}) {
		return publicationRecord{}, errors.New("prepare capture: new capture cannot have an archived tree")
	}
	proposed.State = OperationCapturing
	proposed.ObservedTip = ""
	proposed.ReleaseDone = false
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO publication_ops
			(id, fingerprint, sealed_input, capture_input, candidate_commit_id, created_at, expires_at, state, observed_tip, release_done)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		proposed.ID, proposed.Fingerprint, sealedInput, proposed.CaptureInput, proposed.CommitID,
		formatMetadataTime(proposed.CreatedAt), formatMetadataTime(proposed.ExpiresAt), string(OperationCapturing), ""); err != nil {
		return publicationRecord{}, fmt.Errorf("insert capture operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return publicationRecord{}, fmt.Errorf("commit capture preparation: %w", err)
	}
	return proposed, nil
}

// checkCapture expires or rejects a reserved capture before the service reads
// uploaded payloads. A still-current operation remains in capturing state.
func (m *Metadata) checkCapture(ctx context.Context, id string, now time.Time) (publicationRecord, error) {
	if m == nil || m.db == nil {
		return publicationRecord{}, errors.New("check capture: database is not open")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("begin capture check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	operation, err := scanPublication(tx.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return publicationRecord{}, ErrOperationNotFound
	}
	if err != nil {
		return publicationRecord{}, fmt.Errorf("read capture check state: %w", err)
	}
	if operation.State != OperationCapturing {
		return operation, nil
	}
	if !operation.ExpiresAt.After(now) {
		operation.State = OperationExpired
		if err := updatePublicationOutcome(ctx, tx, operation); err != nil {
			return publicationRecord{}, err
		}
	} else {
		actualTip, err := headTx(ctx, tx, operation.Input.Branch)
		if err != nil {
			return publicationRecord{}, err
		}
		if actualTip != operation.Input.Base {
			operation.State = OperationStale
			operation.ObservedTip = actualTip
			if err := updatePublicationOutcome(ctx, tx, operation); err != nil {
				return publicationRecord{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return publicationRecord{}, fmt.Errorf("commit capture check: %w", err)
	}
	return operation, nil
}

// finishCapture attaches a verified archive to a reserved capture and changes
// it to the ordinary pending-publication state.
func (m *Metadata) finishCapture(ctx context.Context, id string, tree TreeRef) (publicationRecord, error) {
	if m == nil || m.db == nil {
		return publicationRecord{}, errors.New("finish capture: database is not open")
	}
	if tree.Format == "" || tree.ID == "" {
		return publicationRecord{}, errors.New("finish capture: tree format and ID must be nonempty")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("begin capture completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	operation, err := scanPublication(tx.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return publicationRecord{}, ErrOperationNotFound
	}
	if err != nil {
		return publicationRecord{}, fmt.Errorf("read capture completion state: %w", err)
	}
	if operation.State == OperationPending || operation.State == OperationAccepted {
		if operation.Input.Tree != tree {
			return publicationRecord{}, ErrOperationConflict
		}
		return operation, nil
	}
	if operation.State != OperationCapturing {
		return operation, nil
	}
	var namespace string
	if err := tx.QueryRowContext(ctx, `SELECT namespace FROM project_binding WHERE singleton = 1`).Scan(&namespace); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return publicationRecord{}, ErrRepositoryMismatch
		}
		return publicationRecord{}, fmt.Errorf("read capture repository binding: %w", err)
	}
	var rootExists int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM archive_roots WHERE namespace = ? AND format = ? AND archive_id = ?`,
		namespace, tree.Format, tree.ID).Scan(&rootExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return publicationRecord{}, ErrMetadataNotFound
		}
		return publicationRecord{}, fmt.Errorf("check captured archive root: %w", err)
	}
	operation.Input.Tree = tree
	sealedInput, err := json.Marshal(operation.Input)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("encode completed capture input: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE publication_ops SET sealed_input = ?, state = ? WHERE id = ? AND state = ?`,
		sealedInput, string(OperationPending), id, string(OperationCapturing)); err != nil {
		return publicationRecord{}, fmt.Errorf("persist completed capture: %w", err)
	}
	operation.State = OperationPending
	if err := tx.Commit(); err != nil {
		return publicationRecord{}, fmt.Errorf("commit capture completion: %w", err)
	}
	return operation, nil
}

func (m *Metadata) putFileFacts(ctx context.Context, namespace string, facts []kopia.FileFact) error {
	if m == nil || m.db == nil {
		return errors.New("store file facts: database is not open")
	}
	if namespace == "" {
		return errors.New("store file facts: namespace is empty")
	}
	for _, fact := range facts {
		decoded, err := hex.DecodeString(fact.SHA256)
		if fact.Ref == "" || err != nil || len(decoded) != 32 || strings.ToLower(fact.SHA256) != fact.SHA256 || fact.Size < 0 {
			return fmt.Errorf("store file facts: invalid fact for reference %q", fact.Ref)
		}
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file facts write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, fact := range facts {
		result, err := tx.ExecContext(ctx,
			`INSERT INTO file_facts (namespace, ref, sha256, size) VALUES (?, ?, ?, ?)
			 ON CONFLICT(namespace, ref) DO NOTHING`, namespace, fact.Ref, fact.SHA256, fact.Size)
		if err != nil {
			return fmt.Errorf("insert file fact %q: %w", fact.Ref, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check file fact insert %q: %w", fact.Ref, err)
		}
		if inserted != 0 {
			continue
		}
		var existingSHA string
		var existingSize int64
		if err := tx.QueryRowContext(ctx, `SELECT sha256, size FROM file_facts WHERE namespace = ? AND ref = ?`, namespace, fact.Ref).Scan(&existingSHA, &existingSize); err != nil {
			return fmt.Errorf("read existing file fact %q: %w", fact.Ref, err)
		}
		if existingSHA != fact.SHA256 || existingSize != fact.Size {
			return ErrMetadataConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file facts: %w", err)
	}
	return nil
}

func (m *Metadata) fileFact(ctx context.Context, namespace, ref string) (kopia.FileFact, error) {
	if m == nil || m.db == nil {
		return kopia.FileFact{}, errors.New("read file fact: database is not open")
	}
	var fact kopia.FileFact
	fact.Ref = ref
	err := m.db.QueryRowContext(ctx, `SELECT sha256, size FROM file_facts WHERE namespace = ? AND ref = ?`, namespace, ref).Scan(&fact.SHA256, &fact.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return kopia.FileFact{}, ErrMetadataNotFound
	}
	if err != nil {
		return kopia.FileFact{}, fmt.Errorf("read file fact: %w", err)
	}
	return fact, nil
}

func (m *Metadata) publication(ctx context.Context, id string) (publicationRecord, error) {
	if m == nil || m.db == nil {
		return publicationRecord{}, errors.New("read publication: database is not open")
	}
	result, err := scanPublication(m.db.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return publicationRecord{}, ErrOperationNotFound
	}
	if err != nil {
		return publicationRecord{}, fmt.Errorf("read publication: %w", err)
	}
	return result, nil
}

func (m *Metadata) acceptPublication(ctx context.Context, id string, now time.Time) (publicationRecord, error) {
	if m == nil || m.db == nil {
		return publicationRecord{}, errors.New("accept publication: database is not open")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("begin publication acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	operation, err := scanPublication(tx.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return publicationRecord{}, ErrOperationNotFound
	}
	if err != nil {
		return publicationRecord{}, fmt.Errorf("read publication for acceptance: %w", err)
	}
	if operation.State != OperationPending {
		return operation, nil
	}
	if err := validatePublicationParents(operation.Input); err != nil {
		return publicationRecord{}, fmt.Errorf("accept publication: %w", err)
	}
	if !operation.ExpiresAt.After(now) {
		operation.State = OperationExpired
		if err := updatePublicationOutcome(ctx, tx, operation); err != nil {
			return publicationRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return publicationRecord{}, fmt.Errorf("commit expired publication: %w", err)
		}
		return operation, nil
	}

	actualTip, err := headTx(ctx, tx, operation.Input.Branch)
	if err != nil {
		return publicationRecord{}, err
	}
	if actualTip != operation.Input.Base {
		operation.State = OperationStale
		operation.ObservedTip = actualTip
		if err := updatePublicationOutcome(ctx, tx, operation); err != nil {
			return publicationRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return publicationRecord{}, fmt.Errorf("commit stale publication: %w", err)
		}
		return operation, nil
	}

	var parents []string
	if operation.Input.Base != "" {
		if _, err := loadCommitTx(ctx, tx, operation.Input.Base); err != nil {
			return publicationRecord{}, err
		}
		parents = []string{operation.Input.Base}
	}
	if operation.Input.MergeParent != "" {
		if _, err := loadCommitTx(ctx, tx, operation.Input.MergeParent); err != nil {
			return publicationRecord{}, err
		}
		parents = append(parents, operation.Input.MergeParent)
	}
	commit := Commit{
		ID: operation.CommitID, Parents: parents, Tree: operation.Input.Tree,
		Author: operation.Input.Author, Message: operation.Input.Message,
		CreatedAt: operation.CreatedAt,
	}
	if err := putCommitTx(ctx, tx, commit); err != nil {
		return publicationRecord{}, err
	}
	protectUntil := now.Add(operation.ExpiresAt.Sub(operation.CreatedAt))
	if !protectUntil.After(now) {
		protectUntil = now.Add(24 * time.Hour)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE payload_retention SET protect_until = ? WHERE commit_id = ? AND expired_at = ''`,
		formatMetadataTime(protectUntil), commit.ID)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("set accepted payload grace: %w", err)
	}
	retentionUpdated, err := result.RowsAffected()
	if err != nil {
		return publicationRecord{}, fmt.Errorf("check accepted payload grace: %w", err)
	}
	if retentionUpdated != 1 {
		return publicationRecord{}, fmt.Errorf("accepted commit %q has no retained payload row", commit.ID)
	}
	result, err = tx.ExecContext(ctx,
		`INSERT INTO branches (name, tip) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET tip = excluded.tip WHERE branches.tip = ?`,
		operation.Input.Branch, operation.CommitID, operation.Input.Base)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("advance publication branch: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return publicationRecord{}, fmt.Errorf("check publication branch update: %w", err)
	}
	if updated == 0 {
		return publicationRecord{}, fmt.Errorf("advance branch %q from expected tip %q affected no rows", operation.Input.Branch, operation.Input.Base)
	}

	operation.State = OperationAccepted
	operation.ObservedTip = ""
	if err := updatePublicationOutcome(ctx, tx, operation); err != nil {
		return publicationRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return publicationRecord{}, fmt.Errorf("commit publication acceptance: %w", err)
	}
	return operation, nil
}

func validatePublicationParents(input publicationInput) error {
	if input.MergeParent != "" && input.Base == "" {
		return errors.New("merge parent requires a base parent")
	}
	if input.MergeParent != "" && input.MergeParent == input.Base {
		return errors.New("merge parents must be distinct")
	}
	return nil
}

func (m *Metadata) stopPublication(ctx context.Context, id string, now time.Time, expireOnly bool) (publicationRecord, error) {
	if m == nil || m.db == nil {
		return publicationRecord{}, errors.New("stop publication: database is not open")
	}
	if err := validateOperationID(id); err != nil {
		return publicationRecord{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return publicationRecord{}, fmt.Errorf("begin publication stop: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	operation, err := scanPublication(tx.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		if expireOnly {
			return publicationRecord{}, ErrOperationNotFound
		}
		operation = publicationRecord{
			ID: id, Fingerprint: abortedBeforeArrivalFingerprint,
			CreatedAt: now.UTC(), ExpiresAt: now.UTC(), State: OperationAborted,
			ReleaseDone: true,
		}
		sealedInput, marshalErr := json.Marshal(operation.Input)
		if marshalErr != nil {
			return publicationRecord{}, fmt.Errorf("encode abort tombstone: %w", marshalErr)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO publication_ops
				(id, fingerprint, sealed_input, capture_input, candidate_commit_id, created_at, expires_at, state, observed_tip, release_done)
			 VALUES (?, ?, ?, X'', '', ?, ?, ?, '', 1)`,
			operation.ID, operation.Fingerprint, sealedInput,
			formatMetadataTime(operation.CreatedAt), formatMetadataTime(operation.ExpiresAt), string(OperationAborted)); err != nil {
			return publicationRecord{}, fmt.Errorf("persist abort-before-arrival tombstone: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return publicationRecord{}, fmt.Errorf("commit abort-before-arrival tombstone: %w", err)
		}
		return operation, nil
	}
	if err != nil {
		return publicationRecord{}, fmt.Errorf("read publication to stop: %w", err)
	}
	if operation.State == OperationAccepted {
		return operation, ErrAlreadyAccepted
	}
	if operation.State != OperationPending && operation.State != OperationCapturing {
		return operation, nil
	}
	if !operation.ExpiresAt.After(now) {
		operation.State = OperationExpired
	} else if expireOnly {
		return operation, nil
	} else {
		operation.State = OperationAborted
	}
	if err := updatePublicationOutcome(ctx, tx, operation); err != nil {
		return publicationRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return publicationRecord{}, fmt.Errorf("commit publication stop: %w", err)
	}
	return operation, nil
}

func (m *Metadata) rejectAbortTombstone(ctx context.Context, id string) error {
	if err := validateOperationID(id); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	operation, err := m.publication(ctx, id)
	if errors.Is(err, ErrOperationNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if isAbortBeforeArrival(operation) {
		return ErrOperationAborted
	}
	return nil
}

func isAbortBeforeArrival(operation publicationRecord) bool {
	return operation.Fingerprint == abortedBeforeArrivalFingerprint &&
		operation.State == OperationAborted && operation.ReleaseDone &&
		operation.CommitID == "" && len(operation.CaptureInput) == 0
}

func (m *Metadata) markPublicationReleased(ctx context.Context, id string) error {
	if m == nil || m.db == nil {
		return errors.New("mark publication released: database is not open")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin release completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	operation, err := scanPublication(tx.QueryRowContext(ctx, publicationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOperationNotFound
	}
	if err != nil {
		return fmt.Errorf("read publication release state: %w", err)
	}
	if operation.State == OperationAccepted {
		return ErrAlreadyAccepted
	}
	if operation.State == OperationPending || operation.State == OperationCapturing {
		return errors.New("cannot release content for an active publication")
	}
	if operation.ReleaseDone {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publication_ops SET release_done = 1 WHERE id = ? AND state = ?`, id, string(operation.State)); err != nil {
		return fmt.Errorf("mark publication released: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit release completion: %w", err)
	}
	return nil
}

func (m *Metadata) recoverablePublications(ctx context.Context, afterID string, limit int) ([]publicationRecord, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("list recoverable publications: database is not open")
	}
	if limit <= 0 {
		return nil, errors.New("list recoverable publications: limit must be positive")
	}
	if limit > maxRecoverablePublicationBatch {
		limit = maxRecoverablePublicationBatch
	}
	rows, err := m.db.QueryContext(ctx, publicationSelect+`
	 WHERE id > ? AND (state IN ('capturing', 'pending') OR (state IN ('stale', 'aborted', 'expired') AND release_done = 0))
	 ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("query recoverable publications: %w", err)
	}
	defer rows.Close()
	var result []publicationRecord
	for rows.Next() {
		operation, err := scanPublication(rows)
		if err != nil {
			return nil, fmt.Errorf("scan recoverable publication: %w", err)
		}
		result = append(result, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable publications: %w", err)
	}
	return result, nil
}

func (m *Metadata) head(ctx context.Context, branch string) (string, error) {
	if m == nil || m.db == nil {
		return "", errors.New("read branch head: database is not open")
	}
	var tip string
	err := m.db.QueryRowContext(ctx, `SELECT tip FROM branches WHERE name = ?`, branch).Scan(&tip)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read branch head: %w", err)
	}
	return tip, nil
}

func (m *Metadata) commit(ctx context.Context, id string) (Commit, error) {
	if m == nil || m.db == nil {
		return Commit{}, errors.New("read commit: database is not open")
	}
	return loadCommit(ctx, m.db, id)
}

const publicationSelect = `SELECT id, fingerprint, sealed_input, capture_input, candidate_commit_id, created_at, expires_at, state, observed_tip, release_done FROM publication_ops`

type rowScanner interface {
	Scan(...any) error
}

func scanPublication(row rowScanner) (publicationRecord, error) {
	var result publicationRecord
	var input []byte
	var createdAt, expiresAt, state string
	var releaseDone int
	if err := row.Scan(&result.ID, &result.Fingerprint, &input, &result.CaptureInput, &result.CommitID, &createdAt, &expiresAt, &state, &result.ObservedTip, &releaseDone); err != nil {
		return publicationRecord{}, err
	}
	if len(result.CaptureInput) == 0 {
		result.CaptureInput = nil
	}
	if err := json.Unmarshal(input, &result.Input); err != nil {
		return publicationRecord{}, fmt.Errorf("decode sealed publication input: %w", err)
	}
	var errFromTime error
	result.CreatedAt, errFromTime = parseMetadataTime(createdAt)
	if errFromTime != nil {
		return publicationRecord{}, fmt.Errorf("decode publication creation time: %w", errFromTime)
	}
	result.ExpiresAt, errFromTime = parseMetadataTime(expiresAt)
	if errFromTime != nil {
		return publicationRecord{}, fmt.Errorf("decode publication expiration time: %w", errFromTime)
	}
	result.State = OperationState(state)
	result.ReleaseDone = releaseDone != 0
	return result, nil
}

func updatePublicationOutcome(ctx context.Context, tx *sql.Tx, operation publicationRecord) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE publication_ops SET state = ?, observed_tip = ? WHERE id = ?`,
		string(operation.State), operation.ObservedTip, operation.ID); err != nil {
		return fmt.Errorf("update publication outcome: %w", err)
	}
	return nil
}

func headTx(ctx context.Context, tx *sql.Tx, branch string) (string, error) {
	var tip string
	err := tx.QueryRowContext(ctx, `SELECT tip FROM branches WHERE name = ?`, branch).Scan(&tip)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read branch head in transaction: %w", err)
	}
	return tip, nil
}

func putCommitTx(ctx context.Context, tx *sql.Tx, commit Commit) error {
	data, err := json.Marshal(commit)
	if err != nil {
		return fmt.Errorf("encode commit: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO commits (id, data) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`, commit.ID, data)
	if err != nil {
		return fmt.Errorf("insert commit: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check commit insert: %w", err)
	}
	if inserted != 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO payload_retention (commit_id, expired_at, released, protect_until) VALUES (?, '', 0, '')`, commit.ID); err != nil {
			return fmt.Errorf("insert commit payload retention: %w", err)
		}
		return nil
	}
	var existing []byte
	if err := tx.QueryRowContext(ctx, `SELECT data FROM commits WHERE id = ?`, commit.ID).Scan(&existing); err != nil {
		return fmt.Errorf("read existing commit: %w", err)
	}
	if !bytes.Equal(existing, data) {
		return fmt.Errorf("commit ID %q is already bound to different commit data", commit.ID)
	}
	var retained int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM payload_retention WHERE commit_id = ?`, commit.ID).Scan(&retained); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("commit %q has no payload retention row", commit.ID)
	} else if err != nil {
		return fmt.Errorf("read commit payload retention: %w", err)
	}
	return nil
}

func loadCommit(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Commit, error) {
	var data []byte
	err := queryer.QueryRowContext(ctx, `SELECT data FROM commits WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Commit{}, ErrCommitNotFound
	}
	if err != nil {
		return Commit{}, fmt.Errorf("read commit: %w", err)
	}
	var result Commit
	if err := json.Unmarshal(data, &result); err != nil {
		return Commit{}, fmt.Errorf("decode commit: %w", err)
	}
	return result, nil
}

func loadCommitTx(ctx context.Context, tx *sql.Tx, id string) (Commit, error) {
	return loadCommit(ctx, tx, id)
}

func formatMetadataTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseMetadataTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
