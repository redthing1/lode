package model

import "testing"

func TestIntersectionPreservesNestedSelectionRules(t *testing.T) {
	selections := []Selection{
		{}, {{Include: true}},
		{{Path: "assets", Include: true}, {Path: "assets/cache"}, {Path: "assets/cache/keep", Include: true}},
		{{Include: true}, {Path: "assets"}, {Path: "assets/model", Include: true}},
		{{Path: "assets/cache", Include: true}, {Path: "references/a", Include: true}},
	}
	for _, a := range selections {
		for _, b := range selections {
			got := a.Intersect(b)
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"", "assets", "assets.aux", "assets/model", "assets/model/file", "assets/cache", "assets/cache/temp", "assets/cache/keep", "assets/cache/keep/a", "references", "references/a", "references/b"} {
				if got.Includes(name) != (a.Includes(name) && b.Includes(name)) {
					t.Fatalf("intersection %v and %v at %q: %v", a, b, name, got)
				}
			}
		}
	}
}
