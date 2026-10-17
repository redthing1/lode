package kopia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"

	"github.com/redthing1/lode/internal/model"
)

var (
	ErrInvalidChange    = errors.New("invalid tree change")
	ErrOutsideSelection = errors.New("change outside selection")
	ErrStructure        = errors.New("conflicting tree structure")
)

// Change replaces one entry after checking its stored Before identity. Absence
// removes a file, or removes a directory only if its resulting contents are empty.
// A directory After ensures presence; its children come only from exact changes.
type Change struct {
	Path          model.Path
	Before, After Entry
}

type edit struct {
	change   *Change
	children map[string]*edit
}

// Apply constructs a complete tree while reusing untouched objects. An absent
// base starts an empty project. No existing root is modified or released.
func Apply(ctx context.Context, w repo.RepositoryWriter, base Entry, selection model.Selection, changes []Change) (Entry, error) {
	if base.State().Kind != model.Absent && base.State().Kind != model.Directory {
		return Entry{}, fmt.Errorf("%w: project root must be a directory", ErrStructure)
	}
	plan := &edit{}
	for i := range changes {
		change := &changes[i]
		if change.Path == (model.Path{}) {
			return Entry{}, fmt.Errorf("%w: root replacement is not an entry change", ErrInvalidChange)
		}
		before, after := change.Before.State(), change.After.State()
		if err := before.Validate(); err != nil {
			return Entry{}, fmt.Errorf("%s before: %w", change.Path, err)
		}
		if err := after.Validate(); err != nil {
			return Entry{}, fmt.Errorf("%s after: %w", change.Path, err)
		}
		structuralDirectory := before.Kind != model.File && after.Kind != model.File
		if !selection.Includes(change.Path) && !(structuralDirectory && selection.Intersects(change.Path)) {
			return Entry{}, fmt.Errorf("%w: %s", ErrOutsideSelection, change.Path)
		}
		node := plan
		for _, name := range strings.Split(change.Path.String(), "/") {
			if node.children == nil {
				node.children = make(map[string]*edit)
			}
			if node.children[name] == nil {
				node.children[name] = &edit{}
			}
			node = node.children[name]
		}
		if node.change != nil {
			return Entry{}, fmt.Errorf("%w: duplicate path %s", ErrInvalidChange, change.Path)
		}
		node.change = change
	}
	root, err := applyEdit(ctx, w, model.Path{}, base, plan)
	if err != nil {
		return Entry{}, err
	}
	if root.data == nil {
		return writeDirectory(ctx, w, model.Path{}, Directory(), nil)
	}
	return root, nil
}

func applyEdit(ctx context.Context, w repo.RepositoryWriter, p model.Path, original Entry, node *edit) (Entry, error) {
	target := original
	if node.change != nil {
		if !sameStoredEntry(original, node.change.Before) {
			return Entry{}, fmt.Errorf("%w: %s precondition changed", model.ErrConflict, p)
		}
		target = node.change.After
	}
	if original.State().Kind == model.File && len(node.children) > 0 && target.State().Kind != model.Directory {
		return Entry{}, fmt.Errorf("%w: %s is a file ancestor", ErrStructure, p)
	}
	children := make(map[string]Entry)
	if original.State().Kind == model.Directory {
		var err error
		children, err = readDirectory(ctx, w, original)
		if err != nil {
			return Entry{}, fmt.Errorf("read %s: %w", p, err)
		}
	}
	changed := false
	for _, name := range sortedNames(node.children) {
		childPath, err := model.ParsePath(strings.TrimPrefix(p.String()+"/"+name, "./"))
		if err != nil {
			return Entry{}, err
		}
		before := children[name]
		after, err := applyEdit(ctx, w, childPath, before, node.children[name])
		if err != nil {
			return Entry{}, err
		}
		changed = changed || !sameStoredEntry(before, after)
		if after.data == nil {
			delete(children, name)
		} else {
			children[name] = after
		}
	}
	switch target.State().Kind {
	case model.File:
		if len(children) != 0 {
			return Entry{}, fmt.Errorf("%w: %s still has descendants", ErrStructure, p)
		}
		if _, err := w.VerifyObject(ctx, target.data.ObjectID); err != nil {
			return Entry{}, fmt.Errorf("verify %s: %w", p, err)
		}
		return target, nil
	case model.Absent:
		if len(children) == 0 {
			return Entry{}, nil
		}
		if node.change != nil && addsDescendant(node) {
			return Entry{}, fmt.Errorf("%w: %s removal includes new descendants", ErrInvalidChange, p)
		}
	}
	if original.State().Kind == model.Directory && !changed {
		return original, nil
	}
	return writeDirectory(ctx, w, p, original, children)
}

func addsDescendant(node *edit) bool {
	for _, child := range node.children {
		if child.change != nil && child.change.After.data != nil || addsDescendant(child) {
			return true
		}
	}
	return false
}

func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func readDirectory(ctx context.Context, r repo.Repository, entry Entry) (map[string]Entry, error) {
	if entry.State().Kind != model.Directory || entry.data.ObjectID.String() == "" {
		return nil, fmt.Errorf("%w: expected a stored directory", model.ErrInvalidEntry)
	}
	reader, err := r.OpenObject(ctx, entry.data.ObjectID)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var directory snapshot.DirManifest
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&directory); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("directory has trailing data")
	}
	if directory.StreamType != "kopia:directory" || directory.Summary == nil || directory.Summary.IncompleteReason != "" || directory.Summary.FatalErrorCount != 0 || directory.Summary.IgnoredErrorCount != 0 {
		return nil, errors.New("invalid or incomplete directory")
	}
	entries := make(map[string]Entry, len(directory.Entries))
	for _, child := range directory.Entries {
		if child == nil {
			return nil, errors.New("nil directory entry")
		}
		p, err := model.ParsePath(child.Name)
		if err != nil || p == (model.Path{}) || p.Parent() != (model.Path{}) {
			return nil, fmt.Errorf("invalid directory child name %q", child.Name)
		}
		if _, duplicate := entries[child.Name]; duplicate {
			return nil, fmt.Errorf("duplicate directory child %q", child.Name)
		}
		e := Entry{data: child}
		if err := e.State().Validate(); err != nil || child.ObjectID.String() == "" || child.FileSize < 0 {
			return nil, fmt.Errorf("invalid stored entry %q", child.Name)
		}
		if child.Type == snapshot.EntryTypeDirectory && (child.DirSummary == nil || child.DirSummary.IncompleteReason != "" || child.DirSummary.FatalErrorCount != 0 || child.DirSummary.IgnoredErrorCount != 0) {
			return nil, fmt.Errorf("incomplete directory entry %q", child.Name)
		}
		entries[child.Name] = e
	}
	return entries, nil
}

func writeDirectory(ctx context.Context, w repo.RepositoryWriter, p model.Path, original Entry, children map[string]Entry) (Entry, error) {
	var builder snapshotfs.DirManifestBuilder
	for _, name := range sortedNames(children) {
		child := children[name].data.Clone()
		child.Name = name
		builder.AddEntry(child)
	}
	entry := Directory().data
	if original.State().Kind == model.Directory {
		entry = original.data.Clone()
	}
	directory := builder.Build(entry.ModTime, "")
	id, err := snapshotfs.WriteDirManifest(ctx, w, p.String(), directory, metadataCompression)
	if err != nil {
		return Entry{}, err
	}
	entry.Name, entry.ObjectID, entry.DirSummary = p.String(), id, directory.Summary
	return Entry{data: entry}, nil
}

// Lookup reads only directory metadata. A missing path returns explicit absence;
// an unreadable directory returns an error.
func Lookup(ctx context.Context, r repo.Repository, root Entry, p model.Path) (Entry, error) {
	current := root
	if p == (model.Path{}) {
		return current, nil
	}
	for _, name := range strings.Split(p.String(), "/") {
		if current.data == nil {
			return Entry{}, nil
		}
		children, err := readDirectory(ctx, r, current)
		if err != nil {
			return Entry{}, err
		}
		current = children[name]
	}
	return current, nil
}
