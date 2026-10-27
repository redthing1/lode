package project

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gofrs/flock"

	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

// Service is the sole publication coordinator for a project database.
// Its ownership lock spans native and SQLite operations. Local owns that lock
// across resource opening and closing; standalone services own their own lock.
type Service struct {
	metadata    *Metadata
	roots       ContentRoots
	objects     *kopia.Objects
	maintenance ContentMaintenance
	id          string
	namespace   string
	ttl         time.Duration
	now         func() time.Time
	owner       *flock.Flock
	mu          sync.Mutex
	contentMu   sync.RWMutex
	readMu      sync.Mutex
	readers     map[string]int
	readTTL     time.Duration
	closed      atomic.Bool
}

func NewService(ctx context.Context, metadata *Metadata, roots ContentRoots, options Options) (*Service, error) {
	return newService(ctx, metadata, roots, options, nil)
}

// newService borrows a Local owner's lock when provided. Closing the service
// stops operations, while Local keeps ownership until its storage is closed.
func newService(ctx context.Context, metadata *Metadata, roots ContentRoots, options Options, borrowed *flock.Flock) (*Service, error) {
	if metadata == nil || metadata.db == nil || metadata.path == "" || roots == nil || roots.Namespace() == "" {
		return nil, errors.New("publication service needs an open database and content repository")
	}
	if options.Objects != nil && options.Objects.Namespace() != roots.Namespace() {
		return nil, ErrRepositoryMismatch
	}
	if options.Maintenance != nil && options.Maintenance.Namespace() != roots.Namespace() {
		return nil, ErrRepositoryMismatch
	}
	if options.ReadTTL < 0 || options.ReadTTL > time.Hour {
		return nil, errors.New("read hold lifetime must be positive and at most one hour")
	}
	if options.ReadTTL == 0 {
		options.ReadTTL = 15 * time.Minute
	}
	if options.OperationTTL < 0 {
		return nil, errors.New("operation lifetime must be positive")
	}
	if options.OperationTTL == 0 {
		options.OperationTTL = 24 * time.Hour
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	owner := borrowed
	if owner == nil {
		var err error
		owner, err = acquireProjectOwner(metadata.path)
		if err != nil {
			return nil, err
		}
	} else if !owner.Locked() || owner.Path() != metadata.path+".service-lock" {
		return nil, errors.New("project service requires its metadata owner's lock")
	}
	releaseOnError := func() {
		if borrowed == nil {
			_ = owner.Close()
		}
	}
	// Another version could migrate the database after OpenMetadata returned
	// and before this service acquired ownership. Recheck under the owner lock.
	var schemaVersion int
	if err := metadata.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		releaseOnError()
		return nil, fmt.Errorf("read service schema version: %w", err)
	}
	if schemaVersion != metadataSchemaVersion {
		releaseOnError()
		return nil, fmt.Errorf("project schema changed before service ownership: version %d", schemaVersion)
	}
	id, err := metadata.bindProject(ctx, roots.Namespace(), newID())
	if err != nil {
		releaseOnError()
		return nil, err
	}
	serviceOwner := owner
	if borrowed != nil {
		serviceOwner = nil
	}
	return &Service{
		metadata: metadata, roots: roots, objects: options.Objects, id: id, namespace: roots.Namespace(),
		ttl: options.OperationTTL, now: options.Now, owner: serviceOwner,
		maintenance: options.Maintenance, readTTL: options.ReadTTL, readers: make(map[string]int),
	}, nil
}

func acquireProjectOwner(metadataPath string) (*flock.Flock, error) {
	path := metadataPath + ".service-lock"
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("project owner lock must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect project owner lock: %w", err)
	}
	owner := flock.New(path)
	locked, err := owner.TryLock()
	if err != nil {
		_ = owner.Close()
		return nil, fmt.Errorf("lock project service: %w", err)
	}
	if !locked {
		_ = owner.Close()
		return nil, ErrProjectBusy
	}
	return owner, nil
}

// ID is the stable project identity stored in the metadata database.
func (s *Service) ID() string { return s.id }

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed.Store(true)
	s.contentMu.Lock()
	defer s.contentMu.Unlock()
	if s.owner != nil {
		return s.owner.Close()
	}
	return nil
}

// Publish seals one complete version and attempts to advance its branch.
// A failure after sealing can leave a pending operation; Resume or Recover
// continues those exact inputs. An accepted retry never republishes content.
func (s *Service) Publish(ctx context.Context, request PublishRequest) (Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Commit{}, ErrServiceClosed
	}
	if err := validatePublication(request); err != nil {
		return Commit{}, err
	}
	if err := s.metadata.rejectAbortTombstone(ctx, request.OperationID); err != nil {
		return Commit{}, err
	}

	input := publicationInput{
		Branch: request.Branch, Base: request.Base,
		Tree:   TreeRef{Format: request.Tree.Format, ID: rootDescriptorID(request.Tree.Root)},
		Author: request.Author, Message: request.Message,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return Commit{}, fmt.Errorf("encode publication: %w", err)
	}
	digest := sha256.Sum256(append([]byte("lode-publication-v1\x00"), encoded...))
	fingerprint := hex.EncodeToString(digest[:])
	existing, err := s.metadata.publication(ctx, request.OperationID)
	if err == nil {
		if existing.Fingerprint != fingerprint {
			return Commit{}, ErrOperationConflict
		}
		return s.resume(ctx, existing)
	}
	if !errors.Is(err, ErrOperationNotFound) {
		return Commit{}, err
	}
	if err := s.requireRetained(ctx, request.Base); err != nil {
		return Commit{}, err
	}
	if err := s.admitNewWrite(ctx, 0); err != nil {
		return Commit{}, err
	}

	// Only verified server-native metadata can enter the immutable archive.
	// Protection revalidates availability after the durable intent is sealed.
	if err := s.roots.Validate(ctx, request.Tree); err != nil {
		return Commit{}, fmt.Errorf("validate publication tree: %w", err)
	}
	if _, err := s.metadata.Import(ctx, s.namespace, request.Tree.Format, request.Tree.Root, request.Tree.Directories); err != nil {
		return Commit{}, err
	}
	now := s.now().UTC()
	record, err := s.metadata.preparePublication(ctx, publicationRecord{
		ID: request.OperationID, Fingerprint: fingerprint, Input: input,
		CommitID: newID(), CreatedAt: now, ExpiresAt: now.Add(s.ttl),
		State: OperationPending,
	})
	if err != nil {
		return Commit{}, err
	}
	return s.resume(ctx, record)
}

// Resume continues a sealed operation without recapturing or changing its tree.
func (s *Service) Resume(ctx context.Context, operationID string) (Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Commit{}, ErrServiceClosed
	}
	record, err := s.metadata.publication(ctx, operationID)
	if err != nil {
		return Commit{}, err
	}
	return s.resume(ctx, record)
}

func (s *Service) resume(ctx context.Context, record publicationRecord) (Commit, error) {
	record, err := s.resumeRecord(ctx, record)
	if err != nil {
		return Commit{}, err
	}
	switch record.State {
	case OperationCapturing:
		return Commit{}, ErrUploadRequired
	case OperationAccepted:
		return s.metadata.commit(ctx, record.CommitID)
	case OperationStale:
		return Commit{}, fmt.Errorf("%w: expected %q, found %q", ErrStaleHead, record.Input.Base, record.ObservedTip)
	case OperationAborted:
		return Commit{}, ErrOperationAborted
	case OperationExpired:
		return Commit{}, ErrOperationExpired
	default:
		return Commit{}, fmt.Errorf("unexpected publication state %q", record.State)
	}
}

func (s *Service) resumeRecord(ctx context.Context, record publicationRecord) (publicationRecord, error) {
	if record.State == OperationAccepted {
		return record, nil
	}
	if (record.State == OperationPending || record.State == OperationCapturing) && !s.now().Before(record.ExpiresAt) {
		var err error
		record, err = s.metadata.stopPublication(ctx, record.ID, s.now().UTC(), true)
		if err != nil {
			return record, err
		}
	}
	if record.State == OperationCapturing {
		if record.Input.MergeParent == "" {
			return record, nil
		}
		var err error
		record, err = s.resumeMergeCapture(ctx, record)
		if err != nil {
			return record, err
		}
	}
	if record.State == OperationPending {
		archive, err := s.archive(ctx, record.Input.Tree)
		if err != nil {
			return record, err
		}
		if err := s.roots.Protect(ctx, s.protectionKey(record.ID), archive); err != nil {
			return record, fmt.Errorf("protect publication %q: %w", record.ID, err)
		}
		// Native protection is flushed before this short acceptance transaction.
		// Never release it on a database error: acceptance might have committed.
		record, err = s.metadata.acceptPublication(ctx, record.ID, s.now().UTC())
		if err != nil {
			return record, err
		}
	}
	if record.State != OperationAccepted {
		return s.releaseTerminal(ctx, record)
	}
	return record, nil
}

func (s *Service) releaseTerminal(ctx context.Context, record publicationRecord) (publicationRecord, error) {
	switch record.State {
	case OperationStale, OperationAborted, OperationExpired:
	default:
		return record, fmt.Errorf("cannot release publication in state %q", record.State)
	}
	if record.ReleaseDone {
		return record, nil
	}
	if err := s.roots.Release(ctx, s.protectionKey(record.ID)); err != nil {
		return record, fmt.Errorf("release publication %q: %w", record.ID, err)
	}
	if err := s.metadata.markPublicationReleased(ctx, record.ID); err != nil {
		return record, err
	}
	record.ReleaseDone = true
	return record, nil
}

// Abort cancels unaccepted work and releases its native protection. An accepted
// operation cannot be aborted, including after its original lease has elapsed.
func (s *Service) Abort(ctx context.Context, operationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	record, err := s.metadata.stopPublication(ctx, operationID, s.now().UTC(), false)
	if err != nil {
		return err
	}
	_, err = s.releaseTerminal(ctx, record)
	return err
}

// Recover resumes pending operations and retries terminal protection releases.
// Expired operations are cancelled. An IO failure remains explicit and does not
// prevent other operations in the bounded scan from being reconciled.
func (s *Service) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	return s.recoverLocked(ctx)
}

func (s *Service) recoverLocked(ctx context.Context) error {
	var failures []error
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		records, err := s.metadata.recoverablePublications(ctx, cursor, 64)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if len(records) == 0 {
			break
		}
		for _, record := range records {
			cursor = record.ID
			if _, err := s.resumeRecord(ctx, record); err != nil {
				failures = append(failures, fmt.Errorf("recover publication %q: %w", record.ID, err))
			}
		}
	}
	failures = append(failures, s.pruneReadLeases(ctx), s.releaseExpiredPayloads(ctx))
	return errors.Join(failures...)
}

func (s *Service) Operation(ctx context.Context, id string) (Operation, error) {
	if s.closed.Load() {
		return Operation{}, ErrServiceClosed
	}
	record, err := s.metadata.publication(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	return record.operation(), nil
}

// Head returns an empty ID for an absent branch.
func (s *Service) Head(ctx context.Context, branch string) (string, error) {
	if s.closed.Load() {
		return "", ErrServiceClosed
	}
	if err := validateBranchName(branch); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return s.metadata.head(ctx, branch)
}

func (s *Service) Commit(ctx context.Context, id string) (Commit, error) {
	if s.closed.Load() {
		return Commit{}, ErrServiceClosed
	}
	return s.metadata.commit(ctx, id)
}

func (s *Service) archive(ctx context.Context, tree TreeRef) (kopia.Archive, error) {
	root, err := s.metadata.Root(ctx, s.namespace, tree.Format, tree.ID)
	if err != nil {
		return kopia.Archive{}, err
	}
	return kopia.LoadArchive(ctx, tree.Format, root, func(ctx context.Context, id string) ([]byte, error) {
		return s.metadata.Directory(ctx, s.namespace, tree.Format, id)
	})
}

func (s *Service) protectionKey(operationID string) string {
	return s.id + "/" + operationID
}

func newID() string {
	var value [16]byte
	// crypto/rand.Read either fills the buffer or terminates the process.
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

func validatePublication(request PublishRequest) error {
	if err := validateRequestMetadata(request.OperationID, request.Branch, request.Base, request.Author, request.Message); err != nil {
		return err
	}
	if request.Tree.Format == "" || len(request.Tree.Root) == 0 {
		return errors.New("publication needs a complete stored tree")
	}
	return nil
}

func validateRequestMetadata(operationID, branchName, base, author, message string) error {
	if err := validateOperationID(operationID); err != nil {
		return err
	}
	if err := validateBranchName(branchName); err != nil {
		return err
	}
	if base != "" {
		decoded, err := hex.DecodeString(base)
		if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != base {
			return errors.New("invalid base commit ID")
		}
	}
	if len(author) > 1024 || len(message) > 64<<10 ||
		!utf8.ValidString(author) || !utf8.ValidString(message) ||
		strings.ContainsRune(author, 0) || strings.ContainsRune(message, 0) {
		return errors.New("invalid publication author or message")
	}
	return nil
}

func validateBranchName(branchName string) error {
	branch, err := model.ParsePath(branchName)
	if err != nil || branch.IsRoot() || len(branchName) > 255 ||
		strings.IndexFunc(branchName, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return errors.New("invalid branch name")
	}
	return nil
}

func validateOperationID(operationID string) error {
	if len(operationID) == 0 || len(operationID) > 128 ||
		strings.IndexFunc(operationID, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) >= 0 {
		return errors.New("operation ID must contain 1–128 ASCII letters, digits, hyphens or underscores")
	}
	return nil
}
