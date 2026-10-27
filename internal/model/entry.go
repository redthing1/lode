// Package model defines project entries and selection without IO.
package model

import (
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Kind string

const (
	File      Kind = "file"
	Directory Kind = "directory"
	Absent    Kind = "absent"
)

type Entry struct {
	Path   string `json:"path"`
	Kind   Kind   `json:"kind"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// ValidPath accepts canonical project-relative paths. Root is implicit.
func ValidPath(p string) bool {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || path.Clean(p) != p || !utf8.ValidString(p) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	if p == ".lode" || strings.HasPrefix(p, ".lode/") {
		return false
	}
	return strings.IndexFunc(p, unicode.IsControl) < 0
}

func (e Entry) Validate() error {
	if !ValidPath(e.Path) {
		return fmt.Errorf("invalid project path %q", e.Path)
	}
	switch e.Kind {
	case File:
		hash, err := hex.DecodeString(e.SHA256)
		if e.Size < 0 || err != nil || len(hash) != 32 || hex.EncodeToString(hash) != e.SHA256 {
			return fmt.Errorf("invalid file identity for %q", e.Path)
		}
	case Directory, Absent:
		if e.Size != 0 || e.SHA256 != "" {
			return fmt.Errorf("non-file %q has file contents", e.Path)
		}
	default:
		return fmt.Errorf("invalid entry kind %q", e.Kind)
	}
	return nil
}

type Rule struct {
	Path    string `json:"path"`
	Include bool   `json:"include"`
}

type Selection []Rule

func (s Selection) Validate() error {
	seen := map[string]bool{}
	for _, r := range s {
		if r.Path != "" && !ValidPath(r.Path) {
			return fmt.Errorf("invalid selection path %q", r.Path)
		}
		if seen[r.Path] {
			return fmt.Errorf("duplicate selection path %q", r.Path)
		}
		seen[r.Path] = true
	}
	return nil
}

// Includes uses the nearest ancestor rule; an empty rule path is the root.
func (s Selection) Includes(p string) bool {
	depth, included := -1, false
	for _, r := range s {
		if len(r.Path) > depth && (r.Path == "" || p == r.Path || strings.HasPrefix(p, r.Path+"/")) {
			depth, included = len(r.Path), r.Include
		}
	}
	return included
}

// Descends reports whether walking a directory can find selected entries.
func (s Selection) Descends(p string) bool {
	if s.Includes(p) {
		return true
	}
	for _, r := range s {
		if r.Include && (p == "" || strings.HasPrefix(r.Path, p+"/")) {
			return true
		}
	}
	return false
}

// Apply changes exact paths. Deleting a directory does not implicitly delete
// its descendants, which may be absent from a partial workspace.
func Apply(base, changes []Entry) ([]Entry, error) {
	tree := make(map[string]Entry, len(base)+len(changes))
	for _, e := range base {
		tree[e.Path] = e
	}
	changed := make(map[string]Kind, len(changes))
	for _, e := range changes {
		if err := e.Validate(); err != nil {
			return nil, err
		}
		if _, ok := changed[e.Path]; ok {
			return nil, fmt.Errorf("duplicate change %q", e.Path)
		}
		changed[e.Path] = e.Kind
		if e.Kind == Absent {
			delete(tree, e.Path)
		} else {
			tree[e.Path] = e
		}
	}
	for _, e := range changes {
		if e.Kind == Absent {
			continue
		}
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if changed[parent] == Absent {
				return nil, fmt.Errorf("%q needs deleted directory %q", e.Path, parent)
			}
			if _, ok := tree[parent]; !ok {
				tree[parent] = Entry{Path: parent, Kind: Directory}
			}
		}
	}
	result := make([]Entry, 0, len(tree))
	for _, e := range tree {
		result = append(result, e)
	}
	if err := ValidateTree(result); err != nil {
		return nil, err
	}
	Sort(result)
	return result, nil
}

func Sort(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
}

// ValidateTree rejects structural conflicts rather than deleting unseen entries.
func ValidateTree(entries []Entry) error {
	byPath := make(map[string]Kind, len(entries))
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.Kind == Absent {
			return fmt.Errorf("absent entry in complete tree: %s", e.Path)
		}
		if _, exists := byPath[e.Path]; exists {
			return fmt.Errorf("duplicate entry %q", e.Path)
		}
		byPath[e.Path] = e.Kind
	}
	for _, e := range entries {
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if byPath[parent] != Directory {
				return fmt.Errorf("%q requires directory %q", e.Path, parent)
			}
		}
	}
	return nil
}
