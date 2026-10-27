package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

// Next counts completed actions. Reaching the end durably decides to commit;
// cancellation instead counts down, including the possibly interrupted action.
type transition struct {
	Target    State          `json:"target"`
	Changes   []model.Change `json:"changes"`
	Next      int            `json:"-"`
	Cancelled bool           `json:"-"`
	Source    string         `json:"source,omitempty"`
}

type transitionProgress struct {
	Next      int  `json:"next"`
	Cancelled bool `json:"cancelled"`
}

func (w *Workspace) loadTransition() (transition, error) {
	var p transition
	if err := w.load("transition.json", &p); err != nil {
		return p, err
	}
	var progress transitionProgress
	if err := w.load("transition-progress.json", &progress); err != nil {
		return p, fmt.Errorf("load transition progress: %w", err)
	}
	p.Next, p.Cancelled = progress.Next, progress.Cancelled
	return p, nil
}

func (w *Workspace) storeTransitionProgress(p *transition) error {
	return w.store("transition-progress.json", transitionProgress{Next: p.Next, Cancelled: p.Cancelled})
}

func recoveryName(i int, suffix string) string {
	return fmt.Sprintf(".lode/transition/%d.%s", i, suffix)
}

func (w *Workspace) runTransition(ctx context.Context, p *transition) error {
	if err := p.Target.validate(); err != nil {
		return err
	}
	if p.Target.Project != w.state.Project || p.Next < 0 || p.Next > len(p.Changes) {
		return errors.New("invalid transition state")
	}
	if p.Source != "" && !project.ValidID(p.Source) {
		return errors.New("invalid transition content source")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	version := p.Source
	if version == "" {
		version = p.Target.Base
	}
	for _, c := range p.Changes {
		if err := c.Before.Validate(); err != nil {
			return err
		}
		if err := c.After.Validate(); err != nil {
			return err
		}
		if c.Before.Path != c.After.Path || c.Before == c.After {
			return errors.New("invalid transition change")
		}
	}
	if p.Cancelled {
		for p.Next > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			i := p.Next - 1
			if err := w.undoChange(ctx, p.Changes[i], i); err != nil {
				return err
			}
			p.Next--
			if err := w.storeTransitionProgress(p); err != nil {
				return err
			}
		}
	} else {
		for p.Next < len(p.Changes) {
			if err := ctx.Err(); err != nil {
				return err
			}
			i := p.Next
			if err := w.applyChange(ctx, version, p.Changes[i], i); err != nil {
				return err
			}
			p.Next++
			if err := w.storeTransitionProgress(p); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.store("state.json", p.Target); err != nil {
			return err
		}
		w.state = p.Target
	}
	if err := w.cleanTransition(ctx, p); err != nil {
		return err
	}
	if err := w.remove("transition.json"); err != nil {
		return err
	}
	return w.remove("transition-progress.json")
}

func (w *Workspace) cancelTransition(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := w.loadTransition()
	if err != nil {
		return err
	}
	if !p.Cancelled {
		if p.Next == len(p.Changes) {
			return errors.New("transition already committed; use resume to finish cleanup")
		}
		p.Cancelled = true
		p.Next++ // The active action may already have moved or installed a file.
		if err := w.storeTransitionProgress(&p); err != nil {
			return err
		}
	}
	return w.runTransition(ctx, &p)
}

func (w *Workspace) applyChange(ctx context.Context, version string, c model.Change, i int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, old, new := c.After.Path, recoveryName(i, "old"), recoveryName(i, "new")
	if c.After.Kind == model.File {
		if err := w.stage(ctx, version, c, i); err != nil {
			return err
		}
	}
	backup, err := w.identity(ctx, old, name)
	if err != nil {
		return err
	}
	if c.Before.Kind != model.Absent && backup.Kind == model.Absent {
		actual, err := w.identity(ctx, name, name)
		if err != nil {
			return err
		}
		if actual != c.Before {
			return fmt.Errorf("%s changed since the transition was planned", name)
		}
		if actual.Kind == model.Directory {
			if err := w.emptyDirectory(name); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := renameNoReplace(w.root, name, old); err != nil {
			return err
		}
		backup, err = w.identity(ctx, old, name)
		if err != nil {
			return err
		}
	}
	if backup != c.Before {
		return fmt.Errorf("%s changed during replacement; its contents are preserved at %s; cancel to restore them", name, old)
	}
	if backup.Kind == model.Directory {
		if err := w.emptyDirectory(old); err != nil {
			return err
		}
	}
	if backup.Kind == model.File {
		f, err := openRegular(w.root, old)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, statErr := w.root.Lstat(name)
	missing := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !missing {
		return statErr
	}
	switch c.After.Kind {
	case model.Absent:
		if !missing {
			return fmt.Errorf("unexpected entry at %s", name)
		}
	case model.Directory:
		if missing {
			if err := w.root.Mkdir(name, 0755); err != nil {
				return err
			}
		} else if !info.IsDir() {
			return fmt.Errorf("unexpected entry at %s", name)
		}
	case model.File:
		if missing {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.root.Link(new, name); err != nil {
				return err
			}
		} else if !w.sameFile(new, name) {
			actual, err := w.identity(ctx, name, name)
			if err != nil {
				return err
			}
			if actual != c.After {
				return fmt.Errorf("unexpected contents at %s; move them aside before resuming", name)
			}
		}
	}
	return errors.Join(syncDirectory(w.root, path.Dir(name)), syncDirectory(w.root, ".lode/transition"))
}

func (w *Workspace) stage(ctx context.Context, version string, change model.Change, i int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := change.After
	new, part := recoveryName(i, "new"), recoveryName(i, "part")
	if w.sameFile(new, e.Path) {
		return nil
	} // An installed inode may have newer edits.
	entry, err := w.identity(ctx, new, e.Path)
	if err != nil {
		return err
	}
	if entry == e {
		return nil
	}
	if entry.Kind != model.Absent {
		return fmt.Errorf("staged contents changed; preserve %s before resuming", new)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.root.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	client, err := w.client()
	if err != nil {
		return err
	}
	var basis *os.File
	if change.Before.Kind == model.File {
		basis, err = w.openFile(e.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if basis != nil {
			defer basis.Close()
		}
	}
	f, err := openNoFollow(w.root, part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if basis == nil {
		err = client.Download(ctx, version, e, nil, 0, f)
	} else {
		err = client.Download(ctx, version, e, basis, change.Before.Size, f)
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return renameNoReplace(w.root, part, new)
}

func (w *Workspace) undoChange(ctx context.Context, c model.Change, i int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, old, new, undo := c.After.Path, recoveryName(i, "old"), recoveryName(i, "new"), recoveryName(i, "undo")
	backup, err := w.identity(ctx, old, name)
	if err != nil {
		return err
	}
	if c.Before.Kind != model.Absent && backup.Kind == model.Absent {
		// Either this action never moved the original, or a previous rollback
		// restored it before its progress write. Preserve the current entry.
		return errors.Join(syncDirectory(w.root, path.Dir(name)), syncDirectory(w.root, ".lode/transition"))
	}
	removed, err := w.identity(ctx, undo, name)
	if err != nil {
		return err
	}
	if removed.Kind != model.Absent {
		if removed != c.After || (removed.Kind == model.Directory && w.emptyDirectory(undo) != nil) {
			if _, err := w.root.Lstat(name); errors.Is(err, os.ErrNotExist) {
				_ = renameNoReplace(w.root, undo, name)
			}
			return fmt.Errorf("changed contents preserved at %s or %s; move them aside before cancelling", name, undo)
		}
	} else {
		current, err := w.identity(ctx, name, name)
		if err != nil {
			return err
		}
		if current.Kind != model.Absent {
			if c.Before.Kind == model.Absent && c.After.Kind == model.File && !w.sameFile(new, name) {
				return nil
			}
			if current != c.After {
				return fmt.Errorf("%s has newer changes; move it aside before cancelling", name)
			}
			if current.Kind == model.Directory {
				if err := w.emptyDirectory(name); err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := renameNoReplace(w.root, name, undo); err != nil {
				return err
			}
			removed, err = w.identity(ctx, undo, name)
			if err != nil {
				return err
			}
			if removed != c.After || (removed.Kind == model.Directory && w.emptyDirectory(undo) != nil) {
				_ = renameNoReplace(w.root, undo, name)
				return fmt.Errorf("%s changed during cancellation; its contents are preserved", name)
			}
		}
	}
	if backup.Kind != model.Absent {
		if err := ctx.Err(); err != nil {
			return err
		}
		return renameNoReplace(w.root, old, name)
	}
	return syncDirectory(w.root, path.Dir(name))
}

func (w *Workspace) cleanTransition(ctx context.Context, p *transition) error {
	for i, c := range p.Changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, suffix := range []string{"old", "new", "undo", "part"} {
			name := recoveryName(i, suffix)
			info, err := w.root.Lstat(name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if suffix != "part" {
				// An extra hard link means removing this private name cannot
				// remove the only named copy of a user's subsequent edit.
				linked := suffix != "old" && info.Mode().IsRegular() && hasMultipleLinks(info)
				if !linked {
					actual, err := w.identity(ctx, name, c.After.Path)
					if err != nil {
						return err
					}
					expected := c.After
					if suffix == "old" {
						expected = c.Before
					}
					if actual != expected {
						return fmt.Errorf("changed contents preserved at %s; move them aside before resuming cleanup", name)
					}
				}
				if info.IsDir() {
					if err := w.emptyDirectory(name); err != nil {
						return err
					}
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.root.Remove(name); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.root.Remove(".lode/transition"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(w.root, ".lode")
}

func (w *Workspace) sameFile(a, b string) bool {
	one, err := w.root.Lstat(a)
	if err != nil || !one.Mode().IsRegular() {
		return false
	}
	two, err := w.root.Lstat(b)
	return err == nil && two.Mode().IsRegular() && os.SameFile(one, two)
}
