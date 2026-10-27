package model

import (
	"errors"
	"strings"
	"testing"
)

func TestReconcileWholeEntries(t *testing.T) {
	baseFile := Entry{Kind: File, ContentID: "base"}
	localFile := Entry{Kind: File, ContentID: "local"}
	targetFile := Entry{Kind: File, ContentID: "target"}
	absent := Entry{Kind: Absent}
	directory := Entry{Kind: Directory}

	cases := []struct {
		name                string
		base, local, target Entry
		want                Entry
		wantErr             error
	}{
		{"local unchanged accepts target", baseFile, baseFile, targetFile, targetFile, nil},
		{"target unchanged preserves local", baseFile, localFile, baseFile, localFile, nil},
		{"identical file result agrees", baseFile, localFile, localFile, localFile, nil},
		{"addition on target", absent, absent, targetFile, targetFile, nil},
		{"local addition preserved", absent, localFile, absent, localFile, nil},
		{"deletion on target", baseFile, baseFile, absent, absent, nil},
		{"local deletion preserved", baseFile, absent, baseFile, absent, nil},
		{"divergent file edits conflict", baseFile, localFile, targetFile, Entry{}, ErrConflict},
		{"addition versus different addition conflicts", absent, localFile, targetFile, Entry{}, ErrConflict},
		{"delete versus edit conflicts", baseFile, absent, targetFile, Entry{}, ErrConflict},
		{"file versus directory conflicts", baseFile, localFile, directory, Entry{}, ErrConflict},
		{"clean directory accepts deletion", directory, directory, absent, absent, nil},
		{"identical deletion agrees", baseFile, absent, absent, absent, nil},
		{"identical addition agrees", absent, localFile, localFile, localFile, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reconcile(tc.base, tc.local, tc.target)
			if !errors.Is(err, tc.wantErr) || (err == nil && got != tc.want) {
				t.Fatalf("Reconcile() = (%#v, %v), want (%#v, %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestEntryValidationAndReconcileRejectInvalidZeroValue(t *testing.T) {
	invalid := []Entry{
		{},
		{Kind: File},
		{Kind: Absent, ContentID: "id"},
		{Kind: Directory, ContentID: "id"},
		{Kind: 99},
	}
	for _, entry := range invalid {
		if !errors.Is(entry.Validate(), ErrInvalidEntry) {
			t.Errorf("Validate(%#v) should fail", entry)
		}
		absent := Entry{Kind: Absent}
		for position := range 3 {
			entries := [3]Entry{absent, absent, absent}
			entries[position] = entry
			_, err := Reconcile(entries[0], entries[1], entries[2])
			if !errors.Is(err, ErrInvalidEntry) {
				t.Errorf("Reconcile with invalid entry %#v at position %d error = %v, want ErrInvalidEntry", entry, position, err)
			}
			wantContext := []string{"base", "local", "target"}[position]
			if err == nil || !strings.Contains(err.Error(), wantContext) {
				t.Errorf("invalid entry error %v lacks %q context", err, wantContext)
			}
		}
		if _, err := Reconcile(entry, entry, entry); !errors.Is(err, ErrInvalidEntry) {
			t.Errorf("equal invalid entries %#v must be rejected, got %v", entry, err)
		}
	}
}
