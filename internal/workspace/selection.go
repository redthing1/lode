package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func (w *Workspace) Load(ctx context.Context, paths []string) error {
	return w.changeSelection(ctx, paths, true)
}

// Unload removes clean controlled copies and changes selection, not history.
// Unmanaged directory contents remain in place; local changes cannot be hidden.
func (w *Workspace) Unload(ctx context.Context, paths []string) error {
	return w.changeSelection(ctx, paths, false)
}

func (w *Workspace) changeSelection(ctx context.Context, paths []string, include bool) error {
	if w.state.Pending != nil {
		return ErrPending
	}
	selection, err := project.Selection(w.state.Selection)
	if err != nil {
		return err
	}
	logical, err := selectionPaths(paths)
	if err != nil {
		return err
	}
	for _, p := range logical {
		if include {
			selection = selection.Include(p)
		} else {
			selection = selection.Exclude(p)
		}
	}
	if err = w.verifyRemote(ctx); err != nil {
		return err
	}
	rules := portableSelection(selection)
	nodes, err := fetchEntries(ctx, w.client, w.state.Base, rules)
	if err != nil {
		return err
	}
	next := w.state
	next.Selection = rules
	next.Baseline = nodes
	operation := "unload"
	if include {
		operation = "load"
	}
	return w.startTransition(ctx, operation, w.state.Base, nodes, next, transitionOptions{})
}

// Restore materializes requested selected paths from history while retaining
// the current base. Divergent local edits require resolution before restoring.
func (w *Workspace) Restore(ctx context.Context, from string, paths []string) error {
	return w.RestoreWithChoices(ctx, from, paths, nil)
}

// RestoreWithChoices materializes requested selected paths from history while
// retaining the current base. Choices apply only within the restore scope.
func (w *Workspace) RestoreWithChoices(ctx context.Context, from string, paths []string, rawChoices []project.Choice) error {
	if w.state.Pending != nil {
		return ErrPending
	}
	if from == "" {
		return errors.New("restore requires an exact commit")
	}
	selection, err := project.Selection(w.state.Selection)
	if err != nil {
		return err
	}
	logical, err := selectionPaths(paths)
	if err != nil {
		return err
	}
	var requested model.Selection
	for _, p := range logical {
		if !selection.Intersects(p) {
			return fmt.Errorf("restore path %q is not selected; load it first", p)
		}
		requested = requested.Include(p)
	}
	requested = requested.Intersection(selection)
	choices, err := project.Choices(rawChoices)
	if err != nil {
		return err
	}
	if err := validateChoiceScope(choices, requested); err != nil {
		return err
	}
	if err = w.verifyRemote(ctx); err != nil {
		return err
	}
	source, err := fetchEntries(ctx, w.client, from, portableSelection(requested))
	if err != nil {
		return err
	}
	view := nodeMap(w.state.Baseline)
	for p := range view {
		logical, _ := model.ParsePath(p)
		if requested.Includes(logical) {
			delete(view, p)
		}
	}
	for _, node := range source {
		p, _ := model.ParsePath(node.Path)
		if !requested.Includes(p) && view[node.Path].Kind == model.File {
			return fmt.Errorf("%w: restore needs directory parent %q", ErrConflict, p)
		}
		view[node.Path] = node
	}
	return w.startTransition(ctx, "restore", from, sortedNodes(view), w.state, transitionOptions{restoreScope: requested, choices: choices})
}

func selectionPaths(paths []string) ([]model.Path, error) {
	if len(paths) == 0 {
		return nil, errors.New("at least one project path is required")
	}
	result := make([]model.Path, 0, len(paths))
	for _, value := range paths {
		p, err := model.ParsePath(value)
		if err != nil || value == ".lode" || strings.HasPrefix(value, ".lode/") {
			return nil, fmt.Errorf("invalid or reserved project path %q", value)
		}
		result = append(result, p)
	}
	return result, nil
}
