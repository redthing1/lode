package workspace

import (
	"context"

	"github.com/redthing1/lode/internal/project"
)

// BranchReference resolves a branch once so a caller can pin a merge source.
func (w *Workspace) BranchReference(ctx context.Context, name string) (project.Branch, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.Branch{}, err
	}
	return w.client.Branch(ctx, name)
}

func (w *Workspace) CreateBranch(ctx context.Context, name, from string) error {
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	if from == "" {
		from = w.state.Base
	}
	return w.client.CreateBranch(ctx, name, from)
}

func (w *Workspace) DeleteBranch(ctx context.Context, name string) error {
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	branch, err := w.client.Branch(ctx, name)
	if err != nil {
		return err
	}
	return w.client.DeleteBranch(ctx, name, branch.Tip)
}

func (w *Workspace) Branches(ctx context.Context, after string, limit int) ([]project.Branch, string, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return nil, "", err
	}
	return w.client.Branches(ctx, after, limit)
}
