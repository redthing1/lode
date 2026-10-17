package model_test

import (
	"errors"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestReconcileWholeEntryChanges(t *testing.T) {
	absent := model.Entry{Kind: model.Absent}
	directory := model.Entry{Kind: model.Directory}
	a := model.Entry{Kind: model.File, ContentID: "digest:a"}
	b := model.Entry{Kind: model.File, ContentID: "digest:b"}
	c := model.Entry{Kind: model.File, ContentID: "digest:c"}
	executable := model.Entry{Kind: model.File, ContentID: "digest:a", Executable: true}
	for _, tc := range []struct {
		name                      string
		base, local, target, want model.Entry
		conflict                  bool
	}{
		{"incoming edit", a, a, b, b, false},
		{"local edit", a, b, a, b, false},
		{"same edit", a, b, b, b, false},
		{"different edits", a, b, c, model.Entry{}, true},
		{"local deletion", a, absent, a, absent, false},
		{"incoming deletion", a, a, absent, absent, false},
		{"both delete", a, absent, absent, absent, false},
		{"edit versus delete", a, b, absent, model.Entry{}, true},
		{"different additions", absent, a, b, model.Entry{}, true},
		{"same addition", absent, a, a, a, false},
		{"incoming directory", absent, absent, directory, directory, false},
		{"conflicting types", absent, a, directory, model.Entry{}, true},
		{"local executable change", a, executable, a, executable, false},
		{"attributes versus bytes", a, executable, b, model.Entry{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := model.Reconcile(tc.base, tc.local, tc.target)
			if tc.conflict {
				if !errors.Is(err, model.ErrConflict) {
					t.Fatalf("got %v, want ErrConflict", err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got (%+v, %v), want %+v", got, err, tc.want)
			}
		})
	}
}

func TestEntryRejectsUnknownAndInconsistentFacts(t *testing.T) {
	absent := model.Entry{Kind: model.Absent}
	for _, invalid := range []model.Entry{
		{},
		{Kind: model.EntryKind(255)},
		{Kind: model.File},
		{Kind: model.Absent, ContentID: "digest:a"},
		{Kind: model.Absent, Executable: true},
		{Kind: model.Directory, ContentID: "subtree:a"},
		{Kind: model.Directory, Executable: true},
	} {
		if err := invalid.Validate(); !errors.Is(err, model.ErrInvalidEntry) {
			t.Fatalf("Validate(%+v): got %v, want ErrInvalidEntry", invalid, err)
		}
		for position := range 3 {
			entries := [3]model.Entry{absent, absent, absent}
			entries[position] = invalid
			if _, err := model.Reconcile(entries[0], entries[1], entries[2]); !errors.Is(err, model.ErrInvalidEntry) {
				t.Fatalf("invalid input %d was treated as a valid transition: %v", position, err)
			}
		}
		if _, err := model.Reconcile(invalid, invalid, invalid); !errors.Is(err, model.ErrInvalidEntry) {
			t.Fatal("equal invalid inputs must not bypass validation")
		}
	}
}
