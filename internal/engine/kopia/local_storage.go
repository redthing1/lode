package kopia

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
)

const localFilesystemStorageType = "lode-filesystem"

// localStorage adds local filesystem durability steps to Kopia's native
// filesystem provider without changing its blob naming or sharding behavior.
type localStorage struct {
	blob.Storage
	rootPath string
	syncFile func(string) error
	syncDir  func(string) error
}

type shardedPathProvider interface {
	GetShardedPathAndFilePath(context.Context, blob.ID) (string, string, error)
}

// NewLocalStorage creates a durable local filesystem blob store.
//
// This implementation requires Linux and a local filesystem that honors
// file and directory fsync. It does not establish that a device or remote
// filesystem actually honors flush requests.
func NewLocalStorage(ctx context.Context, path string, create bool) (blob.Storage, error) {
	if path == "" {
		return nil, errors.New("local storage path is empty")
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("durable local blob storage is only supported on Linux (running %s)", runtime.GOOS)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve local storage path: %w", err)
	}

	base, err := filesystem.New(ctx, &filesystem.Options{Path: absPath}, create)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(absPath); err != nil {
		_ = base.Close(ctx)
		return nil, fmt.Errorf("stat local storage path: %w", err)
	}

	st := &localStorage{
		Storage:  base,
		rootPath: absPath,
		syncFile: syncLocalFile,
		syncDir:  syncLocalDir,
	}

	if create {
		if err := st.syncDirectoryChain(absPath); err != nil {
			_ = base.Close(ctx)
			return nil, fmt.Errorf("persist local storage directory: %w", err)
		}
	}

	return st, nil
}

func (s *localStorage) ConnectionInfo() blob.ConnectionInfo {
	return blob.ConnectionInfo{
		Type:   localFilesystemStorageType,
		Config: &filesystem.Options{Path: s.rootPath},
	}
}

func (s *localStorage) PutBlob(ctx context.Context, id blob.ID, data blob.Bytes, opts blob.PutOptions) error {
	if err := s.Storage.PutBlob(ctx, id, data, opts); err != nil {
		return err
	}

	paths, ok := s.Storage.(shardedPathProvider)
	if !ok {
		return errors.New("local filesystem provider does not expose sharded paths")
	}

	dir, file, err := paths.GetShardedPathAndFilePath(ctx, id)
	if err != nil {
		return fmt.Errorf("resolve stored blob path: %w", err)
	}

	if err := s.syncFile(file); err != nil {
		return fmt.Errorf("sync stored blob %q: %w", id, err)
	}

	if err := s.syncDirectoryChain(dir); err != nil {
		return fmt.Errorf("sync stored blob directory %q: %w", id, err)
	}

	return nil
}

func (s *localStorage) DeleteBlob(ctx context.Context, id blob.ID) error {
	paths, ok := s.Storage.(shardedPathProvider)
	if !ok {
		return errors.New("local filesystem provider does not expose sharded paths")
	}

	dir, _, err := paths.GetShardedPathAndFilePath(ctx, id)
	if err != nil {
		return fmt.Errorf("resolve blob directory: %w", err)
	}

	if err := s.Storage.DeleteBlob(ctx, id); err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		// Native DeleteBlob treats a missing blob as an idempotent success. If
		// its shard directory is absent, there was no directory entry to persist.
		return nil
	} else if err != nil {
		return fmt.Errorf("stat blob directory %q: %w", id, err)
	}

	if err := s.syncDirectoryChain(dir); err != nil {
		return fmt.Errorf("sync deleted blob directory %q: %w", id, err)
	}

	return nil
}

func (s *localStorage) syncDirectoryChain(dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve directory %q: %w", dir, err)
	}
	root, err := filepath.Abs(s.rootPath)
	if err != nil {
		return fmt.Errorf("resolve storage root %q: %w", s.rootPath, err)
	}

	// The native sharded path must remain under the configured storage root.
	rel, err := filepath.Rel(root, absDir)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || (len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator)) {
		return fmt.Errorf("blob directory %q is outside storage root %q", absDir, root)
	}

	stop := filepath.Dir(root)
	for current := absDir; ; current = filepath.Dir(current) {
		if err := s.syncDir(current); err != nil {
			return err
		}
		if current == stop {
			break
		}
	}

	return nil
}

func syncLocalFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}

	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}

func syncLocalDir(path string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("directory fsync is only supported by this backend on Linux (running %s)", runtime.GOOS)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}

	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}

func init() {
	blob.AddSupportedStorage(localFilesystemStorageType, filesystem.Options{}, func(ctx context.Context, options *filesystem.Options, create bool) (blob.Storage, error) {
		return newLocalStorageFromOptions(ctx, options, create)
	})
}

func newLocalStorageFromOptions(ctx context.Context, options *filesystem.Options, create bool) (blob.Storage, error) {
	st, err := NewLocalStorage(ctx, options.Path, create)
	if err != nil {
		return nil, err
	}

	return st, nil
}

var _ blob.Storage = (*localStorage)(nil)
