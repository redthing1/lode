// Package content stores opaque bytes and native protected trees in Kopia.
package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
)

const operationLabel = "lode.operation"

var ErrStorage = errors.New("content storage failed")

type Store struct {
	repository repo.Repository
	directory  string
}

// Object identifies native content. Directory counts let parents reuse a subtree
// without opening it; file identity remains the caller's size and SHA-256.
type Object struct {
	ID          string `json:"id"`
	Bytes       int64  `json:"bytes,omitempty"`
	Files       int64  `json:"files,omitempty"`
	Directories int64  `json:"directories,omitempty"`
}

// Open uses an existing parent directory and an instance-owned password.
// Rebuilding connection settings makes cold copies independent of their old path.
func Open(ctx context.Context, directory, password string, create bool) (_ *Store, err error) {
	if password == "" {
		return nil, errors.New("empty repository password")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err := syncDirectory(filepath.Dir(root)); err != nil {
			return nil, err
		}
	}
	st, err := newStorage(ctx, filepath.Join(root, "repository"), create)
	if err != nil {
		return nil, err
	}
	defer st.Close(ctx)
	if create {
		if err := repo.Initialize(ctx, st, nil, password); err != nil && !errors.Is(err, repo.ErrAlreadyInitialized) {
			return nil, err
		}
	}
	config := filepath.Join(root, "kopia.config")
	temporary := filepath.Join(root, "kopia.connect")
	defer os.Remove(temporary)
	if err := repo.Connect(ctx, temporary, st, password, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Username: "lode", Hostname: "local"},
	}); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, config); err != nil {
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		return nil, err
	}
	r, err := repo.Open(ctx, config, password, &repo.Options{DisableRepositoryLog: true})
	if err != nil {
		return nil, err
	}
	return &Store{repository: r, directory: filepath.Join(root, "repository")}, nil
}

func (s *Store) Close(ctx context.Context) error { return s.repository.Close(ctx) }

func (s *Store) Read(ctx context.Context, ref string) (io.ReadSeekCloser, error) {
	id, err := object.ParseID(ref)
	if err != nil {
		return nil, err
	}
	r, err := s.repository.OpenObject(ctx, id)
	if err != nil {
		return nil, storageError(err)
	}
	return objectReader{r}, nil
}

func storageError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStorage, err)
}

type objectReader struct{ io.ReadSeekCloser }

func (r objectReader) Read(p []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(p)
	return n, storageError(err)
}
func (r objectReader) Seek(n int64, whence int) (int64, error) {
	pos, err := r.ReadSeekCloser.Seek(n, whence)
	return pos, storageError(err)
}

type Root struct{ Operation, Object string }

// Roots returns native protections; only the project service decides which live.
func (s *Store) Roots(ctx context.Context) (map[string]Root, error) {
	ms, err := s.repository.FindManifests(ctx, map[string]string{})
	if err != nil {
		return nil, err
	}
	roots := make(map[string]Root)
	for _, m := range ms {
		op := m.Labels[operationLabel]
		if op == "" {
			continue
		}
		snapshot, err := snapshot.LoadSnapshot(ctx, s.repository, m.ID)
		if err != nil {
			return nil, err
		}
		if snapshot.RootEntry == nil {
			return nil, fmt.Errorf("protected snapshot %s has no root", m.ID)
		}
		roots[string(m.ID)] = Root{Operation: op, Object: snapshot.RootEntry.ObjectID.String()}
	}
	return roots, nil
}

func (s *Store) Release(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return repo.WriteSession(ctx, s.repository, repo.WriteSessionOptions{Purpose: "lode release"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		for _, id := range ids {
			if err := w.DeleteManifest(ctx, manifest.ID(id)); err != nil {
				return err
			}
		}
		return nil
	})
}
