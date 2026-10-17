package model_test

import (
	"slices"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestSelectionSupportsPartialTraversal(t *testing.T) {
	hero := mustPath(t, "assets/hero")
	s := (model.Selection{}).Include(hero)
	for _, tc := range []struct {
		path                 string
		included, intersects bool
	}{
		{".", false, true},
		{"assets", false, true},
		{"assets/hero", true, true},
		{"assets/hero/mesh", true, true},
		{"assets/heroine", false, false},
		{"assets/shared", false, false},
	} {
		p := mustPath(t, tc.path)
		if got := s.Includes(p); got != tc.included {
			t.Errorf("Includes(%q): got %t, want %t", tc.path, got, tc.included)
		}
		if got := s.Intersects(p); got != tc.intersects {
			t.Errorf("Intersects(%q): got %t, want %t", tc.path, got, tc.intersects)
		}
	}
}

func TestSelectionResetsNarrowerOverrides(t *testing.T) {
	root, assets := model.Path{}, mustPath(t, "assets")
	hero, cache := mustPath(t, "assets/hero"), mustPath(t, "assets/hero/cache")
	s := (model.Selection{}).Include(root).Exclude(assets).Include(hero).Exclude(cache)
	if !s.Includes(hero) || s.Includes(cache) || s.Includes(mustPath(t, "assets/shared")) {
		t.Fatal("most specific overrides were not honored")
	}

	loaded := s.Include(assets)
	if !loaded.Includes(cache) || !loaded.Includes(mustPath(t, "assets/shared")) {
		t.Fatal("loading a parent must clear narrower exclusions")
	}
	if want := []model.Rule{{Path: root, Include: true}}; !slices.Equal(loaded.Rules(), want) {
		t.Fatalf("redundant include survived: %v", loaded.Rules())
	}

	unloaded := s.Exclude(assets)
	if unloaded.Intersects(assets) || unloaded.Includes(hero) {
		t.Fatal("unloading a parent must clear narrower includes")
	}
	if want := []model.Rule{{Path: root, Include: true}, {Path: assets, Include: false}}; !slices.Equal(unloaded.Rules(), want) {
		t.Fatalf("unexpected exclusions: %v", unloaded.Rules())
	}
	if empty := s.Exclude(root); empty.Intersects(root) || len(empty.Rules()) != 0 {
		t.Fatal("excluding the root must select nothing")
	}
}

func TestSelectionValuesAreIndependent(t *testing.T) {
	root, assets := model.Path{}, mustPath(t, "assets")
	full := (model.Selection{}).Include(root)
	partial := full.Exclude(assets)
	if !full.Includes(assets) || partial.Includes(assets) {
		t.Fatal("changing a copied selection changed its source")
	}
	rules := full.Rules()
	rules[0].Include = false
	if !full.Includes(assets) {
		t.Fatal("changing returned rules changed the selection")
	}
	var empty model.Selection
	if empty.Includes(root) || empty.Intersects(root) || len(empty.Exclude(assets).Rules()) != 0 {
		t.Fatal("the zero selection must select nothing without redundant exclusions")
	}
}

func TestSelectionRulesAreDeterministic(t *testing.T) {
	a, b := mustPath(t, "assets/hero"), mustPath(t, "assets/shared")
	first := (model.Selection{}).Include(b).Include(a)
	second := (model.Selection{}).Include(a).Include(b)
	want := []model.Rule{{Path: a, Include: true}, {Path: b, Include: true}}
	if !slices.Equal(first.Rules(), want) || !slices.Equal(second.Rules(), want) {
		t.Fatalf("equivalent selections have different rules: %v and %v", first.Rules(), second.Rules())
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
		names := []string{".", "assets", "assets/hero", "assets/hero/mesh", "assets/heroine", "assets/shared", "assets/shared/cache", "other", "asset", "assets/hero/future"}
		paths := make([]model.Path, len(names))
		for i, name := range names {
			paths[i] = mustPath(t, name)
		}
		var s model.Selection
		expected := make([]bool, len(paths))
		for step, operation := range operations {
			index, include := int(operation>>1)%len(paths), operation&1 != 0
			previous, previousRules := s, s.Rules()
			if include {
				s = s.Include(paths[index])
				if !slices.Equal(s.Include(paths[index]).Rules(), s.Rules()) {
					t.Fatal("include is not idempotent")
				}
			} else {
				s = s.Exclude(paths[index])
				if !slices.Equal(s.Exclude(paths[index]).Rules(), s.Rules()) {
					t.Fatal("exclude is not idempotent")
				}
			}
			if !slices.Equal(previous.Rules(), previousRules) {
				t.Fatal("an operation mutated the previous selection")
			}
			// Replay subtree assignments over explicit paths, without interpreting rules.
			for i, name := range names {
				if referenceContains(names[index], name) {
					expected[i] = include
				}
			}
			for i, p := range paths {
				if got := s.Includes(p); got != expected[i] {
					t.Fatalf("step %d, Includes(%q): got %t, want %t", step, names[i], got, expected[i])
				}
				intersects := false
				for j, name := range names {
					intersects = intersects || (expected[j] && referenceContains(names[i], name))
				}
				if got := s.Intersects(p); got != intersects {
					t.Fatalf("step %d, Intersects(%q): got %t, want %t", step, names[i], got, intersects)
				}
			}
		}
	})
}
