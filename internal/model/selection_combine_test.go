package model

import (
	"reflect"
	"testing"
)

func TestSelectionUnionAndIntersectionSemantics(t *testing.T) {
	path := func(value string) Path {
		t.Helper()
		result, err := ParsePath(value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	left := Selection{}.
		Include(Path{}).
		Exclude(path("assets")).
		Exclude(path("assets-old")).
		Include(path("assets/scene")).
		Exclude(path("assets/scene/private")).
		Include(path("assets/scene/private/open"))
	right := Selection{}.
		Include(path("assets/textures")).
		Include(path("assets/scene/private")).
		Exclude(path("assets-old")).
		Include(path("outside"))

	leftRules := append([]Rule(nil), left.Rules()...)
	rightRules := append([]Rule(nil), right.Rules()...)
	union := left.Union(right)
	intersection := left.Intersection(right)

	probes := []Path{
		{}, path("assets"), path("assets/scene"), path("assets/scene/private"),
		path("assets/scene/private/open"), path("assets/scene/private/open/file"),
		path("assets/scene/other"), path("assets/textures"), path("assets/textures/albedo"),
		path("assets-old"), path("assets-old/neighbor"), path("outside"), path("outside/file"),
		path("unselected"),
	}
	for _, candidate := range probes {
		if got, want := union.Includes(candidate), left.Includes(candidate) || right.Includes(candidate); got != want {
			t.Errorf("union Includes(%q)=%t, want %t", candidate, got, want)
		}
		if got, want := intersection.Includes(candidate), left.Includes(candidate) && right.Includes(candidate); got != want {
			t.Errorf("intersection Includes(%q)=%t, want %t", candidate, got, want)
		}
		var wantUnionIntersects, wantIntersectionIntersects bool
		for _, descendant := range probes {
			if !candidate.Contains(descendant) {
				continue
			}
			if left.Includes(descendant) || right.Includes(descendant) {
				wantUnionIntersects = true
			}
			if left.Includes(descendant) && right.Includes(descendant) {
				wantIntersectionIntersects = true
			}
		}
		if got := union.Intersects(candidate); got != wantUnionIntersects {
			t.Errorf("union Intersects(%q)=%t, want %t", candidate, got, wantUnionIntersects)
		}
		if got := intersection.Intersects(candidate); got != wantIntersectionIntersects {
			t.Errorf("intersection Intersects(%q)=%t, want %t", candidate, got, wantIntersectionIntersects)
		}
	}
	if !reflect.DeepEqual(left.Rules(), leftRules) || !reflect.DeepEqual(right.Rules(), rightRules) {
		t.Fatal("selection combination mutated an input")
	}
	if union.Includes(path("assets-old/neighbor")) || !union.Includes(path("assets/textures/albedo")) || intersection.Includes(path("assets/scene/private/closed")) {
		t.Fatal("component neighbors, nested re-includes, or unselected paths were combined incorrectly")
	}
}
