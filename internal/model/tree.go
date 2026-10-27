package model

import (
	"errors"
	"fmt"
	"sort"
)

var ErrTreeConflict = errors.New("changes conflict with the complete project structure")

// ApplyChanges applies explicit whole-entry changes to a complete tree. A nil
// or empty base denotes an empty root directory. Other bases must contain root
// and every ancestor directory. Absence is represented by a missing map entry.
//
// Deletion removes only the named entry, never omitted descendants. Missing
// parents of new entries are created as directories unless the same change set
// explicitly deletes them. File/directory transitions must account for every
// affected descendant. Inputs are unchanged; the returned map is caller-owned.
func ApplyChanges(base, changes map[Path]Entry) (map[Path]Entry, error) {
	tree := make(map[Path]Entry, len(base)+len(changes)+1)
	for path, entry := range base {
		tree[path] = entry
	}
	if len(tree) == 0 {
		tree[Path{}] = Entry{Kind: Directory}
	}
	if err := validateTree(tree); err != nil {
		return nil, fmt.Errorf("invalid base: %w", err)
	}
	for _, path := range sortedPaths(changes) {
		entry := changes[path]
		if path.IsRoot() {
			return nil, fmt.Errorf("%w: project root cannot be replaced or deleted", ErrTreeConflict)
		}
		if err := entry.Validate(); err != nil {
			return nil, fmt.Errorf("change at %q: %w", path, err)
		}
		if entry.Kind == Absent {
			delete(tree, path)
		} else {
			tree[path] = entry
		}
	}
	for _, path := range sortedPaths(changes) {
		if changes[path].Kind == Absent {
			continue
		}
		for parent := path.Parent(); !parent.IsRoot(); parent = parent.Parent() {
			if deleted, ok := changes[parent]; ok && deleted.Kind == Absent {
				return nil, fmt.Errorf("%w: %q remains below deleted %q", ErrTreeConflict, path, parent)
			}
			if _, exists := tree[parent]; !exists {
				tree[parent] = Entry{Kind: Directory}
			}
		}
	}
	if err := validateTree(tree); err != nil {
		return nil, err
	}
	return tree, nil
}

func validateTree(tree map[Path]Entry) error {
	if tree[Path{}] != (Entry{Kind: Directory}) {
		return fmt.Errorf("%w: missing directory root", ErrTreeConflict)
	}
	for _, path := range sortedPaths(tree) {
		entry := tree[path]
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("entry at %q: %w", path, err)
		}
		if entry.Kind == Absent {
			return fmt.Errorf("%w: stored absence at %q", ErrTreeConflict, path)
		}
		if !path.IsRoot() && tree[path.Parent()].Kind != Directory {
			return fmt.Errorf("%w: %q has no directory parent", ErrTreeConflict, path)
		}
	}
	return nil
}

func sortedPaths(entries map[Path]Entry) []Path {
	paths := make([]Path, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].String() < paths[j].String() })
	return paths
}
