package workspace

import (
	"context"

	"github.com/redthing1/lode/internal/project"
)

func (w *Workspace) Collect(ctx context.Context, request project.CollectRequest) (project.CollectionResult, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.CollectionResult{}, err
	}
	return w.client.Collect(ctx, request)
}

func (w *Workspace) CapacityPolicy(ctx context.Context) (project.CapacityPolicy, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.CapacityPolicy{}, err
	}
	return w.client.CapacityPolicy(ctx)
}

func (w *Workspace) SetCapacityPolicy(ctx context.Context, policy project.CapacityPolicy) error {
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	return w.client.SetCapacityPolicy(ctx, policy)
}
