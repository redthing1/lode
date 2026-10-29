package model

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type Side string

const (
	Local    Side = "local"
	Incoming Side = "incoming"
)

type Conflict struct {
	Path                  string
	Base, Local, Incoming Kind
}

// Reconcile combines independent changes and reports whole-entry conflicts.
// A directory/file or delete/edit conflict covers the entire affected subtree.
// Choices must name actual conflicts; unresolved conflicts produce no result.
func Reconcile(base, local, incoming []Entry, choices map[string]Side) ([]Entry, []Conflict, error) {
	var trees [3]*entryTree
	for i, entries := range [][]Entry{base, local, incoming} {
		tree, err := makeTree(entries)
		if err != nil {
			return nil, nil, err
		}
		trees[i] = tree
	}
	for name, side := range choices {
		if !ValidPath(name) || side != Local && side != Incoming {
			return nil, nil, fmt.Errorf("invalid conflict choice for %q", name)
		}
	}
	used := map[string]bool{}
	var conflicts []Conflict
	var merge func(*entryTree, *entryTree, *entryTree) *entryTree
	merge = func(b, l, r *entryTree) *entryTree {
		switch {
		case sameTree(l, r):
			return l
		case sameTree(b, l):
			return r
		case sameTree(b, r):
			return l
		case treeKind(l) == Directory && treeKind(r) == Directory:
			result := &entryTree{entry: l.entry, children: map[string]*entryTree{}}
			for _, name := range childNames(b, l, r) {
				if child := merge(treeChild(b, name), treeChild(l, name), treeChild(r, name)); child != nil {
					result.children[name] = child
				}
			}
			return result
		default:
			name := ""
			for _, t := range []*entryTree{b, l, r} {
				if t != nil {
					name = t.entry.Path
					break
				}
			}
			if side, ok := choices[name]; ok {
				used[name] = true
				if side == Local {
					return l
				}
				return r
			}
			conflicts = append(conflicts, Conflict{Path: name, Base: treeKind(b), Local: treeKind(l), Incoming: treeKind(r)})
			return nil
		}
	}
	result := merge(trees[0], trees[1], trees[2])
	for name := range choices {
		if !used[name] {
			return nil, nil, fmt.Errorf("%q is not a conflict", name)
		}
	}
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}
	return flattenTree(result), nil, nil
}

// Materialized includes the ordinary ancestor directories needed to place a
// selected inventory on disk. It does not add unselected file contents.
func Materialized(entries []Entry) ([]Entry, error) {
	tree, err := makeTree(entries)
	if err != nil {
		return nil, err
	}
	return flattenTree(tree), nil
}

type entryTree struct {
	entry    Entry
	children map[string]*entryTree
}

func makeTree(entries []Entry) (*entryTree, error) {
	if err := ValidateInventory(entries); err != nil {
		return nil, err
	}
	root := &entryTree{entry: Entry{Kind: Directory}, children: map[string]*entryTree{}}
	for _, e := range entries {
		node, prefix := root, ""
		for _, name := range strings.Split(e.Path, "/") {
			prefix = path.Join(prefix, name)
			if node.children[name] == nil {
				node.children[name] = &entryTree{entry: Entry{Path: prefix, Kind: Directory}, children: map[string]*entryTree{}}
			}
			node = node.children[name]
		}
		node.entry = e
	}
	return root, nil
}
func treeKind(t *entryTree) Kind {
	if t == nil {
		return Absent
	}
	return t.entry.Kind
}
func treeChild(t *entryTree, name string) *entryTree {
	if t == nil {
		return nil
	}
	return t.children[name]
}
func sameTree(a, b *entryTree) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.entry != b.entry || len(a.children) != len(b.children) {
		return false
	}
	for name, child := range a.children {
		if !sameTree(child, b.children[name]) {
			return false
		}
	}
	return true
}
func childNames(trees ...*entryTree) []string {
	seen := map[string]bool{}
	for _, tree := range trees {
		if tree != nil {
			for name := range tree.children {
				seen[name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func flattenTree(tree *entryTree) []Entry {
	var result []Entry
	var visit func(*entryTree)
	visit = func(t *entryTree) {
		if t == nil {
			return
		}
		if t.entry.Path != "" {
			result = append(result, t.entry)
		}
		for _, name := range childNames(t) {
			visit(t.children[name])
		}
	}
	visit(tree)
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

type Change struct {
	Before Entry `json:"before"`
	After  Entry `json:"after"`
}

// PlanChanges removes old children before changing their parents, then creates
// new parents before children. File replacements remain one reversible action.
func PlanChanges(before, after []Entry) ([]Change, error) {
	for _, entries := range [][]Entry{before, after} {
		if err := ValidateInventory(entries); err != nil {
			return nil, err
		}
	}
	old, next := map[string]Entry{}, map[string]Entry{}
	for _, e := range before {
		old[e.Path] = e
	}
	for _, e := range after {
		next[e.Path] = e
	}
	var remove, put []Change
	for name, e := range old {
		if target, exists := next[name]; !exists || target.Kind != e.Kind {
			remove = append(remove, Change{Before: e, After: Entry{Path: name, Kind: Absent}})
		}
	}
	for name, e := range next {
		previous, exists := old[name]
		if !exists || previous.Kind != e.Kind {
			previous = Entry{Path: name, Kind: Absent}
		}
		if previous != e {
			put = append(put, Change{Before: previous, After: e})
		}
	}
	less := func(a, b Change) bool {
		x, y := a.Before.Path, b.Before.Path
		if dx, dy := strings.Count(x, "/"), strings.Count(y, "/"); dx != dy {
			return dx < dy
		}
		return x < y
	}
	sort.Slice(remove, func(i, j int) bool { return less(remove[j], remove[i]) })
	sort.Slice(put, func(i, j int) bool { return less(put[i], put[j]) })
	return append(remove, put...), nil
}

func Selected(entries []Entry, selection Selection) []Entry {
	var result []Entry
	for _, e := range entries {
		if selection.Includes(e.Path) {
			result = append(result, e)
		}
	}
	return result
}
