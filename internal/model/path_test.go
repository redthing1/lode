package model

import (
	"errors"
	"testing"
)

func TestParsePathCanonicalPaths(t *testing.T) {
	valid := []string{"", "assets", "assets/scene.blend", "Hero", "hero", "café/mesh", "cafe\u0301/mesh", "資料/模型"}
	for _, value := range valid {
		path, err := ParsePath(value)
		if err != nil {
			t.Errorf("ParsePath(%q): %v", value, err)
			continue
		}
		if path.String() != value {
			t.Errorf("ParsePath(%q).String() = %q", value, path.String())
		}
	}
	if got := (Path{}).String(); got != "" || !(Path{}).IsRoot() {
		t.Fatalf("zero Path should be root, got %q", got)
	}
	if mustPath(t, "Hero") == mustPath(t, "hero") || mustPath(t, "café/mesh") == mustPath(t, "cafe\u0301/mesh") {
		t.Fatal("path comparison or storage changed case or Unicode text")
	}

	invalid := []string{
		"/assets", "assets/", "assets//mesh", ".", "..", "assets/./mesh",
		"assets/../mesh", "bad\\name", "bad\x00name", string([]byte{0xff}),
	}
	for _, value := range invalid {
		if _, err := ParsePath(value); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ParsePath(%q) error = %v, want ErrInvalidPath", value, err)
		}
	}
}

func TestPathParentAndComponentAncestry(t *testing.T) {
	root := mustPath(t, "")
	assets := mustPath(t, "assets")
	child := mustPath(t, "assets/scene.blend")
	neighbor := mustPath(t, "assets-old/scene.blend")
	if root.Parent() != root || assets.Parent() != root || child.Parent() != assets {
		t.Fatal("Parent did not return the component parent")
	}
	if !assets.Contains(assets) || !assets.Contains(child) || assets.Contains(neighbor) || child.Contains(assets) {
		t.Fatal("Contains did not respect equality and component boundaries")
	}
	if !root.Contains(neighbor) {
		t.Fatal("root should contain every path")
	}
}

func FuzzParsePathRoundTrip(f *testing.F) {
	for _, seed := range []string{"", "a", "a/b", "assets-old", "café/mesh"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		path, err := ParsePath(value)
		if err == nil {
			if path.String() != value {
				t.Fatalf("round trip changed %q to %q", value, path.String())
			}
			parent := path.Parent()
			if !parent.Contains(path) {
				t.Fatalf("parent %q does not contain %q", parent.String(), value)
			}
		}
	})
}

func mustPath(t *testing.T, value string) Path {
	t.Helper()
	path, err := ParsePath(value)
	if err != nil {
		t.Fatalf("ParsePath(%q): %v", value, err)
	}
	return path
}
