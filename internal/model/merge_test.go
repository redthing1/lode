package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
)

func mergeFile(path, contents string) Entry {
	hash := sha256.Sum256([]byte(contents))
	return Entry{Path: path, Kind: File, Size: int64(len(contents)), SHA256: hex.EncodeToString(hash[:])}
}

func requireMerge(t *testing.T, base, ours, theirs []Entry, choices []Choice, want []Entry) {
	t.Helper()
	got, err := Merge(base, ours, theirs, choices)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge() = %#v, want %#v", got, want)
	}
}

func requireConflict(t *testing.T, base, ours, theirs []Entry, choices []Choice, want ...string) {
	t.Helper()
	got, err := Merge(base, ours, theirs, choices)
	var conflict *Conflicts
	if !errors.As(err, &conflict) {
		t.Fatalf("Merge() error = %v, want *Conflicts", err)
	}
	if !reflect.DeepEqual(conflict.Paths, want) {
		t.Fatalf("conflicts = %#v, want %#v", conflict.Paths, want)
	}
	if got != nil {
		t.Fatalf("Merge() returned partial result with conflicts: %#v", got)
	}
}

func TestMergeCombinesIndependentFileChanges(t *testing.T) {
	base := []Entry{mergeFile("a", "base-a"), mergeFile("b", "base-b")}
	ours := []Entry{mergeFile("a", "ours-a"), mergeFile("b", "base-b")}
	theirs := []Entry{mergeFile("a", "base-a"), mergeFile("b", "theirs-b")}
	requireMerge(t, base, ours, theirs, nil, []Entry{mergeFile("a", "ours-a"), mergeFile("b", "theirs-b")})
}

func TestMergeAcceptsEqualConcurrentChange(t *testing.T) {
	base := []Entry{mergeFile("file", "before")}
	changed := []Entry{mergeFile("file", "same result")}
	requireMerge(t, base, changed, changed, nil, changed)
}

func TestMergeReportsEditDeleteConflictAndResolvesEitherSide(t *testing.T) {
	base := []Entry{mergeFile("file", "before")}
	ours := []Entry{mergeFile("file", "edited")}
	requireConflict(t, base, ours, nil, nil, "file")
	requireMerge(t, base, ours, nil, []Choice{{Path: "file", Ours: true}}, ours)
	requireMerge(t, base, ours, nil, []Choice{{Path: "file"}}, []Entry{})
}

func TestMergeReportsFileDirectoryConflictsAndChoicesCoverSubtree(t *testing.T) {
	base := []Entry{mergeFile("node", "old")}
	ours := []Entry{mergeFile("node", "ours")}
	theirs := []Entry{{Path: "node", Kind: Directory}, mergeFile("node/child", "theirs")}
	requireConflict(t, base, ours, theirs, nil, "node")
	requireMerge(t, base, ours, theirs, []Choice{{Path: "node", Ours: true}}, ours)
	requireMerge(t, base, ours, theirs, []Choice{{Path: "node"}}, theirs)
}

func TestMergeCompletesPartialInventoriesWithImplicitParents(t *testing.T) {
	// The local inventory selects only dir/local; the omitted parent is still
	// logically present, allowing an independent remote addition beneath it.
	base := []Entry{mergeFile("dir/base", "base")}
	ours := []Entry{mergeFile("dir/base", "base"), mergeFile("dir/local", "local")}
	theirs := []Entry{mergeFile("dir/base", "remote"), mergeFile("dir/remote", "remote child")}
	want := []Entry{{Path: "dir", Kind: Directory}, mergeFile("dir/base", "remote"), mergeFile("dir/local", "local"), mergeFile("dir/remote", "remote child")}
	requireMerge(t, base, ours, theirs, nil, want)
}

func TestMergeUsesNearestNestedChoice(t *testing.T) {
	base := []Entry{{Path: "tree", Kind: Directory}, mergeFile("tree/a", "base-a"), mergeFile("tree/sub/b", "base-b")}
	ours := []Entry{{Path: "tree", Kind: Directory}, mergeFile("tree/a", "ours-a"), mergeFile("tree/sub/b", "ours-b")}
	theirs := []Entry{{Path: "tree", Kind: Directory}, mergeFile("tree/a", "theirs-a"), mergeFile("tree/sub/b", "theirs-b")}
	requireMerge(t, base, ours, theirs, []Choice{{Path: "tree", Ours: true}, {Path: "tree/sub"}}, []Entry{{Path: "tree", Kind: Directory}, mergeFile("tree/a", "ours-a"), {Path: "tree/sub", Kind: Directory}, mergeFile("tree/sub/b", "theirs-b")})
}

func TestMergeUnchangedBranchReturnsOtherInventory(t *testing.T) {
	base := []Entry{mergeFile("root", "root"), mergeFile("folder/child", "child")}
	theirs := []Entry{{Path: "folder", Kind: Directory}, mergeFile("folder/child", "new child"), mergeFile("new", "new file")}
	requireMerge(t, base, base, theirs, nil, theirs)
	requireMerge(t, base, theirs, base, nil, theirs)
}

func TestMergeRejectsDuplicateAndInvalidChoices(t *testing.T) {
	base := []Entry{mergeFile("file", "base")}
	for _, choices := range [][]Choice{
		{{Path: "file"}, {Path: "file", Ours: true}},
		{{Path: "../file"}},
		{{Path: ".lode/state"}},
	} {
		if _, err := Merge(base, base, base, choices); err == nil {
			t.Errorf("Merge(..., choices=%#v) accepted invalid choices", choices)
		}
	}
}
