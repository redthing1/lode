package content

import (
	"context"
	"fmt"
	"sort"

	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/redthing1/lode/internal/model"
)

// Directory reads the native entries used by snapshot GC. File hashes belong
// to Lode's logical metadata and are verified separately against actual bytes.
func (s *Store) Directory(ctx context.Context, ref string) ([]Entry, error) {
	id, err := object.ParseID(ref)
	if err != nil {
		return nil, err
	}
	dir := snapshotfs.DirectoryEntry(s.repository, id, nil)
	defer dir.Close()
	iter, err := dir.Iterate(ctx)
	if err != nil {
		return nil, storageError(err)
	}
	defer iter.Close()
	var result []Entry
	for {
		child, err := iter.Next(ctx)
		if err != nil {
			return nil, storageError(err)
		}
		if child == nil {
			break
		}
		entry, ok := child.(snapshot.HasDirEntry)
		if !ok {
			return nil, fmt.Errorf("native entry %q has no metadata", child.Name())
		}
		d := entry.DirEntry()
		e := Entry{Entry: model.Entry{Path: d.Name}, Object: Object{ID: d.ObjectID.String()}}
		switch d.Type {
		case snapshot.EntryTypeFile:
			e.Kind, e.Size = model.File, d.FileSize
		case snapshot.EntryTypeDirectory:
			if d.DirSummary == nil {
				return nil, fmt.Errorf("native directory %q has no summary", d.Name)
			}
			e.Kind = model.Directory
			e.Object.Bytes = d.DirSummary.TotalFileSize
			e.Object.Files = d.DirSummary.TotalFileCount
			e.Object.Directories = d.DirSummary.TotalDirCount
		default:
			return nil, fmt.Errorf("unsupported native entry %q", d.Name)
		}
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
