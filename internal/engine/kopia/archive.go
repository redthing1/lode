package kopia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	"github.com/redthing1/lode/internal/model"
)

// archiveFormat identifies the exact native representation expected below:
// Kopia v0.23.1 DirManifest JSON streams and DirEntry JSON root descriptors.
// It does not promise compatibility with other Kopia versions or formats.
const archiveFormat = "kopia-v0.23.1-dir-json-v1"

// A single directory stream can be wide. This bound limits one record read;
// closure memory is proportional to the complete metadata tree and is not
// globally bounded by this proof adapter.
const maxDirectoryBytes = 1 << 30

// Go recursion remains bounded even for an adversarially deep directory tree.
const maxDirectoryDepth = 4096

var (
	ErrInvalidArchive = errors.New("invalid Kopia metadata archive")
	ErrArchiveVersion = errors.New("unsupported Kopia metadata archive format")
)

// Archive contains the native root descriptor and the raw expanded native
// directory streams reachable from it. File objects and their payload bytes
// are deliberately not included. Directory map keys are native object IDs.
// The value is versioned native JSON, not a cross-version interchange format.
type Archive struct {
	Format      string
	Root        []byte
	Directories map[string][]byte
}

// ExportTree copies the complete directory-metadata closure beneath root from
// repository r. It reads no file objects. The caller owns repository flushing
// and payload-root protection needed to keep objects readable during export.
func ExportTree(ctx context.Context, r repo.Repository, root *snapshot.DirEntry) (Archive, error) {
	if r == nil || root == nil {
		return Archive{}, fmt.Errorf("%w: missing repository or root", ErrInvalidArchive)
	}
	rootBytes, err := json.Marshal(root)
	if err != nil {
		return Archive{}, fmt.Errorf("encode root descriptor: %w", err)
	}
	if err := validateRoot(root); err != nil {
		return Archive{}, err
	}
	return loadArchiveClosure(ctx, archiveFormat, rootBytes, func(ctx context.Context, id string) ([]byte, error) {
		parsed, err := object.ParseID(id)
		if err != nil {
			return nil, err
		}
		reader, err := r.OpenObject(ctx, parsed)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxDirectoryBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > maxDirectoryBytes {
			return nil, fmt.Errorf("directory exceeds %d bytes", maxDirectoryBytes)
		}
		return data, nil
	})
}

// LoadArchive reconstructs an archive from a root descriptor and a raw-record
// lookup. It is suitable for SQLite reads: the persistence layer need only
// retrieve one opaque directory stream at a time; Kopia JSON decoding remains
// private to this adapter. Returned records form a complete detached closure.
func LoadArchive(ctx context.Context, format string, root []byte, readDirectory func(context.Context, string) ([]byte, error)) (Archive, error) {
	if readDirectory == nil {
		return Archive{}, fmt.Errorf("%w: missing directory reader", ErrInvalidArchive)
	}
	return loadArchiveClosure(ctx, format, root, readDirectory)
}

func loadArchiveClosure(ctx context.Context, format string, rootBytes []byte, readDirectory func(context.Context, string) ([]byte, error)) (Archive, error) {
	if format != archiveFormat {
		return Archive{}, fmt.Errorf("%w: %q", ErrArchiveVersion, format)
	}
	var root snapshot.DirEntry
	if err := decodeSingleJSON(rootBytes, &root); err != nil {
		return Archive{}, fmt.Errorf("%w: root descriptor: %v", ErrInvalidArchive, err)
	}
	if err := validateRoot(&root); err != nil {
		return Archive{}, err
	}
	a := Archive{Format: format, Root: append([]byte(nil), rootBytes...), Directories: make(map[string][]byte)}
	visiting := map[string]bool{}
	var visit func(*snapshot.DirEntry, int) error
	visit = func(entry *snapshot.DirEntry, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > maxDirectoryDepth {
			return fmt.Errorf("%w: directory depth exceeds %d", ErrInvalidArchive, maxDirectoryDepth)
		}
		id := entry.ObjectID.String()
		if visiting[id] {
			return fmt.Errorf("%w: directory cycle at %q", ErrInvalidArchive, id)
		}
		if _, ok := a.Directories[id]; ok {
			return nil
		}
		visiting[id] = true
		defer delete(visiting, id)
		data, err := readDirectory(ctx, id)
		if err != nil {
			return fmt.Errorf("read directory %q: %w", id, err)
		}
		if len(data) > maxDirectoryBytes {
			return fmt.Errorf("%w: directory %q exceeds %d bytes", ErrInvalidArchive, id, maxDirectoryBytes)
		}
		dir, err := decodeDirectory(data)
		if err != nil {
			return fmt.Errorf("%w: directory %q: %v", ErrInvalidArchive, id, err)
		}
		a.Directories[id] = append([]byte(nil), data...)
		for _, child := range dir.Entries {
			if child.Type == snapshot.EntryTypeDirectory {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(&root, 0); err != nil {
		return Archive{}, err
	}
	if err := ctx.Err(); err != nil {
		return Archive{}, err
	}
	return a, nil
}

// ArchivedTree is a validated immutable view of an Archive. It owns independent
// decoded metadata, so later caller mutation cannot invalidate its validation.
type ArchivedTree struct {
	namespace string
	root      *snapshot.DirEntry
	dirs      map[string]snapshot.DirManifest
}

// OpenArchive validates the root descriptor, every directory record, and the
// complete reachable directory closure once. The namespace is part of file
// identity because Kopia object IDs are repository-scoped.
func OpenArchive(namespace string, archive Archive) (*ArchivedTree, error) {
	if namespace == "" {
		return nil, fmt.Errorf("%w: empty repository namespace", ErrInvalidArchive)
	}
	if archive.Format != archiveFormat {
		return nil, fmt.Errorf("%w: %q", ErrArchiveVersion, archive.Format)
	}
	if len(archive.Root) == 0 {
		return nil, fmt.Errorf("%w: missing root descriptor", ErrInvalidArchive)
	}
	var root snapshot.DirEntry
	if err := decodeSingleJSON(archive.Root, &root); err != nil {
		return nil, fmt.Errorf("%w: root descriptor: %v", ErrInvalidArchive, err)
	}
	if err := validateRoot(&root); err != nil {
		return nil, err
	}
	tree := &ArchivedTree{namespace: namespace, root: root.Clone(), dirs: make(map[string]snapshot.DirManifest, len(archive.Directories))}
	for id, raw := range archive.Directories {
		parsed, err := object.ParseID(id)
		if err != nil || parsed.String() != id {
			return nil, fmt.Errorf("%w: malformed directory object ID %q", ErrInvalidArchive, id)
		}
		dir, err := decodeDirectory(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: directory %q: %v", ErrInvalidArchive, id, err)
		}
		tree.dirs[id] = dir
	}
	visiting, reachable := map[string]bool{}, map[string]bool{}
	var visit func(string, int) error
	visit = func(id string, depth int) error {
		if depth > maxDirectoryDepth {
			return fmt.Errorf("%w: directory depth exceeds %d", ErrInvalidArchive, maxDirectoryDepth)
		}
		if visiting[id] {
			return fmt.Errorf("%w: directory cycle at %q", ErrInvalidArchive, id)
		}
		if reachable[id] {
			return nil
		}
		dir, ok := tree.dirs[id]
		if !ok {
			return fmt.Errorf("%w: missing directory record %q", ErrInvalidArchive, id)
		}
		visiting[id], reachable[id] = true, true
		for _, child := range dir.Entries {
			if child.Type == snapshot.EntryTypeDirectory {
				if err := visit(child.ObjectID.String(), depth+1); err != nil {
					return err
				}
			}
		}
		delete(visiting, id)
		return nil
	}
	if err := visit(root.ObjectID.String(), 0); err != nil {
		return nil, err
	}
	if len(reachable) != len(tree.dirs) {
		return nil, fmt.Errorf("%w: archive contains unreachable directory records", ErrInvalidArchive)
	}
	return tree, nil
}

// RootDescriptor returns a caller-owned copy of the native root metadata.
func (t *ArchivedTree) RootDescriptor() *snapshot.DirEntry {
	if t == nil || t.root == nil {
		return nil
	}
	return t.root.Clone()
}

// Lookup returns a path's whole-entry comparison identity using only archived
// metadata. A missing path is explicit absence; no repository or payload read
// occurs. Directories have no content identity in the model.
func (t *ArchivedTree) Lookup(path model.Path) (model.Entry, error) {
	if t == nil || t.root == nil {
		return model.Entry{}, fmt.Errorf("%w: nil archived tree", ErrInvalidArchive)
	}
	current := t.root
	if path == (model.Path{}) {
		return model.Entry{Kind: model.Directory}, nil
	}
	for _, name := range strings.Split(path.String(), "/") {
		if current.Type != snapshot.EntryTypeDirectory {
			return model.Entry{Kind: model.Absent}, nil
		}
		dir, ok := t.dirs[current.ObjectID.String()]
		if !ok {
			return model.Entry{}, fmt.Errorf("%w: missing directory %q", ErrInvalidArchive, current.ObjectID.String())
		}
		current = nil
		for _, child := range dir.Entries {
			if child.Name == name {
				current = child
				break
			}
		}
		if current == nil {
			return model.Entry{Kind: model.Absent}, nil
		}
	}
	switch current.Type {
	case snapshot.EntryTypeDirectory:
		return model.Entry{Kind: model.Directory}, nil
	case snapshot.EntryTypeFile:
		return model.Entry{Kind: model.File, ContentID: scopedObjectID(t.namespace, current.ObjectID.String())}, nil
	default:
		return model.Entry{}, fmt.Errorf("%w: unsupported native type %q", ErrInvalidArchive, current.Type)
	}
}

func scopedObjectID(namespace, nativeID string) string {
	return fmt.Sprintf("kopia:v1:%d:%s%s", len(namespace), namespace, nativeID)
}

func validateRoot(root *snapshot.DirEntry) error {
	if root == nil || root.Type != snapshot.EntryTypeDirectory || root.ObjectID.String() == "" || !validSummary(root.DirSummary) {
		return fmt.Errorf("%w: root must be a complete stored directory", ErrInvalidArchive)
	}
	if id, err := object.ParseID(root.ObjectID.String()); err != nil || id.String() != root.ObjectID.String() {
		return fmt.Errorf("%w: malformed root object ID", ErrInvalidArchive)
	}
	return nil
}

func decodeDirectory(data []byte) (snapshot.DirManifest, error) {
	var dir snapshot.DirManifest
	if len(data) == 0 || len(data) > maxDirectoryBytes {
		return dir, fmt.Errorf("directory stream size is invalid")
	}
	if err := decodeSingleJSON(data, &dir); err != nil {
		return dir, err
	}
	if dir.StreamType != "kopia:directory" || !validSummary(dir.Summary) {
		return dir, errors.New("invalid or incomplete directory summary")
	}
	seen := make(map[string]struct{}, len(dir.Entries))
	for _, child := range dir.Entries {
		if child == nil {
			return dir, errors.New("nil directory entry")
		}
		namePath, err := model.ParsePath(child.Name)
		if err != nil || namePath == (model.Path{}) || namePath.Parent() != (model.Path{}) {
			return dir, fmt.Errorf("invalid child name %q", child.Name)
		}
		if _, ok := seen[child.Name]; ok {
			return dir, fmt.Errorf("duplicate child name %q", child.Name)
		}
		seen[child.Name] = struct{}{}
		if child.ObjectID.String() == "" {
			return dir, fmt.Errorf("missing object ID for %q", child.Name)
		}
		parsedID, err := object.ParseID(child.ObjectID.String())
		if err != nil || parsedID.String() != child.ObjectID.String() {
			return dir, fmt.Errorf("invalid object ID for %q", child.Name)
		}
		switch child.Type {
		case snapshot.EntryTypeFile:
			if child.FileSize < 0 || child.DirSummary != nil {
				return dir, fmt.Errorf("invalid file metadata for %q", child.Name)
			}
		case snapshot.EntryTypeDirectory:
			if !validSummary(child.DirSummary) {
				return dir, fmt.Errorf("incomplete child directory %q", child.Name)
			}
		default:
			return dir, fmt.Errorf("unsupported entry type %q for %q", child.Type, child.Name)
		}
	}
	return dir, nil
}

func validSummary(s *fs.DirectorySummary) bool {
	return s != nil && s.IncompleteReason == "" && s.FatalErrorCount == 0 && s.IgnoredErrorCount == 0
}

func decodeSingleJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON data")
		}
		return err
	}
	return nil
}
