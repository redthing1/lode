package kopia

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	"github.com/redthing1/lode/internal/model"
)

// OpenArchivedFile loads only the directory records on path from an archived
// tree, then opens the selected native file object. Directory records outside
// that ancestor chain are not read or validated.
func (o *Objects) OpenArchivedFile(ctx context.Context, format string, rootBytes []byte, readDirectory func(context.Context, string) ([]byte, error), path model.Path) (io.ReadCloser, Node, error) {
	if o == nil || o.repo == nil {
		return nil, Node{}, fmt.Errorf("nil Kopia objects adapter")
	}
	if readDirectory == nil {
		return nil, Node{}, fmt.Errorf("%w: missing directory reader", ErrInvalidArchive)
	}
	if format != archiveFormat {
		return nil, Node{}, fmt.Errorf("%w: %q", ErrArchiveVersion, format)
	}
	canonical, err := model.ParsePath(path.String())
	if err != nil || canonical != path {
		return nil, Node{}, fmt.Errorf("%w: invalid file path", ErrInvalidArchive)
	}
	if err := ctx.Err(); err != nil {
		return nil, Node{}, err
	}
	var root snapshot.DirEntry
	if err := decodeSingleJSON(rootBytes, &root); err != nil {
		return nil, Node{}, fmt.Errorf("%w: root descriptor: %v", ErrInvalidArchive, err)
	}
	if err := validateRoot(&root); err != nil {
		return nil, Node{}, err
	}
	if path.IsRoot() {
		return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFile, path)
	}
	parts := strings.Split(path.String(), "/")
	if len(parts) > maxDirectoryDepth+1 {
		return nil, Node{}, fmt.Errorf("%w: directory depth exceeds %d", ErrInvalidArchive, maxDirectoryDepth)
	}

	current := &root
	ancestors := make(map[string]struct{}, len(parts))
	for index, name := range parts {
		if err := ctx.Err(); err != nil {
			return nil, Node{}, err
		}
		if current.Type != snapshot.EntryTypeDirectory {
			return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFound, path)
		}
		id := current.ObjectID.String()
		if _, seen := ancestors[id]; seen {
			return nil, Node{}, fmt.Errorf("%w: directory cycle at %q", ErrInvalidArchive, id)
		}
		ancestors[id] = struct{}{}
		if index > maxDirectoryDepth {
			return nil, Node{}, fmt.Errorf("%w: directory depth exceeds %d", ErrInvalidArchive, maxDirectoryDepth)
		}
		data, err := readDirectory(ctx, id)
		if err != nil {
			return nil, Node{}, fmt.Errorf("read directory %q: %w", id, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, Node{}, err
		}
		if len(data) > maxDirectoryBytes {
			return nil, Node{}, fmt.Errorf("%w: directory %q exceeds %d bytes", ErrInvalidArchive, id, maxDirectoryBytes)
		}
		dir, err := decodeDirectory(data)
		if err != nil {
			return nil, Node{}, fmt.Errorf("%w: directory %q: %v", ErrInvalidArchive, id, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, Node{}, err
		}

		var child *snapshot.DirEntry
		for _, entry := range dir.Entries {
			if entry.Name == name {
				child = entry
				break
			}
		}
		if child == nil {
			return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFound, path)
		}
		if index != len(parts)-1 {
			if child.Type != snapshot.EntryTypeDirectory {
				return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFound, path)
			}
			current = child
			continue
		}
		if child.Type != snapshot.EntryTypeFile {
			return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFile, path)
		}
		return o.openArchivedFileObject(ctx, path, child)
	}
	return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFound, path)
}

func (o *Objects) openArchivedFileObject(ctx context.Context, path model.Path, entry *snapshot.DirEntry) (io.ReadCloser, Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, Node{}, err
	}
	raw, err := object.ParseID(entry.ObjectID.String())
	if err != nil || raw.String() != entry.ObjectID.String() {
		return nil, Node{}, fmt.Errorf("%w: malformed file object ID at %q", ErrInvalidArchive, path)
	}
	reader, err := o.repo.OpenObject(ctx, raw)
	if err != nil {
		return nil, Node{}, err
	}
	actualSize := reader.Length()
	if actualSize != entry.FileSize {
		_ = reader.Close()
		return nil, Node{}, fmt.Errorf("%w: recorded size %d, native size %d", ErrInvalidArchive, entry.FileSize, actualSize)
	}
	return reader, Node{Path: path, Kind: model.File, Ref: entry.ObjectID.String(), Size: entry.FileSize}, nil
}
