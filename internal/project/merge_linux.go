package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

var (
	ErrAlreadyMerged      = errors.New("source is already integrated")
	ErrMergeBase          = errors.New("multiple common ancestors; automatic integration is unavailable")
	ErrHistoryUnavailable = errors.New("required ancestry has been removed; restore selected contents instead")
)

type ConflictsError struct{ Conflicts []model.Conflict }

func (e *ConflictsError) Error() string {
	return fmt.Sprintf("%d conflicting paths; choose target or incoming contents", len(e.Conflicts))
}

// mergeChanges runs under the publication gate. Only logical metadata reaches
// reconciliation; changed files reuse verified objects from the retained source.
func (s *Store) mergeChanges(ctx context.Context, r model.Save) ([]model.Entry, map[string]content.Object, error) {
	incoming, err := s.version(ctx, r.Merge)
	if err != nil {
		return nil, nil, err
	}
	if !incoming.Retained {
		return nil, nil, ErrExpired
	}
	base, err := s.mergeBase(ctx, r.Base, r.Merge)
	if err != nil {
		return nil, nil, err
	}
	var trees [3][]model.Entry
	for i, id := range []string{base, r.Base, r.Merge} {
		if id == "" {
			continue
		}
		v, err := s.version(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if err := s.walk(ctx, v.root, "", model.Selection{{Include: true}}, func(e model.Entry) error {
			trees[i] = append(trees[i], e)
			return nil
		}); err != nil {
			return nil, nil, err
		}
		sort.Slice(trees[i], func(a, b int) bool { return trees[i][a].Path < trees[i][b].Path })
	}
	merged, conflicts, err := model.Reconcile(trees[0], trees[1], trees[2], r.Choices)
	if err != nil {
		return nil, nil, errors.Join(ErrRequest, err)
	}
	if len(conflicts) != 0 {
		return nil, nil, &ConflictsError{conflicts}
	}
	changes := model.Changes(trees[1], merged, model.Selection{{Include: true}})
	reuse := make(map[string]content.Object)
	for _, e := range changes {
		if e.Kind != model.File {
			continue
		}
		entry, err := s.lookup(ctx, incoming.root, e.Path)
		if err != nil {
			return nil, nil, err
		}
		if entry.Entry != e {
			return nil, nil, ErrMetadata
		}
		reuse[e.Path] = entry.Object
	}
	return changes, reuse, nil
}

// mergeBase follows both parents. A missing parent is an unknown boundary, not
// an empty root. Unknown history strictly before a proven base is harmless;
// unknown history elsewhere could conceal a better base and must stop merging.
func (s *Store) mergeBase(ctx context.Context, target, source string) (string, error) {
	cache := map[string]model.Version{}
	missing := map[string]bool{}
	ancestors := func(start string) (map[string]bool, error) {
		seen := map[string]bool{}
		queue := []string{start}
		for len(queue) > 0 {
			id := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			if id == "" || missing[id] {
				continue
			}
			v, ok := cache[id]
			if !ok {
				stored, err := s.version(ctx, id)
				if errors.Is(err, sql.ErrNoRows) {
					missing[id] = true
					continue
				}
				if err != nil {
					return nil, err
				}
				v = stored.Version
				cache[id] = v
			}
			queue = append(queue, v.Parent)
			if v.MergeParent != "" {
				queue = append(queue, v.MergeParent)
			}
		}
		return seen, nil
	}
	left, err := ancestors(target)
	if err != nil {
		return "", err
	}
	if left[source] {
		return "", ErrAlreadyMerged
	}
	right, err := ancestors(source)
	if err != nil {
		return "", err
	}
	if right[target] {
		return target, nil
	}
	best := map[string]bool{}
	for id := range left {
		if right[id] {
			best[id] = true
		}
	}
	// Every known ancestor of a common node is common. Removing immediate
	// parents from the intersection therefore leaves just its maximal nodes.
	for id := range left {
		if !right[id] {
			continue
		}
		if v, ok := cache[id]; ok {
			delete(best, v.Parent)
			if v.MergeParent != "" {
				delete(best, v.MergeParent)
			}
		}
	}
	if len(best) == 0 {
		return "", ErrHistoryUnavailable
	}
	for id := range best {
		if missing[id] {
			return "", ErrHistoryUnavailable
		}
	}
	if len(best) != 1 {
		return "", ErrMergeBase
	}
	base := ""
	for id := range best {
		base = id
	}
	before, err := ancestors(base)
	if err != nil {
		return "", err
	}
	for id := range missing {
		if !before[id] {
			return "", ErrHistoryUnavailable
		}
	}
	return base, nil
}
