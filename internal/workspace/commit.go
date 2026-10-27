package workspace

import (
	"context"
	"errors"
	"io"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

// Commit durably records exact input facts before any upload. Working files
// remain user data; acceptance changes the baseline without rewriting them.
func (w *Workspace) Commit(ctx context.Context, author, message string) (project.Commit, error) {
	changes, err := w.Status(ctx)
	if err != nil {
		return project.Commit{}, err
	}
	if len(changes) == 0 {
		return project.Commit{}, ErrNothingChanged
	}
	if len(changes) > project.MaxChanges {
		return project.Commit{}, errors.New("too many changes for one commit; select a smaller working set")
	}
	if err = w.verifyRemote(ctx); err != nil {
		return project.Commit{}, err
	}
	next := w.state
	next.Pending = &pending{Kind: "commit", Request: &project.ChangeRequest{
		OperationID: randomID(), Branch: w.state.Branch, Base: w.state.Base,
		Author: author, Message: message, Changes: changes,
	}}
	if err = project.ValidateChanges(*next.Pending.Request); err != nil {
		return project.Commit{}, err
	}
	if err = w.save(next); err != nil {
		return project.Commit{}, err
	}
	return w.Resume(ctx)
}

// Resume retries sealed input. If capture is still required, changed local
// bytes fail the original SHA/size checks and the journal remains explicit.
func (w *Workspace) Resume(ctx context.Context) (project.Commit, error) {
	return w.ResumeWithChoices(ctx, nil)
}

// ResumeWithChoices can resolve newer local edits when an accepted merge has
// not yet sealed its filesystem transition. Choices never alter the accepted
// server commit or a transition whose filesystem actions are already sealed.
func (w *Workspace) ResumeWithChoices(ctx context.Context, choices []project.Choice) (project.Commit, error) {
	if w.state.Pending == nil {
		return project.Commit{}, errors.New("no incomplete workspace operation")
	}
	if len(choices) != 0 && w.state.Pending.Kind != "merge" {
		return project.Commit{}, errors.New("resume choices require a merge awaiting workspace reconciliation")
	}
	if _, err := project.Choices(choices); err != nil {
		return project.Commit{}, err
	}
	if w.state.Pending.Kind != "abandon" {
		if err := w.verifyRemote(ctx); err != nil {
			return project.Commit{}, err
		}
	}
	switch w.state.Pending.Kind {
	case "transition":
		return project.Commit{}, w.resumeTransition(ctx)
	case "checkout":
		return w.checkout(ctx)
	case "merge":
		return w.resumeMerge(ctx, choices)
	case "commit":
		commit, err := w.client.Apply(ctx, *w.state.Pending.Request, func(path model.Path) (io.ReadCloser, error) { return w.openRegular(path.String()) })
		if err != nil {
			return project.Commit{}, err
		}
		return commit, w.finishCommit(ctx, commit)
	case "abandon":
		return project.Commit{}, w.resumeAbandon(ctx)
	default:
		return project.Commit{}, errors.New("unknown workspace operation")
	}
}

func (w *Workspace) finishCommit(ctx context.Context, commit project.Commit) error {
	request := w.state.Pending.Request
	if commit.ID == "" || request.Base == "" && len(commit.Parents) != 0 || request.Base != "" && (len(commit.Parents) != 1 || commit.Parents[0] != request.Base) {
		return errors.New("remote returned an unexpected commit")
	}
	nodes, err := fetchEntries(ctx, w.client, commit.ID, w.state.Selection)
	if err != nil {
		return err
	}
	next := w.state
	next.Base = commit.ID
	next.Baseline = nodes
	next.Pending = nil
	return w.save(next)
}

// Abort resolves the server outcome before removing the journal. If the
// operation was accepted, it records that result and preserves newer local edits.
func (w *Workspace) Abort(ctx context.Context) error {
	if w.state.Pending == nil {
		return errors.New("no incomplete workspace operation")
	}
	switch w.state.Pending.Kind {
	case "checkout", "transition":
		abandon := *w.state.Pending
		abandon.Kind = "abandon"
		next := w.state
		next.Pending = &abandon
		if err := w.save(next); err != nil {
			return err
		}
		return w.resumeAbandon(ctx)
	case "abandon":
		return w.resumeAbandon(ctx)
	case "commit", "merge":
	default:
		return errors.New("workspace materialization must be resumed")
	}
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	var id string
	if w.state.Pending.Kind == "merge" {
		id = w.state.Pending.Merge.OperationID
	} else {
		id = w.state.Pending.Request.OperationID
	}
	if err := w.client.Abort(ctx, id); err != nil {
		if !errors.Is(err, project.ErrAlreadyAccepted) {
			return err
		}
		_, err = w.Resume(ctx)
		return err
	}
	next := w.state
	next.Pending = nil
	return w.save(next)
}

func (w *Workspace) History(ctx context.Context, limit int) ([]project.Commit, string, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return nil, "", err
	}
	return w.client.History(ctx, w.state.Base, limit)
}
