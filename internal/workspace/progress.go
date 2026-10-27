package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// A transition's plan stays in state.json. Only this small cursor is replaced
// after each completed action, so checkpoint IO is independent of plan width.
// The digest binds the cursor to the exact immutable plan, including its ID.
type transitionProgress struct {
	Version int
	ID      string
	Plan    string
	Index   int
}

func transitionID(p *pending) string {
	if p == nil || p.Transition == nil {
		return ""
	}
	return p.Transition.ID
}

func progressName(id string) string { return "progress-" + id + ".json" }

func progressPlan(t *transition) (string, error) {
	plan := *t
	plan.Index = 0
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// prepareProgress publishes a durable initial cursor before state.json can
// refer to it. A crash in between leaves only a disposable orphan cursor.
func (w *Workspace) prepareProgress(next state) (state, error) {
	if next.Pending == nil || next.Pending.Transition == nil {
		w.progressID, w.progressHash = "", ""
		return next, nil
	}
	t := next.Pending.Transition
	if t.ProgressVersion == 0 && next.Pending.Kind == "transition" {
		copyTransition := *t
		copyTransition.ProgressVersion = 1
		copyPending := *next.Pending
		copyPending.Transition = &copyTransition
		next.Pending = &copyPending
		t = &copyTransition
		hash, err := progressPlan(t)
		if err != nil {
			return next, err
		}
		initial := transitionProgress{Version: 1, ID: t.ID, Plan: hash, Index: t.Index}
		existing, err := w.readProgress(t.ID)
		if err == nil {
			if existing != initial {
				return next, errors.New("initial transition progress conflicts with an existing cursor")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return next, err
		}
		if _, err := w.writeProgress(initial); err != nil {
			return next, err
		}
	}
	if t.ProgressVersion == 1 {
		hash, err := progressPlan(t)
		if err != nil {
			return next, err
		}
		w.progressID, w.progressHash = t.ID, hash
	}
	return next, nil
}

func (w *Workspace) readProgress(id string) (transitionProgress, error) {
	f, err := w.private.OpenFile(progressName(id), readFlags(), 0)
	if err != nil {
		return transitionProgress{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return transitionProgress{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1024 {
		return transitionProgress{}, errors.New("transition progress must be a small private regular file")
	}
	var progress transitionProgress
	decoder := json.NewDecoder(io.LimitReader(f, 1025))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&progress); err != nil {
		return progress, fmt.Errorf("read transition progress: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || progress.Version != 1 || progress.ID != id || !validReadID(progress.ID) || progress.Index < 0 {
		return progress, errors.New("invalid transition progress")
	}
	digest, err := hex.DecodeString(progress.Plan)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != progress.Plan {
		return progress, errors.New("invalid transition progress plan digest")
	}
	return progress, nil
}

func (w *Workspace) loadProgress() error {
	if w.state.Pending == nil || w.state.Pending.Transition == nil || w.state.Pending.Transition.ProgressVersion == 0 {
		return w.pruneProgress()
	}
	t := w.state.Pending.Transition
	progress, err := w.readProgress(t.ID)
	if err != nil {
		return fmt.Errorf("open pending transition progress: %w", err)
	}
	hash, err := progressPlan(t)
	if err != nil {
		return err
	}
	if progress.Plan != hash || progress.Index < t.Index || progress.Index > len(t.Actions) {
		return errors.New("transition progress does not match its sealed plan")
	}
	t.Index = progress.Index
	w.progressID, w.progressHash = t.ID, hash
	return w.pruneProgress()
}

func (w *Workspace) checkpointTransition(index int) error {
	t := w.state.Pending.Transition
	if t.ProgressVersion != 1 || w.progressID != t.ID || w.progressHash == "" || index != t.Index+1 || index > len(t.Actions) {
		return errors.New("invalid transition checkpoint")
	}
	progress := transitionProgress{Version: 1, ID: t.ID, Plan: w.progressHash, Index: index}
	replaced, err := w.writeProgress(progress)
	if replaced {
		copyTransition := *t
		copyTransition.Index = index
		copyPending := *w.state.Pending
		copyPending.Transition = &copyTransition
		w.state.Pending = &copyPending
	}
	return err
}

func (w *Workspace) writeProgress(progress transitionProgress) (bool, error) {
	encoded, err := json.Marshal(progress)
	if err != nil {
		return false, err
	}
	return w.replacePrivate(progressName(progress.ID), "progress-", append(encoded, '\n'))
}

// replacePrivate replaces metadata only after its complete new bytes are synced.
// A true result means the rename completed even if the parent sync then failed.
func (w *Workspace) replacePrivate(destination, prefix string, data []byte) (bool, error) {
	name := prefix + randomID() + ".tmp"
	f, err := w.private.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	defer w.private.Remove(name)
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return false, err
	}
	replace := w.private.Rename
	if w.replaceMetadata != nil {
		replace = w.replaceMetadata
	}
	if err := replace(name, destination); err != nil {
		return false, err
	}
	return true, syncRoot(w.private, ".")
}

func (w *Workspace) removeProgress(id string) error {
	if err := w.private.Remove(progressName(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRoot(w.private, ".")
}

// Only well-formed generated cursors without an active owner are disposable.
// A legacy inline journal also owns its ID during a possible interrupted upgrade.
func (w *Workspace) pruneProgress() error {
	f, err := w.private.Open(".")
	if err != nil {
		return err
	}
	names, readErr := f.Readdirnames(-1)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return err
	}
	active := transitionID(w.state.Pending)
	for _, name := range names {
		if !strings.HasPrefix(name, "progress-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "progress-"), ".json")
		if !validReadID(id) || id == active {
			continue
		}
		if _, err := w.readProgress(id); err != nil {
			continue
		}
		if err := w.removeProgress(id); err != nil {
			return err
		}
	}
	return nil
}
