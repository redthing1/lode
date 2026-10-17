package model_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func mustPath(t testing.TB, name string) model.Path {
	t.Helper()
	p, err := model.ParsePath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPathPreservesCanonicalNames(t *testing.T) {
	for _, name := range []string{".", "assets", "assets/hero.blend", "asset set/hero.blend", "資料/模型", "sample:variant", "a/..b"} {
		t.Run(name, func(t *testing.T) {
			if got := mustPath(t, name).String(); got != name {
				t.Fatalf("path changed: got %q, want %q", got, name)
			}
		})
	}
	if (model.Path{}).String() != "." {
		t.Fatal("zero path must be the project root")
	}
	if mustPath(t, "Hero") == mustPath(t, "hero") {
		t.Fatal("project paths must preserve case")
	}
}

func TestPathRejectsTraversalAndAmbiguousNames(t *testing.T) {
	for _, name := range []string{"", "/", "/assets", "./assets", "..", "../assets", "assets/..", "assets/../other", "assets/.", "assets//hero", "assets/", "assets\\hero", "a\x00b", string([]byte{0xff})} {
		t.Run(name, func(t *testing.T) {
			if _, err := model.ParsePath(name); !errors.Is(err, model.ErrInvalidPath) {
				t.Fatalf("ParsePath(%q): got %v, want ErrInvalidPath", name, err)
			}
		})
	}
}

func TestPathHierarchy(t *testing.T) {
	for _, tc := range []struct {
		parent string
		child  string
		want   bool
	}{
		{".", ".", true},
		{".", "assets/hero", true},
		{"assets", ".", false},
		{"assets/hero", "assets/hero", true},
		{"assets/hero", "assets/hero/mesh", true},
		{"assets/hero", "assets/heroine", false},
		{"assets/hero", "assets/Hero/mesh", false},
	} {
		if got := mustPath(t, tc.parent).Contains(mustPath(t, tc.child)); got != tc.want {
			t.Errorf("%q contains %q: got %t, want %t", tc.parent, tc.child, got, tc.want)
		}
	}
	for _, tc := range []struct{ child, parent string }{
		{".", "."}, {"assets", "."}, {"assets/hero/mesh", "assets/hero"},
	} {
		if got := mustPath(t, tc.child).Parent().String(); got != tc.parent {
			t.Errorf("parent of %q: got %q, want %q", tc.child, got, tc.parent)
		}
	}
}

// Use component lists as an independent reference for subtree membership.
func referenceContains(parent, child string) bool {
	if parent == "." {
		return true
	}
	a, b := strings.Split(parent, "/"), strings.Split(child, "/")
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func FuzzPathHierarchy(f *testing.F) {
	f.Add("assets/hero", "assets/heroine")
	f.Add(".", "assets/hero")
	f.Add("資料", "資料/模型")
	f.Fuzz(func(t *testing.T, parent, child string) {
		p, err := model.ParsePath(parent)
		if err != nil {
			return
		}
		q, err := model.ParsePath(child)
		if err != nil {
			return
		}
		if got, want := p.Contains(q), referenceContains(parent, child); got != want {
			t.Fatalf("%q contains %q: got %t, want %t", parent, child, got, want)
		}
		if !q.Parent().Contains(q) {
			t.Fatalf("parent of %q does not contain it", child)
		}
		if parsed := mustPath(t, q.String()); parsed != q {
			t.Fatal("path did not round trip")
		}
	})
}
