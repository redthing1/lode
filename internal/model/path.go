// Package model defines project paths, local selection, and entry transitions.
package model

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

var ErrInvalidPath = errors.New("invalid project path")

// Path is a canonical, slash-separated project path. Its zero value is the root.
// Host filesystem names and platform restrictions are handled at the IO boundary.
type Path struct {
	name string
}

// ParsePath accepts relative paths and "." for the root. It rejects ambiguous
// separators and traversal rather than cleaning them into a different path.
func ParsePath(name string) (Path, error) {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\x00\\") {
		return Path{}, fmt.Errorf("%w: %q", ErrInvalidPath, name)
	}
	if name == "." {
		return Path{}, nil
	}
	return Path{name: name}, nil
}

func (p Path) String() string {
	if p.name == "" {
		return "."
	}
	return p.name
}

// Parent returns the containing directory. The root is its own parent.
func (p Path) Parent() Path {
	if i := strings.LastIndexByte(p.name, '/'); i >= 0 {
		return Path{name: p.name[:i]}
	}
	return Path{}
}

// Contains reports whether other is p or one of its descendants.
func (p Path) Contains(other Path) bool {
	return p.name == "" || p == other || strings.HasPrefix(other.name, p.name+"/")
}
