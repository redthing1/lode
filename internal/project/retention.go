package project

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

func validateRetentionPolicy(policy RetentionPolicy) error {
	if policy.KeepLast < 0 || policy.KeepLast > 10000 || policy.KeepDays < 0 || policy.KeepDays > 36500 {
		return fmt.Errorf("%w: keep-last must be 0–10000 and keep-days 0–36500", ErrInvalidRequest)
	}
	return nil
}

func (s *Service) RetentionPolicy(ctx context.Context) (RetentionPolicy, error) {
	if s.closed.Load() {
		return RetentionPolicy{}, ErrServiceClosed
	}
	return s.metadata.retentionPolicy(ctx)
}

// SetRetentionPolicy changes future eligibility. It never restores expired
// contents or releases roots; cleanup is a separate, revalidated operation.
func (s *Service) SetRetentionPolicy(ctx context.Context, policy RetentionPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	if err := validateRetentionPolicy(policy); err != nil {
		return err
	}
	return s.metadata.setRetentionPolicy(ctx, policy)
}

func (s *Service) requireRetained(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	record, err := s.metadata.payload(ctx, id)
	if err != nil {
		return err
	}
	if !record.ExpiredAt.IsZero() {
		return ErrPayloadExpired
	}
	return nil
}

func availability(record payloadRecord) Availability {
	result := Availability{CommitID: record.CommitID, State: "retained", Reasons: []string{}}
	if !record.ExpiredAt.IsZero() {
		result.State = "expired"
		result.ExpiredAt = record.ExpiredAt
		result.ReleasePending = !record.Released
	}
	return result
}

// PayloadStatuses reads a bounded batch without loading the retention graph.
// History consumers can display availability separately from immutable commits.
func (s *Service) PayloadStatuses(ctx context.Context, ids []string) ([]Availability, error) {
	if s.closed.Load() {
		return nil, ErrServiceClosed
	}
	if len(ids) > 1000 {
		return nil, fmt.Errorf("%w: at most 1000 versions", ErrInvalidRequest)
	}
	result := make([]Availability, 0, len(ids))
	for _, id := range ids {
		if err := validateCommitID(id); err != nil {
			return nil, fmt.Errorf("%w: invalid version", ErrInvalidRequest)
		}
		record, err := s.metadata.payload(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, availability(record))
	}
	return result, nil
}

// ExplainRetention returns current protection reasons, not a reservation that
// outlives this call. Eligibility is always recomputed before expiration.
func (s *Service) ExplainRetention(ctx context.Context, id string) (Availability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return Availability{}, ErrServiceClosed
	}
	snapshot, reasons, err := s.retentionPlan(ctx, s.now().UTC())
	if err != nil {
		return Availability{}, err
	}
	payload, ok := snapshot.Payloads[id]
	if !ok {
		return Availability{}, ErrCommitNotFound
	}
	result := availability(payload)
	if result.State == "retained" {
		result.Reasons = reasons[id]
	}
	return result, nil
}

func (s *Service) PreviewRetention(ctx context.Context, after string, limit int) (RetentionPreview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return RetentionPreview{}, ErrServiceClosed
	}
	if limit < 1 || limit > 1000 {
		return RetentionPreview{}, fmt.Errorf("%w: limit must be 1–1000", ErrInvalidRequest)
	}
	if after != "" {
		if err := validateCommitID(after); err != nil {
			return RetentionPreview{}, fmt.Errorf("%w: invalid cursor", ErrInvalidRequest)
		}
	}
	now := s.now().UTC()
	snapshot, reasons, err := s.retentionPlan(ctx, now)
	if err != nil {
		return RetentionPreview{}, err
	}
	var candidates []RetentionCandidate
	for id, commit := range snapshot.Commits {
		if snapshot.Payloads[id].ExpiredAt.IsZero() && len(reasons[id]) == 0 {
			candidates = append(candidates, RetentionCandidate{CommitID: id, CreatedAt: commit.CreatedAt})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CommitID < candidates[j].CommitID })
	result := RetentionPreview{Policy: snapshot.Policy, EvaluatedAt: now, TotalCandidates: len(candidates), Candidates: []RetentionCandidate{}}
	start := sort.Search(len(candidates), func(i int) bool { return candidates[i].CommitID > after })
	end := min(start+limit, len(candidates))
	result.Candidates = append(result.Candidates, candidates[start:end]...)
	if end < len(candidates) && end > start {
		result.Next = candidates[end-1].CommitID
	}
	return result, nil
}

// Expire revalidates requested candidates against current protections, commits
// irreversible release intent, then releases native roots. A native failure
// leaves explicit release work for Recover; it cannot restore eligibility for
// new tags or branches. Existing shared payload may remain physically present.
func (s *Service) Expire(ctx context.Context, ids []string) (ExpirationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ExpirationResult{}, ErrServiceClosed
	}
	if len(ids) == 0 || len(ids) > 1000 {
		return ExpirationResult{}, fmt.Errorf("%w: request 1–1000 versions", ErrInvalidRequest)
	}
	unique := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := validateCommitID(id); err != nil {
			return ExpirationResult{}, fmt.Errorf("%w: invalid version", ErrInvalidRequest)
		}
		if unique[id] {
			return ExpirationResult{}, fmt.Errorf("%w: duplicate version", ErrInvalidRequest)
		}
		unique[id] = true
	}
	now := s.now().UTC()
	snapshot, reasons, err := s.retentionPlan(ctx, now)
	if err != nil {
		return ExpirationResult{}, err
	}
	result := ExpirationResult{Expired: []string{}, Protected: []Availability{}}
	for _, id := range ids {
		payload, exists := snapshot.Payloads[id]
		if !exists {
			return ExpirationResult{}, ErrCommitNotFound
		}
		if len(reasons[id]) != 0 && payload.ExpiredAt.IsZero() {
			item := availability(payload)
			item.Reasons = reasons[id]
			result.Protected = append(result.Protected, item)
		} else {
			result.Expired = append(result.Expired, id)
		}
	}
	if err := s.metadata.expirePayloads(ctx, result.Expired, now); err != nil {
		return ExpirationResult{}, err
	}
	return result, s.releaseExpiredPayloads(ctx)
}

func (s *Service) releaseExpiredPayloads(ctx context.Context) error {
	var failures []error
	for after := ""; ; {
		page, err := s.metadata.expiredPayloadRoots(ctx, after, 64)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if len(page) == 0 {
			return errors.Join(failures...)
		}
		for _, item := range page {
			after = item.CommitID
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			if err := s.roots.Release(ctx, s.protectionKey(item.OperationID)); err != nil {
				failures = append(failures, fmt.Errorf("release expired version %s: %w", item.CommitID, err))
				continue
			}
			if err := s.metadata.markPayloadReleased(ctx, item.CommitID); err != nil {
				failures = append(failures, err)
			}
		}
	}
}

// retentionPlan runs under the service mutation lock. The one-service owner
// lock prevents another authoritative writer between this snapshot and expiry.
func (s *Service) retentionPlan(ctx context.Context, now time.Time) (retentionSnapshot, map[string][]string, error) {
	snapshot, err := s.metadata.retentionSnapshot(ctx)
	if err != nil {
		return retentionSnapshot{}, nil, err
	}
	if err := validateRetentionPolicy(snapshot.Policy); err != nil {
		return retentionSnapshot{}, nil, err
	}
	reasons := make(map[string][]string)
	add := func(id, reason string) error {
		if id == "" {
			return nil
		}
		payload, exists := snapshot.Payloads[id]
		if !exists {
			return fmt.Errorf("protection refers to unknown version %q", id)
		}
		if !payload.ExpiredAt.IsZero() {
			return fmt.Errorf("protection %q refers to expired version %q", reason, id)
		}
		reasons[id] = append(reasons[id], reason)
		return nil
	}
	for id, commit := range snapshot.Commits {
		payload, exists := snapshot.Payloads[id]
		if !exists {
			return retentionSnapshot{}, nil, fmt.Errorf("version %q has no retention state", id)
		}
		if !payload.ExpiredAt.IsZero() {
			continue
		}
		if payload.ProtectUntil.After(now) {
			_ = add(id, "publication grace")
		}
		if snapshot.Policy.KeepDays > 0 && !commit.CreatedAt.Before(now.Add(-time.Duration(snapshot.Policy.KeepDays)*24*time.Hour)) {
			_ = add(id, "recent time window")
		}
	}
	for _, branch := range snapshot.Branches {
		if err := add(branch.Tip, "branch:"+branch.Name); err != nil {
			return retentionSnapshot{}, nil, err
		}
		id := branch.Tip
		seen := make(map[string]bool)
		for count := 0; count < snapshot.Policy.KeepLast && id != ""; count++ {
			if seen[id] {
				return retentionSnapshot{}, nil, errors.New("cycle in retention history")
			}
			seen[id] = true
			commit, exists := snapshot.Commits[id]
			if !exists {
				return retentionSnapshot{}, nil, ErrCommitNotFound
			}
			if snapshot.Payloads[id].ExpiredAt.IsZero() {
				if err := add(id, "recent branch history:"+branch.Name); err != nil {
					return retentionSnapshot{}, nil, err
				}
			}
			id = ""
			if len(commit.Parents) != 0 {
				id = commit.Parents[0]
			}
		}
	}
	for _, tag := range snapshot.Tags {
		if err := add(tag.CommitID, "tag:"+tag.Name); err != nil {
			return retentionSnapshot{}, nil, err
		}
	}
	for _, lease := range snapshot.Leases {
		if lease.ExpiresAt.After(now) {
			if err := add(lease.CommitID, "read:"+lease.ID); err != nil {
				return retentionSnapshot{}, nil, err
			}
		}
	}
	for _, op := range snapshot.Operations {
		if !op.ExpiresAt.After(now) {
			continue
		}
		if err := add(op.Input.Base, "operation:"+op.ID); err != nil {
			return retentionSnapshot{}, nil, err
		}
		if err := add(op.Input.MergeParent, "operation:"+op.ID); err != nil {
			return retentionSnapshot{}, nil, err
		}
	}
	s.readMu.Lock()
	for id, count := range s.readers {
		if count > 0 {
			if err := add(id, "active file stream"); err != nil {
				s.readMu.Unlock()
				return retentionSnapshot{}, nil, err
			}
		}
	}
	s.readMu.Unlock()
	for id := range reasons {
		sort.Strings(reasons[id])
	}
	return snapshot, reasons, nil
}
