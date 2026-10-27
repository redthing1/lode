package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

type recoveryAction struct {
	Path   string
	Before project.Node
	After  project.Node
	Old    string
	Fresh  string
}

type recoveryJournal struct {
	Version   int
	Base      string
	Branch    string
	Selection []project.SelectionRule
	Baseline  []project.Node
	Pending   pending
	Actions   []recoveryAction
}

func (w *Workspace) RecoveryDirectory() string { return w.lastRecovery }

func (w *Workspace) resumeAbandon(ctx context.Context) error {
	p := w.state.Pending
	if p == nil || p.Kind != "abandon" {
		return errors.New("no local materialization to abandon")
	}
	if p.Transition == nil && p.ReadID == "" {
		copyPending := *p
		copyPending.ReadID = randomID()
		next := w.state
		next.Pending = &copyPending
		if err := w.save(next); err != nil {
			return err
		}
		p = w.state.Pending
	}
	if err := validateState(w.state); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, encoded, id, err := w.abandonJournal()
	if err != nil {
		return err
	}
	archive := recoveryDirectory(id)
	stage := ""
	if p.Transition != nil {
		stage = transitionDirectory(p.Transition)
	}
	if exists, err := w.directoryExists(archive); err != nil {
		return err
	} else if exists {
		if err := w.requireRecoveryJournal(archive, encoded); err != nil {
			return err
		}
		if stage != "" {
			if stageExists, err := w.pathExists(stage); err != nil {
				return err
			} else if stageExists {
				return fmt.Errorf("%w: recovery archive and transition staging both exist", ErrConflict)
			}
		} else if buildExists, err := w.pathExists(recoveryBuildDirectory(id)); err != nil {
			return err
		} else if buildExists {
			return fmt.Errorf("%w: recovery archive and checkout staging both exist", ErrConflict)
		}
		return w.finishAbandon(id, archive)
	}

	if stage != "" {
		if err := w.ensureDirectory(stage); err != nil {
			return err
		}
		if err := w.ensureRecoveryJournal(stage, encoded); err != nil {
			return err
		}
		if err := w.restoreAbandonedBackups(ctx, p.Transition); err != nil {
			return err
		}
		if err := syncRoot(w.root, stage); err != nil {
			return err
		}
		if err := w.move(stage, archive); err != nil {
			return err
		}
	} else {
		build := recoveryBuildDirectory(id)
		if err := w.ensureDirectory(build); err != nil {
			return err
		}
		if err := w.ensureRecoveryJournal(build, encoded); err != nil {
			return err
		}
		if err := syncRoot(w.root, build); err != nil {
			return err
		}
		if err := w.move(build, archive); err != nil {
			return err
		}
	}
	if err := syncRoot(w.private, "."); err != nil {
		return err
	}
	if err := w.requireRecoveryJournal(archive, encoded); err != nil {
		return err
	}
	return w.finishAbandon(id, archive)
}

func (w *Workspace) abandonJournal() (recoveryJournal, []byte, string, error) {
	p := w.state.Pending
	original := *p
	if p.Transition != nil {
		original.Kind = "transition"
	} else {
		original.Kind = "checkout"
	}
	id := original.ReadID
	var actions []recoveryAction
	if original.Transition != nil {
		id = original.Transition.ID
		actions = make([]recoveryAction, 0, len(original.Transition.Actions))
		for index, action := range original.Transition.Actions {
			old, fresh := actionFiles(original.Transition, index)
			actions = append(actions, recoveryAction{
				Path: action.Path, Before: action.Before, After: action.After,
				Old: path.Base(old), Fresh: path.Base(fresh),
			})
		}
	}
	if !validReadID(id) {
		return recoveryJournal{}, nil, "", errors.New("invalid materialization recovery ID")
	}
	journal := recoveryJournal{
		Version: 1, Base: w.state.Base, Branch: w.state.Branch,
		Selection: append([]project.SelectionRule(nil), w.state.Selection...),
		Baseline:  append([]project.Node(nil), w.state.Baseline...),
		Pending:   original, Actions: actions,
	}
	encoded, err := json.Marshal(journal)
	if err != nil {
		return recoveryJournal{}, nil, "", err
	}
	return journal, append(encoded, '\n'), id, nil
}

func (w *Workspace) restoreAbandonedBackups(ctx context.Context, t *transition) error {
	var directories, files []int
	for i, action := range t.Actions {
		switch action.Before.Kind {
		case model.Directory:
			directories = append(directories, i)
		case model.File:
			files = append(files, i)
		}
	}
	less := func(indices []int) {
		sort.Slice(indices, func(i, j int) bool {
			a, b := t.Actions[indices[i]].Path, t.Actions[indices[j]].Path
			da, db := strings.Count(a, "/"), strings.Count(b, "/")
			if da != db {
				return da < db
			}
			return a < b
		})
	}
	less(directories)
	less(files)
	for _, index := range append(directories, files...) {
		if err := ctx.Err(); err != nil {
			return err
		}
		action := t.Actions[index]
		old, _ := actionFiles(t, index)
		backup, err := w.observePath(ctx, old)
		if err != nil {
			return err
		}
		if backup.Kind == model.Absent || !sameNode(backup, action.Before) {
			continue
		}
		if err := w.requireEmptyDirectory(old, backup); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return err
		}
		if err := w.checkParents(action.Path); err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrConflict) {
				continue
			}
			return err
		}
		if _, err := w.root.Lstat(action.Path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := w.move(old, action.Path); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return err
		}
		moved, err := w.observePath(ctx, action.Path)
		if err != nil {
			return err
		}
		if !sameNode(moved, action.Before) {
			return fmt.Errorf("%w: restored local entry changed at %q", ErrConflict, action.Path)
		}
	}
	return nil
}

func (w *Workspace) finishAbandon(id, archive string) error {
	if err := syncRoot(w.root, archive); err != nil {
		return err
	}
	if err := syncRoot(w.private, "."); err != nil {
		return err
	}
	next := w.state
	next.Pending = nil
	if err := w.save(next); err != nil {
		return err
	}
	w.lastRecovery = archive
	w.releaseReadBestEffort(id)
	return nil
}

func (w *Workspace) ensureDirectory(name string) error {
	err := w.root.Mkdir(name, 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := w.root.Lstat(name)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: recovery staging path is not a directory", ErrConflict)
	}
	return nil
}

func (w *Workspace) ensureRecoveryJournal(directory string, expected []byte) error {
	name := path.Join(directory, "journal.json")
	info, err := w.root.Lstat(name)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: recovery journal is not a regular file", ErrConflict)
		}
		return w.compareRecoveryJournal(name, expected)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := w.findRecoveryJournalTemp(directory, expected)
	if err != nil {
		return err
	}
	if temporary == "" {
		temporary = path.Join(directory, "journal-"+randomID()+".tmp")
		f, err := w.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow(), 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(expected)
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return err
		}
		if err := syncRoot(w.root, directory); err != nil {
			return err
		}
	}
	// A complete temporary file from an interrupted attempt may have been
	// written before that attempt reached Sync. Re-establish durability before
	// publishing its name, even when its bytes already match.
	if err := syncRoot(w.root, temporary); err != nil {
		return err
	}
	if err := w.move(temporary, name); err != nil {
		if _, statErr := w.root.Lstat(name); statErr == nil {
			return w.compareRecoveryJournal(name, expected)
		}
		return err
	}
	return w.compareRecoveryJournal(name, expected)
}

func (w *Workspace) findRecoveryJournalTemp(directory string, expected []byte) (string, error) {
	f, err := w.root.OpenFile(directory, readFlags(), 0)
	if err != nil {
		return "", err
	}
	names, readErr := f.Readdirnames(-1)
	if err = errors.Join(readErr, f.Close()); err != nil {
		return "", err
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasPrefix(name, "journal-") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		candidate := path.Join(directory, name)
		if err := w.compareRecoveryJournal(candidate, expected); err == nil {
			return candidate, nil
		} else if !errors.Is(err, ErrConflict) && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

func (w *Workspace) requireRecoveryJournal(directory string, expected []byte) error {
	info, err := w.root.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: recovery archive is not a directory", ErrConflict)
	}
	if err := w.compareRecoveryJournal(path.Join(directory, "journal.json"), expected); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: recovery archive has no journal", ErrConflict)
		}
		return err
	}
	return nil
}

func (w *Workspace) compareRecoveryJournal(name string, expected []byte) error {
	f, err := w.root.OpenFile(name, readFlags(), 0)
	if err != nil {
		return err
	}
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return fmt.Errorf("%w: recovery journal is not a regular file", ErrConflict)
	}
	actual, readErr := io.ReadAll(io.LimitReader(f, int64(len(expected))+1))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("%w: recovery journal does not match pending operation", ErrConflict)
	}
	return nil
}

func (w *Workspace) pathExists(name string) (bool, error) {
	_, err := w.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (w *Workspace) directoryExists(name string) (bool, error) {
	info, err := w.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%w: recovery path is not a directory", ErrConflict)
	}
	return true, nil
}

func recoveryDirectory(id string) string      { return ".lode/recovery-" + id }
func recoveryBuildDirectory(id string) string { return ".lode/recovery-build-" + id }
