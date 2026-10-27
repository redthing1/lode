package model

import "sort"

// Rule records an effective inclusion policy for a path and its subtree.
type Rule struct {
	Path     Path
	Included bool
}

// Selection is an immutable set of subtree policies. The zero value selects
// nothing. More specific rules override their ancestors.
type Selection struct {
	rules map[Path]bool
}

// Include selects path and its entire subtree, replacing any narrower rules.
func (s Selection) Include(path Path) Selection {
	return s.set(path, true)
}

// Exclude excludes path and its entire subtree, replacing any narrower rules.
func (s Selection) Exclude(path Path) Selection {
	return s.set(path, false)
}

func (s Selection) set(path Path, included bool) Selection {
	out := Selection{rules: make(map[Path]bool, len(s.rules)+1)}
	for rulePath, value := range s.rules {
		if !path.Contains(rulePath) {
			out.rules[rulePath] = value
		}
	}
	if out.Includes(path) != included {
		out.rules[path] = included
	}
	return out
}

// Includes reports the effective policy at path.
func (s Selection) Includes(path Path) bool {
	for {
		if included, ok := s.rules[path]; ok {
			return included
		}
		if path.IsRoot() {
			return false
		}
		path = path.Parent()
	}
}

// Rules returns a root-first, lexical snapshot of the effective rules.
func (s Selection) Rules() []Rule {
	rules := make([]Rule, 0, len(s.rules))
	for path, included := range s.rules {
		rules = append(rules, Rule{Path: path, Included: included})
	}
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].Path.String() < rules[j].Path.String()
	})
	return rules
}

// Intersects reports whether path or any descendant is included. It lets a
// tree walker descend through an excluded ancestor to reach an included child.
func (s Selection) Intersects(path Path) bool {
	if s.Includes(path) {
		return true
	}
	for rulePath, included := range s.rules {
		if included && path.Contains(rulePath) {
			return true
		}
	}
	return false
}

// Union selects every path included by either selection.
func (s Selection) Union(other Selection) Selection {
	return combineSelections(s, other, func(a, b bool) bool { return a || b })
}

// Intersection selects only paths included by both selections.
func (s Selection) Intersection(other Selection) Selection {
	return combineSelections(s, other, func(a, b bool) bool { return a && b })
}

func combineSelections(a, b Selection, included func(bool, bool) bool) Selection {
	boundaries := map[Path]bool{{}: true}
	for p := range a.rules {
		boundaries[p] = true
	}
	for p := range b.rules {
		boundaries[p] = true
	}
	ordered := make([]Path, 0, len(boundaries))
	for p := range boundaries {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() < ordered[j].String() })
	var result Selection
	for _, p := range ordered {
		result = result.set(p, included(a.Includes(p), b.Includes(p)))
	}
	return result
}
