package model

import (
	"strings"
	"testing"
)

func TestPathsAndChanges(t *testing.T) {
	for _, p := range []string{"", ".", "../file", "dir/../file", "/file", "dir\\file", ".lode/state", "a\x00b"} {
		if ValidPath(p) {
			t.Errorf("accepted %q", p)
		}
	}
	file := func(path string) Entry { return Entry{Path: path, Kind: File, SHA256: strings.Repeat("0", 64)} }
	for _, changes := range [][]Entry{
		{file("a"), file("a")},
		{file("a"), file("a/b")},
		{{Path: "a", Kind: Absent}, file("a/b")},
	} {
		if err := ValidateChanges(changes); err == nil {
			t.Fatalf("accepted ambiguous changes: %v", changes)
		}
	}
	if err := ValidateChanges([]Entry{file("a"), {Path: "a/old", Kind: Absent}}); err != nil {
		t.Fatalf("explicit directory-to-file replacement: %v", err)
	}
}

func TestSelectionBoundaries(t *testing.T) {
	s := Selection{{Path: "assets", Include: true}, {Path: "assets/cache", Include: false}, {Path: "assets/cache/keep", Include: true}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"assets": true, "assets/a": true, "assets/cache/a": false, "assets/cache/keep/a": true, "references/a": false, "assets2/a": false} {
		if s.Includes(name) != want {
			t.Errorf("Includes(%q)", name)
		}
	}
	if !s.Descends("") || !s.Descends("assets/cache") || s.Descends("references") {
		t.Fatal("incorrect traversal")
	}
}

func TestSelectedInventoryAndDirectoryRemoval(t *testing.T) {
	file := Entry{Path: "d/file", Kind: File, SHA256: strings.Repeat("0", 64)}
	if err := ValidateInventory([]Entry{file}); err != nil {
		t.Fatal(err)
	}
	for _, entries := range [][]Entry{
		{file, file},
		{file, {Path: "d", Kind: File, SHA256: file.SHA256}},
		{{Path: "d", Kind: Absent}},
	} {
		if err := ValidateInventory(entries); err == nil {
			t.Fatalf("invalid inventory accepted: %v", entries)
		}
	}
	base := []Entry{{Path: "d", Kind: Directory}, file}
	partial := Selection{{Path: "d", Include: true}, {Path: "d/unseen", Include: false}}
	changes := Changes(base, nil, partial)
	if len(changes) != 1 || changes[0] != (Entry{Path: "d/file", Kind: Absent}) {
		t.Fatalf("partial deletion: %v", changes)
	}
	if got := ApplySelected(base, changes); len(got) != 1 || got[0].Kind != Directory {
		t.Fatalf("remaining directory: %v", got)
	}
	if got := Changes(base, nil, Selection{{Path: "d", Include: true}}); len(got) != 2 {
		t.Fatalf("complete deletion: %v", got)
	}
}
