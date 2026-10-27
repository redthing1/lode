// Package model contains pure values and rules for project paths and entries.
// It does not perform filesystem discovery, persistence, or content storage.
package model

import (
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrInvalidPath = errors.New("invalid project path")

// Path is a canonical, slash-separated project path. Its zero value is root.
// Paths compare exact UTF-8 bytes; this type does not normalize Unicode or
// claim that names are portable across filesystems.
type Path struct {
	value string
}

// ParsePath constructs a canonical project path. The empty string denotes
// root. Invalid paths are rejected rather than cleaned or rewritten.
func ParsePath(value string) (Path, error) {
	if value == "" {
		return Path{}, nil
	}
	if !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\\") ||
		strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return Path{}, ErrInvalidPath
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return Path{}, ErrInvalidPath
		}
	}
	return Path{value: value}, nil
}

// String returns the canonical path, or the empty string for root.
func (p Path) String() string { return p.value }

// IsRoot reports whether p denotes the project root.
func (p Path) IsRoot() bool { return p.value == "" }

// Parent returns p's parent. Root is its own parent.
func (p Path) Parent() Path {
	if i := strings.LastIndexByte(p.value, '/'); i >= 0 {
		return Path{value: p.value[:i]}
	}
	return Path{}
}

// Contains reports whether p is equal to other or is one of its ancestors.
// Component boundaries matter, so "assets" does not contain "assets-old".
func (p Path) Contains(other Path) bool {
	if p.value == "" {
		return true
	}
	return other.value == p.value || strings.HasPrefix(other.value, p.value+"/")
}
