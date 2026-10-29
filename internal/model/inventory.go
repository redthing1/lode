package model

import (
	"fmt"
	"path"
	"sort"
)

// ValidateInventory permits omitted parents in a selected inventory, but rejects
// conflicting entries and file ancestors before any local files are touched.
func ValidateInventory(entries []Entry) error {
	kinds := map[string]Kind{}
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return err
		}
		if _, ok := kinds[e.Path]; ok || e.Kind == Absent {
			return fmt.Errorf("invalid inventory entry %q", e.Path)
		}
		kinds[e.Path] = e.Kind
	}
	for _, e := range entries {
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if kinds[parent] == File {
				return fmt.Errorf("%q has file ancestor %q", e.Path, parent)
			}
		}
	}
	return nil
}

func Changes(base, observed []Entry, selection Selection) []Entry {
	before := map[string]Entry{}
	for _, e := range base {
		before[e.Path] = e
	}
	var changes []Entry
	for _, e := range observed {
		if before[e.Path] != e {
			changes = append(changes, e)
		}
		delete(before, e.Path)
	}
	for _, e := range before {
		if e.Kind == Directory && !selection.Covers(e.Path) {
			continue
		}
		changes = append(changes, Entry{Path: e.Path, Kind: Absent})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// ApplySelected updates exact baseline entries after an accepted save.
func ApplySelected(base, changes []Entry) []Entry {
	entries := map[string]Entry{}
	for _, e := range base {
		entries[e.Path] = e
	}
	for _, e := range changes {
		if e.Kind == Absent {
			delete(entries, e.Path)
		} else {
			entries[e.Path] = e
		}
	}
	result := make([]Entry, 0, len(entries))
	for _, e := range entries {
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}
