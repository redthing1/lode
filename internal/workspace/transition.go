package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
	activity "github.com/redthing1/lode/internal/progress"
	"github.com/redthing1/lode/internal/remote"
)

type transition struct {
	ID      string         `json:"id"`
	Base    string         `json:"base"`
	Target  State          `json:"target"`
	Source  string         `json:"source"`
	Changes []model.Change `json:"changes"`
}
type transitionProgress struct {
	ID      string `json:"id"`
	Cursor  int    `json:"cursor"`
	Reverse bool   `json:"reverse,omitempty"`
}

func stateDigest(s State) string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (w *Workspace) ready() error {
	for _, name := range []string{"save.json", "transition.json"} {
		if exists, err := w.pending(name); err != nil {
			return err
		} else if exists {
			return ErrPending
		}
	}
	return w.clearTransition()
}

func (w *Workspace) beginTransition(ctx context.Context, c *remote.Client, target State, source string, before, after []model.Entry) error {
	if err := target.validate(); err != nil {
		return err
	}
	changes, err := model.PlanChanges(before, after)
	if err != nil {
		return err
	}
	changes, err = w.keepUnselectedDirectories(changes)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		if err := w.writeJSON("state.json", target); err != nil {
			return err
		}
		w.state = target
		return nil
	}
	id := uuid.NewString()
	plan := transition{ID: id, Base: stateDigest(w.state), Target: target, Source: source, Changes: changes}
	if err := w.writeTransition(plan); err != nil {
		return err
	}
	if err := w.resumeTransition(ctx, c, false); err != nil {
		return fmt.Errorf("workspace operation unfinished; run lode resume or lode cancel: %w", err)
	}
	return nil
}

func (w *Workspace) writeTransition(plan transition) error {
	if err := w.meta.Mkdir("staging", 0700); err != nil {
		return err
	}
	if err := syncPath(w.meta, "."); err != nil {
		return err
	}
	// Progress is durable before the plan authorizes any working-file changes.
	if err := w.writeJSON("progress.json", transitionProgress{ID: plan.ID}); err != nil {
		return err
	}
	if err := w.writeJSON("transition.json", plan); err != nil {
		return err
	}
	return nil
}

// An excluded child can keep a physical parent present even when the selected
// tree no longer needs it. A file cannot replace such a parent.
func (w *Workspace) keepUnselectedDirectories(changes []model.Change) ([]model.Change, error) {
	removed, created := map[string]bool{}, map[string]bool{}
	for _, c := range changes {
		if c.After.Kind == model.Absent {
			removed[c.Before.Path] = true
		} else {
			created[c.After.Path] = true
		}
	}
	var result []model.Change
	for _, c := range changes {
		if c.Before.Kind == model.Directory && c.After.Kind == model.Absent {
			f, err := w.root.Open(filepath.FromSlash(c.Before.Path))
			if err != nil {
				return nil, err
			}
			children, err := f.ReadDir(-1)
			closeErr := f.Close()
			if err = errors.Join(err, closeErr); err != nil {
				return nil, err
			}
			keep := false
			for _, child := range children {
				if !removed[path.Join(c.Before.Path, child.Name())] {
					keep = true
					break
				}
			}
			if keep {
				if created[c.Before.Path] {
					return nil, fmt.Errorf("cannot replace %q: it contains local files outside this operation", c.Before.Path)
				}
				delete(removed, c.Before.Path)
				continue
			}
		}
		result = append(result, c)
	}
	return result, nil
}

func (w *Workspace) resumeTransition(ctx context.Context, c *remote.Client, cancel bool) error {
	if info, err := w.meta.Lstat("staging"); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New("invalid recovery directory")
	}
	var plan transition
	if err := w.readJSON("transition.json", &plan); err != nil {
		return err
	}
	if err := plan.Target.validate(); err != nil {
		return err
	}
	if plan.ID == "" || (stateDigest(w.state) != plan.Base && stateDigest(w.state) != stateDigest(plan.Target)) {
		return errors.New("operation does not match the workspace baseline")
	}
	var progress transitionProgress
	if err := w.readJSON("progress.json", &progress); err != nil {
		return err
	}
	if progress.ID != plan.ID || progress.Cursor < -1 || progress.Cursor > len(plan.Changes) || (!progress.Reverse && progress.Cursor < 0) || (progress.Reverse && progress.Cursor >= len(plan.Changes)) {
		return errors.New("invalid workspace progress")
	}
	for _, a := range plan.Changes {
		if a.Before.Path != a.After.Path || !model.ValidPath(a.Before.Path) || a.Before == a.After {
			return errors.New("invalid workspace action")
		}
		if err := model.ValidateChanges([]model.Entry{a.Before}); err != nil {
			return err
		}
		if err := model.ValidateChanges([]model.Entry{a.After}); err != nil {
			return err
		}
	}
	// Reaching the end commits the operation. Cancellation after that point only
	// completes the baseline write and cleanup, even if that write was interrupted.
	if cancel && !progress.Reverse && progress.Cursor < len(plan.Changes) {
		progress.Reverse = true
		if err := w.writeJSON("progress.json", progress); err != nil {
			return err
		}
	}
	preparedUntil := progress.Cursor
	for progress.Reverse && progress.Cursor >= 0 || !progress.Reverse && progress.Cursor < len(plan.Changes) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !progress.Reverse && c != nil && progress.Cursor >= preparedUntil {
			preparedUntil = min(progress.Cursor+remote.DownloadBatchFiles, len(plan.Changes))
			if err := w.prepareFiles(ctx, c, plan, progress.Cursor, preparedUntil); err != nil {
				return err
			}
		}
		a := plan.Changes[progress.Cursor]
		var err error
		if progress.Reverse {
			activity.Report(ctx, activity.Event{Phase: "Reverting files", Path: a.Before.Path})
			err = w.reverseAction(ctx, a, progress.Cursor)
		} else {
			activity.Report(ctx, activity.Event{Phase: "Updating files", Path: a.After.Path})
			err = w.applyAction(ctx, c, plan.Source, a, progress.Cursor)
		}
		if err != nil {
			return err
		}
		if progress.Reverse {
			progress.Cursor--
		} else {
			progress.Cursor++
		}
		if err := w.writeJSON("progress.json", progress); err != nil {
			return err
		}
	}
	activity.Report(ctx, activity.Event{Phase: "Finishing workspace"})
	if !progress.Reverse {
		if err := w.writeJSON("state.json", plan.Target); err != nil {
			return err
		}
		w.state = plan.Target
	}
	if err := w.cleanTransition(ctx, plan); err != nil {
		return err
	}
	if err := w.remove("transition.json"); err != nil {
		return err
	}
	return w.clearTransition()
}

func stageName(index int, side string) string { return "staging/" + strconv.Itoa(index) + "." + side }
func stagePath(index int, side string) string { return ".lode/" + stageName(index, side) }

func (w *Workspace) matchesStage(name string, index int, side string) (bool, error) {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := w.root.Lstat(filepath.FromSlash(parent))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, nil
		}
	}
	current, err := w.root.Lstat(filepath.FromSlash(name))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current.IsDir() {
		var id string
		err := w.readJSON(stageName(index, side)+".id", &id)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		f, err := w.root.Open(filepath.FromSlash(name))
		if err != nil {
			return false, err
		}
		defer f.Close()
		actual, err := directoryIdentity(f)
		return actual == id, err
	}
	saved, err := w.meta.Lstat(stageName(index, side))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return current.Mode().IsRegular() && saved.Mode().IsRegular() && os.SameFile(current, saved), nil
}

func (w *Workspace) verifyFile(ctx context.Context, name string, e model.Entry) error {
	info, err := w.root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("preserving unexpected local entry %q", name)
	}
	got, err := w.observeFile(ctx, name, info)
	if err != nil {
		return err
	}
	got.Path = e.Path
	if got != e {
		return fmt.Errorf("preserving changed file %q", name)
	}
	return nil
}

func (w *Workspace) prepareEntry(ctx context.Context, c *remote.Client, version string, change model.Change, index int) error {
	e := change.After
	if e.Kind == model.Absent {
		return nil
	}
	name := stageName(index, "new")
	info, err := w.meta.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if e.Kind == model.Directory {
		if errors.Is(err, os.ErrNotExist) {
			if exists, err := w.pending(name + ".id"); err != nil {
				return err
			} else if exists {
				return fmt.Errorf("missing staged directory for %q", e.Path)
			}
			if err := w.meta.Mkdir(name, 0755); err != nil {
				return err
			}
		} else if !info.IsDir() {
			return fmt.Errorf("invalid staged directory for %q", e.Path)
		}
		return w.recordDirectory(name, index, "new")
	}
	if err == nil {
		return w.verifyFile(ctx, stagePath(index, "new"), e)
	}
	if c == nil {
		return errors.New("resuming this operation requires server access")
	}
	var basis io.ReadSeeker
	if change.Before.Kind == model.File {
		f, err := w.openSource(change.Before)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			defer f.Close()
			basis = f
		}
	}
	return w.stageFile(index, func(out io.Writer) error {
		return c.Download(ctx, w.state.Owner, w.state.Project, version, e, basis, change.Before.Size, out)
	})
}

func (w *Workspace) stageFile(index int, write func(io.Writer) error) error {
	name := stageName(index, "new")
	part := name + ".part"
	if err := w.remove(part); err != nil {
		return err
	}
	f, err := w.meta.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := w.meta.Rename(part, name); err != nil {
		return err
	}
	return syncPath(w.meta, "staging")
}

// Prepare only missing small-file replacements within the next action window.
// Verified files use the existing staging names; no transfer state is persisted.
func (w *Workspace) prepareFiles(ctx context.Context, c *remote.Client, plan transition, start, end int) error {
	var files []model.Entry
	var indexes []int
	var bytes int64
	flush := func() error {
		if len(files) == 0 {
			return nil
		}
		err := c.DownloadBatch(ctx, w.state.Owner, w.state.Project, plan.Source, files, func(i int, write func(io.Writer) error) error {
			return w.stageFile(indexes[i], write)
		})
		files, indexes, bytes = nil, nil, 0
		return err
	}
	for i := start; i < end; i++ {
		e := plan.Changes[i].After
		if e.Kind != model.File || e.Size > remote.DownloadBatchFileSize {
			continue
		}
		if exists, err := w.pending(stageName(i, "new")); err != nil {
			return err
		} else if exists {
			continue
		}
		if bytes+e.Size > remote.DownloadBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		files, indexes, bytes = append(files, e), append(indexes, i), bytes+e.Size
	}
	return flush()
}

func (w *Workspace) recordDirectory(name string, index int, side string) error {
	f, err := w.meta.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	id, err := directoryIdentity(f)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return w.writeJSON(stageName(index, side)+".id", id)
}

func (w *Workspace) preserveEntry(ctx context.Context, e model.Entry, index int) error {
	if e.Kind == model.Absent {
		return nil
	}
	name := e.Path
	old := stageName(index, "old")
	exists, err := w.pending(old)
	if err != nil {
		return err
	}
	if !exists {
		info, err := w.root.Lstat(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		if e.Kind == model.Directory {
			if !info.IsDir() {
				return fmt.Errorf("preserving unexpected local entry %q", name)
			}
			var id string
			idName := old + ".id"
			if err := w.readJSON(idName, &id); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if id != "" {
				same, err := w.matchesStage(name, index, "old")
				if err != nil {
					return err
				}
				if !same {
					return fmt.Errorf("preserving replaced directory %q", name)
				}
			} else {
				f, err := w.root.Open(filepath.FromSlash(name))
				if err != nil {
					return err
				}
				id, err = directoryIdentity(f)
				err = errors.Join(err, f.Close())
				if err != nil {
					return err
				}
				if err := w.writeJSON(idName, id); err != nil {
					return err
				}
			}
			if err := w.moveEmptyDirectory(name, stagePath(index, "old")); err != nil {
				return err
			}
		} else {
			if err := w.verifyFile(ctx, name, e); err != nil {
				return err
			}
			if err := w.root.Link(filepath.FromSlash(name), filepath.FromSlash(stagePath(index, "old"))); err != nil {
				return err
			}
			if err := syncPath(w.meta, "staging"); err != nil {
				return err
			}
		}
	}
	if e.Kind == model.File {
		if exists {
			if err := w.verifyFile(ctx, stagePath(index, "old"), e); err != nil {
				return err
			}
		}
		same, err := w.matchesStage(name, index, "old")
		if err != nil {
			return err
		}
		if same {
			if err := syncPath(w.meta, old); err != nil {
				return err
			}
			if err := w.root.Remove(filepath.FromSlash(name)); err != nil {
				return err
			}
			if err := syncPath(w.root, filepath.FromSlash(path.Dir(name))); err != nil {
				return err
			}
		} else if _, err := w.root.Lstat(filepath.FromSlash(name)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("preserving unexpected local entry %q", name), err)
		}
	}
	return nil
}

func (w *Workspace) applyAction(ctx context.Context, c *remote.Client, version string, a model.Change, index int) error {
	name := a.Before.Path
	if err := w.checkParents(name); err != nil {
		return err
	}
	if a.After.Kind != model.Absent {
		done, err := w.matchesStage(name, index, "new")
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	if err := w.prepareEntry(ctx, c, version, a, index); err != nil {
		return err
	}
	if err := w.preserveEntry(ctx, a.Before, index); err != nil {
		return err
	}
	if _, err := w.root.Lstat(filepath.FromSlash(name)); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("preserving unexpected local entry %q", name), err)
	}
	switch a.After.Kind {
	case model.File:
		if err := w.root.Link(filepath.FromSlash(stagePath(index, "new")), filepath.FromSlash(name)); err != nil {
			return err
		}
		return syncPath(w.root, filepath.FromSlash(path.Dir(name)))
	case model.Directory:
		return w.moveEmptyDirectory(stagePath(index, "new"), name)
	}
	return nil
}

func (w *Workspace) moveEmptyDirectory(from, to string) error {
	f, err := w.root.Open(filepath.FromSlash(from))
	if err != nil {
		return err
	}
	children, err := f.ReadDir(1)
	err = errors.Join(ignoreEOF(err), f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if len(children) != 0 {
		return fmt.Errorf("preserving nonempty directory %q", from)
	}
	if _, err := w.root.Lstat(filepath.FromSlash(to)); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("preserving unexpected local entry %q", to), err)
	}
	if err := w.root.Rename(filepath.FromSlash(from), filepath.FromSlash(to)); err != nil {
		return err
	}
	return errors.Join(syncPath(w.root, filepath.FromSlash(path.Dir(from))), syncPath(w.root, filepath.FromSlash(path.Dir(to))))
}
func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (w *Workspace) reverseAction(ctx context.Context, a model.Change, index int) error {
	name := a.Before.Path
	// This action may not have started, or may already have been reversed before
	// the progress write. In either case leave subsequent local edits alone.
	if a.Before.Kind != model.Absent {
		same, err := w.matchesStage(name, index, "old")
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		exists, err := w.pending(stageName(index, "old"))
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
	}
	if err := w.checkParents(name); err != nil {
		return err
	}
	installed, err := w.matchesStage(name, index, "new")
	if err != nil {
		return err
	}
	if installed {
		if a.After.Kind == model.File {
			if err := w.verifyFile(ctx, name, a.After); err != nil {
				return err
			}
			if err := w.root.Remove(filepath.FromSlash(name)); err != nil {
				return err
			}
			if err := syncPath(w.root, filepath.FromSlash(path.Dir(name))); err != nil {
				return err
			}
		} else if a.After.Kind == model.Directory {
			if err := w.moveEmptyDirectory(name, stagePath(index, "new")); err != nil {
				return err
			}
		}
	}
	if a.Before.Kind == model.Absent {
		return nil
	}
	if _, err := w.root.Lstat(filepath.FromSlash(name)); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("preserving unexpected local entry %q", name), err)
	}
	if a.Before.Kind == model.Directory {
		return w.moveEmptyDirectory(stagePath(index, "old"), name)
	}
	if err := syncPath(w.meta, stageName(index, "old")); err != nil {
		return err
	}
	if err := w.root.Link(filepath.FromSlash(stagePath(index, "old")), filepath.FromSlash(name)); err != nil {
		return err
	}
	return syncPath(w.root, filepath.FromSlash(path.Dir(name)))
}

func (w *Workspace) cleanTransition(ctx context.Context, plan transition) error {
	// Cleanup is replayable while transition.json exists. Sync these flat
	// staging entries together before allowing the caller to remove that plan.
	remove := func(name string) error {
		err := w.meta.Remove(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for i, a := range plan.Changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		for j, e := range []model.Entry{a.Before, a.After} {
			side := "old"
			if j == 1 {
				side = "new"
			}
			name := stageName(i, side)
			info, err := w.meta.Lstat(name)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				if info.Mode().IsRegular() {
					same, err := w.matchesStage(e.Path, i, side)
					if err != nil {
						return err
					}
					if !same {
						if err := w.verifyFile(ctx, stagePath(i, side), e); err != nil {
							return fmt.Errorf("move preserved recovery contents outside .lode, then resume: %w", err)
						}
					}
				} else if !info.IsDir() {
					return fmt.Errorf("unexpected recovery entry %q", name)
				}
				if err := remove(name); err != nil {
					return err
				}
			}
			if err := remove(name + ".id"); err != nil {
				return err
			}
			if err := remove(name + ".id.next"); err != nil {
				return err
			}
		}
		if err := remove(stageName(i, "new") + ".part"); err != nil {
			return err
		}
	}
	return syncPath(w.meta, "staging")
}

func (w *Workspace) clearTransition() error {
	if err := w.meta.Remove("staging"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return w.remove("progress.json")
}
