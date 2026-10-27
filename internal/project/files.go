package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

const (
	MaxChanges        = 4096
	MaxFileSize int64 = 1 << 40
)

var ErrInvalidRequest = errors.New("invalid project request")

// ValidateChanges checks portable request facts without IO or modifying input.
// Clients use it before persisting an operation they will need to retry.
func ValidateChanges(request ChangeRequest) error {
	_, _, err := normalizeChanges(request)
	return err
}

// Apply seals exact sparse changes before reading any payload. It constructs a
// complete candidate on the server; unmentioned paths retain their base entries.
// File readers are requested in canonical path order and always closed.
func (s *Service) Apply(ctx context.Context, request ChangeRequest, openFile func(model.Path) (io.ReadCloser, error)) (Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Commit{}, ErrServiceClosed
	}
	if s.objects == nil {
		return Commit{}, errors.New("project content IO is not configured")
	}
	request, changes, err := normalizeChanges(request)
	if err != nil {
		return Commit{}, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Commit{}, err
	}
	digest := sha256.Sum256(append([]byte("lode-sparse-capture-v1\x00"), encoded...))
	now := s.now().UTC()
	record, err := s.metadata.prepareCapture(ctx, publicationRecord{
		ID: request.OperationID, Fingerprint: hex.EncodeToString(digest[:]), CaptureInput: encoded,
		Input:    publicationInput{Branch: request.Branch, Base: request.Base, Author: request.Author, Message: request.Message},
		CommitID: newID(), CreatedAt: now, ExpiresAt: now.Add(s.ttl), State: OperationCapturing,
	})
	if err != nil {
		return Commit{}, err
	}
	record, err = s.metadata.checkCapture(ctx, record.ID, s.now().UTC())
	if err != nil {
		return Commit{}, err
	}
	if record.State != OperationCapturing {
		return s.resume(ctx, record)
	}
	budget, err := captureBudget(request.Changes)
	if err != nil {
		return Commit{}, err
	}
	if err := s.admitNewWrite(ctx, budget); err != nil {
		return Commit{}, err
	}
	var base *kopia.Archive
	if record.Input.Base != "" {
		if err := s.requireRetained(ctx, record.Input.Base); err != nil {
			return Commit{}, err
		}
		commit, err := s.metadata.commit(ctx, record.Input.Base)
		if err != nil {
			return Commit{}, err
		}
		archive, err := s.archive(ctx, commit.Tree)
		if err != nil {
			return Commit{}, err
		}
		base = &archive
	}
	remaining := budget
	sizes := make(map[model.Path]uint64, len(changes))
	for _, change := range changes {
		if change.Kind == model.File {
			sizes[change.Path] = uint64(change.Size) * 2
		}
	}
	guardedOpen := func(path model.Path) (io.ReadCloser, error) {
		if err := s.admitNewWrite(ctx, remaining); err != nil {
			return nil, err
		}
		if openFile == nil {
			return nil, errors.New("file upload reader is missing")
		}
		reader, err := openFile(path)
		if err == nil {
			remaining -= sizes[path]
		}
		return reader, err
	}
	archive, facts, err := s.objects.Build(ctx, base, changes, guardedOpen)
	if err != nil {
		return Commit{}, fmt.Errorf("capture changes: %w", err)
	}
	if err := s.metadata.putFileFacts(ctx, s.namespace, facts); err != nil {
		return Commit{}, err
	}
	id, err := s.metadata.Import(ctx, s.namespace, archive.Format, archive.Root, archive.Directories)
	if err != nil {
		return Commit{}, err
	}
	record, err = s.metadata.finishCapture(ctx, record.ID, TreeRef{Format: archive.Format, ID: id})
	if err != nil {
		return Commit{}, err
	}
	return s.resume(ctx, record)
}

func normalizeChanges(request ChangeRequest) (ChangeRequest, []kopia.Change, error) {
	if err := validateRequestMetadata(request.OperationID, request.Branch, request.Base, request.Author, request.Message); err != nil {
		return ChangeRequest{}, nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if len(request.Changes) > MaxChanges {
		return ChangeRequest{}, nil, fmt.Errorf("%w: at most %d changes per operation", ErrInvalidRequest, MaxChanges)
	}
	request.Changes = append([]Change(nil), request.Changes...)
	sort.Slice(request.Changes, func(i, j int) bool { return request.Changes[i].Path < request.Changes[j].Path })
	changes := make([]kopia.Change, 0, len(request.Changes))
	for i, change := range request.Changes {
		path, err := model.ParsePath(change.Path)
		if err != nil || path.IsRoot() || len(change.Path) > 4096 || change.Path == ".lode" || strings.HasPrefix(change.Path, ".lode/") {
			return ChangeRequest{}, nil, fmt.Errorf("%w: invalid or reserved path %q", ErrInvalidRequest, change.Path)
		}
		if i > 0 && request.Changes[i-1].Path == change.Path {
			return ChangeRequest{}, nil, fmt.Errorf("%w: duplicate path %q", ErrInvalidRequest, change.Path)
		}
		switch change.Kind {
		case model.File:
			digest, err := hex.DecodeString(change.SHA256)
			if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != change.SHA256 || change.Size < 0 || change.Size > MaxFileSize {
				return ChangeRequest{}, nil, fmt.Errorf("%w: invalid file facts for %q", ErrInvalidRequest, change.Path)
			}
		case model.Directory, model.Absent:
			if change.SHA256 != "" || change.Size != 0 {
				return ChangeRequest{}, nil, fmt.Errorf("%w: non-file facts for %q", ErrInvalidRequest, change.Path)
			}
		default:
			return ChangeRequest{}, nil, fmt.Errorf("%w: unknown kind for %q", ErrInvalidRequest, change.Path)
		}
		changes = append(changes, kopia.Change{Path: path, Kind: change.Kind, SHA256: change.SHA256, Size: change.Size})
	}
	return request, changes, nil
}

// Selection reconstructs a detached logical selection from portable rules.
func Selection(rules []SelectionRule) (model.Selection, error) {
	if len(rules) > 4096 {
		return model.Selection{}, fmt.Errorf("%w: too many selection rules", ErrInvalidRequest)
	}
	ordered := append([]SelectionRule(nil), rules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var selection model.Selection
	for i, rule := range ordered {
		path, err := model.ParsePath(rule.Path)
		if err != nil || len(rule.Path) > 4096 || i > 0 && ordered[i-1].Path == rule.Path {
			return model.Selection{}, fmt.Errorf("%w: invalid selection rule %q", ErrInvalidRequest, rule.Path)
		}
		if rule.Included {
			selection = selection.Include(path)
		} else {
			selection = selection.Exclude(path)
		}
	}
	return selection, nil
}

// Entries returns only requested entry metadata, with a bounded response page.
// The empty commit ID denotes the initial empty project state.
func (s *Service) Entries(ctx context.Context, commitID string, rules []SelectionRule, after string, limit int) ([]Node, string, error) {
	if s.closed.Load() {
		return nil, "", ErrServiceClosed
	}
	if s.objects == nil {
		return nil, "", errors.New("project content IO is not configured")
	}
	selection, err := Selection(rules)
	if err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("%w: page limit must be 1–1000", ErrInvalidRequest)
	}
	if _, err := model.ParsePath(after); err != nil {
		return nil, "", fmt.Errorf("%w: invalid page cursor", ErrInvalidRequest)
	}
	if commitID == "" {
		return []Node{}, "", nil
	}
	commit, err := s.metadata.commit(ctx, commitID)
	if err != nil {
		return nil, "", err
	}
	root, err := s.metadata.Root(ctx, s.namespace, commit.Tree.Format, commit.Tree.ID)
	if err != nil {
		return nil, "", err
	}
	entries, next, err := s.objects.ListArchived(ctx, commit.Tree.Format, root, func(ctx context.Context, objectID string) ([]byte, error) {
		return s.metadata.Directory(ctx, s.namespace, commit.Tree.Format, objectID)
	}, selection, after, limit)
	if err != nil {
		return nil, "", err
	}
	refs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Kind == model.File {
			refs = append(refs, entry.Ref)
		}
	}
	facts, err := s.metadata.fileFacts(ctx, s.namespace, refs)
	if err != nil {
		return nil, "", err
	}
	nodes := make([]Node, 0, len(entries))
	for _, entry := range entries {
		node := Node{Path: entry.Path.String(), Kind: entry.Kind, Size: entry.Size}
		if entry.Kind == model.File {
			fact, found := facts[entry.Ref]
			if !found {
				return nil, "", fmt.Errorf("read file facts for %q: %w", entry.Path, ErrMetadataNotFound)
			}
			if fact.Size != entry.Size {
				return nil, "", fmt.Errorf("stored file size disagrees at %q", entry.Path)
			}
			node.SHA256 = fact.SHA256
		}
		nodes = append(nodes, node)
	}
	return nodes, next, nil
}

// OpenFile streams one requested file without returning engine credentials or
// repository-wide indexes. The caller must close the reader.
func (s *Service) OpenFile(ctx context.Context, commitID, path string) (io.ReadCloser, Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, Node{}, ErrServiceClosed
	}
	if s.objects == nil {
		return nil, Node{}, errors.New("project content IO is not configured")
	}
	logical, err := model.ParsePath(path)
	if err != nil || logical.IsRoot() {
		return nil, Node{}, fmt.Errorf("%w: invalid file path", ErrInvalidRequest)
	}
	if err := s.requireRetained(ctx, commitID); err != nil {
		return nil, Node{}, err
	}
	s.contentMu.RLock()
	opened := false
	defer func() {
		if !opened {
			s.contentMu.RUnlock()
		}
	}()
	commit, err := s.metadata.commit(ctx, commitID)
	if err != nil {
		return nil, Node{}, err
	}
	root, err := s.metadata.Root(ctx, s.namespace, commit.Tree.Format, commit.Tree.ID)
	if err != nil {
		return nil, Node{}, err
	}
	reader, entry, err := s.objects.OpenArchivedFile(ctx, commit.Tree.Format, root, func(ctx context.Context, objectID string) ([]byte, error) {
		return s.metadata.Directory(ctx, s.namespace, commit.Tree.Format, objectID)
	}, logical)
	if err != nil {
		return nil, Node{}, err
	}
	node, err := s.node(ctx, entry)
	if err != nil {
		_ = reader.Close()
		return nil, Node{}, err
	}
	s.readMu.Lock()
	s.readers[commitID]++
	s.readMu.Unlock()
	opened = true
	return s.trackReader(ctx, commitID, reader), node, nil
}

func (s *Service) node(ctx context.Context, entry kopia.Node) (Node, error) {
	node := Node{Path: entry.Path.String(), Kind: entry.Kind, Size: entry.Size}
	if entry.Kind == model.File {
		fact, err := s.metadata.fileFact(ctx, s.namespace, entry.Ref)
		if err != nil {
			return Node{}, fmt.Errorf("read file facts for %q: %w", entry.Path, err)
		}
		if fact.Size != entry.Size {
			return Node{}, fmt.Errorf("stored file size disagrees at %q", entry.Path)
		}
		node.SHA256 = fact.SHA256
	}
	return node, nil
}

// History follows first parents from an exact commit, returning at most limit
// records. Next is an exact commit ID and does not require a full graph fetch.
func (s *Service) History(ctx context.Context, start string, limit int) ([]Commit, string, error) {
	if s.closed.Load() {
		return nil, "", ErrServiceClosed
	}
	if limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("%w: history limit must be 1–1000", ErrInvalidRequest)
	}
	commits := make([]Commit, 0, limit)
	for len(commits) < limit && start != "" {
		commit, err := s.metadata.commit(ctx, start)
		if err != nil {
			return nil, "", err
		}
		commits = append(commits, commit)
		start = ""
		if len(commit.Parents) > 0 {
			start = commit.Parents[0]
		}
	}
	return commits, start, nil
}
