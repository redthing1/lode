package kopia_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/project"
)

func TestArchiveCostSharingAndWideDirectoryRecords(t *testing.T) {
	const versionCount = 4
	const wideFileCount = 1024

	f := newArchiveFixture(t)
	payloadA := archivePayload(128, 901)
	payloadB := archivePayload(128, 902)
	var roots []*snapshot.DirEntry
	var wideDirs []*snapshot.DirEntry
	var stableDirectory *snapshot.DirEntry
	var baseEntries []*snapshot.DirEntry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		fileA, err := captureProofFile(ctx, w, "payload-a.bin", payloadA)
		if err != nil {
			return err
		}
		fileB, err := captureProofFile(ctx, w, "payload-b.bin", payloadB)
		if err != nil {
			return err
		}
		if fileA.ObjectID == fileB.ObjectID {
			return fmt.Errorf("deterministic fixture payloads unexpectedly share an object ID")
		}
		stableFile := fileA.Clone()
		stableFile.Name = "kept.bin"
		stableDirectory, err = writeProofDirectory(ctx, w, "stable", stableFile)
		if err != nil {
			return err
		}

		baseEntries = make([]*snapshot.DirEntry, wideFileCount)
		for i := range baseEntries {
			entry := fileA.Clone()
			entry.Name = fmt.Sprintf("file-%04d.bin", i)
			baseEntries[i] = entry
		}
		for version := range versionCount {
			entries := make([]*snapshot.DirEntry, wideFileCount)
			for i, base := range baseEntries {
				entries[i] = base.Clone()
				if i < version {
					entries[i].ObjectID = fileB.ObjectID
					entries[i].FileSize = fileB.FileSize
					entries[i].ModTime = fileB.ModTime
				}
			}
			wide, err := writeProofDirectory(ctx, w, "wide", entries...)
			if err != nil {
				return err
			}
			wideDirs = append(wideDirs, wide)
			root, err := writeProofDirectory(ctx, w, "", stableDirectory, wide)
			if err != nil {
				return err
			}
			roots = append(roots, root)
			if _, err := f.saveRoot(w, root, fmt.Sprintf("cost-v%d", version)); err != nil {
				return err
			}
		}
		return nil
	})

	namespace := "archive-cost-fixture"
	archives := make([]lodekopia.Archive, versionCount)
	trees := make([]*lodekopia.ArchivedTree, versionCount)
	archiveIDs := make([]string, versionCount)
	databasePath := filepath.Join(f.dir, "archive-cost.sqlite")
	metadata, err := project.OpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var closureBytes int64
	uniqueDirectories := map[string][]byte{}
	uniqueRoots := map[string][]byte{}
	for version, root := range roots {
		archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
		if err != nil {
			t.Fatalf("export version %d: %v", version, err)
		}
		archives[version] = archive
		tree, err := lodekopia.OpenArchive(namespace, archive)
		if err != nil {
			t.Fatalf("validate version %d: %v", version, err)
		}
		trees[version] = tree

		archiveID, err := metadata.Import(f.ctx, namespace, archive.Format, archive.Root, archive.Directories)
		if err != nil {
			t.Fatalf("import version %d: %v", version, err)
		}
		archiveIDs[version] = archiveID
		closureBytes += int64(len(archive.Root))
		if prior, ok := uniqueRoots[archiveID]; ok {
			if !bytes.Equal(prior, archive.Root) {
				t.Fatalf("root archive ID %q maps to different descriptors", archiveID)
			}
		} else {
			uniqueRoots[archiveID] = append([]byte(nil), archive.Root...)
		}
		for id, raw := range archive.Directories {
			closureBytes += int64(len(raw))
			if prior, ok := uniqueDirectories[id]; ok {
				if !bytes.Equal(prior, raw) {
					t.Fatalf("native directory ID %q maps to different raw records", id)
				}
			} else {
				uniqueDirectories[id] = append([]byte(nil), raw...)
			}
		}
	}

	stableID := stableDirectory.ObjectID.String()
	stableRaw := archives[0].Directories[stableID]
	if len(stableRaw) == 0 {
		t.Fatalf("version 0 archive lacks stable directory %q", stableID)
	}
	seenWideIDs := make(map[string]bool, versionCount)
	for version, archive := range archives {
		if got := archive.Directories[stableID]; !bytes.Equal(got, stableRaw) {
			t.Fatalf("stable directory bytes changed in version %d", version)
		}
		gotID := wideDirs[version].ObjectID.String()
		if seenWideIDs[gotID] {
			t.Fatalf("wide directory object ID %q was reused at version %d", gotID, version)
		}
		seenWideIDs[gotID] = true
		if raw := archive.Directories[wideDirs[version].ObjectID.String()]; len(raw) == 0 {
			t.Fatalf("version %d archive lacks its wide directory record", version)
		}
	}

	uniqueBytes := int64(0)
	for _, raw := range uniqueDirectories {
		uniqueBytes += int64(len(raw))
	}
	for _, raw := range uniqueRoots {
		uniqueBytes += int64(len(raw))
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d versions, %d wide entries: raw metadata sum of closures=%d bytes, unique archived raw metadata=%d bytes (%d directories, %d roots), SQLite file=%d bytes",
		versionCount, wideFileCount, closureBytes, uniqueBytes, len(uniqueDirectories), len(uniqueRoots), stat.Size())

	metadata, err = project.OpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	paths := []string{"stable/kept.bin", "wide/file-0000.bin", "wide/file-0001.bin", "wide/file-0002.bin", "wide/file-0003.bin", "wide/file-1023.bin"}
	for _, version := range []int{0, versionCount - 1} {
		archive := archives[version]
		root, err := metadata.Root(f.ctx, namespace, archive.Format, archiveIDs[version])
		if err != nil {
			t.Fatalf("reload root for version %d: %v", version, err)
		}
		reloaded, err := lodekopia.LoadArchive(f.ctx, archive.Format, root, func(ctx context.Context, objectID string) ([]byte, error) {
			return metadata.Directory(ctx, namespace, archive.Format, objectID)
		})
		if err != nil {
			t.Fatalf("reload closure for version %d: %v", version, err)
		}
		reloadedTree, err := lodekopia.OpenArchive(namespace, reloaded)
		if err != nil {
			t.Fatalf("open reloaded version %d: %v", version, err)
		}
		for _, path := range paths {
			p := proofPath(t, path)
			want, err := trees[version].Lookup(p)
			if err != nil {
				t.Fatalf("lookup original %s in version %d: %v", path, version, err)
			}
			got, err := reloadedTree.Lookup(p)
			if err != nil {
				t.Fatalf("lookup reloaded %s in version %d: %v", path, version, err)
			}
			if got != want {
				t.Errorf("version %d path %s identity = %+v, want %+v", version, path, got, want)
			}
		}
	}
}
