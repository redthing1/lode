package model

import (
	"errors"
	"fmt"
	"sort"
)

var ErrInvalidHistory = errors.New("invalid commit history graph")

// CommonBases returns the best common ancestors reachable from left and
// right. It validates only the ancestry reachable from those tips; unrelated
// graph records do not affect the result. Multiple best bases are preserved
// for callers to handle explicitly. The graph is traversed iteratively and is
// not size-limited here.
func CommonBases(parents map[string][]string, left, right string) ([]string, error) {
	leftAncestors, err := reachableAncestors(parents, left)
	if err != nil {
		return nil, fmt.Errorf("left ancestry: %w", err)
	}
	rightAncestors, err := reachableAncestors(parents, right)
	if err != nil {
		return nil, fmt.Errorf("right ancestry: %w", err)
	}

	common := make(map[string]struct{})
	for id := range leftAncestors {
		if _, ok := rightAncestors[id]; ok {
			common[id] = struct{}{}
		}
	}
	if len(common) == 0 {
		return nil, nil
	}

	// Starting at every common node's parents marks common ancestors that
	// have a common descendant. Those nodes are older candidates, not best bases.
	queue := make([]string, 0, len(common))
	for id := range common {
		queue = append(queue, parents[id]...)
	}
	visited := make(map[string]struct{})
	dominated := make(map[string]struct{})
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := visited[id]; ok {
			continue
		}
		visited[id] = struct{}{}
		if _, ok := common[id]; ok {
			dominated[id] = struct{}{}
		}
		queue = append(queue, parents[id]...)
	}

	bases := make([]string, 0, len(common)-len(dominated))
	for id := range common {
		if _, old := dominated[id]; !old {
			bases = append(bases, id)
		}
	}
	sort.Strings(bases)
	return bases, nil
}

func reachableAncestors(parents map[string][]string, start string) (map[string]struct{}, error) {
	if start == "" {
		return nil, fmt.Errorf("%w: empty tip ID", ErrInvalidHistory)
	}
	type frame struct {
		id   string
		next int
	}
	state := make(map[string]uint8)
	reachable := make(map[string]struct{})

	enter := func(id string) error {
		if id == "" {
			return fmt.Errorf("%w: empty commit ID", ErrInvalidHistory)
		}
		parentIDs, ok := parents[id]
		if !ok {
			return fmt.Errorf("%w: missing commit %q", ErrInvalidHistory, id)
		}
		if len(parentIDs) > 2 {
			return fmt.Errorf("%w: commit %q has more than two parents", ErrInvalidHistory, id)
		}
		seenParents := make(map[string]struct{}, len(parentIDs))
		for _, parent := range parentIDs {
			if parent == "" {
				return fmt.Errorf("%w: commit %q has an empty parent ID", ErrInvalidHistory, id)
			}
			if _, duplicate := seenParents[parent]; duplicate {
				return fmt.Errorf("%w: commit %q repeats parent %q", ErrInvalidHistory, id, parent)
			}
			seenParents[parent] = struct{}{}
		}
		state[id] = 1
		return nil
	}

	if err := enter(start); err != nil {
		return nil, err
	}
	stack := []frame{{id: start}}
	for len(stack) > 0 {
		current := &stack[len(stack)-1]
		parentIDs := parents[current.id]
		if current.next == len(parentIDs) {
			state[current.id] = 2
			reachable[current.id] = struct{}{}
			stack = stack[:len(stack)-1]
			continue
		}
		parent := parentIDs[current.next]
		current.next++
		switch state[parent] {
		case 1:
			return nil, fmt.Errorf("%w: cycle through commit %q", ErrInvalidHistory, parent)
		case 2:
			continue
		default:
			if err := enter(parent); err != nil {
				return nil, err
			}
			stack = append(stack, frame{id: parent})
		}
	}
	return reachable, nil
}
