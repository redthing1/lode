package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/progress"
	"github.com/redthing1/lode/internal/remote"
)

type pendingSave struct {
	Actor   string     `json:"actor"`
	Request model.Save `json:"request"`
}

func (w *Workspace) Save(ctx context.Context, token, message string) (model.Version, error) {
	progress.Report(ctx, progress.Event{Phase: "Connecting"})
	if err := w.ready(); err != nil {
		return model.Version{}, err
	}
	c, err := w.Client(token)
	if err != nil {
		return model.Version{}, err
	}
	defer c.HTTP.CloseIdleConnections()
	identity, err := c.Me(ctx)
	if err != nil {
		return model.Version{}, err
	}
	changes, err := w.Status(ctx)
	if err != nil {
		return model.Version{}, err
	}
	if len(changes) == 0 {
		return model.Version{}, ErrNoChanges
	}
	id, err := uuid.NewV7()
	if err != nil {
		return model.Version{}, err
	}
	pending := pendingSave{Actor: identity.User, Request: model.Save{ID: id.String(), Branch: w.state.Branch, Base: w.state.Version, Message: message, Changes: changes}}
	if err := w.writeJSON("save.json", pending); err != nil {
		return model.Version{}, err
	}
	return w.publish(ctx, c, pending)
}

func (w *Workspace) acceptSave(pending pendingSave, v model.Version) error {
	r := pending.Request
	if v.ID != r.ID || v.Parent != r.Base || v.MergeParent != r.Merge || v.Message != r.Message || v.Actor != pending.Actor {
		return errors.New("save response does not match the pending request")
	}
	if w.state.Version != r.ID {
		if w.state.Version != r.Base || w.state.Branch != r.Branch {
			return errors.New("pending save does not match the workspace baseline")
		}
		// A merge's complete tree may contain changes outside this workspace.
		// Keep the old baseline so ordinary update can reconcile current files.
		if r.Merge != "" {
			return w.remove("save.json")
		}
		next := w.state
		next.Version = v.ID
		next.Baseline = model.ApplySelected(next.Baseline, r.Changes)
		if err := next.validate(); err != nil {
			return err
		}
		if err := w.writeJSON("state.json", next); err != nil {
			return fmt.Errorf("version %s accepted; local state needs recovery: %w", v.ID, err)
		}
		w.state = next
	}
	return w.remove("save.json")
}

func (w *Workspace) Resume(ctx context.Context, token string) error {
	if exists, err := w.pending("transition.json"); err != nil {
		return err
	} else if exists {
		c, err := w.Client(token)
		if err != nil {
			return err
		}
		defer c.HTTP.CloseIdleConnections()
		return w.resumeTransition(ctx, c, false)
	}
	if exists, err := w.pending("save.json"); err != nil {
		return err
	} else if !exists {
		return w.clearTransition()
	}
	var pending pendingSave
	if err := w.readJSON("save.json", &pending); err != nil {
		return err
	}
	if err := pending.Request.Validate(); err != nil {
		return err
	}
	c, err := w.Client(token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	identity, err := c.Me(ctx)
	if err != nil {
		return err
	}
	if identity.User != pending.Actor {
		return errors.New("sign in with the account that started this save")
	}
	_, err = w.publish(ctx, c, pending)
	if err != nil {
		var failure *remote.Error
		if errors.As(err, &failure) && failure.Code == "cancelled" {
			return w.remove("save.json")
		}
		return err
	}
	return nil
}

func (w *Workspace) publish(ctx context.Context, c *remote.Client, pending pendingSave) (model.Version, error) {
	v, err := c.Save(ctx, w.state.Owner, w.state.Project, pending.Request, w.state.Baseline, func(e model.Entry) (io.ReadCloser, error) { return w.openSource(e) })
	if err != nil && pending.Request.Merge != "" {
		var failure *remote.Error
		if errors.As(err, &failure) {
			switch failure.Code {
			case "conflicts", "already_merged", "ancestry", "request", "not_found", "expired", "stale":
				// Settle a refused attempt before allowing a new choice/request.
				// A concurrent accepted retry remains authoritative.
				settled, cancelErr := c.Cancel(ctx, w.state.Owner, w.state.Project, pending.Request)
				if cancelErr != nil {
					return model.Version{}, errors.Join(err, fmt.Errorf("merge request remains pending; run lode resume or lode cancel: %w", cancelErr))
				}
				if settled.ID == "" {
					return model.Version{}, errors.Join(err, w.remove("save.json"))
				}
				v, err = settled, nil
			}
		}
	}
	if err != nil {
		return model.Version{}, err
	}
	progress.Report(ctx, progress.Event{Phase: "Finishing save"})
	if err := w.acceptSave(pending, v); err != nil {
		return v, err
	}
	if pending.Request.Merge != "" {
		if err := w.update(ctx, c, nil); err != nil {
			return v, fmt.Errorf("merge %s accepted; update local files with lode update, or lode resume if a transition is pending: %w", model.ShortVersion(v.ID), err)
		}
	}
	return v, nil
}

// Cancel reverses an unfinished local transition or settles a pending save.
// An accepted save advances only the baseline; current files are untouched.
func (w *Workspace) Cancel(ctx context.Context, token string) error {
	if exists, err := w.pending("transition.json"); err != nil {
		return err
	} else if exists {
		return w.resumeTransition(ctx, nil, true)
	}
	return w.cancelSave(ctx, token)
}

func (w *Workspace) cancelSave(ctx context.Context, token string) error {
	if exists, err := w.pending("save.json"); err != nil {
		return err
	} else if !exists {
		return nil
	}
	var pending pendingSave
	if err := w.readJSON("save.json", &pending); err != nil {
		return err
	}
	c, err := w.Client(token)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	identity, err := c.Me(ctx)
	if err != nil {
		return err
	}
	if identity.User != pending.Actor {
		return errors.New("sign in with the account that started this save")
	}
	v, err := c.Cancel(ctx, w.state.Owner, w.state.Project, pending.Request)
	if err != nil {
		return err
	}
	if v.ID != "" {
		return w.acceptSave(pending, v)
	}
	return w.remove("save.json")
}
