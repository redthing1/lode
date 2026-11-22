package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

func Clone(ctx context.Context, projectURL string, c *remote.Client, directory string, options ConnectOptions) (*Workspace, error) {
	target, err := connect(ctx, projectURL, c, options)
	if err != nil {
		return nil, err
	}
	target.Version, err = c.Head(ctx, target.Owner, target.Project, target.Branch)
	if err != nil {
		return nil, err
	}
	target.Baseline, err = c.Inventory(ctx, target.Owner, target.Project, target.Version, target.Selection)
	if err != nil {
		return nil, err
	}
	entries, err := model.Materialized(target.Baseline)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := checkWorkspacePath(entry.Path); err != nil {
			return nil, err
		}
	}
	if err := os.Mkdir(directory, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	parent, err := os.Open(filepath.Dir(directory))
	if err != nil {
		return nil, err
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
		return nil, err
	}
	existing, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var before []model.Entry
	if len(existing) != 0 {
		if len(target.Scope) == 0 {
			return nil, errors.New("clone destination must be empty; use --scope to clone selected paths into an existing folder")
		}
		before, err = cloneDirectories(ctx, directory, entries)
		if err != nil {
			return nil, err
		}
	}
	initial := target
	initial.Version = ""
	if len(initial.Scope) == 0 {
		initial.Selection = model.Selection{}
	}
	initial.Baseline = nil
	changes, err := model.PlanChanges(before, entries)
	if err != nil {
		return nil, err
	}
	plan := transition{ID: uuid.NewString(), Base: stateDigest(initial), Target: target, Source: target.Version, Changes: changes}
	w, err := initialize(directory, initial, &plan)
	if err != nil {
		return nil, err
	}
	if err := w.resumeTransition(ctx, c, false); err != nil {
		w.Close()
		return nil, fmt.Errorf("clone in %q is unfinished; run lode resume or lode cancel in that directory: %w", directory, err)
	}
	return w, nil
}

// Materialized entries include parents before their children. Pre-existing
// directories are kept out of the plan so cancellation leaves them in place.
func cloneDirectories(ctx context.Context, directory string, entries []model.Entry) ([]model.Entry, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if _, err := root.Lstat(".lode"); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(errors.New("workspace metadata already exists; run lode status"), err)
	}
	var before []model.Entry
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := root.Lstat(filepath.FromSlash(entry.Path))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if entry.Kind != model.Directory || !info.IsDir() {
			return nil, fmt.Errorf("cannot clone over existing path %q; move it aside and retry", entry.Path)
		}
		before = append(before, entry)
	}
	return before, nil
}

type ConflictsError struct{ Conflicts []model.Conflict }

func (e *ConflictsError) Error() string {
	return fmt.Sprintf("%d conflicting paths; choose local or incoming contents", len(e.Conflicts))
}

// Update first settles a pending save, then reconciles the selected branch tip.
// The new baseline is the incoming version; retained local edits stay unsaved.
func (w *Workspace) Update(ctx context.Context, token string, choices map[string]model.Side) error {
	if pending, err := w.pending("save.json"); err != nil {
		return err
	} else if pending {
		if err := w.cancelSave(ctx, token); err != nil {
			var failure *remote.Error
			if !errors.As(err, &failure) || failure.Code != "operation_expired" {
				return err
			}
			// An expired outcome is unknown, not proof of non-publication.
			// Explicit update can reconcile from the old baseline. The server's
			// cutoff prevents the discarded request from publishing later.
			if err := w.remove("save.json"); err != nil {
				return err
			}
		}
	}
	if err := w.ready(); err != nil {
		return err
	}
	c, err := w.Client(token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	return w.update(ctx, c, choices)
}

func (w *Workspace) update(ctx context.Context, c *remote.Client, choices map[string]model.Side) error {
	target := w.state
	var err error
	target.Version, err = c.Head(ctx, target.Owner, target.Project, target.Branch)
	if err != nil {
		return err
	}
	target.Baseline, err = c.Inventory(ctx, target.Owner, target.Project, target.Version, target.Selection)
	if err != nil {
		return err
	}
	local, err := w.observeSelection(ctx, w.state.Selection, target.Baseline...)
	if err != nil {
		return err
	}
	merged, conflicts, err := model.Reconcile(w.state.Baseline, local, target.Baseline, choices)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return &ConflictsError{conflicts}
	}
	return w.beginTransition(ctx, c, target, target.Version, local, merged)
}

// Select changes local presence at the current version without moving the
// branch baseline. Dirty paths cannot be unloaded; newly loaded collisions use
// the same explicit whole-entry choices as update.
func (w *Workspace) Select(ctx context.Context, token string, selection model.Selection, choices map[string]model.Side) error {
	selection, err := withinScope(selection, w.state.Scope)
	if err != nil {
		return err
	}
	if err := w.ready(); err != nil {
		return err
	}
	old, err := w.observeSelection(ctx, w.state.Selection)
	if err != nil {
		return err
	}
	for _, e := range model.Changes(w.state.Baseline, model.Selected(old, w.state.Selection), w.state.Selection) {
		if !selection.Includes(e.Path) {
			return fmt.Errorf("cannot unload changed path %q; save or move it aside first", e.Path)
		}
	}
	c, err := w.Client(token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	target := w.state
	target.Selection = selection
	target.Baseline, err = c.Inventory(ctx, target.Owner, target.Project, target.Version, selection)
	if err != nil {
		return err
	}
	local, err := w.observeSelection(ctx, selection, target.Baseline...)
	if err != nil {
		return err
	}
	if len(target.Baseline) == 0 && len(model.Selected(local, selection)) == 0 && len(w.state.Baseline) > 0 {
		for _, rule := range selection {
			if rule.Include {
				return errors.New("selection matches no files or directories")
			}
		}
	}
	merged, conflicts, err := model.Reconcile(model.Selected(w.state.Baseline, selection), local, target.Baseline, choices)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return &ConflictsError{conflicts}
	}
	// Both observations describe the same disk, including structural parents.
	union := map[string]model.Entry{}
	for _, entries := range [][]model.Entry{old, local} {
		for _, e := range entries {
			if previous, ok := union[e.Path]; ok && previous != e {
				return fmt.Errorf("%q changed while inspecting the selection", e.Path)
			}
			union[e.Path] = e
		}
	}
	before := make([]model.Entry, 0, len(union))
	for _, e := range union {
		before = append(before, e)
	}
	return w.beginTransition(ctx, c, target, target.Version, before, merged)
}
