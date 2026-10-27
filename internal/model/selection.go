package model

import "sort"

// Intersect selects only paths included by both rule sets. The union of rule
// boundaries is enough because inclusion is constant between those boundaries.
func (s Selection) Intersect(other Selection) Selection {
	names := map[string]bool{"": true}
	for _, r := range s {
		names[r.Path] = true
	}
	for _, r := range other {
		names[r.Path] = true
	}
	result := make(Selection, 0, len(names))
	for name := range names {
		result = append(result, Rule{Path: name, Include: s.Includes(name) && other.Includes(name)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}
