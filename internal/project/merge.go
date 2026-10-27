package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

var (
	ErrMergeConflict      = errors.New("merge has unresolved whole-entry conflicts")
	ErrUnrelatedHistory   = errors.New("commits have no common ancestor")
	ErrAmbiguousMergeBase = errors.New("multiple best common ancestors require an explicit supported merge strategy")
	ErrHistoryLimit       = errors.New("merge ancestry exceeds the supported limit")
)

const maxMergeAncestors = 100000

// Choice selects a complete path/subtree from one side. More specific paths
// override ancestor choices. The root path selects the entire tree explicitly.
type Choice struct {
	Path string
	Side model.Side
}

type MergeSpec struct {
	Branch  string
	Base    string
	Other   string
	Choices []Choice
}

type MergeRequest struct {
	OperationID string
	MergeSpec
	Author  string
	Message string
}

type MergePreview struct {
	CommonBase     string
	Conflicts      []string
	Next           string
	TotalConflicts int
}

// Choices validates portable choice rules without IO. Path membership and
// structural validity are checked by the model against the actual trees.
func Choices(rules []Choice) (map[model.Path]model.Side, error) {
	if len(rules) > 4096 {
		return nil, fmt.Errorf("%w: too many choices", ErrInvalidRequest)
	}
	result := make(map[model.Path]model.Side, len(rules))
	for _, rule := range rules {
		p, err := model.ParsePath(rule.Path)
		if err != nil || len(rule.Path) > 4096 || rule.Path == ".lode" || strings.HasPrefix(rule.Path, ".lode/") {
			return nil, fmt.Errorf("%w: invalid choice path %q", ErrInvalidRequest, rule.Path)
		}
		if rule.Side != model.LocalSide && rule.Side != model.TargetSide {
			return nil, fmt.Errorf("%w: invalid choice side", ErrInvalidRequest)
		}
		if _, exists := result[p]; exists {
			return nil, fmt.Errorf("%w: duplicate choice at %q", ErrInvalidRequest, p)
		}
		result[p] = rule.Side
	}
	return result, nil
}

func normalizeMergeSpec(spec MergeSpec) (MergeSpec, error) {
	if err := validateRequestMetadata("merge", spec.Branch, spec.Base, "", ""); err != nil {
		return MergeSpec{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := validateCommitID(spec.Base); err != nil {
		return MergeSpec{}, fmt.Errorf("%w: missing or invalid destination tip", ErrInvalidRequest)
	}
	if err := validateCommitID(spec.Other); err != nil || spec.Other == spec.Base {
		return MergeSpec{}, fmt.Errorf("%w: merge needs two different exact tips", ErrInvalidRequest)
	}
	if _, err := Choices(spec.Choices); err != nil {
		return MergeSpec{}, err
	}
	spec.Choices = append([]Choice(nil), spec.Choices...)
	sort.Slice(spec.Choices, func(i, j int) bool { return spec.Choices[i].Path < spec.Choices[j].Path })
	return spec, nil
}

func normalizeMerge(request MergeRequest) (MergeRequest, []byte, string, error) {
	if err := validateRequestMetadata(request.OperationID, request.Branch, request.Base, request.Author, request.Message); err != nil {
		return MergeRequest{}, nil, "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	spec, err := normalizeMergeSpec(request.MergeSpec)
	if err != nil {
		return MergeRequest{}, nil, "", err
	}
	request.MergeSpec = spec
	encoded, err := json.Marshal(request)
	if err != nil {
		return MergeRequest{}, nil, "", err
	}
	digest := sha256.Sum256(append([]byte("lode-merge-v1\x00"), encoded...))
	return request, encoded, hex.EncodeToString(digest[:]), nil
}

func ValidateMergeRequest(request MergeRequest) error {
	_, _, _, err := normalizeMerge(request)
	return err
}

// PreviewMerge compares complete server metadata and returns a bounded page of
// unresolved paths, including paths outside any client's materialized selection.
// It neither captures payload nor reserves an operation ID.
func (s *Service) PreviewMerge(ctx context.Context, spec MergeSpec, after string, limit int) (MergePreview, error) {
	if s.closed.Load() {
		return MergePreview{}, ErrServiceClosed
	}
	if limit < 1 || limit > 1000 {
		return MergePreview{}, fmt.Errorf("%w: preview limit must be 1–1000", ErrInvalidRequest)
	}
	if _, err := model.ParsePath(after); err != nil {
		return MergePreview{}, fmt.Errorf("%w: invalid preview cursor", ErrInvalidRequest)
	}
	spec, err := normalizeMergeSpec(spec)
	if err != nil {
		return MergePreview{}, err
	}
	if err = s.checkMergeHead(ctx, spec); err != nil {
		return MergePreview{}, err
	}
	calculation, err := s.calculateMerge(ctx, spec)
	if err != nil {
		return MergePreview{}, err
	}
	preview := MergePreview{CommonBase: calculation.base, Conflicts: []string{}, TotalConflicts: len(calculation.conflicts)}
	start := sort.Search(len(calculation.conflicts), func(i int) bool { return calculation.conflicts[i].String() > after })
	end := min(start+limit, len(calculation.conflicts))
	for _, p := range calculation.conflicts[start:end] {
		preview.Conflicts = append(preview.Conflicts, p.String())
	}
	if end < len(calculation.conflicts) && end > start {
		preview.Next = calculation.conflicts[end-1].String()
	}
	return preview, nil
}

// Merge records one immutable two-parent commit. Whole-file references are
// composed on the server; no selected or omitted payload needs to be uploaded.
func (s *Service) Merge(ctx context.Context, request MergeRequest) (Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Commit{}, ErrServiceClosed
	}
	request, encoded, fingerprint, err := normalizeMerge(request)
	if err != nil {
		return Commit{}, err
	}
	if err = s.metadata.rejectAbortTombstone(ctx, request.OperationID); err != nil {
		return Commit{}, err
	}
	existing, err := s.metadata.publication(ctx, request.OperationID)
	if err == nil {
		if existing.Fingerprint != fingerprint || !bytes.Equal(existing.CaptureInput, encoded) {
			return Commit{}, ErrOperationConflict
		}
		return s.resume(ctx, existing)
	}
	if !errors.Is(err, ErrOperationNotFound) {
		return Commit{}, err
	}
	if err = s.checkMergeHead(ctx, request.MergeSpec); err != nil {
		return Commit{}, err
	}
	calculation, err := s.calculateMerge(ctx, request.MergeSpec)
	if err != nil {
		return Commit{}, err
	}
	// No durable intent is needed for a comparison that has unresolved choices.
	if len(calculation.conflicts) != 0 {
		return Commit{}, ErrMergeConflict
	}
	if err = s.validateMergeSources(ctx, calculation); err != nil {
		return Commit{}, err
	}
	if err = s.admitNewWrite(ctx, 0); err != nil {
		return Commit{}, err
	}
	now := s.now().UTC()
	record, err := s.metadata.prepareCapture(ctx, publicationRecord{
		ID: request.OperationID, Fingerprint: fingerprint, CaptureInput: encoded,
		Input:    publicationInput{Branch: request.Branch, Base: request.Base, MergeParent: request.Other, Author: request.Author, Message: request.Message},
		CommitID: newID(), CreatedAt: now, ExpiresAt: now.Add(s.ttl), State: OperationCapturing,
	})
	if err != nil {
		return Commit{}, err
	}
	record, err = s.finishMerge(ctx, record, calculation)
	if err != nil {
		return Commit{}, err
	}
	return s.resume(ctx, record)
}

type mergeCalculation struct {
	base      string
	local     kopia.Archive
	target    kopia.Archive
	entries   map[model.Path]model.Entry
	conflicts []model.Path
}

func (s *Service) checkMergeHead(ctx context.Context, spec MergeSpec) error {
	head, err := s.metadata.head(ctx, spec.Branch)
	if err != nil {
		return err
	}
	if head != spec.Base {
		return ErrStaleHead
	}
	return nil
}

func (s *Service) calculateMerge(ctx context.Context, spec MergeSpec) (mergeCalculation, error) {
	if err := s.requireRetained(ctx, spec.Base); err != nil {
		return mergeCalculation{}, err
	}
	if err := s.requireRetained(ctx, spec.Other); err != nil {
		return mergeCalculation{}, err
	}
	if s.objects == nil {
		return mergeCalculation{}, errors.New("project content IO is not configured")
	}
	graph := make(map[string][]string)
	commits := make(map[string]Commit)
	queue := []string{spec.Base, spec.Other}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return mergeCalculation{}, err
		}
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, seen := graph[id]; seen {
			continue
		}
		if len(graph) >= maxMergeAncestors {
			return mergeCalculation{}, ErrHistoryLimit
		}
		commit, err := s.metadata.commit(ctx, id)
		if err != nil {
			return mergeCalculation{}, err
		}
		commits[id] = commit
		graph[id] = commit.Parents
		queue = append(queue, commit.Parents...)
	}
	bases, err := model.CommonBases(graph, spec.Base, spec.Other)
	if err != nil {
		return mergeCalculation{}, err
	}
	if len(bases) == 0 {
		return mergeCalculation{}, ErrUnrelatedHistory
	}
	if len(bases) != 1 {
		return mergeCalculation{}, ErrAmbiguousMergeBase
	}
	baseArchive, err := s.archive(ctx, commits[bases[0]].Tree)
	if err != nil {
		return mergeCalculation{}, err
	}
	ours, err := s.archive(ctx, commits[spec.Base].Tree)
	if err != nil {
		return mergeCalculation{}, err
	}
	theirs, err := s.archive(ctx, commits[spec.Other].Tree)
	if err != nil {
		return mergeCalculation{}, err
	}
	baseEntries, err := s.objects.Entries(baseArchive)
	if err != nil {
		return mergeCalculation{}, err
	}
	oursEntries, err := s.objects.Entries(ours)
	if err != nil {
		return mergeCalculation{}, err
	}
	theirsEntries, err := s.objects.Entries(theirs)
	if err != nil {
		return mergeCalculation{}, err
	}
	choices, err := Choices(spec.Choices)
	if err != nil {
		return mergeCalculation{}, err
	}
	entries, conflicts, err := model.ReconcileTreesWithChoices(baseEntries, oursEntries, theirsEntries, choices)
	if err != nil {
		return mergeCalculation{}, err
	}
	return mergeCalculation{base: bases[0], local: ours, target: theirs, entries: entries, conflicts: conflicts}, nil
}

func (s *Service) validateMergeSources(ctx context.Context, calculation mergeCalculation) error {
	if err := s.roots.Validate(ctx, calculation.local); err != nil {
		return fmt.Errorf("validate destination merge tip: %w", err)
	}
	if err := s.roots.Validate(ctx, calculation.target); err != nil {
		return fmt.Errorf("validate source merge tip: %w", err)
	}
	return nil
}

func (s *Service) finishMerge(ctx context.Context, record publicationRecord, calculation mergeCalculation) (publicationRecord, error) {
	archive, err := s.objects.Compose(ctx, calculation.local, calculation.target, calculation.entries)
	if err != nil {
		return record, err
	}
	id, err := s.metadata.Import(ctx, s.namespace, archive.Format, archive.Root, archive.Directories)
	if err != nil {
		return record, err
	}
	return s.metadata.finishCapture(ctx, record.ID, TreeRef{Format: archive.Format, ID: id})
}

func (s *Service) resumeMergeCapture(ctx context.Context, record publicationRecord) (publicationRecord, error) {
	record, err := s.metadata.checkCapture(ctx, record.ID, s.now().UTC())
	if err != nil || record.State != OperationCapturing {
		return record, err
	}
	var request MergeRequest
	if err = json.Unmarshal(record.CaptureInput, &request); err != nil {
		return record, fmt.Errorf("read sealed merge request: %w", err)
	}
	request, encoded, fingerprint, err := normalizeMerge(request)
	if err != nil {
		return record, err
	}
	if fingerprint != record.Fingerprint || !bytes.Equal(encoded, record.CaptureInput) || request.OperationID != record.ID || request.Branch != record.Input.Branch || request.Base != record.Input.Base || request.Other != record.Input.MergeParent || request.Author != record.Input.Author || request.Message != record.Input.Message {
		return record, errors.New("stored merge input does not match its sealed facts")
	}
	calculation, err := s.calculateMerge(ctx, request.MergeSpec)
	if err != nil {
		return record, err
	}
	if len(calculation.conflicts) != 0 {
		return record, ErrMergeConflict
	}
	if err = s.validateMergeSources(ctx, calculation); err != nil {
		return record, err
	}
	return s.finishMerge(ctx, record, calculation)
}
