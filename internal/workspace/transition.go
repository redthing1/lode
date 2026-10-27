package workspace

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

type transition struct {
	ID              string
	Operation       string
	Source          string
	Base            string
	Branch          string
	Selection       []project.SelectionRule
	Baseline        []project.Node
	Actions         []fileAction
	Index           int
	ProgressVersion int `json:",omitempty"`
}

type fileAction struct {
	Path   string
	Before project.Node
	After  project.Node
}

type transitionOptions struct {
	restoreScope model.Selection
	choices      map[model.Path]model.Side
	resumeMerge  bool
}

// Update reconciles the selected working tree with the current branch tip.
// Local-only changes remain changes against the new baseline.
func (w *Workspace) Update(ctx context.Context) error {
	return w.UpdateWithChoices(ctx, nil)
}

// UpdateWithChoices reconciles the selected working tree with the current
// branch tip, applying explicit whole-entry subtree choices where needed.
func (w *Workspace) UpdateWithChoices(ctx context.Context, choices []project.Choice) error {
	return w.switchTo(ctx, w.state.Branch, "update", choices)
}

// Switch changes the branch binding only after its selected files have been
// reconciled and materialized. Divergent local work stops before any writes.
func (w *Workspace) Switch(ctx context.Context, branch string) error {
	return w.SwitchWithChoices(ctx, branch, nil)
}

// SwitchWithChoices changes branches after reconciling the selected working
// tree with the target branch using the requested whole-entry choices.
func (w *Workspace) SwitchWithChoices(ctx context.Context, branch string, choices []project.Choice) error {
	return w.switchTo(ctx, branch, "switch", choices)
}

func (w *Workspace) switchTo(ctx context.Context, branch, operation string, rawChoices []project.Choice) error {
	if w.state.Pending != nil {
		return ErrPending
	}
	choices, err := project.Choices(rawChoices)
	if err != nil {
		return err
	}
	selection, err := project.Selection(w.state.Selection)
	if err != nil {
		return err
	}
	if err := validateChoiceScope(choices, selection); err != nil {
		return err
	}
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	reference, err := w.client.Branch(ctx, branch)
	if err != nil {
		return err
	}
	nodes, err := fetchEntries(ctx, w.client, reference.Tip, w.state.Selection)
	if err != nil {
		return err
	}
	next := w.state
	next.Base = reference.Tip
	next.Branch = branch
	next.Baseline = nodes
	return w.startTransition(ctx, operation, reference.Tip, nodes, next, transitionOptions{choices: choices})
}

func (w *Workspace) startTransition(ctx context.Context, operation, source string, target []project.Node, next state, options transitionOptions) error {
	if options.resumeMerge {
		if operation != "merge" || w.state.Pending == nil || w.state.Pending.Kind != "merge" {
			return ErrPending
		}
	} else {
		if operation == "merge" || w.state.Pending != nil {
			return ErrPending
		}
	}
	oldSelection, err := project.Selection(w.state.Selection)
	if err != nil {
		return err
	}
	newSelection, err := project.Selection(next.Selection)
	if err != nil {
		return err
	}
	selectionScope := oldSelection.Union(newSelection)
	if err := validateChoiceScope(options.choices, selectionScope); err != nil {
		return err
	}
	if operation == "restore" {
		if err := validateChoiceScope(options.choices, options.restoreScope); err != nil {
			return fmt.Errorf("restore choice: %w", err)
		}
	}
	local, err := w.scan(ctx, selectionScope)
	if err != nil {
		return err
	}
	base := nodeMap(w.state.Baseline)
	for p, node := range local {
		logical, _ := model.ParsePath(p)
		if oldSelection.Includes(logical) && !newSelection.Includes(logical) && !sameNode(node, base[p]) {
			return fmt.Errorf("%w: cannot unload local changes at %q", ErrConflict, p)
		}
	}
	for p, node := range base {
		logical, _ := model.ParsePath(p)
		if oldSelection.Includes(logical) && !newSelection.Includes(logical) && !sameNode(node, local[p]) {
			return fmt.Errorf("%w: cannot unload local changes at %q", ErrConflict, p)
		}
	}
	targetMap := nodeMap(target)
	baseEntries, localEntries, targetEntries := entryMap(base), entryMap(local), entryMap(targetMap)
	choices := options.choices
	if operation == "restore" && len(choices) != 0 {
		// Validate raw rules against actual facts before flattening them to the
		// restore scope; otherwise a nonexistent path could be hidden by that
		// flattening step.
		if _, _, err := model.ReconcileTreesWithChoices(baseEntries, localEntries, targetEntries, choices); err != nil {
			return err
		}
		choices = scopedChoices(options.restoreScope, choices, baseEntries, localEntries, targetEntries)
	}
	for p, node := range local {
		logical, _ := model.ParsePath(p)
		if operation == "restore" && options.restoreScope.Includes(logical) && model.ChoiceFor(logical, choices) == "" &&
			!sameNode(node, base[p]) && !sameNode(node, targetMap[p]) {
			return fmt.Errorf("%w: restore would replace local changes at %q", ErrConflict, p)
		}
	}
	for p, node := range base {
		logical, _ := model.ParsePath(p)
		if operation == "restore" && options.restoreScope.Includes(logical) && model.ChoiceFor(logical, choices) == "" &&
			!sameNode(node, local[p]) && !sameNode(local[p], targetMap[p]) {
			return fmt.Errorf("%w: restore would replace local changes at %q", ErrConflict, p)
		}
	}
	combined, conflicts, err := model.ReconcileTreesWithChoices(baseEntries, localEntries, targetEntries, choices)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w at %q", ErrConflict, conflicts[0])
	}
	desired := make(map[string]project.Node, len(combined))
	for p, fact := range combined {
		if p.IsRoot() {
			continue
		}
		if entry(local[p.String()]) == fact {
			desired[p.String()] = local[p.String()]
		} else {
			desired[p.String()] = targetMap[p.String()]
		}
	}
	if err = w.preserveUncontrolledDirectories(local, desired); err != nil {
		return err
	}
	if err = validateNodes(sortedNodes(desired)); err != nil {
		return err
	}
	actions := planActions(local, desired)
	journal := &transition{ID: randomID(), Operation: operation, Source: source, Base: next.Base, Branch: next.Branch,
		Selection: next.Selection, Baseline: next.Baseline, Actions: actions}
	if err = validateTransition(journal); err != nil {
		return err
	}
	apply := func(transitionCtx context.Context) error {
		sealed := w.state
		sealed.Pending = &pending{Kind: "transition", Transition: journal}
		if err := w.save(sealed); err != nil {
			return err
		}
		return w.resumeTransitionHeld(transitionCtx)
	}
	if source != "" && hasFileActions(actions) {
		return w.withReadHold(ctx, journal.ID, source, apply)
	}
	return apply(ctx)
}

func hasFileActions(actions []fileAction) bool {
	for _, action := range actions {
		if action.After.Kind == model.File {
			return true
		}
	}
	return false
}

// Directories containing unselected, unmanaged entries remain local directories.
// A file replacement cannot occupy such a directory without discarding data.
func (w *Workspace) preserveUncontrolledDirectories(local, desired map[string]project.Node) error {
	var directories []string
	for p, node := range local {
		if node.Kind == model.Directory && desired[p].Kind != model.Directory {
			directories = append(directories, p)
		}
	}
	sort.Slice(directories, func(i, j int) bool { return deeperFirst(directories[i], directories[j]) })
	for _, p := range directories {
		f, err := w.root.OpenFile(p, readFlags(), 0)
		if err != nil {
			return err
		}
		names, readErr := f.Readdirnames(-1)
		if err = errors.Join(readErr, f.Close()); err != nil {
			return err
		}
		for _, name := range names {
			child := p + "/" + name
			if local[child].Kind != 0 && desired[child].Kind == 0 {
				continue
			}
			if desired[p].Kind == model.File {
				return fmt.Errorf("%w: directory %q still contains local entries", ErrConflict, p)
			}
			desired[p] = local[p]
			break
		}
	}
	return nil
}

func planActions(local, desired map[string]project.Node) []fileAction {
	var removals, additions, updates []fileAction
	for p, before := range local {
		after := desired[p]
		if sameNode(before, after) {
			continue
		}
		if after.Kind == 0 || after.Kind != before.Kind {
			removals = append(removals, fileAction{Path: p, Before: before, After: absentNode(p)})
		} else {
			updates = append(updates, fileAction{Path: p, Before: before, After: after})
		}
	}
	for p, after := range desired {
		if before := local[p]; before.Kind == 0 || before.Kind != after.Kind {
			additions = append(additions, fileAction{Path: p, Before: absentNode(p), After: after})
		}
	}
	sort.Slice(removals, func(i, j int) bool { return deeperFirst(removals[i].Path, removals[j].Path) })
	sort.Slice(additions, func(i, j int) bool {
		a, b := additions[i].Path, additions[j].Path
		da, db := strings.Count(a, "/"), strings.Count(b, "/")
		if da != db {
			return da < db
		}
		return a < b
	})
	sort.Slice(updates, func(i, j int) bool { return updates[i].Path < updates[j].Path })
	return append(append(removals, additions...), updates...)
}

func deeperFirst(a, b string) bool {
	da, db := strings.Count(a, "/"), strings.Count(b, "/")
	if da != db {
		return da > db
	}
	return a < b
}

func absentNode(p string) project.Node { return project.Node{Path: p, Kind: model.Absent} }
func nodeMap(nodes []project.Node) map[string]project.Node {
	result := make(map[string]project.Node, len(nodes))
	for _, node := range nodes {
		result[node.Path] = node
	}
	return result
}
func entryMap(nodes map[string]project.Node) map[model.Path]model.Entry {
	result := map[model.Path]model.Entry{{}: {Kind: model.Directory}}
	for p, node := range nodes {
		logical, _ := model.ParsePath(p)
		result[logical] = entry(node)
	}
	return result
}

func validateChoiceScope(choices map[model.Path]model.Side, scope model.Selection) error {
	for path := range choices {
		if !scope.Intersects(path) {
			return fmt.Errorf("%w: choice path %q is outside the selected scope", model.ErrInvalidChoice, path)
		}
	}
	return nil
}

func scopedChoices(scope model.Selection, choices map[model.Path]model.Side, trees ...map[model.Path]model.Entry) map[model.Path]model.Side {
	result := make(map[model.Path]model.Side)
	for _, tree := range trees {
		for path := range tree {
			if !scope.Includes(path) {
				continue
			}
			if side := model.ChoiceFor(path, choices); side != "" {
				result[path] = side
			}
		}
	}
	return result
}

func validateTransition(t *transition) error {
	if t == nil {
		return errors.New("missing transition journal")
	}
	id, err := hex.DecodeString(t.ID)
	if err != nil || len(id) != 16 || hex.EncodeToString(id) != t.ID || t.Index < 0 || t.Index > len(t.Actions) || t.Branch == "" {
		return errors.New("invalid transition journal")
	}
	if t.ProgressVersion != 0 && t.ProgressVersion != 1 {
		return errors.New("unsupported transition progress format")
	}
	switch t.Operation {
	case "update", "switch", "load", "unload", "restore", "merge":
	default:
		return errors.New("unknown transition operation")
	}
	if _, err := project.Selection(t.Selection); err != nil {
		return err
	}
	if err := project.ValidateChanges(project.ChangeRequest{OperationID: t.ID, Branch: t.Branch, Base: t.Base}); err != nil {
		return err
	}
	if t.Source != "" {
		if err := project.ValidateChanges(project.ChangeRequest{OperationID: t.ID, Branch: t.Branch, Base: t.Source}); err != nil {
			return err
		}
	}
	if err := validateNodes(t.Baseline); err != nil {
		return err
	}
	for _, action := range t.Actions {
		if action.Path != action.Before.Path || action.Path != action.After.Path || sameNode(action.Before, action.After) {
			return errors.New("invalid transition action")
		}
		for _, node := range []project.Node{action.Before, action.After} {
			if node.Kind == model.Absent {
				if node.SHA256 != "" || node.Size != 0 {
					return errors.New("invalid absent transition fact")
				}
				copy := node
				copy.Kind = model.Directory
				if err := validateNode(copy); err != nil {
					return err
				}
			} else if err := validateNode(node); err != nil {
				return err
			}
		}
	}
	return nil
}

func transitionDirectory(t *transition) string { return ".lode/transition-" + t.ID }
func actionFiles(t *transition, index int) (old, fresh string) {
	dir := transitionDirectory(t)
	return fmt.Sprintf("%s/old-%d", dir, index), fmt.Sprintf("%s/new-%d", dir, index)
}

func (w *Workspace) resumeTransition(ctx context.Context) error {
	t := w.state.Pending.Transition
	needsRead, err := w.transitionNeedsRead(ctx, t)
	if err != nil {
		return err
	}
	if t.Source != "" && needsRead {
		return w.withReadHold(ctx, t.ID, t.Source, w.resumeTransitionHeld)
	}
	err = w.resumeTransitionHeld(ctx)
	if err == nil {
		w.releaseReadBestEffort(t.ID)
	}
	return err
}

func (w *Workspace) transitionNeedsRead(ctx context.Context, t *transition) (bool, error) {
	for index := t.Index; index < len(t.Actions); index++ {
		action := t.Actions[index]
		if action.After.Kind != model.File {
			continue
		}
		_, fresh := actionFiles(t, index)
		staged, err := w.observePath(ctx, fresh)
		if err != nil {
			return false, err
		}
		if staged.Kind == model.File && sameNode(staged, action.After) {
			continue
		}
		current, err := w.observePath(ctx, action.Path)
		if err != nil {
			return false, err
		}
		if sameNode(current, action.After) || w.sameFile(fresh, action.Path) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func (w *Workspace) resumeTransitionHeld(ctx context.Context) error {
	t := w.state.Pending.Transition
	if err := validateTransition(t); err != nil {
		return err
	}
	if t.ProgressVersion == 0 {
		// Upgrade an older inline cursor before any further file mutation.
		if err := w.save(w.state); err != nil {
			return err
		}
		t = w.state.Pending.Transition
	}
	directory := transitionDirectory(t)
	if err := w.root.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := w.root.Lstat(directory)
	if err != nil || !info.IsDir() {
		return errors.New("transition staging directory is invalid")
	}
	if err = syncRoot(w.private, "."); err != nil {
		return err
	}
	for i := 0; i < t.Index; i++ {
		if err = w.cleanAction(ctx, t, i); err != nil {
			return err
		}
	}
	for t.Index < len(t.Actions) {
		index := t.Index
		if err = w.applyAction(ctx, t, index); err != nil {
			return err
		}
		if err = w.checkpointTransition(index + 1); err != nil {
			return err
		}
		t = w.state.Pending.Transition
		if err = w.cleanAction(ctx, t, index); err != nil {
			return err
		}
	}
	if err = w.root.Remove(directory); err != nil {
		return err
	}
	if err = syncRoot(w.private, "."); err != nil {
		return err
	}
	next := w.state
	next.Base = t.Base
	next.Branch = t.Branch
	next.Selection = t.Selection
	next.Baseline = t.Baseline
	next.Pending = nil
	return w.save(next)
}

func (w *Workspace) applyAction(ctx context.Context, t *transition, index int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	action := t.Actions[index]
	old, fresh := actionFiles(t, index)
	current, err := w.observePath(ctx, action.Path)
	if err != nil {
		return err
	}
	if sameNode(current, action.After) || action.After.Kind == model.File && w.sameFile(fresh, action.Path) {
		// The staged hard link also identifies an install followed by a local
		// edit before the progress checkpoint. Keep those newer working bytes.
		if current.Kind == model.File {
			f, err := w.openRegular(action.Path)
			if err != nil {
				return err
			}
			if err = errors.Join(f.Sync(), f.Close()); err != nil {
				return err
			}
		}
		return w.syncExistingParent(action.Path)
	}
	if action.After.Kind == model.File {
		if err := w.stageTransitionFile(ctx, t.Source, action.After, fresh); err != nil {
			return err
		}
	}
	backup, err := w.observePath(ctx, old)
	if err != nil {
		return err
	}
	if backup.Kind != model.Absent {
		if !sameNode(backup, action.Before) {
			return fmt.Errorf("%w: saved local entry changed at %q", ErrConflict, old)
		}
		if err := w.requireEmptyDirectory(old, backup); err != nil {
			return err
		}
		if current.Kind != model.Absent {
			return fmt.Errorf("%w: unexpected entry at %q", ErrConflict, action.Path)
		}
	} else {
		if !sameNode(current, action.Before) {
			return fmt.Errorf("%w at %q", ErrConflict, action.Path)
		}
		if current.Kind != model.Absent {
			if err := w.requireEmptyDirectory(action.Path, current); err != nil {
				return err
			}
			if err := w.move(action.Path, old); err != nil {
				return err
			}
			moved, err := w.observePath(ctx, old)
			if err != nil {
				return err
			}
			if !sameNode(moved, action.Before) {
				return errors.Join(fmt.Errorf("%w: file changed during move at %q", ErrConflict, action.Path), w.move(old, action.Path))
			}
			if err := w.requireEmptyDirectory(old, moved); err != nil {
				return errors.Join(err, w.move(old, action.Path))
			}
		}
	}
	switch action.After.Kind {
	case model.Absent:
		return nil
	case model.Directory:
		if err := w.root.Mkdir(action.Path, 0755); err != nil {
			return err
		}
		return syncRoot(w.root, path.Dir(action.Path))
	case model.File:
		return w.link(fresh, action.Path)
	default:
		return errors.New("invalid transition target")
	}
}

func (w *Workspace) observePath(ctx context.Context, p string) (project.Node, error) {
	info, err := w.root.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return absentNode(p), nil
	}
	if err != nil {
		return project.Node{}, err
	}
	if info.IsDir() {
		return project.Node{Path: p, Kind: model.Directory}, nil
	}
	if !info.Mode().IsRegular() {
		return project.Node{}, fmt.Errorf("%w: unsupported local entry %q", ErrConflict, p)
	}
	return w.observeFile(ctx, p, info)
}

func (w *Workspace) requireEmptyDirectory(p string, node project.Node) error {
	if node.Kind != model.Directory {
		return nil
	}
	f, err := w.root.OpenFile(p, readFlags(), 0)
	if err != nil {
		return err
	}
	names, readErr := f.Readdirnames(-1)
	if err = errors.Join(readErr, f.Close()); err != nil {
		return err
	}
	if len(names) != 0 {
		return fmt.Errorf("%w: local directory %q is not empty", ErrConflict, p)
	}
	return nil
}

func (w *Workspace) sameFile(a, b string) bool {
	ai, err := w.root.Lstat(a)
	if err != nil || !ai.Mode().IsRegular() {
		return false
	}
	bi, err := w.root.Lstat(b)
	return err == nil && bi.Mode().IsRegular() && os.SameFile(ai, bi)
}

func (w *Workspace) syncExistingParent(p string) error {
	for parent := path.Dir(p); ; parent = path.Dir(parent) {
		err := syncRoot(w.root, parent)
		if !errors.Is(err, os.ErrNotExist) || parent == "." {
			return err
		}
	}
}

func (w *Workspace) stageTransitionFile(ctx context.Context, commit string, node project.Node, fresh string) error {
	staged, err := w.observePath(ctx, fresh)
	if err != nil {
		return err
	}
	if staged.Kind != model.Absent {
		if !sameNode(staged, node) {
			return fmt.Errorf("%w: staged contents changed at %q", ErrConflict, fresh)
		}
		return syncRoot(w.root, path.Dir(fresh))
	}
	name, err := w.fetchFile(ctx, commit, node)
	if err != nil {
		return err
	}
	defer w.private.Remove(name)
	return moveNoReplace(w.root, ".lode/"+name, fresh)
}

func (w *Workspace) move(from, to string) error {
	if w.moveEntry != nil {
		return w.moveEntry(from, to)
	}
	return moveNoReplace(w.root, from, to)
}
func (w *Workspace) link(from, to string) error {
	if w.linkEntry != nil {
		return w.linkEntry(from, to)
	}
	return linkNoReplace(w.root, from, to)
}

func (w *Workspace) cleanAction(ctx context.Context, t *transition, index int) error {
	old, fresh := actionFiles(t, index)
	backup, err := w.observePath(ctx, old)
	if err != nil {
		return err
	}
	if backup.Kind != model.Absent {
		if !sameNode(backup, t.Actions[index].Before) {
			return fmt.Errorf("%w: saved local contents changed at %q", ErrConflict, old)
		}
		if err = w.requireEmptyDirectory(old, backup); err != nil {
			return err
		}
		if err = w.root.Remove(old); err != nil {
			return err
		}
	}
	if err = w.root.Remove(fresh); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRoot(w.root, transitionDirectory(t))
}
