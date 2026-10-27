package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/model"
)

func (w *Workspace) Update(ctx context.Context, choices []model.Choice) error {
	if err := w.pending(); err != nil {
		return err
	}
	client, err := w.client()
	if err != nil {
		return err
	}
	target := w.state
	target.Base, err = client.Head(ctx, target.Branch)
	if err != nil {
		return err
	}
	if target.Base == w.state.Base && len(choices) == 0 {
		return nil
	}
	target.Baseline, err = client.Entries(ctx, target.Base, target.Selection)
	if err != nil {
		return err
	}
	return w.transitionTo(ctx, target, choices)
}

// Select changes presence at the current version. Unloading any local change,
// including a deletion or untracked entry, requires resolving it first.
func (w *Workspace) Select(ctx context.Context, selection model.Selection) error {
	if err := w.pending(); err != nil {
		return err
	}
	if err := selection.Validate(); err != nil {
		return err
	}
	if selection == nil {
		selection = model.Selection{}
	}
	client, err := w.client()
	if err != nil {
		return err
	}
	target := w.state
	target.Selection = selection
	target.Baseline, err = client.Entries(ctx, target.Base, selection)
	if err != nil {
		return err
	}
	return w.transitionTo(ctx, target, nil)
}

func (w *Workspace) transitionTo(ctx context.Context, target State, choices []model.Choice) error {
	if err := w.pending(); err != nil {
		return err
	}
	if err := target.validate(); err != nil {
		return err
	}
	local, err := w.observe(w.state.Selection, target.Selection)
	if err != nil {
		return err
	}
	for _, change := range model.Diff(w.state.Baseline, local) {
		p := change.After.Path
		if w.state.Selection.Includes(p) && !target.Selection.Includes(p) {
			return fmt.Errorf("cannot unload changed path %s", p)
		}
	}
	merged, err := model.Merge(w.state.Baseline, local, target.Baseline, choices)
	if err != nil {
		return err
	}
	inScope := func(p string) bool { return w.state.Selection.Includes(p) || target.Selection.Includes(p) }
	var desired []model.Entry
	for _, e := range merged {
		if inScope(e.Path) {
			desired = append(desired, e)
		}
	}
	desired, err = model.Apply(nil, desired)
	if err != nil {
		return err
	}
	local, err = model.Apply(nil, local)
	if err != nil {
		return err
	}
	old := map[string]model.Entry{}
	for _, e := range local {
		old[e.Path] = e
	}
	// Include physical containers omitted from selected inventories. Existing
	// unselected containers remain in place when their selected children leave.
	for _, e := range desired {
		if !inScope(e.Path) {
			actual, err := w.identity(e.Path, e.Path)
			if err != nil {
				return err
			}
			if actual.Kind != model.Absent && actual.Kind != model.Directory {
				return &model.Conflicts{Paths: []string{e.Path}}
			}
			if actual.Kind == model.Directory {
				old[e.Path] = actual
			}
		}
	}
	local = nil
	wanted := map[string]bool{}
	for _, e := range desired {
		wanted[e.Path] = true
	}
	for _, e := range old {
		local = append(local, e)
		if !inScope(e.Path) && !wanted[e.Path] {
			desired = append(desired, e)
		}
	}
	changes := model.Diff(local, desired)
	// Existing descendants leave before a parent changes type. New parents
	// arrive before new children. Each path is replaced only once.
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		oldA, oldB := a.Before.Kind != model.Absent, b.Before.Kind != model.Absent
		if oldA != oldB {
			return oldA
		}
		da, db := strings.Count(a.After.Path, "/"), strings.Count(b.After.Path, "/")
		if da != db {
			if oldA {
				return da > db
			}
			return da < db
		}
		return a.After.Path < b.After.Path
	})
	removed := map[string]bool{}
	for _, c := range changes {
		if c.After.Kind == model.Absent {
			removed[c.After.Path] = true
		}
	}
	physical := changes[:0]
	for _, c := range changes {
		keepContainer := false
		if c.Before.Kind == model.Directory && c.After.Kind != model.Directory {
			f, err := w.root.Open(c.Before.Path)
			if err != nil {
				return err
			}
			entries, readErr := f.ReadDir(-1)
			err = errors.Join(readErr, f.Close())
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !removed[path.Join(c.Before.Path, e.Name())] {
					if c.After.Kind == model.Absent && !target.Selection.Includes(c.After.Path) {
						keepContainer = true
						break
					}
					return fmt.Errorf("directory %s contains an unselected or changed entry: %s", c.Before.Path, e.Name())
				}
			}
		}
		if keepContainer {
			removed[c.After.Path] = false
			continue
		}
		physical = append(physical, c)
	}
	changes = physical
	if len(changes) == 0 {
		if err := w.store("state.json", target); err != nil {
			return err
		}
		w.state = target
		return nil
	}
	if err := w.root.Mkdir(".lode/transition", 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := w.root.Lstat(".lode/transition")
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("transition recovery path is not a directory")
		}
		if err := w.emptyDirectory(".lode/transition"); err != nil {
			return err
		}
	}
	if err := syncDirectory(w.root, ".lode"); err != nil {
		return err
	}
	plan := transition{Target: target, Changes: changes}
	if err := w.store("transition.json", plan); err != nil {
		return err
	}
	return w.runTransition(ctx, &plan)
}

func (w *Workspace) emptyDirectory(name string) error {
	f, err := w.root.Open(name)
	if err != nil {
		return err
	}
	entries, readErr := f.ReadDir(1)
	closeErr := f.Close()
	if len(entries) > 0 {
		return fmt.Errorf("directory %s is not empty", name)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return errors.Join(readErr, closeErr)
	}
	return closeErr
}
