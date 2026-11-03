package model

import (
	"reflect"
	"strings"
	"testing"
)

func TestReuseFilesMatchesContentsAcrossPaths(t *testing.T) {
	original := Entry{Path: "FILE.iso", Kind: File, Size: 42, SHA256: strings.Repeat("a", 64)}
	duplicate := original
	duplicate.Path = "duplicate"
	renamed := original
	renamed.Path = "FILE.ISO"
	wrongSize := original
	wrongSize.Path, wrongSize.Size = "size", 43
	wrongHash := original
	wrongHash.Path, wrongHash.SHA256 = "hash", strings.Repeat("b", 64)
	base := []Entry{duplicate, original, {Path: "directory", Kind: Directory}}
	changes := []Entry{renamed, wrongSize, wrongHash, {Path: original.Path, Kind: Absent}, {Path: "new", Kind: Directory}}
	if got, want := ReuseFiles(base, changes), map[string]string{renamed.Path: original.Path}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reuse = %v, want %v", got, want)
	}
	if got := ReuseFiles(nil, changes); len(got) != 0 {
		t.Fatalf("reused contents without a baseline: %v", got)
	}
}

func TestSaveReuseValidation(t *testing.T) {
	file := Entry{Path: "new", Kind: File, SHA256: strings.Repeat("a", 64)}
	for _, tc := range []struct {
		name string
		save Save
	}{
		{"no base", Save{Branch: "main", Changes: []Entry{file}, Reuse: map[string]string{"new": "old"}}},
		{"no change", Save{Branch: "main", Base: "base", Reuse: map[string]string{"new": "old"}}},
		{"directory", Save{Branch: "main", Base: "base", Changes: []Entry{{Path: "new", Kind: Directory}}, Reuse: map[string]string{"new": "old"}}},
		{"invalid path", Save{Branch: "main", Base: "base", Changes: []Entry{file}, Reuse: map[string]string{"new": "../old"}}},
		{"merge", Save{Branch: "main", Base: "base", Merge: "other", Reuse: map[string]string{"new": "old"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.save.Validate(); err == nil {
				t.Fatal("accepted invalid reuse")
			}
		})
	}
	if err := (Save{Branch: "main", Base: "base", Changes: []Entry{file}, Reuse: map[string]string{"new": "old"}}).Validate(); err != nil {
		t.Fatal(err)
	}
}
