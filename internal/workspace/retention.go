package workspace

import (
	"context"

	"github.com/redthing1/lode/internal/project"
)

func (w *Workspace) CreateTag(ctx context.Context, name, commitID string) error {
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	if commitID == "" {
		commitID = w.state.Base
	}
	return w.client.CreateTag(ctx, name, commitID)
}

func (w *Workspace) DeleteTag(ctx context.Context, name string) error {
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	tag, err := w.client.Tag(ctx, name)
	if err != nil {
		return err
	}
	return w.client.DeleteTag(ctx, name, tag.CommitID)
}

func (w *Workspace) Tag(ctx context.Context, name string) (project.Tag, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.Tag{}, err
	}
	return w.client.Tag(ctx, name)
}

func (w *Workspace) Tags(ctx context.Context, after string, limit int) ([]project.Tag, string, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return nil, "", err
	}
	return w.client.Tags(ctx, after, limit)
}

func (w *Workspace) RetentionPolicy(ctx context.Context) (project.RetentionPolicy, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.RetentionPolicy{}, err
	}
	return w.client.RetentionPolicy(ctx)
}

func (w *Workspace) SetRetentionPolicy(ctx context.Context, policy project.RetentionPolicy) error {
	if err := w.verifyRemote(ctx); err != nil {
		return err
	}
	return w.client.SetRetentionPolicy(ctx, policy)
}

func (w *Workspace) PreviewRetention(ctx context.Context, after string, limit int) (project.RetentionPreview, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.RetentionPreview{}, err
	}
	return w.client.PreviewRetention(ctx, after, limit)
}

func (w *Workspace) Expire(ctx context.Context, ids []string) (project.ExpirationResult, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.ExpirationResult{}, err
	}
	return w.client.Expire(ctx, ids)
}

func (w *Workspace) ExplainRetention(ctx context.Context, id string) (project.Availability, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.Availability{}, err
	}
	return w.client.ExplainRetention(ctx, id)
}

func (w *Workspace) PayloadStatuses(ctx context.Context, ids []string) ([]project.Availability, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return nil, err
	}
	return w.client.PayloadStatuses(ctx, ids)
}

func (w *Workspace) StorageStats(ctx context.Context) (project.StorageStats, error) {
	if err := w.verifyRemote(ctx); err != nil {
		return project.StorageStats{}, err
	}
	return w.client.StorageStats(ctx)
}
