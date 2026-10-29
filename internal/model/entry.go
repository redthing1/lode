// Package model defines file identities and path selection without IO.
package model

import (
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Kind string

const (
	File      Kind = "file"
	Directory Kind = "directory"
	Absent    Kind = "absent"
)

// Entry names an exact project-relative path. Absent is a change, not a tree entry.
type Entry struct {
	Path   string `json:"path"`
	Kind   Kind   `json:"kind"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

func ValidPath(name string) bool {
	if name == "" || name == "." || len(name) > 4096 || !utf8.ValidString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, "\\") {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) > 256 || parts[0] == ".lode" {
		return false
	}
	for _, p := range parts {
		if p == ".." || len(p) > 255 {
			return false
		}
	}
	return strings.IndexFunc(name, unicode.IsControl) < 0
}

func (e Entry) Validate() error {
	if !ValidPath(e.Path) {
		return fmt.Errorf("invalid project path %q", e.Path)
	}
	switch e.Kind {
	case File:
		h, err := hex.DecodeString(e.SHA256)
		if e.Size < 0 || err != nil || len(h) != 32 || hex.EncodeToString(h) != e.SHA256 {
			return fmt.Errorf("invalid file identity for %q", e.Path)
		}
	case Directory, Absent:
		if e.Size != 0 || e.SHA256 != "" {
			return fmt.Errorf("non-file %q has file contents", e.Path)
		}
	default:
		return fmt.Errorf("invalid entry kind %q", e.Kind)
	}
	return nil
}

// ValidateChanges rejects ambiguous requests before any content is written.
func ValidateChanges(changes []Entry) error {
	seen := make(map[string]Kind, len(changes))
	for _, e := range changes {
		if err := e.Validate(); err != nil {
			return err
		}
		if _, ok := seen[e.Path]; ok {
			return fmt.Errorf("duplicate change %q", e.Path)
		}
		seen[e.Path] = e.Kind
	}
	for _, e := range changes {
		for parent := path.Dir(e.Path); parent != "."; parent = path.Dir(parent) {
			if kind, ok := seen[parent]; ok && kind != Directory && e.Kind != Absent {
				return fmt.Errorf("%q requires directory %q", e.Path, parent)
			}
		}
	}
	return nil
}
