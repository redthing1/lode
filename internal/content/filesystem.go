//go:build linux

package content

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
)

type filesystemConfig struct {
	Path string `json:"path"`
}
type durableFilesystem struct {
	blob.Storage
	path    string
	syncDir func(string) error
}
type shardedPathProvider interface {
	GetShardedPathAndFilePath(context.Context, blob.ID) (string, string, error)
}

func init() {
	blob.AddSupportedStorage("lode-filesystem", filesystemConfig{}, func(ctx context.Context, cfg *filesystemConfig, create bool) (blob.Storage, error) {
		return newStorage(ctx, cfg.Path, create)
	})
}

func newStorage(ctx context.Context, path string, create bool) (blob.Storage, error) {
	st, err := filesystem.New(ctx, &filesystem.Options{Path: path}, create)
	if err != nil {
		return nil, err
	}
	return &durableFilesystem{Storage: st, path: path}, nil
}
func (s *durableFilesystem) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{Type: "lode-filesystem", Config: &filesystemConfig{Path: s.path}}
}
func (s *durableFilesystem) DisplayName() string { return "Lode filesystem: " + s.path }
func (s *durableFilesystem) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if err := s.Storage.PutBlob(ctx, id, data, opts); err != nil {
		return err
	}
	return s.syncShard(ctx, id)
}
func (s *durableFilesystem) DeleteBlob(ctx context.Context, id blob.ID) error {
	if err := s.Storage.DeleteBlob(ctx, id); err != nil {
		return err
	}
	return s.syncShard(ctx, id)
}
func (s *durableFilesystem) syncShard(ctx context.Context, id blob.ID) error {
	p, ok := s.Storage.(shardedPathProvider)
	if !ok {
		return fmt.Errorf("filesystem storage does not expose Kopia shard paths")
	}
	dir, _, err := p.GetShardedPathAndFilePath(ctx, id)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(s.path)
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("Kopia shard directory escapes repository root")
	}
	for current := dir; ; current = filepath.Dir(current) {
		if s.syncDir != nil {
			err = s.syncDir(current)
		} else {
			err = syncDirectory(current)
		}
		if err != nil {
			return err
		}
		if current == root {
			break
		}
		next := filepath.Dir(current)
		if next == current {
			return fmt.Errorf("repository root is not an ancestor of shard directory")
		}
	}
	return nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
