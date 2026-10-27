//go:build linux

package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/compression"
	kopiacontent "github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"

	"github.com/redthing1/lode/internal/model"
)

const operationLabel = "lode.operation"

type Entry struct {
	model.Entry
	Object string
}

type Store struct {
	repository repo.Repository
}

// Open opens or initializes the native content repository rooted in directory.
func Open(ctx context.Context, directory, password string, create bool) (result *Store, retErr error) {
	if password == "" {
		return nil, errors.New("empty content repository password")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	baseCreated, err := ensureBaseDirectory(root, create)
	if err != nil {
		return nil, err
	}
	config := filepath.Join(root, "kopia.config")
	storagePath := filepath.Join(root, "repository")
	st, err := newStorage(ctx, storagePath, create)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := st.Close(ctx); closeErr != nil {
			if result != nil {
				retErr = errors.Join(retErr, result.Close(ctx))
				result = nil
			}
			retErr = errors.Join(retErr, closeErr)
		}
	}()

	if create {
		if err := repo.Initialize(ctx, st, nil, password); err != nil && !errors.Is(err, repo.ErrAlreadyInitialized) {
			return nil, err
		}
	}
	tempConfig := filepath.Join(root, "kopia.connect")
	defer os.Remove(tempConfig)
	if err = repo.Connect(ctx, tempConfig, st, password, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{Hostname: "local", Username: "lode", Description: "Lode content store"},
		CachingOptions: kopiacontent.CachingOptions{
			CacheDirectory:              filepath.Join(root, "cache"),
			ContentCacheSizeBytes:       256 << 20,
			ContentCacheSizeLimitBytes:  512 << 20,
			MetadataCacheSizeBytes:      64 << 20,
			MetadataCacheSizeLimitBytes: 128 << 20,
		},
	}); err != nil {
		return nil, err
	}
	if err := os.Rename(tempConfig, config); err != nil {
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		return nil, fmt.Errorf("sync content store directory: %w", err)
	}
	if baseCreated {
		if err := syncDirectory(filepath.Dir(root)); err != nil {
			return nil, fmt.Errorf("sync parent of new content store directory: %w", err)
		}
	}
	r, err := repo.Open(ctx, config, password, &repo.Options{DisableRepositoryLog: true})
	if err != nil {
		return nil, err
	}
	return &Store{repository: r}, nil
}

func ensureBaseDirectory(path string, create bool) (created bool, err error) {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("content store path is not a directory")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("content store directory: %w", err)
	}
	if !create {
		return false, fmt.Errorf("content store directory: %w", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return false, err
	}
	info, err = os.Stat(path)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("content store path is not a directory")
	}
	return true, nil
}

func (s *Store) Close(ctx context.Context) error { return s.repository.Close(ctx) }

// Capture persists changed file objects, composes the complete tree and protects it
// with a native snapshot tagged by operationID. The caller supplies the complete tree.
func (s *Store) Capture(ctx context.Context, operationID string, entries []Entry, write func(model.Entry, io.Writer) error) (string, []Entry, error) {
	if operationID == "" {
		return "", nil, errors.New("empty operation ID")
	}
	captured := append([]Entry(nil), entries...)
	var rootID manifest.ID
	err := repo.WriteSession(ctx, s.repository, repo.WriteSessionOptions{Purpose: "lode capture"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		for i := range captured {
			e := &captured[i]
			if e.Kind != model.File {
				e.Object = ""
				continue
			}
			if e.Object != "" {
				continue
			}
			if e.Size < 0 || e.Size == math.MaxInt64 {
				return fmt.Errorf("invalid or unsupported file size for %q", e.Path)
			}
			if write == nil {
				return fmt.Errorf("no content source for %q", e.Path)
			}
			ow := w.NewObjectWriter(ctx, object.WriterOptions{Compressor: compression.Name("zstd-fastest")})
			h := sha256.New()
			sink := &captureWriter{out: io.MultiWriter(ow, h), remaining: e.Size}
			if err := write(e.Entry, sink); err != nil {
				_ = ow.Close()
				return err
			}
			if sink.remaining != 0 || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
				_ = ow.Close()
				return fmt.Errorf("content identity mismatch for %q", e.Path)
			}
			oid, err := ow.Result()
			if err != nil {
				_ = ow.Close()
				return err
			}
			if err := ow.Close(); err != nil {
				return err
			}
			e.Object = oid.String()
		}
		rootEntry, err := writeTree(ctx, w, captured)
		if err != nil {
			return err
		}
		now := fs.UTCTimestampFromTime(time.Now())
		man := &snapshot.Manifest{Source: snapshot.SourceInfo{Host: "local", UserName: "lode", Path: "/"}, Description: "Lode operation " + operationID, StartTime: now, EndTime: now, RootEntry: rootEntry, Tags: map[string]string{operationLabel: operationID}}
		rootID, err = snapshot.SaveSnapshot(ctx, w, man)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return string(rootID), captured, nil
}

type captureWriter struct {
	out       io.Writer
	remaining int64
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("file exceeds declared size")
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type treeNode struct {
	name     string
	entry    *Entry
	children map[string]*treeNode
}

func writeTree(ctx context.Context, w repo.RepositoryWriter, entries []Entry) (*snapshot.DirEntry, error) {
	root := &treeNode{children: map[string]*treeNode{}}
	for i := range entries {
		parts := strings.Split(entries[i].Path, "/")
		n := root
		for j, p := range parts {
			if n.children == nil {
				n.children = map[string]*treeNode{}
			}
			c := n.children[p]
			if c == nil {
				c = &treeNode{name: p, children: map[string]*treeNode{}}
				n.children[p] = c
			}
			n = c
			if j == len(parts)-1 {
				n.entry = &entries[i]
			}
		}
	}
	return writeNode(ctx, w, "", root)
}
func writeNode(ctx context.Context, w repo.RepositoryWriter, path string, n *treeNode) (*snapshot.DirEntry, error) {
	if n.entry != nil && n.entry.Kind == model.File {
		oid, err := object.ParseID(n.entry.Object)
		if err != nil {
			return nil, fmt.Errorf("invalid native object reference for %q: %w", n.entry.Path, err)
		}
		return &snapshot.DirEntry{Name: n.name, Type: snapshot.EntryTypeFile, FileSize: n.entry.Size, ObjectID: oid}, nil
	}
	keys := make([]string, 0, len(n.children))
	for k := range n.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var builder snapshotfs.DirManifestBuilder
	for _, k := range keys {
		child := n.children[k]
		childPath := k
		if path != "" {
			childPath = path + "/" + k
		}
		de, err := writeNode(ctx, w, childPath, child)
		if err != nil {
			return nil, err
		}
		builder.AddEntry(de)
	}
	// Directory mtimes are not part of Lode's entry model. A stable zero time keeps
	// identical logical directories on the same native object ID.
	dm := builder.Build(fs.UTCTimestamp(0), "")
	oid, err := snapshotfs.WriteDirManifest(ctx, w, path, dm, compression.Name("zstd-fastest"))
	if err != nil {
		return nil, err
	}
	return &snapshot.DirEntry{Name: n.name, Type: snapshot.EntryTypeDirectory, ObjectID: oid, DirSummary: dm.Summary, ModTime: dm.Summary.MaxModTime}, nil
}

func (s *Store) Roots(ctx context.Context) (map[string][]string, error) {
	ms, err := s.repository.FindManifests(ctx, map[string]string{})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, m := range ms {
		if id := m.Labels[operationLabel]; id != "" {
			out[id] = append(out[id], string(m.ID))
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out, nil
}
func (s *Store) Release(ctx context.Context, root string) error {
	id := manifest.ID(root)
	if _, err := snapshot.LoadSnapshot(ctx, s.repository, id); err != nil {
		if errors.Is(err, snapshot.ErrSnapshotNotFound) {
			return nil
		}
		return err
	}
	return repo.WriteSession(ctx, s.repository, repo.WriteSessionOptions{Purpose: "lode release"}, func(ctx context.Context, w repo.RepositoryWriter) error { return w.DeleteManifest(ctx, id) })
}
func (s *Store) Read(ctx context.Context, objectRef string) (io.ReadSeekCloser, error) {
	id, err := object.ParseID(objectRef)
	if err != nil {
		return nil, fmt.Errorf("invalid native object reference: %w", err)
	}
	r, err := s.repository.OpenObject(ctx, id)
	if err != nil {
		return nil, err
	}
	return r, nil
}
