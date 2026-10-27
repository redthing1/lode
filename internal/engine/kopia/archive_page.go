package kopia

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kopia/kopia/snapshot"

	"github.com/redthing1/lode/internal/model"
)

type archivedPageItem struct {
	path      model.Path
	entry     *snapshot.DirEntry
	ancestors *archivedDirectoryAncestor
	emit      bool
}

type archivedSiblingCursor struct {
	key       model.Path
	parent    model.Path
	entries   []*snapshot.DirEntry
	index     int
	ancestors *archivedDirectoryAncestor
}

type archivedDirectoryAncestor struct {
	id     string
	depth  int
	parent *archivedDirectoryAncestor
}

type archivedPageFrontierItem struct {
	path    model.Path
	entry   *archivedPageItem
	sibling *archivedSiblingCursor
}

type archivedPageFrontier []archivedPageFrontierItem

func (h archivedPageFrontier) Len() int { return len(h) }
func (h archivedPageFrontier) Less(i, j int) bool {
	return h[i].path.String() < h[j].path.String()
}
func (h archivedPageFrontier) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *archivedPageFrontier) Push(value any) {
	*h = append(*h, value.(archivedPageFrontierItem))
}
func (h *archivedPageFrontier) Pop() any {
	old := *h
	last := old[len(old)-1]
	old[len(old)-1] = archivedPageFrontierItem{}
	*h = old[:len(old)-1]
	return last
}

// ListArchived walks just the directory records needed for one ordered page.
// Directory decoding and validation remain in the Kopia adapter; the reader
// callback only retrieves one opaque record at a time. Corruption in an
// unvisited branch is deferred until a request reaches that branch. Every
// visited manifest is still decoded and sorted in full because the native
// format has no index for seeking within a directory stream.
func (o *Objects) ListArchived(ctx context.Context, format string, rootBytes []byte, readDirectory func(context.Context, string) ([]byte, error), selection model.Selection, after string, limit int) ([]Node, string, error) {
	if o == nil {
		return nil, "", fmt.Errorf("nil Kopia objects adapter")
	}
	if readDirectory == nil {
		return nil, "", fmt.Errorf("%w: missing directory reader", ErrInvalidArchive)
	}
	if limit < 1 || limit > 1000 {
		return nil, "", ErrInvalidPage
	}
	cursor, err := model.ParsePath(after)
	if err != nil {
		return nil, "", ErrInvalidPage
	}
	if format != archiveFormat {
		return nil, "", fmt.Errorf("%w: %q", ErrArchiveVersion, format)
	}
	var root snapshot.DirEntry
	if err := decodeSingleJSON(rootBytes, &root); err != nil {
		return nil, "", fmt.Errorf("%w: root descriptor: %v", ErrInvalidArchive, err)
	}
	if err := validateRoot(&root); err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	frontier := archivedPageFrontier{}
	heap.Init(&frontier)
	if err := pushArchivedDirectory(ctx, &frontier, readDirectory, model.Path{}, &root, nil, 0, selection, cursor); err != nil {
		return nil, "", err
	}

	page := make([]Node, 0, limit+1)
	for len(frontier) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		item := heap.Pop(&frontier).(archivedPageFrontierItem)
		if item.sibling != nil {
			siblings := item.sibling
			child := siblings.entries[siblings.index]
			siblings.index++
			if err := pushArchivedSiblingCursor(&frontier, siblings, selection, cursor); err != nil {
				return nil, "", err
			}
			if siblings.index < len(siblings.entries) {
				heap.Push(&frontier, archivedPageFrontierItem{path: siblings.key, sibling: siblings})
			}
			pageItem, included, err := archivedPageChild(siblings.parent, child, siblings.ancestors, selection, cursor)
			if err != nil {
				return nil, "", err
			}
			if included {
				heap.Push(&frontier, archivedPageFrontierItem{path: pageItem.path, entry: &pageItem})
			}
			continue
		}

		pageItem := item.entry
		if pageItem.emit {
			kind := model.File
			if pageItem.entry.Type == snapshot.EntryTypeDirectory {
				kind = model.Directory
			}
			node := Node{Path: pageItem.path, Kind: kind}
			if kind == model.File {
				node.Ref = pageItem.entry.ObjectID.String()
				node.Size = pageItem.entry.FileSize
			}
			page = append(page, node)
			if len(page) > limit {
				next := page[limit-1].Path.String()
				return page[:limit], next, nil
			}
		}
		if pageItem.entry.Type == snapshot.EntryTypeDirectory && selection.Intersects(pageItem.path) {
			if err := pushArchivedDirectory(ctx, &frontier, readDirectory, pageItem.path, pageItem.entry, pageItem.ancestors, pageItem.ancestors.depth+1, selection, cursor); err != nil {
				return nil, "", err
			}
		}
	}
	return page, "", nil
}

func pushArchivedDirectory(ctx context.Context, frontier *archivedPageFrontier, readDirectory func(context.Context, string) ([]byte, error), parent model.Path, entry *snapshot.DirEntry, ancestors *archivedDirectoryAncestor, depth int, selection model.Selection, after model.Path) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := entry.ObjectID.String()
	for ancestor := ancestors; ancestor != nil; ancestor = ancestor.parent {
		if ancestor.id == id {
			return fmt.Errorf("%w: directory cycle at %q", ErrInvalidArchive, id)
		}
	}
	if depth > maxDirectoryDepth {
		return fmt.Errorf("%w: directory depth exceeds %d", ErrInvalidArchive, maxDirectoryDepth)
	}
	data, err := readDirectory(ctx, id)
	if err != nil {
		return fmt.Errorf("read directory %q: %w", id, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A single native directory record is the smallest seekable unit. Bound it
	// using the same limit as complete archive loading before decoding the JSON.
	if len(data) > maxDirectoryBytes {
		return fmt.Errorf("%w: directory %q exceeds %d bytes", ErrInvalidArchive, id, maxDirectoryBytes)
	}
	dir, err := decodeDirectory(data)
	if err != nil {
		return fmt.Errorf("%w: directory %q: %v", ErrInvalidArchive, id, err)
	}
	sort.Slice(dir.Entries, func(i, j int) bool { return dir.Entries[i].Name < dir.Entries[j].Name })
	chain := &archivedDirectoryAncestor{id: id, depth: depth, parent: ancestors}
	if len(dir.Entries) == 0 {
		return nil
	}
	cursor := &archivedSiblingCursor{parent: parent, entries: dir.Entries, ancestors: chain}
	if err := pushArchivedSiblingCursor(frontier, cursor, selection, after); err != nil {
		return err
	}
	if cursor.index < len(cursor.entries) {
		heap.Push(frontier, archivedPageFrontierItem{path: cursor.key, sibling: cursor})
	}
	return nil
}

func pushArchivedSiblingCursor(frontier *archivedPageFrontier, cursor *archivedSiblingCursor, selection model.Selection, after model.Path) error {
	for cursor.index < len(cursor.entries) {
		child := cursor.entries[cursor.index]
		item, include, err := archivedPageChild(cursor.parent, child, cursor.ancestors, selection, after)
		if err != nil {
			return err
		}
		if !include {
			cursor.index++
			continue
		}
		cursor.key = item.path
		return nil
	}
	return nil
}

func archivedPageChild(parent model.Path, child *snapshot.DirEntry, ancestors *archivedDirectoryAncestor, selection model.Selection, after model.Path) (archivedPageItem, bool, error) {
	pathText := child.Name
	if !parent.IsRoot() {
		pathText = parent.String() + "/" + child.Name
	}
	path, err := model.ParsePath(pathText)
	if err != nil {
		return archivedPageItem{}, false, err
	}
	directory := child.Type == snapshot.EntryTypeDirectory
	if directory {
		if !selection.Intersects(path) {
			return archivedPageItem{}, false, nil
		}
	} else if !selection.Includes(path) {
		return archivedPageItem{}, false, nil
	}

	item := archivedPageItem{path: path, entry: child, ancestors: ancestors, emit: true}
	comparison := path.String()
	afterText := after.String()
	if comparison > afterText {
		return item, true, nil
	}
	if comparison == afterText {
		item.emit = false
		return item, directory, nil
	}
	if !directory {
		return archivedPageItem{}, false, nil
	}
	item.emit = false
	directoryPrefix := comparison + "/"
	if strings.HasPrefix(afterText, directoryPrefix) || directoryPrefix > afterText {
		return item, true, nil
	}
	return archivedPageItem{}, false, nil
}
