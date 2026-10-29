package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/redthing1/lode/internal/model"
)

// Switch changes the selected branch and baseline. Unsaved selected work can
// follow a branch at the same version; otherwise it must be saved first.
func (w *Workspace) Switch(ctx context.Context, token, branch string) error {
	if !model.ValidReference(branch) {
		return errors.New("invalid branch name")
	}
	if err := w.ready(); err != nil {
		return err
	}
	c, err := w.Client(token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	target := w.state
	target.Branch = branch
	target.Version, err = c.Head(ctx, target.Owner, target.Project, branch)
	if err != nil {
		return err
	}
	if target.Version == w.state.Version {
		return w.beginTransition(ctx, c, target, target.Version, nil, nil)
	}
	local, err := w.observeSelection(ctx, w.state.Selection)
	if err != nil {
		return err
	}
	if len(model.Changes(w.state.Baseline, model.Selected(local, w.state.Selection), w.state.Selection)) != 0 {
		return errors.New("save selected changes before switching branches")
	}
	target.Baseline, err = c.Inventory(ctx, target.Owner, target.Project, target.Version, target.Selection)
	if err != nil {
		return err
	}
	entries, err := model.Materialized(target.Baseline)
	if err != nil {
		return err
	}
	return w.beginTransition(ctx, c, target, target.Version, local, entries)
}

// Restore changes working files, leaving the selected branch and saved baseline
// intact. Only clean targeted paths are replaced; the result is unsaved work.
func (w *Workspace) Restore(ctx context.Context, token, ref string, paths []string) error {
	if err := w.ready(); err != nil {
		return err
	}
	requested := model.Selection{{Include: true}}
	if len(paths) > 0 {
		requested = nil
		for _, name := range paths {
			requested = append(requested, model.Rule{Path: name, Include: true})
		}
	}
	if err := requested.Validate(); err != nil {
		return err
	}
	selection := w.state.Selection.Intersect(requested)
	if len(selection) == 0 {
		return errors.New("restore matches no selected paths")
	}
	local, err := w.observeSelection(ctx, selection)
	if err != nil {
		return err
	}
	baseline := model.Selected(w.state.Baseline, selection)
	if changes := model.Changes(baseline, model.Selected(local, selection), selection); len(changes) > 0 {
		return fmt.Errorf("cannot restore changed path %q; save or move it aside first", changes[0].Path)
	}
	c, err := w.Client(token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	v, err := c.Reference(ctx, w.state.Owner, w.state.Project, ref)
	if err != nil {
		return err
	}
	if !v.Retained {
		return errors.New("restore requires a version with available contents")
	}
	entries, err := c.Inventory(ctx, w.state.Owner, w.state.Project, v.ID, selection)
	if err != nil {
		return err
	}
	if len(entries) == 0 && len(baseline) == 0 {
		return errors.New("restore matches no files or directories")
	}
	entries, err = model.Materialized(entries)
	if err != nil {
		return err
	}
	return w.beginTransition(ctx, c, w.state, v.ID, local, entries)
}
