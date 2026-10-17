// Package kopia integrates Kopia objects, directory trees, and snapshot roots.
package kopia

import (
	"context"
	"fmt"
	"io"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	"github.com/redthing1/lode/internal/model"
)

const metadataCompression = "zstd-fastest"

// Entry is an immutable stored entry. Its zero value represents absence.
type Entry struct {
	data *snapshot.DirEntry
}

// State returns semantic entry facts within this repository's identity namespace.
func (e Entry) State() model.Entry {
	if e.data == nil {
		return model.Entry{Kind: model.Absent}
	}
	switch e.data.Type {
	case snapshot.EntryTypeDirectory:
		return model.Entry{Kind: model.Directory}
	case snapshot.EntryTypeFile:
		if e.data.ObjectID.String() == "" {
			return model.Entry{}
		}
		return model.Entry{
			Kind:       model.File,
			ContentID:  "kopia:" + e.data.ObjectID.String(),
			Executable: e.data.Permissions&0o111 != 0,
		}
	default:
		return model.Entry{}
	}
}

// Directory creates an empty-directory intent for Apply. It is not a stored root.
func Directory() Entry {
	return Entry{data: &snapshot.DirEntry{Type: snapshot.EntryTypeDirectory, Permissions: 0o755}}
}

// CaptureFile streams file bytes into the current writer. The caller owns flush
// and root protection; a returned entry alone is not a retained checkpoint.
func CaptureFile(ctx context.Context, w repo.RepositoryWriter, source io.Reader, executable bool) (Entry, error) {
	ow := w.NewObjectWriter(ctx, object.WriterOptions{
		Compressor: metadataCompression, MetadataCompressor: metadataCompression,
	})
	defer ow.Close()
	n, err := io.Copy(ow, source)
	if err != nil {
		return Entry{}, fmt.Errorf("capture file: %w", err)
	}
	id, err := ow.Result()
	if err != nil {
		return Entry{}, fmt.Errorf("finish file: %w", err)
	}
	permissions := snapshot.Permissions(0o644)
	if executable {
		permissions = 0o755
	}
	return Entry{data: &snapshot.DirEntry{
		Type: snapshot.EntryTypeFile, ObjectID: id, FileSize: n,
		Permissions: permissions, ModTime: fs.UTCTimestampFromTime(w.Time()),
	}}, nil
}

func OpenFile(ctx context.Context, r repo.Repository, entry Entry) (object.Reader, error) {
	if entry.State().Kind != model.File {
		return nil, fmt.Errorf("%w: expected a file", model.ErrInvalidEntry)
	}
	return r.OpenObject(ctx, entry.data.ObjectID)
}

// sameStoredEntry compares preconditions, including a directory's stored tree.
// Directory subtree identities do not participate in model.Entry reconciliation.
func sameStoredEntry(a, b Entry) bool {
	if a.State() != b.State() {
		return false
	}
	if a.data == nil {
		return true
	}
	return a.data.ObjectID == b.data.ObjectID
}
