package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/model"
)

var ErrCleanupChanged = errors.New("cleanup review expired or project changed; preview cleanup again")

func (s *Store) Retention(ctx context.Context) (model.Retention, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	p, _, err := s.retention(ctx)
	return p, err
}

func (s *Store) retention(ctx context.Context) (model.Retention, string, error) {
	var p model.Retention
	var cutoff string
	err := s.db.QueryRowContext(ctx, "SELECT versions,history_days,cutoff FROM retention WHERE project=?", s.id).Scan(&p.Versions, &p.HistoryDays, &cutoff)
	return p, cutoff, err
}

func (s *Store) SetRetention(ctx context.Context, p model.Retention) error {
	if err := p.Validate(); err != nil {
		return errors.Join(ErrRequest, err)
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	_, err := s.db.ExecContext(ctx, "UPDATE retention SET versions=?,history_days=? WHERE project=?", p.Versions, p.HistoryDays, s.id)
	return err
}

// UUIDv7 sorts by millisecond timestamp. The suffix includes every ID in the
// cutoff millisecond, including requests that never reached this server.
func operationCutoff(now time.Time) string {
	n := now.Add(-operationLifetime).UnixMilli()
	return fmt.Sprintf("%08x-%04x-ffff-ffff-ffffffffffff", n>>16, n&0xffff)
}

func (s *Store) validOperation(ctx context.Context, id string, now time.Time) error {
	_, cutoff, err := s.retention(ctx)
	if err != nil {
		return err
	}
	return validOperation(id, now, cutoff)
}

type cleanupPlan struct {
	preview                             model.CleanupPreview
	cutoff                              string
	expire, forget, directories, groups []string
}

func (s *Store) PreviewCleanup(ctx context.Context) (model.CleanupPreview, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	p, err := s.planCleanup(ctx, time.Now())
	return p.preview, err
}

// Cleanup excludes mutations and waits for readers before recomputing the
// reviewed decision. Its token is a state precondition, not an authorization secret.
func (s *Store) Cleanup(ctx context.Context, token string) (model.CleanupPreview, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	s.gate.Lock()
	defer s.gate.Unlock()
	stamp, _, ok := strings.Cut(token, ".")
	millis, err := strconv.ParseInt(stamp, 10, 64)
	when := time.UnixMilli(millis)
	now := time.Now()
	if !ok || err != nil || millis <= 0 || when.After(now) || now.Sub(when) > time.Hour {
		return model.CleanupPreview{}, ErrCleanupChanged
	}
	p, err := s.planCleanup(ctx, when)
	if err != nil {
		return model.CleanupPreview{}, err
	}
	if p.preview.Token != token {
		return model.CleanupPreview{}, ErrCleanupChanged
	}
	return s.applyCleanup(ctx, p)
}

// applyCleanup requires both project locks and a reviewed current plan.
func (s *Store) applyCleanup(ctx context.Context, p cleanupPlan) (model.CleanupPreview, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p.preview, err
	}
	defer tx.Rollback()
	// The floor and deletions commit together. Clock rollback cannot make a
	// forgotten receipt, or an unrecorded old request, eligible again.
	if _, err := tx.ExecContext(ctx, "UPDATE retention SET cutoff=? WHERE project=?", p.cutoff, s.id); err != nil {
		return p.preview, err
	}
	for _, batch := range []struct {
		query string
		ids   []string
	}{
		{"UPDATE versions SET protection='' WHERE project=? AND id=?", p.expire},
		{"DELETE FROM versions WHERE project=? AND id=?", p.forget},
		{"DELETE FROM directories WHERE project=? AND id=?", p.directories},
		{"DELETE FROM groups WHERE project=? AND id=?", p.groups},
	} {
		if len(batch.ids) == 0 {
			continue
		}
		stmt, err := tx.PrepareContext(ctx, batch.query)
		if err != nil {
			return p.preview, err
		}
		for _, id := range batch.ids {
			if _, err := stmt.ExecContext(ctx, s.id, id); err != nil {
				stmt.Close()
				return p.preview, err
			}
		}
		if err := stmt.Close(); err != nil {
			return p.preview, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM cancelled WHERE project=? AND id<=?", s.id, p.cutoff); err != nil {
		return p.preview, err
	}
	if err := tx.Commit(); err != nil {
		return p.preview, fmt.Errorf("cleanup acceptance uncertain; inspect a fresh preview: %w", err)
	}
	if err := s.reconcile(ctx); err != nil {
		return p.preview, fmt.Errorf("cleanup applied; content release needs retry: %w", err)
	}
	// All projects share SQLite; only its already-free pages are reclaimed.
	if _, err := s.db.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		return p.preview, fmt.Errorf("cleanup applied; database compaction failed: %w", err)
	}
	return p.preview, nil
}

func (s *Store) planCleanup(ctx context.Context, when time.Time) (cleanupPlan, error) {
	var p cleanupPlan
	when = fromMillis(when.UnixMilli())
	policy, floor, err := s.retention(ctx)
	if err != nil {
		return p, err
	}
	if err := policy.Validate(); err != nil {
		return p, errors.Join(ErrMetadata, err)
	}
	p.preview.Policy = policy
	p.cutoff = max(floor, operationCutoff(when))
	rows, err := s.db.QueryContext(ctx, "SELECT id,parent,merge_parent,actor,message,created,root,protection<>'' FROM versions WHERE project=? ORDER BY created DESC,id", s.id)
	if err != nil {
		return p, err
	}
	var versions []savedVersion
	for rows.Next() {
		var v savedVersion
		var created int64
		if err := rows.Scan(&v.ID, &v.Parent, &v.MergeParent, &v.Actor, &v.Message, &created, &v.root, &v.Retained); err != nil {
			rows.Close()
			return p, err
		}
		v.Created = fromMillis(created)
		versions = append(versions, v)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return p, err
	}
	byID := map[string]savedVersion{}
	for _, v := range versions {
		byID[v.ID] = v
	}
	type reference struct{ Kind, Name, Version string }
	var refs []reference
	rows, err = s.db.QueryContext(ctx, "SELECT 'branch',name,tip FROM branches WHERE project=? UNION ALL SELECT 'tag',name,version FROM tags WHERE project=? ORDER BY 1,2", s.id, s.id)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var ref reference
		if err := rows.Scan(&ref.Kind, &ref.Name, &ref.Version); err != nil {
			rows.Close()
			return p, err
		}
		refs = append(refs, ref)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return p, err
	}
	keep := map[string]bool{}
	for _, ref := range refs {
		if ref.Version == "" {
			if ref.Kind == "tag" {
				return p, ErrMetadata
			}
			continue
		}
		v, ok := byID[ref.Version]
		if !ok || !v.Retained {
			return p, ErrMetadata
		}
		id := ref.Version
		limit := policy.Versions
		if ref.Kind == "tag" {
			limit = 1
		}
		for n := 0; n < limit && id != ""; n++ {
			v, ok := byID[id]
			if !ok {
				break
			}
			keep[id] = true
			id = v.Parent
		}
	}
	var roots []string
	cutoff := when.AddDate(0, 0, -policy.HistoryDays)
	for _, v := range versions {
		if v.Retained {
			if keep[v.ID] {
				p.preview.Retained++
			} else {
				p.expire = append(p.expire, v.ID)
				if len(p.preview.ExpireExamples) < 20 {
					p.preview.ExpireExamples = append(p.preview.ExpireExamples, v.Version)
				}
			}
		}
		if !keep[v.ID] && v.Created.Before(cutoff) && v.ID <= p.cutoff {
			p.forget = append(p.forget, v.ID)
			if len(p.preview.ForgetExamples) < 20 {
				p.preview.ForgetExamples = append(p.preview.ForgetExamples, v.Version)
			}
		} else {
			if v.root == "" {
				return p, ErrMetadata
			}
			roots = append(roots, v.root)
		}
	}
	p.preview.Expire, p.preview.Forget = len(p.expire), len(p.forget)
	p.directories, p.groups, err = s.unreachableMetadata(ctx, roots)
	if err != nil {
		return p, err
	}
	// Include all immutable version IDs and availability so later publication or
	// cleanup invalidates the review, even when its visible counts happen to match.
	state := struct {
		At       int64
		Policy   model.Retention
		Floor    string
		Refs     []reference
		Versions []model.Version
	}{At: when.UnixMilli(), Policy: policy, Floor: p.cutoff, Refs: refs}
	for _, v := range versions {
		state.Versions = append(state.Versions, v.Version)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return p, err
	}
	hash := sha256.Sum256(encoded)
	p.preview.Token = strconv.FormatInt(when.UnixMilli(), 10) + "." + hex.EncodeToString(hash[:])
	return p, nil
}

// Every surviving logical version is a root, including expired contents and
// recent retry outcomes. Missing metadata refuses cleanup before any mutation.
func (s *Store) unreachableMetadata(ctx context.Context, roots []string) ([]string, []string, error) {
	live := map[string]bool{}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if live[id] {
			continue
		}
		if id == "" {
			return nil, nil, ErrMetadata
		}
		entries, err := s.directory(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		live[id] = true
		for _, e := range entries {
			if e.Kind == model.Directory {
				queue = append(queue, e.Object.ID)
			}
		}
	}
	groups := map[string]bool{}
	var directories []string
	rows, err := s.db.QueryContext(ctx, "SELECT id,body FROM directories WHERE project=? ORDER BY id", s.id)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !live[id] {
			directories = append(directories, id)
			continue
		}
		var slots []string
		if err := json.Unmarshal(body, &slots); err != nil || len(slots) != groupCount {
			rows.Close()
			return nil, nil, ErrMetadata
		}
		for _, key := range slots {
			if key != "" {
				groups[key] = true
			}
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	var unused []string
	rows, err = s.db.QueryContext(ctx, "SELECT id FROM groups WHERE project=? ORDER BY id", s.id)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !groups[id] {
			unused = append(unused, id)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	return directories, unused, nil
}
