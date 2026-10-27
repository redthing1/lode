package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/redthing1/lode/internal/model"
)

func Clone(ctx context.Context, url, token, directory string, selection model.Selection) (*Workspace, error) {
	client, target, err := connect(ctx, url, token, "main", selection)
	if err != nil {
		return nil, err
	}
	target.Baseline, err = client.Entries(ctx, target.Base, target.Selection)
	if err != nil {
		return nil, err
	}
	if err := target.validate(); err != nil {
		return nil, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		return nil, err
	}
	if err := errors.Join(syncDirectory(parent, "."), parent.Close()); err != nil {
		return nil, err
	}
	w, err := initialize(directory)
	if err != nil {
		return nil, err
	}
	w.state = target
	w.state.Selection = model.Selection{}
	w.state.Baseline = nil
	if err := w.store("state.json", w.state); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.transitionTo(ctx, target, nil); err != nil {
		w.Close()
		return nil, fmt.Errorf("checkout incomplete; use resume or cancel in %s: %w", directory, err)
	}
	return w, nil
}
