package model

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"
)

func testFile(name, contents string) Entry {
	h := sha256.Sum256([]byte(contents))
	return Entry{Path: name, Kind: File, Size: int64(len(contents)), SHA256: hex.EncodeToString(h[:])}
}

func TestReconcileIndependentEditsAndWholeConflicts(t *testing.T) {
	a, b := testFile("a", "before"), testFile("b", "before")
	localA, remoteB := testFile("a", "local"), testFile("b", "remote")
	got, conflicts, err := Reconcile([]Entry{a, b}, []Entry{localA, b}, []Entry{a, remoteB}, nil)
	if err != nil || len(conflicts) != 0 || !reflect.DeepEqual(got, []Entry{localA, remoteB}) {
		t.Fatalf("independent changes: %v %v %v", got, conflicts, err)
	}
	cases := []struct {
		name                string
		base, local, remote []Entry
		conflict            string
	}{
		{"file edits", []Entry{a}, []Entry{localA}, []Entry{testFile("a", "remote")}, "a"},
		{"delete edit", []Entry{a}, nil, []Entry{localA}, "a"},
		{"delete directory edit", []Entry{testFile("dir/a", "before")}, nil, []Entry{testFile("dir/a", "remote")}, "dir"},
		{"replace directory edit", []Entry{testFile("dir/a", "before")}, []Entry{testFile("dir", "replacement")}, []Entry{testFile("dir/a", "remote")}, "dir"},
		{"two additions", nil, []Entry{a}, []Entry{localA}, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, conflicts, err := Reconcile(tc.base, tc.local, tc.remote, nil)
			if err != nil || got != nil || len(conflicts) != 1 || conflicts[0].Path != tc.conflict {
				t.Fatalf("conflicts: %v %v %v", got, conflicts, err)
			}
			for _, side := range []Side{Local, Incoming} {
				got, conflicts, err = Reconcile(tc.base, tc.local, tc.remote, map[string]Side{tc.conflict: side})
				want := tc.local
				if side == Incoming {
					want = tc.remote
				}
				want, _ = Materialized(want)
				if err != nil || len(conflicts) != 0 || !reflect.DeepEqual(got, want) {
					t.Fatalf("choice %s: %v want %v; %v %v", side, got, want, conflicts, err)
				}
			}
		})
	}
}

func TestReconcileDirectoryReplacementsAndChoiceValidation(t *testing.T) {
	base := []Entry{testFile("d", "old file")}
	local, remote := []Entry{testFile("d/a", "local")}, []Entry{testFile("d/b", "remote")}
	got, conflicts, err := Reconcile(base, local, remote, nil)
	want := []Entry{{Path: "d", Kind: Directory}, local[0], remote[0]}
	if err != nil || len(conflicts) != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("parallel directories: %v %v %v", got, conflicts, err)
	}
	if _, _, err := Reconcile(base, base, base, map[string]Side{"d": Local}); err == nil {
		t.Fatal("unused choice accepted")
	}
	if _, _, err := Reconcile(nil, []Entry{testFile("d", "file"), testFile("d/a", "child")}, nil, nil); err == nil {
		t.Fatal("invalid inventory accepted")
	}
}

func TestPlanChangesOrdersTypeReplacements(t *testing.T) {
	before := []Entry{{Path: "d", Kind: Directory}, testFile("d/a", "old"), testFile("f", "old")}
	after := []Entry{testFile("d", "replacement"), {Path: "f", Kind: Directory}, testFile("f/b", "new")}
	changes, err := PlanChanges(before, after)
	if err != nil {
		t.Fatal(err)
	}
	state := append([]Entry(nil), before...)
	for _, change := range changes {
		state = ApplySelected(state, []Entry{change.After})
		if err := ValidateInventory(state); err != nil {
			t.Fatalf("invalid intermediate tree after %v: %v", change, err)
		}
		for _, e := range state {
			if e.Path == "d/a" && change.Before.Path == "d" {
				t.Fatal("removed parent before child")
			}
		}
	}
	if !reflect.DeepEqual(state, after) {
		t.Fatalf("transition: %v want %v", state, after)
	}
}
