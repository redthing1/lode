package model

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestSelectionUsesNearestAncestorRule(t *testing.T) {
	selection := Selection{
		{Path: "", Include: true},
		{Path: "assets", Include: false},
		{Path: "assets/shared", Include: true},
		{Path: "assets/shared/private", Include: false},
	}
	for path, want := range map[string]bool{
		"README":                      true,
		"assets/a.bin":                false,
		"assets/shared/a.bin":         true,
		"assets/shared/private/a.bin": false,
		"assetset/a.bin":              true,
	} {
		if got := selection.Includes(path); got != want {
			t.Errorf("Includes(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestValidateTreeRejectsStructuralConflicts(t *testing.T) {
	hash := sha256.Sum256([]byte("x"))
	file := Entry{Path: "a", Kind: File, Size: 1, SHA256: hex.EncodeToString(hash[:])}
	tests := []struct {
		name    string
		entries []Entry
	}{
		{"file used as parent", []Entry{file, {Path: "a/b", Kind: Directory}}},
		{"missing parent", []Entry{{Path: "a/b", Kind: Directory}}},
		{"absent cannot be in complete tree", []Entry{{Path: "a", Kind: Absent}}},
		{"duplicate path", []Entry{{Path: "a", Kind: Directory}, {Path: "a", Kind: Directory}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateTree(tt.entries); err == nil {
				t.Fatal("ValidateTree accepted an invalid complete tree")
			}
		})
	}
	if err := ValidateTree([]Entry{{Path: "a", Kind: Directory}, {Path: "a/b", Kind: File, Size: 1, SHA256: hex.EncodeToString(hash[:])}}); err != nil {
		t.Fatalf("ValidateTree rejected a structurally valid tree: %v", err)
	}
}

func TestApplySupportsExplicitFileDirectoryReplacement(t *testing.T) {
	old := []byte("old")
	oldHash := sha256.Sum256(old)
	oldFile := Entry{Path: "node", Kind: File, Size: int64(len(old)), SHA256: hex.EncodeToString(oldHash[:])}
	child := []byte("child")
	childHash := sha256.Sum256(child)
	childFile := Entry{Path: "node/child", Kind: File, Size: int64(len(child)), SHA256: hex.EncodeToString(childHash[:])}
	base := []Entry{oldFile}
	got, err := Apply(base, []Entry{{Path: "node", Kind: Directory}, childFile})
	if err != nil {
		t.Fatalf("file to directory: %v", err)
	}
	if len(got) != 2 || got[0].Path != "node" || got[0].Kind != Directory || got[1] != childFile {
		t.Fatalf("file to directory result = %#v", got)
	}
	got, err = Apply(got, []Entry{oldFile, {Path: "node/child", Kind: Absent}})
	if err != nil {
		t.Fatalf("directory to file: %v", err)
	}
	if len(got) != 1 || got[0] != oldFile {
		t.Fatalf("directory to file result = %#v", got)
	}
}

func TestApplyDoesNotImplicitlyDeleteHiddenDescendants(t *testing.T) {
	base := []Entry{{Path: "tree", Kind: Directory}, {Path: "tree/hidden", Kind: Directory}}
	if _, err := Apply(base, []Entry{{Path: "tree", Kind: Absent}}); err == nil {
		t.Fatal("deleting a directory implicitly removed an omitted descendant")
	}
}
