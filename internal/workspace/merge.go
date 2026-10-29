package workspace

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
)

// Merge publishes complete project states on the server, then updates only the
// selected workspace. The immutable source and choices share save recovery.
func (w *Workspace) Merge(ctx context.Context, token, ref, message string, choices map[string]model.Side) (model.Version, error) {
	if err := w.ready(); err != nil {
		return model.Version{}, err
	}
	changes, err := w.Status(ctx)
	if err != nil {
		return model.Version{}, err
	}
	if len(changes) != 0 {
		return model.Version{}, errors.New("save selected changes before merging")
	}
	c, err := w.Client(token)
	if err != nil {
		return model.Version{}, err
	}
	defer c.HTTP.CloseIdleConnections()
	source, err := c.Reference(ctx, w.state.Owner, w.state.Project, ref)
	if err != nil {
		return model.Version{}, err
	}
	if source.ID == "" {
		return model.Version{}, errors.New("source branch has no saved versions")
	}
	identity, err := c.Me(ctx)
	if err != nil {
		return model.Version{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return model.Version{}, err
	}
	if message == "" {
		message = "Merge " + ref
	}
	r := model.Save{ID: id.String(), Branch: w.state.Branch, Base: w.state.Version, Message: message, Merge: source.ID, Choices: choices}
	if err := r.Validate(); err != nil {
		return model.Version{}, err
	}
	pending := pendingSave{Actor: identity.User, Request: r}
	if err := w.writeJSON("save.json", pending); err != nil {
		return model.Version{}, err
	}
	return w.publish(ctx, c, pending)
}
