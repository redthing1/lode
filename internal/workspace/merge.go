package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/redthing1/lode/internal/project"
)

// PreviewMerge compares the complete server trees, including unloaded paths.
// It does not change the workspace or reserve a publication operation.
func (w *Workspace) PreviewMerge(ctx context.Context, other string, choices []project.Choice, after string, limit int) (project.MergePreview, error) {
	if w.state.Pending != nil {
		return project.MergePreview{}, ErrPending
	}
	if err := w.verifyRemote(ctx); err != nil {
		return project.MergePreview{}, err
	}
	return w.client.PreviewMerge(ctx, project.MergeSpec{Branch: w.state.Branch, Base: w.state.Base, Other: other, Choices: choices}, after, limit)
}

// Merge publishes a complete two-parent server commit, then reconciles the
// selected working files to it. A clean workspace is required at initiation;
// later local edits are checked independently of the sealed server choices.
func (w *Workspace) Merge(ctx context.Context, other, author, message string, choices []project.Choice) (project.Commit, error) {
	request := project.MergeRequest{
		OperationID: randomID(),
		MergeSpec:   project.MergeSpec{Branch: w.state.Branch, Base: w.state.Base, Other: other, Choices: append([]project.Choice(nil), choices...)},
		Author:      author, Message: message,
	}
	if err := project.ValidateMergeRequest(request); err != nil {
		return project.Commit{}, err
	}
	changes, err := w.Status(ctx)
	if err != nil {
		return project.Commit{}, err
	}
	if len(changes) != 0 {
		return project.Commit{}, fmt.Errorf("%w: commit selected local changes before merging", ErrConflict)
	}
	preview, err := w.PreviewMerge(ctx, other, choices, "", 1)
	if err != nil {
		return project.Commit{}, err
	}
	if preview.TotalConflicts != 0 {
		return project.Commit{}, fmt.Errorf("%w: preview the merge and choose complete entries", project.ErrMergeConflict)
	}
	next := w.state
	next.Pending = &pending{Kind: "merge", Merge: &request}
	if err = w.save(next); err != nil {
		return project.Commit{}, err
	}
	return w.Resume(ctx)
}

func (w *Workspace) resumeMerge(ctx context.Context, choices []project.Choice) (project.Commit, error) {
	request := w.state.Pending.Merge
	commit, err := w.client.Merge(ctx, *request)
	if err != nil {
		return project.Commit{}, err
	}
	if commit.ID == "" || len(commit.Parents) != 2 || commit.Parents[0] != request.Base || commit.Parents[1] != request.Other {
		return project.Commit{}, errors.New("remote returned an unexpected merge commit")
	}
	nodes, err := fetchEntries(ctx, w.client, commit.ID, w.state.Selection)
	if err != nil {
		return commit, err
	}
	rules, err := project.Choices(choices)
	if err != nil {
		return commit, err
	}
	next := w.state
	next.Base = commit.ID
	next.Baseline = nodes
	// Keep the merge intent durable until the complete filesystem plan can
	// replace it atomically. A conflict leaves the accepted result resumable.
	err = w.startTransition(ctx, "merge", commit.ID, nodes, next, transitionOptions{choices: rules, resumeMerge: true})
	return commit, err
}
