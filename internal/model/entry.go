package model

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidEntry = errors.New("invalid entry")
	ErrConflict     = errors.New("entry conflict")
)

// EntryKind identifies the explicit state of a path in a project snapshot.
type EntryKind uint8

const (
	Absent EntryKind = iota + 1
	File
	Directory
)

// Entry describes one whole path entry. ContentID is an opaque, non-empty
// comparison identity for files within the same identity namespace. It does
// not describe file bytes or provide an identity derivation mechanism.
type Entry struct {
	Kind      EntryKind
	ContentID string
}

// Validate checks the entry's explicit kind and content identity shape.
func (e Entry) Validate() error {
	switch e.Kind {
	case Absent, Directory:
		if e.ContentID != "" {
			return ErrInvalidEntry
		}
	case File:
		if e.ContentID == "" {
			return ErrInvalidEntry
		}
	default:
		return ErrInvalidEntry
	}
	return nil
}

// Reconcile compares complete entries against a common base. A clean local
// side takes the target; identical outcomes agree; a target-clean side keeps
// the local edit; divergent edits conflict. Structural tree validation,
// scanning, identity derivation, and persistence are outside this model.
func Reconcile(base, local, target Entry) (Entry, error) {
	for _, input := range []struct {
		name  string
		entry Entry
	}{
		{name: "base", entry: base},
		{name: "local", entry: local},
		{name: "target", entry: target},
	} {
		if err := input.entry.Validate(); err != nil {
			return Entry{}, fmt.Errorf("%s: %w", input.name, err)
		}
	}
	switch {
	case local == base:
		return target, nil
	case local == target:
		return local, nil
	case target == base:
		return local, nil
	default:
		return Entry{}, ErrConflict
	}
}
