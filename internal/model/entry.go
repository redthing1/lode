package model

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidEntry = errors.New("invalid entry")
	ErrConflict     = errors.New("conflicting entry changes")
)

type EntryKind uint8

const (
	Absent EntryKind = iota + 1
	File
	Directory
)

// Entry contains confirmed semantic facts about one path. The zero value is
// invalid: absence must be explicit, and failed observations must remain errors.
// Directory children are compared separately from their parent entry.
type Entry struct {
	Kind EntryKind
	// ContentID is a verified equality witness for file bytes in a consistent
	// namespace. An empty ID is not an unknown file; it is an invalid entry.
	ContentID  string
	Executable bool
}

func (e Entry) Validate() error {
	switch e.Kind {
	case File:
		if e.ContentID == "" {
			return fmt.Errorf("%w: file content identity is required", ErrInvalidEntry)
		}
	case Absent, Directory:
		if e.ContentID != "" || e.Executable {
			return fmt.Errorf("%w: only files have content and executable attributes", ErrInvalidEntry)
		}
	default:
		return fmt.Errorf("%w: unknown kind %d", ErrInvalidEntry, e.Kind)
	}
	return nil
}

// Reconcile returns the desired local entry when updating from base to target.
// Compatible local edits remain edits against target. Divergent changes conflict.
// Callers must also validate ancestor and descendant relationships before applying
// a transition; this function compares only the entry at a single path.
func Reconcile(base, local, target Entry) (Entry, error) {
	for _, input := range []struct {
		name  string
		entry Entry
	}{
		{"base", base},
		{"local", local},
		{"target", target},
	} {
		if err := input.entry.Validate(); err != nil {
			return Entry{}, fmt.Errorf("%s: %w", input.name, err)
		}
	}

	switch {
	case local == base, local == target:
		return target, nil
	case target == base:
		return local, nil
	default:
		return Entry{}, ErrConflict
	}
}
