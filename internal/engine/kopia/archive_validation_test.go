package kopia

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"

	"github.com/redthing1/lode/internal/model"
)

const (
	validationRootID  = "10000000000000000000000000000001"
	validationDirID   = "10000000000000000000000000000002"
	validationFileID  = "10000000000000000000000000000003"
	validationExtraID = "10000000000000000000000000000004"
)

func validationEntry(t *testing.T, name string, kind snapshot.EntryType, id string) *snapshot.DirEntry {
	t.Helper()
	objectID, err := object.ParseID(id)
	if err != nil {
		t.Fatal(err)
	}
	entry := &snapshot.DirEntry{Name: name, Type: kind, Permissions: 0o644, ObjectID: objectID}
	if kind == snapshot.EntryTypeDirectory {
		entry.Permissions = 0o755
		entry.DirSummary = &fs.DirectorySummary{TotalDirCount: 1}
	}
	return entry
}

func validationManifest(t *testing.T, entries ...*snapshot.DirEntry) []byte {
	t.Helper()
	data, err := json.Marshal(snapshot.DirManifest{StreamType: "kopia:directory", Entries: entries, Summary: &fs.DirectorySummary{TotalDirCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validationArchive(t *testing.T) Archive {
	t.Helper()
	rootID, err := object.ParseID(validationRootID)
	if err != nil {
		t.Fatal(err)
	}
	root := &snapshot.DirEntry{Type: snapshot.EntryTypeDirectory, Permissions: 0o750, ObjectID: rootID, DirSummary: &fs.DirectorySummary{TotalDirCount: 2}}
	rootBytes, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return Archive{
		Format: archiveFormat,
		Root:   rootBytes,
		Directories: map[string][]byte{
			validationRootID: validationManifest(t, validationEntry(t, "nested", snapshot.EntryTypeDirectory, validationDirID)),
			validationDirID:  validationManifest(t, validationEntry(t, "asset.bin", snapshot.EntryTypeFile, validationFileID)),
		},
	}
}

func validationPath(t *testing.T, value string) model.Path {
	t.Helper()
	path, err := model.ParsePath(value)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenArchiveRejectsIncompleteAndInvalidNativeClosure(t *testing.T) {
	t.Run("missing directory record", func(t *testing.T) {
		archive := validationArchive(t)
		delete(archive.Directories, validationDirID)
		if _, err := OpenArchive("repo", archive); !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("OpenArchive error = %v, want invalid archive", err)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		archive := validationArchive(t)
		archive.Directories[validationRootID] = validationManifest(t, validationEntry(t, "loop", snapshot.EntryTypeDirectory, validationRootID))
		delete(archive.Directories, validationDirID)
		if _, err := OpenArchive("repo", archive); !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("OpenArchive error = %v, want invalid archive", err)
		}
	})

	t.Run("unreachable directory record", func(t *testing.T) {
		archive := validationArchive(t)
		archive.Directories[validationExtraID] = validationManifest(t)
		if _, err := OpenArchive("repo", archive); !errors.Is(err, ErrInvalidArchive) {
			t.Fatalf("OpenArchive error = %v, want invalid archive", err)
		}
	})

	tests := []struct {
		name string
		edit func(*testing.T, *Archive)
	}{
		{name: "invalid native child name", edit: func(t *testing.T, archive *Archive) {
			archive.Directories[validationRootID] = validationManifest(t, validationEntry(t, "nested/asset.bin", snapshot.EntryTypeFile, validationFileID))
			delete(archive.Directories, validationDirID)
		}},
		{name: "unsupported native type", edit: func(t *testing.T, archive *Archive) {
			archive.Directories[validationDirID] = validationManifest(t, validationEntry(t, "asset.bin", snapshot.EntryTypeSymlink, validationFileID))
		}},
		{name: "incomplete nested summary", edit: func(t *testing.T, archive *Archive) {
			child := validationEntry(t, "nested", snapshot.EntryTypeDirectory, validationDirID)
			child.DirSummary.FatalErrorCount = 1
			archive.Directories[validationRootID] = validationManifest(t, child)
		}},
		{name: "incomplete directory manifest summary", edit: func(t *testing.T, archive *Archive) {
			archive.Directories[validationDirID] = mustJSON(t, snapshot.DirManifest{
				StreamType: "kopia:directory", Entries: []*snapshot.DirEntry{validationEntry(t, "asset.bin", snapshot.EntryTypeFile, validationFileID)},
				Summary: &fs.DirectorySummary{FatalErrorCount: 1},
			})
		}},
		{name: "unsupported archive format", edit: func(_ *testing.T, archive *Archive) { archive.Format = "kopia-future" }},
		{name: "trailing JSON data", edit: func(t *testing.T, archive *Archive) {
			archive.Directories[validationDirID] = append(archive.Directories[validationDirID], []byte(" {}")...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := validationArchive(t)
			test.edit(t, &archive)
			_, err := OpenArchive("repo", archive)
			if test.name == "unsupported archive format" {
				if !errors.Is(err, ErrArchiveVersion) {
					t.Fatalf("OpenArchive error = %v, want unsupported format", err)
				}
			} else if !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("OpenArchive error = %v, want invalid archive", err)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenArchiveScopesIdentityAndOwnsValidatedMetadata(t *testing.T) {
	archive := validationArchive(t)
	first, err := OpenArchive("repo-one", archive)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenArchive("repo-two", archive)
	if err != nil {
		t.Fatal(err)
	}
	path := validationPath(t, "nested/asset.bin")
	firstEntry, err := first.Lookup(path)
	if err != nil {
		t.Fatal(err)
	}
	secondEntry, err := second.Lookup(path)
	if err != nil {
		t.Fatal(err)
	}
	if firstEntry.Kind != model.File || secondEntry.Kind != model.File || firstEntry.ContentID == secondEntry.ContentID {
		t.Fatalf("repository-scoped identities = %+v and %+v", firstEntry, secondEntry)
	}

	archive.Root[0] ^= 0xff
	for _, raw := range archive.Directories {
		raw[0] ^= 0xff
	}
	root := first.RootDescriptor()
	if root == nil {
		t.Fatal("RootDescriptor returned nil")
	}
	root.Permissions = 0
	if first.RootDescriptor().Permissions != 0o750 {
		t.Fatal("caller mutation changed the archived root descriptor")
	}
	if got, err := first.Lookup(path); err != nil || got != firstEntry {
		t.Fatalf("caller mutation changed archived lookup: %+v, %v", got, err)
	}
}

func TestLoadArchiveCancellationAndReaderErrors(t *testing.T) {
	archive := validationArchive(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if _, err := LoadArchive(canceled, archive.Format, archive.Root, func(context.Context, string) ([]byte, error) {
		called = true
		return nil, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled LoadArchive error = %v", err)
	}
	if called {
		t.Fatal("LoadArchive called the directory reader after cancellation")
	}

	wantErr := errors.New("database read failed")
	_, err := LoadArchive(context.Background(), archive.Format, archive.Root, func(context.Context, string) ([]byte, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("LoadArchive error = %v, want wrapped reader failure", err)
	}
}
