package model

import (
	"slices"
	"strings"
)

// Rule includes or excludes a path and its descendants.
type Rule struct {
	Path    Path
	Include bool
}

// Selection describes which project paths belong in a workspace. Its zero value
// selects nothing. Selection values can be copied and used without shared mutation.
type Selection struct {
	rules map[Path]bool
}

// Includes reports the most specific rule for p, defaulting to exclusion.
func (s Selection) Includes(p Path) bool {
	for {
		if include, ok := s.rules[p]; ok {
			return include
		}
		if p == (Path{}) {
			return false
		}
		p = p.Parent()
	}
}

// Intersects reports whether p or any descendant is selected. A directory can
// require traversal even when the directory itself is excluded.
func (s Selection) Intersects(p Path) bool {
	if s.Includes(p) {
		return true
	}
	for rule, include := range s.rules {
		if include && p.Contains(rule) {
			return true
		}
	}
	return false
}

// Include returns a selection including p and clears narrower rules beneath it.
func (s Selection) Include(p Path) Selection {
	return s.with(p, true)
}

// Exclude returns a selection excluding p and clears narrower rules beneath it.
func (s Selection) Exclude(p Path) Selection {
	return s.with(p, false)
}

func (s Selection) with(p Path, include bool) Selection {
	next := Selection{rules: make(map[Path]bool, len(s.rules)+1)}
	for rule, value := range s.rules {
		if !p.Contains(rule) {
			next.rules[rule] = value
		}
	}
	if next.Includes(p) != include {
		next.rules[p] = include
	}
	return next
}

// Rules returns normalized overrides, root first and then in lexical path order.
// Root exclusion is implicit; rules equal to their inherited state are omitted.
func (s Selection) Rules() []Rule {
	rules := make([]Rule, 0, len(s.rules))
	for p, include := range s.rules {
		rules = append(rules, Rule{Path: p, Include: include})
	}
	slices.SortFunc(rules, func(a, b Rule) int {
		return strings.Compare(a.Path.name, b.Path.name)
	})
	return rules
}
