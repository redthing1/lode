package model

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type Change struct {
	Before Entry `json:"before"`
	After  Entry `json:"after"`
}

// Choice takes one side for a path and its descendants. The nearest choice
// wins, so structural file/directory conflicts have the same whole-entry rule.
type Choice struct {
	Path string
	Ours bool
}

type Conflicts struct{ Paths []string }

func (c *Conflicts) Error() string { return "conflicting changes: " + strings.Join(c.Paths, ", ") }

func entryAt(entries map[string]Entry, name string) Entry {
	if e, ok := entries[name]; ok {
		return e
	}
	return Entry{Path: name, Kind: Absent}
}

func index(entries []Entry) map[string]Entry {
	result := make(map[string]Entry, len(entries))
	for _, e := range entries {
		result[e.Path] = e
	}
	return result
}

// Diff changes exact entries; it does not imply recursive directory removal.
func Diff(before, after []Entry) []Change {
	a, b := index(before), index(after)
	names := map[string]bool{}
	for p := range a {
		names[p] = true
	}
	for p := range b {
		names[p] = true
	}
	var changes []Change
	for p := range names {
		old, new := entryAt(a, p), entryAt(b, p)
		if old != new {
			changes = append(changes, Change{Before: old, After: new})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].After.Path < changes[j].After.Path })
	return changes
}

// Merge compares opaque entries. Selected inventories may omit their parent
// directories; complete them for structural comparison before merging.
func Merge(base, ours, theirs []Entry, choices []Choice) ([]Entry, error) {
	inputs := [][]Entry{base, ours, theirs}
	trees := make([]map[string]Entry, 0, 3)
	names := map[string]bool{}
	for _, entries := range inputs {
		complete, err := Apply(nil, entries)
		if err != nil {
			return nil, err
		}
		tree := index(complete)
		trees = append(trees, tree)
		for p := range tree {
			names[p] = true
		}
	}
	seen := map[string]bool{}
	for _, choice := range choices {
		if (choice.Path != "" && !ValidPath(choice.Path)) || seen[choice.Path] {
			return nil, fmt.Errorf("invalid or duplicate choice %q", choice.Path)
		}
		seen[choice.Path] = true
	}
	merged := map[string]Entry{}
	conflicts := map[string]bool{}
	for p := range names {
		b, o, t := entryAt(trees[0], p), entryAt(trees[1], p), entryAt(trees[2], p)
		chosen, depth := -1, -1
		for i, c := range choices {
			if len(c.Path) > depth && (c.Path == "" || p == c.Path || strings.HasPrefix(p, c.Path+"/")) {
				chosen, depth = i, len(c.Path)
			}
		}
		var result Entry
		switch {
		case chosen >= 0:
			result = t
			if choices[chosen].Ours {
				result = o
			}
		case o == b:
			result = t
		case t == b || o == t:
			result = o
		default:
			conflicts[p] = true
			continue
		}
		if result.Kind != Absent {
			merged[p] = result
		}
	}
	for p := range merged {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if merged[parent].Kind != Directory {
				conflicts[parent] = true
			}
		}
	}
	if len(conflicts) > 0 {
		paths := make([]string, 0, len(conflicts))
		for p := range conflicts {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		return nil, &Conflicts{Paths: paths}
	}
	result := make([]Entry, 0, len(merged))
	for _, e := range merged {
		result = append(result, e)
	}
	Sort(result)
	return result, nil
}
