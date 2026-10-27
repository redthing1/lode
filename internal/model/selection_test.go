package model

import "testing"

func TestSelectionSubtreePoliciesAndIntersections(t *testing.T) {
	root := Path{}
	assets := mustPath(t, "assets")
	scene := mustPath(t, "assets/scene")
	texture := mustPath(t, "assets/textures")
	neighbor := mustPath(t, "assets-old")

	selection := Selection{}.Exclude(assets).Include(scene)
	if selection.Includes(assets) || selection.Includes(texture) || !selection.Includes(scene) {
		t.Fatal("specific subtree policies did not override the root policy")
	}
	if !selection.Intersects(assets) || !selection.Intersects(root) {
		t.Fatal("excluded ancestor should intersect an included descendant")
	}
	if selection.Intersects(texture) || selection.Includes(neighbor) {
		t.Fatal("selection leaked into an excluded or adjacent subtree")
	}

	altered := selection.Exclude(scene).Include(mustPath(t, "elsewhere"))
	if selection.Includes(root) || !selection.Includes(scene) || altered.Includes(scene) || !altered.Includes(mustPath(t, "elsewhere")) {
		t.Fatal("returned selection should be independent of its source")
	}
}

func TestSelectionReplacesSubtreeAndNormalizes(t *testing.T) {
	assets := mustPath(t, "assets")
	scene := mustPath(t, "assets/scene")
	texture := mustPath(t, "assets/textures")

	s := Selection{}.Include(assets).Exclude(scene).Include(texture).Include(mustPath(t, "z"))
	if got := s.Rules(); len(got) != 3 {
		t.Fatalf("Rules() = %#v, want three effective rules", got)
	}
	replaced := s.Include(assets)
	if got := replaced.Rules(); len(got) != 2 || got[0].Path != assets || !got[0].Included || got[1].Path.String() != "z" {
		t.Fatalf("including subtree did not clear narrower policies: %#v", got)
	}
	if !replaced.Includes(scene) || !replaced.Includes(texture) {
		t.Fatal("replacement policy should cover the whole subtree")
	}

	first := Selection{}.Include(assets).Include(scene)
	second := Selection{}.Include(assets)
	if !sameRules(first.Rules(), second.Rules()) {
		t.Fatalf("redundant rule was retained: %#v", first.Rules())
	}
	if !sameRules(second.Include(assets).Rules(), second.Rules()) {
		t.Fatal("repeating the same policy should be idempotent")
	}
	if got := (Selection{}).Exclude(assets).Rules(); len(got) != 0 {
		t.Fatalf("default exclusion should normalize away: %#v", got)
	}
}

func TestSelectionRulesAreSortedCopies(t *testing.T) {
	s := Selection{}.Include(Path{}).Exclude(mustPath(t, "a")).Include(mustPath(t, "a/b"))
	rules := s.Rules()
	if len(rules) != 3 || !rules[0].Path.IsRoot() || rules[1].Path.String() != "a" || rules[2].Path.String() != "a/b" {
		t.Fatalf("Rules() is not root-first lexical: %#v", rules)
	}
	rules[0].Included = false
	rules[0].Path = mustPath(t, "changed")
	rules = append(rules, Rule{Path: Path{}, Included: true})
	if got := s.Rules(); !got[0].Path.IsRoot() || !got[0].Included {
		t.Fatalf("mutating Rules() result changed selection: %#v", got)
	}
}

func TestSelectionNestedOverridesAndRootReset(t *testing.T) {
	root := Path{}
	a := mustPath(t, "a")
	child := mustPath(t, "a/b")
	deep := mustPath(t, "a/b/deeper")

	s := Selection{}.Include(root).Exclude(a).Include(child).Exclude(deep).Include(deep)
	if s.Includes(a) || !s.Includes(child) || !s.Includes(deep) {
		t.Fatal("nested exclude and reinclude rules were not applied")
	}
	reset := s.Include(root)
	if len(reset.Rules()) != 1 || !reset.Includes(a) || !reset.Includes(deep) {
		t.Fatalf("including root should clear every narrower override: %#v", reset.Rules())
	}
	if len(s.Rules()) != 3 || s.Includes(a) {
		t.Fatal("prior selection changed or retained a redundant reinclude rule")
	}
	empty := s.Exclude(root)
	if len(empty.Rules()) != 0 || empty.Includes(root) || empty.Intersects(root) {
		t.Fatalf("excluding root must restore the empty selection: %#v", empty.Rules())
	}
}

func FuzzSelectionOperations(f *testing.F) {
	f.Add([]byte{1, 2, 5, 6, 3})
	f.Add([]byte{5, 9, 2, 1, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 128 {
			return
		}
		paths := []Path{
			{},
			mustPath(t, "a"),
			mustPath(t, "a-b"),
			mustPath(t, "a/b"),
			mustPath(t, "a/b/deeper"),
		}
		// An explicit small tree supplies an oracle independent of Path.Contains
		// and selection's representation of inherited rules.
		parents := []int{-1, 0, 0, 1, 3}
		contains := func(parent, child int) bool {
			for child >= 0 {
				if parent == child {
					return true
				}
				child = parents[child]
			}
			return false
		}
		var selection Selection
		want := make([]bool, len(paths))
		for step, operation := range operations {
			index := int(operation>>1) % len(paths)
			included := operation&1 != 0
			previous := selection
			beforeWant := append([]bool(nil), want...)
			if included {
				selection = selection.Include(paths[index])
			} else {
				selection = selection.Exclude(paths[index])
			}
			for i := range paths {
				if contains(index, i) {
					want[i] = included
				}
			}
			wantRules := make(map[Path]bool)
			for i, path := range paths {
				inherited := false
				if parents[i] >= 0 {
					inherited = want[parents[i]]
				}
				if want[i] != inherited {
					wantRules[path] = want[i]
				}
			}
			rules := selection.Rules()
			if len(rules) != len(wantRules) {
				t.Fatalf("step %d rules = %#v, want %#v", step, rules, wantRules)
			}
			for _, rule := range rules {
				if included, ok := wantRules[rule.Path]; !ok || included != rule.Included {
					t.Fatalf("step %d unexpected rule: %#v", step, rule)
				}
			}
			for i, path := range paths {
				if got := selection.Includes(path); got != want[i] {
					t.Fatalf("step %d Includes(%q) = %t, want %t", step, path.String(), got, want[i])
				}
				intersects := false
				for j := range paths {
					if contains(i, j) && want[j] {
						intersects = true
						break
					}
				}
				if got := selection.Intersects(path); got != intersects {
					t.Fatalf("step %d Intersects(%q) = %t, want %t", step, path.String(), got, intersects)
				}
				if got := previous.Includes(path); got != beforeWant[i] {
					t.Fatalf("step %d prior selection changed at %q", step, path.String())
				}
			}
		}
	})
}

func sameRules(a, b []Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
