package model

import (
	"fmt"
	"sort"
	"strings"
)

type Rule struct {
	Path    string `json:"path"`
	Include bool   `json:"include"`
}
type Selection []Rule

// Intersect keeps only paths included by both selections. Rule boundaries are
// sufficient; computing an intersection never requires an inventory of files.
func (s Selection) Intersect(other Selection) Selection {
	boundaries := map[string]bool{}
	for _, selection := range []Selection{s, other} {
		for _, rule := range selection {
			boundaries[rule.Path] = true
		}
	}
	names := make([]string, 0, len(boundaries))
	for name := range boundaries {
		names = append(names, name)
	}
	sort.Strings(names) // Ancestors precede their descendants.
	var result Selection
	for _, name := range names {
		include := s.Includes(name) && other.Includes(name)
		if result.Includes(name) != include {
			result = append(result, Rule{Path: name, Include: include})
		}
	}
	return result
}

func (s Selection) Validate() error {
	seen := map[string]bool{}
	for _, r := range s {
		if (r.Path != "" && !ValidPath(r.Path)) || seen[r.Path] {
			return fmt.Errorf("invalid or repeated selection path %q", r.Path)
		}
		seen[r.Path] = true
	}
	return nil
}

// Includes uses the nearest ancestor rule; an empty path names the root.
func (s Selection) Includes(name string) bool {
	depth, included := -1, false
	for _, r := range s {
		if len(r.Path) > depth && (r.Path == "" || r.Path == name || strings.HasPrefix(name, r.Path+"/")) {
			depth, included = len(r.Path), r.Include
		}
	}
	return included
}

func (s Selection) Descends(name string) bool {
	if s.Includes(name) {
		return true
	}
	for _, r := range s {
		if r.Include && (name == "" || strings.HasPrefix(r.Path, name+"/")) {
			return true
		}
	}
	return false
}

// Covers reports whether an entire subtree is selected, including descendants
// that have never been downloaded. Excluded descendants prevent directory removal.
func (s Selection) Covers(name string) bool {
	if !s.Includes(name) {
		return false
	}
	for _, rule := range s {
		if !rule.Include && (name == "" || strings.HasPrefix(rule.Path, name+"/")) {
			return false
		}
	}
	return true
}
