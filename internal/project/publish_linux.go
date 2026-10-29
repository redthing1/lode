package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

var ErrNoChanges = errors.New("no selected changes")

// Source supplies changed bytes and validates stream completion before acceptance.
// Write visits files depth-first, with sorted path components.
type Source struct {
	Write    func(model.Entry, io.Writer, *Basis) error
	Complete func() error
	// Only the project service may supply previously verified native objects.
	reuse map[string]content.Object
}

func fromMillis(n int64) time.Time { return time.UnixMilli(n).UTC() }

// A previously unaccepted operation must be recent. Once this window has passed,
// pruning its receipt cannot allow an old request to publish a second time.
const operationLifetime = 7 * 24 * time.Hour

func validOperation(id string, now time.Time, cutoff string) error {
	u, err := uuid.Parse(id)
	if err != nil || u.Version() != 7 || u.String() != id {
		return fmt.Errorf("%w: invalid operation ID", ErrRequest)
	}
	// A persisted expiry remains definitive even if the clock rolls back far
	// enough to make the request appear to have come from the future.
	if id <= cutoff {
		return ErrOperationExpired
	}
	sec, nsec := u.Time().UnixTime()
	created := time.Unix(sec, nsec)
	if created.After(now.Add(5 * time.Minute)) {
		return fmt.Errorf("%w: operation time is ahead of the server clock", ErrRequest)
	}
	if !created.After(now.Add(-operationLifetime)) {
		return ErrOperationExpired
	}
	return nil
}

// Save publishes only after protected contents are durable. The caller supplies
// the authenticated actor; it is part of the immutable operation fingerprint.
func (s *Store) Save(ctx context.Context, actor string, r model.Save, source Source) (model.Version, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	intent, err := requestIntent(actor, r)
	if err != nil {
		return model.Version{}, err
	}
	v, err := s.outcome(ctx, actor, r.ID, intent)
	if !errors.Is(err, ErrUnaccepted) {
		return v, err
	}
	return s.publish(ctx, actor, r, intent, source)
}

// Resolve waits for an in-flight publication before reporting its outcome,
// without requesting file contents.
func (s *Store) Resolve(ctx context.Context, actor string, r model.Save) (model.Version, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	intent, err := requestIntent(actor, r)
	if err != nil {
		return model.Version{}, err
	}
	v, err := s.outcome(ctx, actor, r.ID, intent)
	if errors.Is(err, ErrUnaccepted) {
		if validity := s.validOperation(ctx, r.ID, time.Now()); validity != nil {
			return model.Version{}, validity
		}
		head, headErr := s.head(ctx, r.Branch)
		if headErr != nil {
			return model.Version{}, headErr
		}
		if head != r.Base {
			return model.Version{}, ErrStale
		}
		return model.Version{}, ErrUnaccepted
	}
	return v, err
}

// Cancel returns an already accepted version, or a zero version after recording
// cancellation for this actor. A delayed upload by that actor cannot publish
// the cancelled ID; another account cannot cancel someone else's pending work.
// Receipts are needed only while the ID can still be accepted.
func (s *Store) Cancel(ctx context.Context, actor string, r model.Save) (model.Version, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	intent, err := requestIntent(actor, r)
	if err != nil {
		return model.Version{}, err
	}
	v, err := s.outcome(ctx, actor, r.ID, intent)
	if errors.Is(err, ErrCancelled) {
		return model.Version{}, nil
	}
	if !errors.Is(err, ErrUnaccepted) {
		return v, err
	}
	now := time.Now()
	if err := s.validOperation(ctx, r.ID, now); err != nil {
		if errors.Is(err, ErrOperationExpired) {
			// The client may discard this pending intent before updating. Make
			// expiry irreversible even when cleanup has not run and the clock
			// later rolls back. A failed cutoff write must not authorize discard.
			if _, floorErr := s.db.ExecContext(ctx, "UPDATE retention SET cutoff=max(cutoff,?) WHERE project=?", operationCutoff(now), s.id); floorErr != nil {
				return model.Version{}, floorErr
			}
		}
		return model.Version{}, err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO cancelled VALUES(?,?,?,?)", s.id, r.ID, actor, intent)
	return model.Version{}, err
}

func (s *Store) outcome(ctx context.Context, actor, id, intent string) (model.Version, error) {
	var cancelled string
	err := s.db.QueryRowContext(ctx, "SELECT intent FROM cancelled WHERE project=? AND id=? AND actor=?", s.id, id, actor).Scan(&cancelled)
	if err == nil {
		if cancelled != intent {
			return model.Version{}, ErrIntent
		}
		return model.Version{}, ErrCancelled
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Version{}, err
	}
	v, err := s.version(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Version{}, ErrUnaccepted
	}
	if err != nil {
		return model.Version{}, err
	}
	if v.intent != intent {
		return model.Version{}, ErrIntent
	}
	return v.Version, nil
}

func requestIntent(actor string, r model.Save) (string, error) {
	if actor == "" {
		return "", ErrRequest
	}
	if err := r.Validate(); err != nil {
		return "", errors.Join(ErrRequest, err)
	}
	r.Changes = append([]model.Entry(nil), r.Changes...)
	sort.Slice(r.Changes, func(i, j int) bool { return r.Changes[i].Path < r.Changes[j].Path })
	encoded, err := json.Marshal(struct {
		Actor   string
		Request model.Save
	}{actor, r})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(encoded)
	return hex.EncodeToString(h[:]), nil
}

func (s *Store) publish(ctx context.Context, actor string, r model.Save, intent string, source Source) (model.Version, error) {
	now := time.Now()
	if err := s.validOperation(ctx, r.ID, now); err != nil {
		return model.Version{}, err
	}
	head, err := s.head(ctx, r.Branch)
	if err != nil {
		return model.Version{}, err
	}
	if head != r.Base {
		return model.Version{}, ErrStale
	}
	if len(r.Changes) == 0 && r.Merge == "" {
		return model.Version{}, ErrNoChanges
	}
	base := ""
	if head != "" {
		v, err := s.version(ctx, head)
		if err != nil {
			return model.Version{}, err
		}
		if !v.Retained {
			return model.Version{}, ErrExpired
		}
		base = v.root
	}
	if r.Merge != "" {
		r.Changes, source.reuse, err = s.mergeChanges(ctx, r)
		if err != nil {
			return model.Version{}, err
		}
	}
	var root content.Object
	var protection string
	var dirs []directory
	err = s.content.Write(ctx, func(w *content.Writer) error {
		var err error
		root, _, err = s.rewrite(ctx, w, base, "", edits(r.Changes), source, &dirs)
		if err != nil {
			return err
		}
		if source.Complete != nil {
			if err := source.Complete(); err != nil {
				return err
			}
		}
		if root.ID == base && r.Merge == "" {
			return ErrNoChanges
		}
		protection, err = w.Protect(r.ID, root)
		return err
	})
	if err != nil {
		return model.Version{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Version{}, err
	}
	defer tx.Rollback()
	if err := s.writeDirectories(ctx, tx, dirs); err != nil {
		return model.Version{}, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO versions VALUES(?,?,?,?,?,?,?,?,?,?)", s.id, r.ID, r.Base, r.Merge, actor, r.Message, now.UnixMilli(), root.ID, protection, intent)
	if err != nil {
		return model.Version{}, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE branches SET tip=? WHERE project=? AND name=? AND tip=?", r.ID, s.id, r.Branch, r.Base)
	if err != nil {
		return model.Version{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return model.Version{}, err
	}
	if n != 1 {
		return model.Version{}, ErrStale
	}
	// Never release native protection when a commit result is uncertain.
	if err := tx.Commit(); err != nil {
		return model.Version{}, fmt.Errorf("accept save: %w", err)
	}
	return model.Version{ID: r.ID, Parent: r.Base, MergeParent: r.Merge, Actor: actor, Message: r.Message, Created: fromMillis(now.UnixMilli()), Retained: true}, nil
}
