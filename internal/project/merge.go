package project

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

func logicalEntries(entries []content.Entry) []model.Entry {
	result := make([]model.Entry, 0, len(entries))
	for _, e := range entries {
		result = append(result, e.Entry)
	}
	return result
}

// mergeEntries returns the persisted first-parent inventory and proposed tree.
func (p *Project) mergeEntries(ctx context.Context, r Request) ([]content.Entry, []content.Entry, error) {
	baseID, err := p.mergeBase(ctx, r.Base, r.SecondParent, r.MergeBase)
	if err != nil {
		return nil, nil, err
	}
	base, err := p.inventory(ctx, baseID, nil)
	if err != nil {
		return nil, nil, err
	}
	ours, err := p.inventory(ctx, r.Base, nil)
	if err != nil {
		return nil, nil, err
	}
	theirs, err := p.inventory(ctx, r.SecondParent, nil)
	if err != nil {
		return nil, nil, err
	}
	merged, err := model.Merge(logicalEntries(base), logicalEntries(ours), logicalEntries(theirs), r.Choices)
	if err != nil {
		return nil, nil, err
	}
	objects := make(map[model.Entry]string, len(ours)+len(theirs))
	for _, e := range ours {
		objects[e.Entry] = e.Object
	}
	for _, e := range theirs {
		objects[e.Entry] = e.Object
	}
	result := make([]content.Entry, 0, len(merged))
	for _, e := range merged {
		object := objects[e]
		if e.Kind == model.File && object == "" {
			return nil, nil, fmt.Errorf("merge has no content source for %s", e.Path)
		}
		result = append(result, content.Entry{Entry: e, Object: object})
	}
	if r.Base == "" {
		ours = theirs
	}
	return ours, result, nil
}

// mergeBase considers both parents, independent of the first-parent checkpoint
// representation. Multiple best bases require an explicit common ancestor.
func (p *Project) mergeBase(ctx context.Context, ours, theirs, chosen string) (string, error) {
	rows, err := p.db.QueryContext(ctx, `WITH RECURSIVE ancestors(id) AS (
 SELECT ? UNION SELECT ?
 UNION SELECT c.parent FROM commits c JOIN ancestors a ON c.id=a.id WHERE c.parent IS NOT NULL
 UNION SELECT c.second_parent FROM commits c JOIN ancestors a ON c.id=a.id WHERE c.second_parent IS NOT NULL
) SELECT c.id,coalesce(c.parent,''),coalesce(c.second_parent,'') FROM commits c JOIN ancestors a ON c.id=a.id`, ours, theirs)
	if err != nil {
		return "", err
	}
	graph := map[string][2]string{}
	for rows.Next() {
		var id string
		var parents [2]string
		if err := rows.Scan(&id, &parents[0], &parents[1]); err != nil {
			rows.Close()
			return "", err
		}
		graph[id] = parents
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}
	for _, id := range []string{ours, theirs} {
		if id != "" {
			if _, ok := graph[id]; !ok {
				return "", ErrNotFound
			}
		}
	}
	ancestors := func(start string) map[string]bool {
		seen := map[string]bool{"": true}
		queue := []string{start}
		for len(queue) > 0 {
			id := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			parents := graph[id]
			queue = append(queue, parents[0], parents[1])
		}
		return seen
	}
	a, b := ancestors(ours), ancestors(theirs)
	if chosen != "" {
		if !a[chosen] || !b[chosen] {
			return "", fmt.Errorf("%w: merge base is not a common ancestor", ErrInvalid)
		}
	}
	if a[theirs] {
		return "", ErrAlreadyMerged
	}
	if chosen != "" {
		return chosen, nil
	}
	older := map[string]bool{}
	for id := range a {
		if b[id] && id != "" {
			parents := graph[id]
			older[parents[0]] = true
			older[parents[1]] = true
		}
	}
	var best []string
	for id := range a {
		if b[id] && !older[id] {
			best = append(best, id)
		}
	}
	sort.Strings(best)
	if len(best) > 1 {
		return "", &MergeBaseConflict{Bases: best}
	}
	if len(best) == 0 {
		return "", errors.New("history has no merge base")
	}
	return best[0], nil
}
