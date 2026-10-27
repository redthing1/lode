package kopia

import (
	"context"
	"fmt"
	"sort"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/compression"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"

	"github.com/redthing1/lode/internal/model"
)

// Entries returns a detached complete tree from native archived metadata. It
// does not open file objects.
func (o *Objects) Entries(archive Archive) (map[model.Path]model.Entry, error) {
	if o == nil || o.repo == nil {
		return nil, fmt.Errorf("nil Kopia objects adapter")
	}
	_, entries, _, err := o.archiveEntries(archive)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Compose writes a tree made from metadata in either source archive. Every
// target file must retain its fact at the same path in at least one source;
// arbitrary native refs cannot be injected through entries. File payloads are
// never opened. The primary archive supplies root and retained directory
// attributes, and its unchanged directory manifests are reused.
func (o *Objects) Compose(ctx context.Context, primary, alternate Archive, entries map[model.Path]model.Entry) (Archive, error) {
	if o == nil || o.repo == nil {
		return Archive{}, fmt.Errorf("nil Kopia objects adapter")
	}
	if err := ctx.Err(); err != nil {
		return Archive{}, err
	}
	primaryRoot, primaryEntries, primaryNative, err := o.archiveEntries(primary)
	if err != nil {
		return Archive{}, fmt.Errorf("open primary archive: %w", err)
	}
	_, alternateEntries, alternateNative, err := o.archiveEntries(alternate)
	if err != nil {
		return Archive{}, fmt.Errorf("open alternate archive: %w", err)
	}
	target, err := model.ApplyChanges(entries, nil)
	if err != nil {
		return Archive{}, fmt.Errorf("invalid composed tree: %w", err)
	}
	available := make(map[model.Path]*snapshot.DirEntry)
	for path, entry := range target {
		if entry.Kind != model.File {
			continue
		}
		if primaryEntries[path] == entry && primaryNative[path] != nil && primaryNative[path].Type == snapshot.EntryTypeFile {
			available[path] = primaryNative[path]
			continue
		}
		if alternateEntries[path] == entry && alternateNative[path] != nil && alternateNative[path].Type == snapshot.EntryTypeFile {
			available[path] = alternateNative[path]
			continue
		}
		return Archive{}, fmt.Errorf("composed file %q has no matching source object", path)
	}

	var root *snapshot.DirEntry
	err = repo.DirectWriteSession(ctx, o.repo, repo.WriteSessionOptions{Purpose: "lode-tree-compose"}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		var buildErr error
		root, buildErr = writeDirectoryTree(ctx, w, primaryRoot, primaryEntries, primaryNative, target, available)
		return buildErr
	})
	if err != nil {
		return Archive{}, err
	}
	if err = o.repo.Refresh(ctx); err != nil {
		return Archive{}, fmt.Errorf("refresh after tree composition: %w", err)
	}
	return ExportTree(ctx, o.repo, root)
}

func (o *Objects) archiveEntries(archive Archive) (*snapshot.DirEntry, map[model.Path]model.Entry, map[model.Path]*snapshot.DirEntry, error) {
	tree, err := OpenArchive(o.namespace, archive)
	if err != nil {
		return nil, nil, nil, err
	}
	root := tree.RootDescriptor()
	entries := map[model.Path]model.Entry{{}: {Kind: model.Directory}}
	native := make(map[model.Path]*snapshot.DirEntry)
	if err = flattenArchive(o.namespace, archive, root, entries, native); err != nil {
		return nil, nil, nil, err
	}
	validated, err := model.ApplyChanges(entries, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return root, validated, native, nil
}

// writeDirectoryTree is shared by capture and metadata-only composition.
// availableNative contains source descriptors for files not reused from base.
func writeDirectoryTree(ctx context.Context, w repo.DirectRepositoryWriter, baseRoot *snapshot.DirEntry, baseEntries map[model.Path]model.Entry, baseNative map[model.Path]*snapshot.DirEntry, targetEntries map[model.Path]model.Entry, availableNative map[model.Path]*snapshot.DirEntry) (*snapshot.DirEntry, error) {
	target, err := model.ApplyChanges(targetEntries, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid directory target: %w", err)
	}
	affected := make(map[model.Path]bool)
	markAncestors := func(path model.Path) {
		for parent := path; ; parent = parent.Parent() {
			affected[parent] = true
			if parent.IsRoot() {
				break
			}
		}
	}
	for path, entry := range target {
		old, exists := baseEntries[path]
		if !exists || old != entry {
			markAncestors(path)
		}
	}
	for path := range baseEntries {
		if _, exists := target[path]; !exists {
			markAncestors(path.Parent())
		}
	}
	if baseRoot == nil {
		affected[model.Path{}] = true
	}
	childrenByParent := make(map[model.Path][]model.Path, len(target))
	for path := range target {
		if !path.IsRoot() {
			parent := path.Parent()
			childrenByParent[parent] = append(childrenByParent[parent], path)
		}
	}
	for parent := range childrenByParent {
		sortPaths(childrenByParent[parent])
	}
	var buildDirectory func(model.Path, *snapshot.DirEntry) (*snapshot.DirEntry, error)
	buildDirectory = func(path model.Path, previous *snapshot.DirEntry) (*snapshot.DirEntry, error) {
		if !affected[path] && previous != nil {
			return previous.Clone(), nil
		}
		builder := &snapshotfs.DirManifestBuilder{}
		for _, childPath := range childrenByParent[path] {
			entry := target[childPath]
			previousEntry := baseNative[childPath]
			var native *snapshot.DirEntry
			if entry.Kind == model.Directory {
				var oldDirectory *snapshot.DirEntry
				if previousEntry != nil && previousEntry.Type == snapshot.EntryTypeDirectory {
					oldDirectory = previousEntry.Clone()
				}
				native, err = buildDirectory(childPath, oldDirectory)
				if err != nil {
					return nil, err
				}
			} else if previousEntry != nil && baseEntries[childPath] == entry {
				native = previousEntry.Clone()
			} else {
				source := availableNative[childPath]
				if source == nil || source.Type != snapshot.EntryTypeFile || source.ObjectID.String() == "" || source.FileSize < 0 {
					return nil, fmt.Errorf("missing matching native file descriptor at %q", childPath)
				}
				native = source.Clone()
			}
			native.Name = childPath.String()
			if !path.IsRoot() {
				native.Name = childPath.String()[len(path.String())+1:]
			}
			builder.AddEntry(native)
		}
		modTime := deterministicModTime()
		permissions := snapshot.Permissions(0755)
		if previous != nil {
			modTime, permissions = previous.ModTime, previous.Permissions
		}
		manifest := builder.Build(modTime, "")
		id, err := snapshotfs.WriteDirManifest(ctx, w, path.String(), manifest, compression.Name("zstd-fastest"))
		if err != nil {
			return nil, fmt.Errorf("write directory %q: %w", path, err)
		}
		result := &snapshot.DirEntry{Type: snapshot.EntryTypeDirectory, Permissions: permissions, ModTime: modTime, ObjectID: id, DirSummary: manifest.Summary}
		if previous != nil {
			result = previous.Clone()
			result.Type = snapshot.EntryTypeDirectory
			result.Permissions = permissions
			result.ModTime = modTime
			result.ObjectID = id
			result.DirSummary = manifest.Summary
		}
		return result, nil
	}
	var initial *snapshot.DirEntry
	if baseRoot != nil {
		initial = baseRoot.Clone()
	}
	return buildDirectory(model.Path{}, initial)
}

func sortPaths(paths []model.Path) {
	sort.Slice(paths, func(i, j int) bool { return paths[i].String() < paths[j].String() })
}
