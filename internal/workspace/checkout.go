package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	parent, err := os.Open(filepath.Dir(directory))
	if err != nil {
		return nil, err
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
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

func copyVerified(dst io.Writer, src io.Reader, e model.Entry) error {
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(dst, hash), src, e.Size); err != nil {
		return fmt.Errorf("read %s: %w", e.Path, err)
	}
	var extra [1]byte
	if n, err := io.ReadFull(src, extra[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("unexpected content length for %s", e.Path)
	}
	if hex.EncodeToString(hash.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("content hash mismatch for %s", e.Path)
	}
	return nil
}
