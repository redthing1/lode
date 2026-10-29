package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/redthing1/lode/internal/model"
)

type Writer struct {
	ctx    context.Context
	native repo.RepositoryWriter
}

var ErrIdentity = errors.New("file changed while saving")

// Write flushes all objects and protections before returning success.
func (s *Store) Write(ctx context.Context, fn func(*Writer) error) error {
	return repo.WriteSession(ctx, s.repository, repo.WriteSessionOptions{Purpose: "lode capture"}, func(ctx context.Context, w repo.RepositoryWriter) error { return fn(&Writer{ctx: ctx, native: w}) })
}

// File accepts a streaming producer, including delta reconstruction, and verifies
// its declared identity before making the object available to the caller.
func (w *Writer) File(e model.Entry, write func(io.Writer) error) (Object, error) {
	if err := e.Validate(); err != nil {
		return Object{}, err
	}
	if e.Kind != model.File || write == nil {
		return Object{}, errors.New("file requires a content source")
	}
	out := w.native.NewObjectWriter(w.ctx, object.WriterOptions{Compressor: compression.Name("zstd-fastest")})
	h := sha256.New()
	limited := &fileWriter{ctx: w.ctx, out: io.MultiWriter(out, h), remaining: e.Size}
	err := write(limited)
	if err == nil && (limited.remaining != 0 || hex.EncodeToString(h.Sum(nil)) != e.SHA256) {
		err = fmt.Errorf("%w: %q", ErrIdentity, e.Path)
	}
	if err != nil {
		return Object{}, errors.Join(err, out.Close())
	}
	id, err := out.Result()
	if err := errors.Join(err, out.Close()); err != nil {
		return Object{}, err
	}
	return Object{ID: id.String()}, nil
}

type fileWriter struct {
	ctx       context.Context
	out       io.Writer
	remaining int64
}

func (w *fileWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("%w: file exceeds declared size", ErrIdentity)
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	return n, storageError(err)
}

// Entry describes one immediate child; Path is its basename.
type Entry struct {
	model.Entry
	Object Object `json:"object"`
}

func nativeEntry(e Entry) (*snapshot.DirEntry, error) {
	id, err := object.ParseID(e.Object.ID)
	if err != nil {
		return nil, err
	}
	d := &snapshot.DirEntry{Name: e.Path, ObjectID: id}
	switch e.Kind {
	case model.File:
		d.Type = snapshot.EntryTypeFile
		d.FileSize = e.Size
	case model.Directory:
		d.Type = snapshot.EntryTypeDirectory
		d.DirSummary = &fs.DirectorySummary{TotalFileSize: e.Object.Bytes, TotalFileCount: e.Object.Files, TotalDirCount: e.Object.Directories}
	default:
		return nil, fmt.Errorf("invalid native entry %q", e.Path)
	}
	return d, nil
}

func (w *Writer) Directory(entries []Entry) (Object, error) {
	var builder snapshotfs.DirManifestBuilder
	seen := map[string]bool{}
	for _, e := range entries {
		child := e.Entry
		child.Path = "child/" + e.Path
		if err := child.Validate(); err != nil {
			return Object{}, err
		}
		if strings.Contains(e.Path, "/") || seen[e.Path] {
			return Object{}, fmt.Errorf("invalid directory child %q", e.Path)
		}
		seen[e.Path] = true
		d, err := nativeEntry(e)
		if err != nil {
			return Object{}, err
		}
		builder.AddEntry(d)
	}
	dm := builder.Build(fs.UTCTimestamp(0), "")
	id, err := snapshotfs.WriteDirManifest(w.ctx, w.native, "", dm, compression.Name("zstd-fastest"))
	if err != nil {
		return Object{}, err
	}
	return Object{ID: id.String(), Bytes: dm.Summary.TotalFileSize, Files: dm.Summary.TotalFileCount, Directories: dm.Summary.TotalDirCount}, nil
}

func (w *Writer) Protect(operation string, root Object) (string, error) {
	if operation == "" {
		return "", errors.New("empty operation ID")
	}
	d, err := nativeEntry(Entry{Entry: model.Entry{Kind: model.Directory}, Object: root})
	if err != nil {
		return "", err
	}
	now := fs.UTCTimestampFromTime(time.Now())
	id, err := snapshot.SaveSnapshot(w.ctx, w.native, &snapshot.Manifest{RootEntry: d, Source: snapshot.SourceInfo{UserName: "lode", Host: "local", Path: "/"}, StartTime: now, EndTime: now, Tags: map[string]string{operationLabel: operation}})
	return string(id), err
}
