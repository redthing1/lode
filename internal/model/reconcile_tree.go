package model

import (
	"errors"
	"fmt"
	"sort"
)

// Side selects one complete subtree during three-way reconciliation.
type Side string

const (
	LocalSide  Side = "local"
	TargetSide Side = "target"
)

var ErrInvalidChoice = errors.New("invalid reconciliation choice")

// ReconcileTrees applies three-way whole-entry reconciliation to complete
// trees. A nil or empty tree denotes a project containing only its root
// directory. Pointwise conflicts and children left below a missing or
// non-directory parent are returned as sorted path conflicts. Inputs are
// unchanged; a successful result is caller-owned.
func ReconcileTrees(base, local, target map[Path]Entry) (map[Path]Entry, []Path, error) {
	return ReconcileTreesWithChoices(base, local, target, nil)
}

// ReconcileTreesWithChoices reconciles complete trees and lets a caller choose
// a whole side for any path subtree. The most-specific ancestor choice wins.
// Choice paths must occur in at least one input tree. Unresolved conflicts and
// structural conflicts remain sorted paths; no conflict resolution is guessed.
func ReconcileTreesWithChoices(base, local, target map[Path]Entry, choices map[Path]Side) (map[Path]Entry, []Path, error) {
	baseTree, err := completeTree(base)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid base tree: %w", err)
	}
	localTree, err := completeTree(local)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid local tree: %w", err)
	}
	targetTree, err := completeTree(target)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid target tree: %w", err)
	}

	paths := make(map[Path]struct{}, len(baseTree)+len(localTree)+len(targetTree))
	for _, tree := range []map[Path]Entry{baseTree, localTree, targetTree} {
		for path := range tree {
			paths[path] = struct{}{}
		}
	}
	for path, side := range choices {
		if side != LocalSide && side != TargetSide {
			return nil, nil, fmt.Errorf("choice at %q: %w", path, ErrInvalidChoice)
		}
		if _, inBase := baseTree[path]; !inBase {
			if _, inLocal := localTree[path]; !inLocal {
				if _, inTarget := targetTree[path]; !inTarget {
					return nil, nil, fmt.Errorf("choice path %q is absent from all input trees: %w", path, ErrInvalidChoice)
				}
			}
		}
	}
	ordered := make([]Path, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sortPaths(ordered)

	result := make(map[Path]Entry, len(paths))
	conflicts := make(map[Path]struct{})
	for _, path := range ordered {
		var entry Entry
		switch ChoiceFor(path, choices) {
		case LocalSide:
			entry = entryOrAbsent(localTree, path)
		case TargetSide:
			entry = entryOrAbsent(targetTree, path)
		default:
			entry, err = Reconcile(entryOrAbsent(baseTree, path), entryOrAbsent(localTree, path), entryOrAbsent(targetTree, path))
			if errors.Is(err, ErrConflict) {
				conflicts[path] = struct{}{}
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("reconcile path %q: %w", path, err)
			}
		}
		if entry.Kind != Absent {
			result[path] = entry
		}
	}

	for path := range result {
		if path.IsRoot() {
			continue
		}
		parent := path.Parent()
		if entry, ok := result[parent]; !ok || entry.Kind != Directory {
			conflicts[parent] = struct{}{}
		}
	}
	if len(conflicts) != 0 {
		orderedConflicts := make([]Path, 0, len(conflicts))
		for path := range conflicts {
			orderedConflicts = append(orderedConflicts, path)
		}
		sortPaths(orderedConflicts)
		return nil, orderedConflicts, nil
	}
	return result, nil, nil
}

// ChoiceFor returns the most-specific choice applying to path, or the empty
// Side when no choice applies.
func ChoiceFor(path Path, choices map[Path]Side) Side {
	if len(choices) == 0 {
		return ""
	}
	for {
		if side, ok := choices[path]; ok {
			return side
		}
		if path.IsRoot() {
			return ""
		}
		path = path.Parent()
	}
}

func entryOrAbsent(tree map[Path]Entry, path Path) Entry {
	if entry, ok := tree[path]; ok {
		return entry
	}
	return Entry{Kind: Absent}
}

func completeTree(tree map[Path]Entry) (map[Path]Entry, error) {
	complete := make(map[Path]Entry, len(tree)+1)
	for path, entry := range tree {
		complete[path] = entry
	}
	if len(complete) == 0 {
		complete[Path{}] = Entry{Kind: Directory}
	}
	if err := validateTree(complete); err != nil {
		return nil, err
	}
	return complete, nil
}

func sortPaths(paths []Path) {
	sort.Slice(paths, func(i, j int) bool {
		return paths[i].String() < paths[j].String()
	})
}
