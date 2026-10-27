package model

import (
	"errors"
	"reflect"
	"testing"
)

func TestReconcileTreesWithChoicesResolvesOpaqueEntryConflict(t *testing.T) {
	file := treePath(t, "asset.bin")
	base := map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "base"}}
	local := map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "local"}}
	target := map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "target"}}

	got, conflicts, err := ReconcileTreesWithChoices(base, local, target, map[Path]Side{file: LocalSide})
	if err != nil || len(conflicts) != 0 || got[file] != local[file] {
		t.Fatalf("local choice: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
}

func TestReconcileTreesWithChoicesSelectsDeletionAndTypeSubtree(t *testing.T) {
	item := treePath(t, "item")
	child := treePath(t, "item/child")
	baseFile := map[Path]Entry{Path{}: {Kind: Directory}, item: {Kind: File, ContentID: "base"}}
	localDeleted := map[Path]Entry{Path{}: {Kind: Directory}}
	targetEdited := map[Path]Entry{Path{}: {Kind: Directory}, item: {Kind: File, ContentID: "target"}}
	got, conflicts, err := ReconcileTreesWithChoices(baseFile, localDeleted, targetEdited, map[Path]Side{item: LocalSide})
	if err != nil || len(conflicts) != 0 || len(got) != 1 {
		t.Fatalf("chosen deletion: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
	got, conflicts, err = ReconcileTreesWithChoices(baseFile, localDeleted, targetEdited, map[Path]Side{item: TargetSide})
	if err != nil || len(conflicts) != 0 || got[item] != targetEdited[item] {
		t.Fatalf("chosen target edit: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}

	localDirectory := map[Path]Entry{
		Path{}: {Kind: Directory}, item: {Kind: Directory}, child: {Kind: File, ContentID: "local child"},
	}
	got, conflicts, err = ReconcileTreesWithChoices(baseFile, localDirectory, targetEdited, map[Path]Side{item: LocalSide})
	if err != nil || len(conflicts) != 0 || got[item] != localDirectory[item] || got[child] != localDirectory[child] {
		t.Fatalf("chosen directory subtree: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
}

func TestReconcileTreesWithChoicesUsesMostSpecificSubtreeRule(t *testing.T) {
	dir := treePath(t, "assets")
	file := treePath(t, "assets/shared.bin")
	localOnly := treePath(t, "assets/local.bin")
	targetOnly := treePath(t, "assets/target.bin")
	base := map[Path]Entry{Path{}: {Kind: Directory}, dir: {Kind: Directory}, file: {Kind: File, ContentID: "base"}}
	local := map[Path]Entry{
		Path{}: {Kind: Directory}, dir: {Kind: Directory}, file: {Kind: File, ContentID: "local"}, localOnly: {Kind: File, ContentID: "local only"},
	}
	target := map[Path]Entry{
		Path{}: {Kind: Directory}, dir: {Kind: Directory}, file: {Kind: File, ContentID: "target"}, targetOnly: {Kind: File, ContentID: "target only"},
	}
	choices := map[Path]Side{dir: TargetSide, file: LocalSide}
	got, conflicts, err := ReconcileTreesWithChoices(base, local, target, choices)
	if err != nil || len(conflicts) != 0 || got[file] != local[file] || got[targetOnly] != target[targetOnly] {
		t.Fatalf("overlapping choices: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
	if _, exists := got[localOnly]; exists {
		t.Fatalf("parent target choice did not apply to descendant: %#v", got)
	}
}

func TestReconcileTreesWithChoicesSupportsRootAndValidatesChoices(t *testing.T) {
	localFile := treePath(t, "local.bin")
	targetFile := treePath(t, "target.bin")
	base := map[Path]Entry{Path{}: {Kind: Directory}}
	local := map[Path]Entry{Path{}: {Kind: Directory}, localFile: {Kind: File, ContentID: "local"}}
	target := map[Path]Entry{Path{}: {Kind: Directory}, targetFile: {Kind: File, ContentID: "target"}}
	got, conflicts, err := ReconcileTreesWithChoices(base, local, target, map[Path]Side{Path{}: TargetSide})
	if err != nil || len(conflicts) != 0 || len(got) != 2 || got[targetFile] != target[targetFile] {
		t.Fatalf("root target choice: got=%#v conflicts=%v err=%v", got, conflicts, err)
	}
	if _, exists := got[localFile]; exists {
		t.Fatalf("root target choice retained local-only path: %#v", got)
	}

	for name, choices := range map[string]map[Path]Side{
		"unknown side": {localFile: "neither"},
		"unknown path": {treePath(t, "absent"): LocalSide},
	} {
		t.Run(name, func(t *testing.T) {
			if result, conflicts, err := ReconcileTreesWithChoices(base, local, target, choices); !errors.Is(err, ErrInvalidChoice) || result != nil || conflicts != nil {
				t.Fatalf("invalid choice: result=%#v conflicts=%v err=%v", result, conflicts, err)
			}
		})
	}
}

func TestReconcileTreesWithChoicesKeepsOtherConflictsAndInputs(t *testing.T) {
	a := treePath(t, "a.bin")
	b := treePath(t, "b.bin")
	base := map[Path]Entry{Path{}: {Kind: Directory}, a: {Kind: File, ContentID: "a-base"}, b: {Kind: File, ContentID: "b-base"}}
	local := map[Path]Entry{Path{}: {Kind: Directory}, a: {Kind: File, ContentID: "a-local"}, b: {Kind: File, ContentID: "b-local"}}
	target := map[Path]Entry{Path{}: {Kind: Directory}, a: {Kind: File, ContentID: "a-target"}, b: {Kind: File, ContentID: "b-target"}}
	baseBefore, localBefore, targetBefore := cloneTree(base), cloneTree(local), cloneTree(target)
	choices := map[Path]Side{a: TargetSide}
	choicesBefore := map[Path]Side{a: TargetSide}
	result, conflicts, err := ReconcileTreesWithChoices(base, local, target, choices)
	if err != nil || result != nil || !reflect.DeepEqual(conflicts, []Path{b}) {
		t.Fatalf("partially chosen conflicts: result=%#v conflicts=%v err=%v", result, conflicts, err)
	}
	if !reflect.DeepEqual(base, baseBefore) || !reflect.DeepEqual(local, localBefore) || !reflect.DeepEqual(target, targetBefore) || !reflect.DeepEqual(choices, choicesBefore) {
		t.Fatal("choice reconciliation mutated an input")
	}

	invalid := map[Path]Entry{Path{}: {Kind: Directory}, a: {Kind: File}}
	if _, _, err := ReconcileTreesWithChoices(invalid, local, target, map[Path]Side{a: "bad"}); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("input validation should precede choice validation, got %v", err)
	}
}
