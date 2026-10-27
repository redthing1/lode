package kopia

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	"github.com/redthing1/lode/internal/model"
)

var (
	ErrPathNotFound = errors.New("Kopia tree path not found")
	ErrPathNotFile  = errors.New("Kopia tree path is not a file")
	ErrInvalidPage  = errors.New("invalid Kopia tree page")
)

// Objects is a narrow tree and file-object adapter over one native repository.
// The repository lifecycle and any retention policy belong to the caller.
type Objects struct {
	repo      repo.DirectRepository
	namespace string
}

func NewObjects(r repo.DirectRepository) (*Objects, error) {
	if r == nil || len(r.UniqueID()) == 0 {
		return nil, fmt.Errorf("missing Kopia repository or unique ID")
	}
	return &Objects{repo: r, namespace: hex.EncodeToString(r.UniqueID())}, nil
}

func (o *Objects) Namespace() string { return o.namespace }

// Change is a whole-entry overlay. File SHA256 values are lowercase hex.
type Change struct {
	Path   model.Path
	Kind   model.EntryKind
	SHA256 string
	Size   int64
}

// FileFact identifies newly captured file data inside this repository.
type FileFact struct {
	Ref    string
	SHA256 string
	Size   int64
}

// Build overlays complete-tree changes and exports the resulting native
// metadata closure. Payload readers are consumed once, in path order. The
// archive is metadata-only; callers must protect its native payload refs.
func (o *Objects) Build(ctx context.Context, base *Archive, changes []Change, openFile func(model.Path) (io.ReadCloser, error)) (Archive, []FileFact, error) {
	if o == nil || o.repo == nil {
		return Archive{}, nil, errors.New("nil Kopia objects adapter")
	}
	if err := ctx.Err(); err != nil {
		return Archive{}, nil, err
	}
	baseEntries := map[model.Path]model.Entry{{}: {Kind: model.Directory}}
	baseNative := map[model.Path]*snapshot.DirEntry{}
	var baseRoot *snapshot.DirEntry
	if base != nil {
		tree, err := OpenArchive(o.namespace, *base)
		if err != nil {
			return Archive{}, nil, fmt.Errorf("open base archive: %w", err)
		}
		baseRoot = tree.RootDescriptor()
		if err := flattenArchive(o.namespace, *base, baseRoot, baseEntries, baseNative); err != nil {
			return Archive{}, nil, err
		}
	}
	changeMap := make(map[model.Path]model.Entry, len(changes))
	ordered := append([]Change(nil), changes...)
	for _, c := range ordered {
		if c.Path.IsRoot() {
			return Archive{}, nil, fmt.Errorf("change cannot replace project root")
		}
		p, err := model.ParsePath(c.Path.String())
		if err != nil || p != c.Path {
			return Archive{}, nil, fmt.Errorf("invalid change path %q", c.Path.String())
		}
		if _, exists := changeMap[c.Path]; exists {
			return Archive{}, nil, fmt.Errorf("duplicate change path %q", c.Path)
		}
		switch c.Kind {
		case model.File:
			if c.Size < 0 || c.Size == int64(^uint64(0)>>1) || !isLowerSHA256(c.SHA256) || openFile == nil {
				return Archive{}, nil, fmt.Errorf("invalid file change at %q", c.Path)
			}
			changeMap[c.Path] = model.Entry{Kind: model.File, ContentID: "pending:" + c.Path.String()}
		case model.Directory:
			if c.Size != 0 || c.SHA256 != "" {
				return Archive{}, nil, fmt.Errorf("directory change has file facts at %q", c.Path)
			}
			changeMap[c.Path] = model.Entry{Kind: model.Directory}
		case model.Absent:
			if c.Size != 0 || c.SHA256 != "" {
				return Archive{}, nil, fmt.Errorf("absence change has file facts at %q", c.Path)
			}
			changeMap[c.Path] = model.Entry{Kind: model.Absent}
		default:
			return Archive{}, nil, fmt.Errorf("unsupported change kind at %q", c.Path)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path.String() < ordered[j].Path.String() })
	targetEntries, err := model.ApplyChanges(baseEntries, changeMap)
	if err != nil {
		return Archive{}, nil, err
	}
	finalRoot := baseRoot
	facts := make([]FileFact, 0, len(changes))
	capturedNative := make(map[model.Path]*snapshot.DirEntry, len(changes))
	err = repo.DirectWriteSession(ctx, o.repo, repo.WriteSessionOptions{Purpose: "lode-tree-overlay"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		for _, c := range ordered {
			if c.Kind != model.File {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			r, err := openFile(c.Path)
			if err != nil {
				if r != nil {
					_ = r.Close()
				}
				return fmt.Errorf("open changed file %q: %w", c.Path, err)
			}
			if r == nil {
				return fmt.Errorf("open changed file %q: nil reader", c.Path)
			}
			writer := w.NewObjectWriter(ctx, object.WriterOptions{Description: "LODE:" + c.Path.String(), Compressor: compression.Name("zstd-fastest"), MetadataCompressor: compression.Name("zstd-fastest")})
			h := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(writer, h), io.LimitReader(r, c.Size+1))
			readerErr := r.Close()
			if copyErr != nil {
				_ = writer.Close()
				return fmt.Errorf("read changed file %q: %w", c.Path, copyErr)
			}
			if readerErr != nil {
				_ = writer.Close()
				return fmt.Errorf("close changed file %q: %w", c.Path, readerErr)
			}
			if n != c.Size {
				_ = writer.Close()
				return fmt.Errorf("changed file %q size is %d, expected %d", c.Path, n, c.Size)
			}
			digest := hex.EncodeToString(h.Sum(nil))
			if digest != c.SHA256 {
				_ = writer.Close()
				return fmt.Errorf("changed file %q SHA256 mismatch", c.Path)
			}
			id, resultErr := writer.Result()
			closeErr := writer.Close()
			if resultErr != nil {
				return fmt.Errorf("finish changed file %q: %w", c.Path, resultErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close native writer for %q: %w", c.Path, closeErr)
			}
			if id.String() == "" {
				return fmt.Errorf("native writer returned empty object for %q", c.Path)
			}
			targetEntries[c.Path] = model.Entry{Kind: model.File, ContentID: scopedObjectID(o.namespace, id.String())}
			capturedNative[c.Path] = &snapshot.DirEntry{
				Type: snapshot.EntryTypeFile, Permissions: 0644, ModTime: deterministicModTime(),
				ObjectID: id, FileSize: n,
			}
			facts = append(facts, FileFact{Ref: id.String(), SHA256: digest, Size: n})
		}
		var err error
		finalRoot, err = writeDirectoryTree(ctx, w, baseRoot, baseEntries, baseNative, targetEntries, capturedNative)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Archive{}, nil, err
	}
	if err := o.repo.Refresh(ctx); err != nil {
		return Archive{}, nil, fmt.Errorf("refresh after tree write: %w", err)
	}
	archive, err := ExportTree(ctx, o.repo, finalRoot)
	if err != nil {
		return Archive{}, nil, err
	}
	return archive, facts, nil
}

func flattenArchive(namespace string, a Archive, root *snapshot.DirEntry, entries map[model.Path]model.Entry, native map[model.Path]*snapshot.DirEntry) error {
	native[model.Path{}] = root.Clone()
	var walk func(model.Path, *snapshot.DirEntry) error
	walk = func(parent model.Path, de *snapshot.DirEntry) error {
		dir, err := decodeDirectory(a.Directories[de.ObjectID.String()])
		if err != nil {
			return err
		}
		for _, child := range dir.Entries {
			p, err := model.ParsePath(child.Name)
			if err != nil {
				return err
			}
			if !parent.IsRoot() {
				p, err = model.ParsePath(parent.String() + "/" + child.Name)
				if err != nil {
					return err
				}
			}
			native[p] = child.Clone()
			switch child.Type {
			case snapshot.EntryTypeFile:
				entries[p] = model.Entry{Kind: model.File, ContentID: scopedObjectID(namespace, child.ObjectID.String())}
			case snapshot.EntryTypeDirectory:
				entries[p] = model.Entry{Kind: model.Directory}
				if err := walk(p, child); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported native type at %q", p)
			}
		}
		return nil
	}
	return walk(model.Path{}, root)
}

func isLowerSHA256(s string) bool {
	if len(s) != sha256.Size*2 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func deterministicModTime() fs.UTCTimestamp { return fs.UTCTimestampFromTime(time.Unix(0, 0).UTC()) }

// Node is a metadata-only tree row. Ref is a native object ID meaningful only
// within the Objects repository and is present only for files.
type Node struct {
	Path model.Path
	Kind model.EntryKind
	Ref  string
	Size int64
}

func (o *Objects) List(archive Archive, selection model.Selection, after string, limit int) ([]Node, string, error) {
	if o == nil || o.repo == nil {
		return nil, "", errors.New("nil Kopia objects adapter")
	}
	if limit < 1 || limit > 1000 {
		return nil, "", ErrInvalidPage
	}
	tree, err := OpenArchive(o.namespace, archive)
	if err != nil {
		return nil, "", err
	}
	root := tree.RootDescriptor()
	all := []Node{}
	var walk func(model.Path, *snapshot.DirEntry) error
	walk = func(parent model.Path, de *snapshot.DirEntry) error {
		dir, err := decodeDirectory(archive.Directories[de.ObjectID.String()])
		if err != nil {
			return err
		}
		for _, child := range dir.Entries {
			p, err := model.ParsePath(child.Name)
			if err != nil {
				return err
			}
			if !parent.IsRoot() {
				p, err = model.ParsePath(parent.String() + "/" + child.Name)
				if err != nil {
					return err
				}
			}
			kind := model.File
			if child.Type == snapshot.EntryTypeDirectory {
				kind = model.Directory
			}
			if selection.Includes(p) || (kind == model.Directory && selection.Intersects(p)) {
				node := Node{Path: p, Kind: kind}
				if kind == model.File {
					node.Ref = child.ObjectID.String()
					node.Size = child.FileSize
				}
				all = append(all, node)
			}
			if child.Type == snapshot.EntryTypeDirectory && selection.Intersects(p) {
				if err := walk(p, child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(model.Path{}, root); err != nil {
		return nil, "", err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Path.String() < all[j].Path.String() })
	start := sort.Search(len(all), func(i int) bool { return all[i].Path.String() > after })
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	var next string
	if end < len(all) && end > start {
		next = all[end-1].Path.String()
	}
	return append([]Node(nil), all[start:end]...), next, nil
}

func (o *Objects) OpenFile(ctx context.Context, archive Archive, path model.Path) (io.ReadCloser, Node, error) {
	if o == nil || o.repo == nil {
		return nil, Node{}, errors.New("nil Kopia objects adapter")
	}
	tree, err := OpenArchive(o.namespace, archive)
	if err != nil {
		return nil, Node{}, err
	}
	entry, err := tree.Lookup(path)
	if err != nil {
		return nil, Node{}, err
	}
	if entry.Kind == model.Absent {
		return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFound, path)
	}
	if entry.Kind != model.File {
		return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFile, path)
	}
	de := tree.root
	if !path.IsRoot() {
		for _, name := range strings.Split(path.String(), "/") {
			dir, err := decodeDirectory(archive.Directories[de.ObjectID.String()])
			if err != nil {
				return nil, Node{}, err
			}
			var found *snapshot.DirEntry
			for _, child := range dir.Entries {
				if child.Name == name {
					found = child
					break
				}
			}
			if found == nil {
				return nil, Node{}, fmt.Errorf("%w: %q", ErrPathNotFound, path)
			}
			de = found
		}
	}
	raw, err := object.ParseID(de.ObjectID.String())
	if err != nil {
		return nil, Node{}, err
	}
	r, err := o.repo.OpenObject(ctx, raw)
	if err != nil {
		return nil, Node{}, err
	}
	actualSize := r.Length()
	if actualSize != de.FileSize {
		_ = r.Close()
		return nil, Node{}, fmt.Errorf("%w: recorded size %d, native size %d", ErrInvalidArchive, de.FileSize, actualSize)
	}
	return r, Node{Path: path, Kind: model.File, Ref: de.ObjectID.String(), Size: de.FileSize}, nil
}
