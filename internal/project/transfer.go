package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/transfer"
)

// MinDeltaFileSize avoids signature and patch setup for small files. Whole-file
// upload remains the fallback when either the target or its basis is smaller.
const MinDeltaFileSize int64 = 1 << 20

// FileSignature describes a transfer basis using logical metadata only. Data
// is nil when the path has no usable large file basis or the target is small.
// No native object or content ID crosses this service boundary.
type FileSignature struct {
	OperationID string
	Base        string
	Path        string
	Basis       Node
	Data        []byte
}

// PrepareChanges durably seals a sparse capture before a client requests any
// per-file signatures or begins streaming file bytes.
func (s *Service) PrepareChanges(ctx context.Context, request ChangeRequest) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Operation{}, ErrServiceClosed
	}
	if s.objects == nil {
		return Operation{}, errors.New("project content IO is not configured")
	}
	normalized, _, record, err := s.prepareChangesLocked(ctx, request)
	if err != nil {
		return Operation{}, err
	}
	if record.State != OperationCapturing {
		_, resumeErr := s.resume(ctx, record)
		settled, readErr := s.metadata.publication(ctx, record.ID)
		if readErr != nil {
			return Operation{}, errors.Join(resumeErr, readErr)
		}
		return settled.operation(), resumeErr
	}
	budget, err := captureBudget(normalized.Changes)
	if err != nil {
		return record.operation(), err
	}
	if err := s.admitNewWrite(ctx, budget); err != nil {
		return record.operation(), err
	}
	return record.operation(), nil
}

// Signature returns one bounded signature for a file in a prepared capture.
// It deliberately does not acquire Service.mu: an in-flight Apply owns that
// mutex while its multipart callback waits for this request.
func (s *Service) Signature(ctx context.Context, operationID, path string) (FileSignature, error) {
	if s.closed.Load() {
		return FileSignature{}, ErrServiceClosed
	}
	if err := validateOperationID(operationID); err != nil {
		return FileSignature{}, fmt.Errorf("%w: invalid transfer operation ID", ErrInvalidRequest)
	}
	logical, err := parseTransferPath(path)
	if err != nil {
		return FileSignature{}, err
	}
	if err := ctx.Err(); err != nil {
		return FileSignature{}, err
	}
	s.contentMu.RLock()
	defer s.contentMu.RUnlock()
	if s.closed.Load() {
		return FileSignature{}, ErrServiceClosed
	}
	if s.objects == nil {
		return FileSignature{}, errors.New("project content IO is not configured")
	}

	record, request, change, err := s.preparedFile(ctx, operationID, logical, s.now().UTC())
	if err != nil {
		return FileSignature{}, err
	}
	result := FileSignature{OperationID: record.ID, Base: record.Input.Base, Path: logical.String(), Basis: Node{Path: logical.String(), Kind: model.Absent}}
	if record.Input.Base == "" {
		return s.finishSignatureFallback(ctx, result, record, request, logical, change)
	}
	if err := s.requireRetained(ctx, record.Input.Base); err != nil {
		return FileSignature{}, err
	}
	commit, err := s.metadata.commit(ctx, record.Input.Base)
	if err != nil {
		return FileSignature{}, err
	}
	root, err := s.metadata.Root(ctx, s.namespace, commit.Tree.Format, commit.Tree.ID)
	if err != nil {
		return FileSignature{}, err
	}
	reader, basisEntry, openErr := s.objects.OpenArchivedFile(ctx, commit.Tree.Format, root, func(ctx context.Context, objectID string) ([]byte, error) {
		return s.metadata.Directory(ctx, s.namespace, commit.Tree.Format, objectID)
	}, logical)
	if openErr != nil {
		switch {
		case errors.Is(openErr, kopia.ErrPathNotFound):
			return s.finishSignatureFallback(ctx, result, record, request, logical, change)
		case errors.Is(openErr, kopia.ErrPathNotFile):
			result.Basis = Node{Path: logical.String(), Kind: model.Directory}
			return s.finishSignatureFallback(ctx, result, record, request, logical, change)
		default:
			return FileSignature{}, fmt.Errorf("open transfer basis %q: %w", logical, openErr)
		}
	}
	result.Basis, err = s.node(ctx, basisEntry)
	if err != nil {
		return FileSignature{}, errors.Join(err, reader.Close())
	}
	if change.Size < MinDeltaFileSize || result.Basis.Size < MinDeltaFileSize {
		if err := reader.Close(); err != nil {
			return FileSignature{}, err
		}
		return s.finishSignatureFallback(ctx, result, record, request, logical, change)
	}
	if _, err := transfer.Parameters(result.Basis.Size, change.Size); errors.Is(err, transfer.ErrInvalidSize) || errors.Is(err, transfer.ErrLimitExceeded) {
		if closeErr := reader.Close(); closeErr != nil {
			return FileSignature{}, closeErr
		}
		return s.finishSignatureFallback(ctx, result, record, request, logical, change)
	} else if err != nil {
		_ = reader.Close()
		return FileSignature{}, err
	}

	if err := ctx.Err(); err != nil {
		return FileSignature{}, errors.Join(err, reader.Close())
	}
	digest := sha256.New()
	hashedReader := &transferHashReader{reader: reader, hash: digest}
	signature, signErr := transfer.Signature(ctx, hashedReader, result.Basis.Size, change.Size)
	closeErr := reader.Close()
	if signErr != nil || closeErr != nil {
		return FileSignature{}, errors.Join(signErr, closeErr)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != result.Basis.SHA256 {
		return FileSignature{}, fmt.Errorf("transfer basis %q SHA256 mismatch: got %s, metadata has %s", logical, got, result.Basis.SHA256)
	}
	if err := s.recheckPreparedFile(ctx, record, request, logical, change, s.now().UTC()); err != nil {
		return FileSignature{}, err
	}
	result.Data = signature
	return result, nil
}

func (s *Service) finishSignatureFallback(ctx context.Context, result FileSignature, record publicationRecord, request ChangeRequest, path model.Path, change Change) (FileSignature, error) {
	if err := s.recheckPreparedFile(ctx, record, request, path, change, s.now().UTC()); err != nil {
		return FileSignature{}, err
	}
	return result, nil
}

func parseTransferPath(value string) (model.Path, error) {
	path, err := model.ParsePath(value)
	if err != nil || path.IsRoot() || len(value) > 4096 || value == ".lode" || len(value) > len(".lode/") && value[:len(".lode/")] == ".lode/" {
		return model.Path{}, fmt.Errorf("%w: invalid transfer path %q", ErrInvalidRequest, value)
	}
	return path, nil
}

func (s *Service) preparedFile(ctx context.Context, operationID string, path model.Path, now time.Time) (publicationRecord, ChangeRequest, Change, error) {
	record, err := s.metadata.publication(ctx, operationID)
	if err != nil {
		return publicationRecord{}, ChangeRequest{}, Change{}, err
	}
	if err := checkSignatureOperation(record, now); err != nil {
		return publicationRecord{}, ChangeRequest{}, Change{}, err
	}
	request, change, err := decodePreparedFile(record, path)
	if err != nil {
		return publicationRecord{}, ChangeRequest{}, Change{}, err
	}
	if record.Input.MergeParent != "" {
		return publicationRecord{}, ChangeRequest{}, Change{}, fmt.Errorf("%w: merge captures do not accept file signatures", ErrInvalidRequest)
	}
	if record.Input.Base != "" {
		if err := s.requireRetained(ctx, record.Input.Base); err != nil {
			return publicationRecord{}, ChangeRequest{}, Change{}, err
		}
	}
	tip, err := s.metadata.head(ctx, record.Input.Branch)
	if err != nil {
		return publicationRecord{}, ChangeRequest{}, Change{}, err
	}
	if tip != record.Input.Base {
		return publicationRecord{}, ChangeRequest{}, Change{}, ErrStaleHead
	}
	return record, request, change, nil
}

func (s *Service) recheckPreparedFile(ctx context.Context, original publicationRecord, request ChangeRequest, path model.Path, change Change, now time.Time) error {
	current, err := s.metadata.publication(ctx, original.ID)
	if err != nil {
		return err
	}
	if err := checkSignatureOperation(current, now); err != nil {
		return err
	}
	currentRequest, currentChange, err := decodePreparedFile(current, path)
	if err != nil {
		return err
	}
	if current.Fingerprint != original.Fingerprint || current.Input.Base != original.Input.Base || currentChange != change || currentRequest.OperationID != request.OperationID {
		return ErrOperationConflict
	}
	if current.Input.Base != "" {
		if err := s.requireRetained(ctx, current.Input.Base); err != nil {
			return err
		}
	}
	tip, err := s.metadata.head(ctx, current.Input.Branch)
	if err != nil {
		return err
	}
	if tip != current.Input.Base {
		return ErrStaleHead
	}
	return nil
}

func checkSignatureOperation(record publicationRecord, now time.Time) error {
	switch record.State {
	case OperationCapturing:
		if !record.ExpiresAt.After(now) {
			return ErrOperationExpired
		}
		return nil
	case OperationAccepted:
		return ErrAlreadyAccepted
	case OperationStale:
		return ErrStaleHead
	case OperationAborted:
		return ErrOperationAborted
	case OperationExpired:
		return ErrOperationExpired
	default:
		return ErrOperationConflict
	}
}

func decodePreparedFile(record publicationRecord, path model.Path) (ChangeRequest, Change, error) {
	var request ChangeRequest
	decoder := json.NewDecoder(bytes.NewReader(record.CaptureInput))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return ChangeRequest{}, Change{}, fmt.Errorf("decode prepared transfer input: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ChangeRequest{}, Change{}, errors.New("prepared transfer input has trailing data")
	}
	normalized, _, err := normalizeChanges(request)
	if err != nil {
		return ChangeRequest{}, Change{}, fmt.Errorf("validate prepared transfer input: %w", err)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return ChangeRequest{}, Change{}, err
	}
	digest := sha256.Sum256(append([]byte("lode-sparse-capture-v1\x00"), encoded...))
	if !bytes.Equal(encoded, record.CaptureInput) || hex.EncodeToString(digest[:]) != record.Fingerprint ||
		request.OperationID != record.ID || request.Branch != record.Input.Branch || request.Base != record.Input.Base ||
		request.Author != record.Input.Author || request.Message != record.Input.Message {
		return ChangeRequest{}, Change{}, errors.New("prepared transfer input disagrees with sealed operation")
	}
	for _, change := range normalized.Changes {
		if change.Path == path.String() {
			if change.Kind != model.File {
				return ChangeRequest{}, Change{}, fmt.Errorf("%w: transfer signature requires a file change at %q", ErrInvalidRequest, path)
			}
			return normalized, change, nil
		}
	}
	return ChangeRequest{}, Change{}, fmt.Errorf("%w: %q is not a changed file in this operation", ErrInvalidRequest, path)
}

type transferHashReader struct {
	reader io.Reader
	hash   hash.Hash
}

func (r *transferHashReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
	}
	return n, err
}
