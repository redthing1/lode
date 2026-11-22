package workspace

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/redthing1/lode/internal/model"
)

func validateScope(scope model.Selection) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	for _, rule := range scope {
		if !rule.Include || rule.Path == "" {
			return errors.New("scope needs paths below the workspace root")
		}
		if err := checkWorkspacePath(rule.Path); err != nil {
			return err
		}
	}
	return nil
}

func withinScope(selection, scope model.Selection) (model.Selection, error) {
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	if len(scope) == 0 {
		return selection, nil
	}
	for _, rule := range selection {
		if rule.Include && !scope.Descends(rule.Path) {
			return nil, fmt.Errorf("%q is outside the workspace scope", rule.Path)
		}
	}
	return selection.Intersect(scope), nil
}

// SetScope updates the workspace boundary without adding or unloading files.
func (w *Workspace) SetScope(scope model.Selection) error {
	if len(scope) == 0 {
		return errors.New("specify at least one scope path")
	}
	if err := validateScope(scope); err != nil {
		return err
	}
	if err := w.ready(); err != nil {
		return err
	}
	for _, entry := range w.state.Baseline {
		if err := checkWorkspacePath(entry.Path); err != nil {
			return err
		}
		if !scope.Includes(entry.Path) {
			return fmt.Errorf("scope would exclude saved path %q", entry.Path)
		}
	}
	next := w.state
	next.Scope = scope
	next.Selection = next.Selection.Intersect(scope)
	if err := w.writeState(next); err != nil {
		return err
	}
	w.state = next
	return nil
}

func gitMetadata(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		if runtime.GOOS == "windows" {
			part, _, _ = strings.Cut(part, ":")
			part = strings.TrimRight(part, ". ")
			if strings.EqualFold(part, "git~1") {
				return true
			}
		}
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}

func checkWorkspacePath(name string) error {
	if gitMetadata(name) {
		return fmt.Errorf("Git metadata path %q is protected", name)
	}
	if !model.ValidPath(name) || !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("unsupported local path %q", name)
	}
	return nil
}
