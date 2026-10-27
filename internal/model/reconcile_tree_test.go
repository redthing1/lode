package model

import (
	"errors"
	"reflect"
	"testing"
)

func TestReconcileTreesCombinesCleanAndDisjointEdits(t *testing.T) {
	root := Path{}
	dir := treePath(t, "assets")
	left := treePath(t, "assets/left.bin")
	right := treePath(t, "assets/right.bin")
	base := map[Path]Entry{
		root: {Kind: Directory}, dir: {Kind: Directory},
		left: {Kind: File, ContentID: "left-base"}, right: {Kind: File, ContentID: "right-base"},
	}
	local := cloneTree(base)
	local[left] = Entry{Kind: File, ContentID: "left-local"}
	target := cloneTree(base)
	target[right] = Entry{Kind: File, ContentID: "right-target"}

	got, conflicts, err := ReconcileTrees(base, local, target)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("ReconcileTrees() conflicts=%v err=%v", conflicts, err)
	}
	if got[left] != local[left] || got[right] != target[right] || got[dir] != (Entry{Kind: Directory}) {
		t.Fatalf("disjoint edits were not combined: %#v", got)
	}
}

func TestReconcileTreesAcceptsTargetAdvanceAndPreservesLocalOnlyEdit(t *testing.T) {
	root := Path{}
	file := treePath(t, "file.bin")
	base := map[Path]Entry{root: {Kind: Directory}, file: {Kind: File, ContentID: "base"}}
	target := map[Path]Entry{root: {Kind: Directory}, file: {Kind: File, ContentID: "target"}}
	got, conflicts, err := ReconcileTrees(base, base, target)
	if err != nil || len(conflicts) != 0 || got[file] != target[file] {
		t.Fatalf("clean local should accept target: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}

	local := map[Path]Entry{root: {Kind: Directory}, file: {Kind: File, ContentID: "local"}}
	got, conflicts, err = ReconcileTrees(base, local, base)
	if err != nil || len(conflicts) != 0 || got[file] != local[file] {
		t.Fatalf("clean target should preserve local edit: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
}

func TestReconcileTreesAgreesOnIdenticalEditsAndEmptyProjects(t *testing.T) {
	root := Path{}
	file := treePath(t, "added.bin")
	base := map[Path]Entry{root: {Kind: Directory}}
	added := map[Path]Entry{root: {Kind: Directory}, file: {Kind: File, ContentID: "same"}}
	got, conflicts, err := ReconcileTrees(base, added, added)
	if err != nil || len(conflicts) != 0 || !reflect.DeepEqual(got, added) {
		t.Fatalf("identical addition: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
	got, conflicts, err = ReconcileTrees(nil, nil, nil)
	if err != nil || len(conflicts) != 0 || !reflect.DeepEqual(got, base) {
		t.Fatalf("empty trees: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
}

func TestReconcileTreesReportsPointwiseConflicts(t *testing.T) {
	file := treePath(t, "item")
	base := map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "base"}}
	for _, tc := range []struct {
		name      string
		local     map[Path]Entry
		target    map[Path]Entry
		conflicts []Path
	}{
		{
			name:      "divergent edits",
			local:     map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "local"}},
			target:    map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "target"}},
			conflicts: []Path{file},
		},
		{
			name:      "delete versus edit",
			local:     map[Path]Entry{Path{}: {Kind: Directory}},
			target:    map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "target"}},
			conflicts: []Path{file},
		},
		{
			name:      "type conflict",
			local:     map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: Directory}},
			target:    map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "target"}},
			conflicts: []Path{file},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, conflicts, err := ReconcileTrees(base, tc.local, tc.target)
			if err != nil || got != nil || !reflect.DeepEqual(conflicts, tc.conflicts) {
				t.Fatalf("ReconcileTrees() = (%#v, %v, %v), want nil, %v, nil", got, conflicts, err, tc.conflicts)
			}
		})
	}
}

func TestReconcileTreesReportsDeletedParentForLocalChild(t *testing.T) {
	root := Path{}
	dir := treePath(t, "folder")
	oldChild := treePath(t, "folder/old.bin")
	newChild := treePath(t, "folder/new.bin")
	base := map[Path]Entry{
		root: {Kind: Directory}, dir: {Kind: Directory}, oldChild: {Kind: File, ContentID: "old"},
	}
	local := cloneTree(base)
	local[newChild] = Entry{Kind: File, ContentID: "new"}
	target := map[Path]Entry{root: {Kind: Directory}}

	got, conflicts, err := ReconcileTrees(base, local, target)
	if err != nil || got != nil || !reflect.DeepEqual(conflicts, []Path{dir}) {
		t.Fatalf("deleted parent conflict: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
}

func TestReconcileTreesSiblingFilesDoNotConflictThroughDirectory(t *testing.T) {
	root := Path{}
	dir := treePath(t, "assets")
	left := treePath(t, "assets/left.bin")
	right := treePath(t, "assets/right.bin")
	base := map[Path]Entry{
		root: {Kind: Directory}, dir: {Kind: Directory},
		left: {Kind: File, ContentID: "left-base"}, right: {Kind: File, ContentID: "right-base"},
	}
	local := cloneTree(base)
	local[left] = Entry{Kind: File, ContentID: "left-local"}
	target := cloneTree(base)
	target[right] = Entry{Kind: File, ContentID: "right-target"}
	got, conflicts, err := ReconcileTrees(base, local, target)
	if err != nil || len(conflicts) != 0 || got[dir] != base[dir] {
		t.Fatalf("sibling edits conflict through their directory: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
}

func TestReconcileTreesLeavesInputsUntouchedAndOutputDetached(t *testing.T) {
	root := Path{}
	file := treePath(t, "file.bin")
	base := map[Path]Entry{root: {Kind: Directory}, file: {Kind: File, ContentID: "base"}}
	local := cloneTree(base)
	local[file] = Entry{Kind: File, ContentID: "local"}
	target := cloneTree(base)
	baseBefore, localBefore, targetBefore := cloneTree(base), cloneTree(local), cloneTree(target)
	got, conflicts, err := ReconcileTrees(base, local, target)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("ReconcileTrees() conflicts=%v err=%v", conflicts, err)
	}
	got[file] = Entry{Kind: File, ContentID: "mutated result"}
	if !reflect.DeepEqual(base, baseBefore) || !reflect.DeepEqual(local, localBefore) || !reflect.DeepEqual(target, targetBefore) {
		t.Fatal("tree reconciliation mutated an input")
	}
}

func TestReconcileTreesValidatesEveryInputBeforeComparing(t *testing.T) {
	root := Path{}
	valid := map[Path]Entry{root: {Kind: Directory}}
	invalid := map[Path]Entry{root: {Kind: Directory}, treePath(t, "bad"): {Kind: File}}
	for _, tc := range []struct {
		name        string
		base, local map[Path]Entry
		target      map[Path]Entry
	}{
		{"base", invalid, valid, valid},
		{"local", valid, invalid, valid},
		{"target", valid, valid, invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if result, conflicts, err := ReconcileTrees(tc.base, tc.local, tc.target); !errors.Is(err, ErrInvalidEntry) || result != nil || conflicts != nil {
				t.Fatalf("invalid %s tree: result=%#v conflicts=%v err=%v", tc.name, result, conflicts, err)
			}
		})
	}
}

func cloneTree(tree map[Path]Entry) map[Path]Entry {
	clone := make(map[Path]Entry, len(tree))
	for path, entry := range tree {
		clone[path] = entry
	}
	return clone
}
