package project

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

type edit struct {
	value    *model.Entry
	children map[string]*edit
}

// Basis is the immutable base file available to a streaming content producer.
// Its reader belongs to the project and is valid only during the callback.
type Basis struct {
	Entry  model.Entry
	Reader io.ReadSeeker
}

func edits(changes []model.Entry) *edit {
	root := &edit{}
	for i := range changes {
		n := root
		for _, part := range strings.Split(changes[i].Path, "/") {
			if n.children == nil {
				n.children = map[string]*edit{}
			}
			if n.children[part] == nil {
				n.children[part] = &edit{}
			}
			n = n.children[part]
		}
		n.value = &changes[i]
	}
	return root
}

// rewrite changes exact paths. A directory can disappear only after explicit
// descendant changes empty it, so an omitted subtree cannot be erased by a save.
func (s *Store) rewrite(ctx context.Context, w *content.Writer, base, prefix string, n *edit, source Source, dirs *[]directory) (content.Object, int, error) {
	entries, err := s.directory(ctx, base)
	if err != nil {
		return content.Object{}, 0, err
	}
	children := make(map[string]content.Entry, len(entries))
	for _, e := range entries {
		children[e.Path] = e
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := n.children[name]
		old, exists := children[name]
		full := name
		if prefix != "" {
			full = prefix + "/" + name
		}
		kind := model.Directory
		if child.value != nil {
			kind = child.value.Kind
		}
		var object content.Object
		count := 0
		if len(child.children) > 0 || kind == model.Directory || (exists && old.Kind == model.Directory) {
			if exists && old.Kind == model.File && len(child.children) > 0 && (child.value == nil || kind != model.Directory) {
				return content.Object{}, 0, fmt.Errorf("%w: %q requires replacing a file with a directory", ErrRequest, full)
			}
			childBase := ""
			if old.Kind == model.Directory {
				childBase = old.Object.ID
			}
			object, count, err = s.rewrite(ctx, w, childBase, full, child, source, dirs)
			if err != nil {
				return content.Object{}, 0, err
			}
			if kind != model.Directory && count != 0 {
				return content.Object{}, 0, fmt.Errorf("%w: %q still contains entries omitted from this save", ErrRequest, full)
			}
		}
		switch kind {
		case model.Absent:
			delete(children, name)
		case model.Directory:
			if !exists && child.value == nil && count == 0 {
				continue
			}
			children[name] = content.Entry{Entry: model.Entry{Path: name, Kind: model.Directory}, Object: object}
		case model.File:
			e := *child.value
			e.Path = name
			if exists && old.Entry == e {
				continue
			}
			if object, ok := source.reuse[full]; ok {
				children[name] = content.Entry{Entry: e, Object: object}
				continue
			}
			if source.Write == nil {
				return content.Object{}, 0, fmt.Errorf("no source for %q", full)
			}
			object, err = w.File(*child.value, func(out io.Writer) error {
				if old.Kind != model.File {
					return source.Write(*child.value, out, nil)
				}
				reader, err := s.content.Read(ctx, old.Object.ID)
				if err != nil {
					return err
				}
				defer reader.Close()
				previous := old.Entry
				previous.Path = full
				return source.Write(*child.value, out, &Basis{Entry: previous, Reader: reader})
			})
			if err != nil {
				return content.Object{}, 0, err
			}
			children[name] = content.Entry{Entry: e, Object: object}
		}
	}
	entries = entries[:0]
	for _, e := range children {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	object, err := w.Directory(entries)
	if err != nil {
		return content.Object{}, 0, err
	}
	d, err := indexDirectory(object.ID, entries)
	if err != nil {
		return content.Object{}, 0, err
	}
	*dirs = append(*dirs, d)
	return object, len(entries), nil
}
