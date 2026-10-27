package kopia_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestListArchivedPagesMatchFullTreeLexicalOrder(t *testing.T) {
	f := newArchiveFixture(t)
	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		file := func(name, data string) (*snapshot.DirEntry, error) {
			return captureProofFile(ctx, writer, name, []byte(data))
		}
		nestedFile, err := file("x", "nested x")
		if err != nil {
			return err
		}
		nested, err := writeProofDirectory(ctx, writer, "a", nestedFile)
		if err != nil {
			return err
		}
		deepFile, err := file("leaf", "deep leaf")
		if err != nil {
			return err
		}
		deep, err := writeProofDirectory(ctx, writer, "sub", deepFile)
		if err != nil {
			return err
		}
		deepParent, err := writeProofDirectory(ctx, writer, "deep", deep)
		if err != nil {
			return err
		}
		zFile, err := file("z", "nested z")
		if err != nil {
			return err
		}
		zDir, err := writeProofDirectory(ctx, writer, "z", zFile)
		if err != nil {
			return err
		}
		aHyphen, err := file("a-z", "hyphen sibling")
		if err != nil {
			return err
		}
		aZero, err := file("a0", "zero sibling")
		if err != nil {
			return err
		}
		unicodeFile, err := file("λ-é", "unicode")
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", nested, aHyphen, aZero, deepParent, unicodeFile, zDir)
		return err
	})
	archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	selection := model.Selection{}.Include(model.Path{})
	want, _, err := objects.List(archive, selection, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var got []lodekopia.Node
	after := ""
	for page := 0; page < 100; page++ {
		entries, next, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
			data, ok := archive.Directories[id]
			if !ok {
				return nil, errors.New("unexpected directory lookup: " + id)
			}
			return data, nil
		}, selection, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, entries...)
		if next == "" {
			break
		}
		after = next
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged archived entries differ from full list\n got: %#v\nwant: %#v", got, want)
	}
	paths := make([]string, len(got))
	for i, entry := range got {
		paths[i] = entry.Path.String()
	}
	index := func(path string) int {
		for i, candidate := range paths {
			if candidate == path {
				return i
			}
		}
		return -1
	}
	if index("a-z") < 0 || index("a/x") < 0 || index("a-z") >= index("a/x") {
		t.Fatalf("lexical ordering around hyphen and slash is wrong: %v", paths)
	}
	afterDirectory, _, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		return archive.Directories[id], nil
	}, selection, "a", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range afterDirectory {
		if entry.Path.String() == "a" {
			t.Fatal("cursor directory was returned again")
		}
	}
	if index := func() int {
		for i, entry := range afterDirectory {
			if entry.Path.String() == "a/x" {
				return i
			}
		}
		return -1
	}(); index < 0 {
		t.Fatalf("cursor at directory skipped its descendants: %+v", afterDirectory)
	}
}

func TestListArchivedPrunesUnselectedAndPreCursorSubtrees(t *testing.T) {
	f := newArchiveFixture(t)
	var root, skipped, selected *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		makeDir := func(name string) (*snapshot.DirEntry, error) {
			entry, err := captureProofFile(ctx, writer, "inside", []byte(name))
			if err != nil {
				return nil, err
			}
			return writeProofDirectory(ctx, writer, name, entry)
		}
		var err error
		skipped, err = makeDir("a")
		if err != nil {
			return err
		}
		selected, err = makeDir("selected")
		if err != nil {
			return err
		}
		other, err := makeDir("other")
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", skipped, other, selected)
		return err
	})
	archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	selectedPath := testArchivePath(t, "selected/inside")
	selection := model.Selection{}.Include(selectedPath)
	reads := map[string]int{}
	entries, _, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		reads[id]++
		return archive.Directories[id], nil
	}, selection, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path.String() != "selected" || entries[1].Path.String() != "selected/inside" {
		t.Fatalf("selected page entries = %+v", entries)
	}
	if reads[skipped.ObjectID.String()] != 0 {
		t.Fatalf("unselected subtree %q was read: %v", skipped.ObjectID, reads)
	}

	reads = map[string]int{}
	all := model.Selection{}.Include(model.Path{})
	if _, _, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		reads[id]++
		return archive.Directories[id], nil
	}, all, "m", 20); err != nil {
		t.Fatal(err)
	}
	if reads[skipped.ObjectID.String()] != 0 {
		t.Fatalf("pre-cursor subtree %q was read: %v", skipped.ObjectID, reads)
	}
}

func TestListArchivedDefersUnvisitedCorruptionAndRejectsVisitedCorruption(t *testing.T) {
	f := newArchiveFixture(t)
	var root, hidden *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		makeDir := func(name string) (*snapshot.DirEntry, error) {
			file, err := captureProofFile(ctx, writer, "inside", []byte(name))
			if err != nil {
				return nil, err
			}
			return writeProofDirectory(ctx, writer, name, file)
		}
		var err error
		hidden, err = makeDir("hidden")
		if err != nil {
			return err
		}
		visible, err := makeDir("visible")
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", hidden, visible)
		return err
	})
	archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatal(err)
	}
	archive.Directories[hidden.ObjectID.String()] = []byte("invalid hidden directory")
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	selection := model.Selection{}.Include(testArchivePath(t, "visible"))
	reads := map[string]int{}
	if _, _, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		reads[id]++
		return archive.Directories[id], nil
	}, selection, "", 20); err != nil {
		t.Fatalf("page of visible subtree read unvisited corrupt data: %v", err)
	}
	if reads[hidden.ObjectID.String()] != 0 {
		t.Fatalf("corrupt unvisited record was read: %v", reads)
	}

	all := model.Selection{}.Include(model.Path{})
	if _, _, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		return archive.Directories[id], nil
	}, all, "", 20); !errors.Is(err, lodekopia.ErrInvalidArchive) || !strings.Contains(err.Error(), hidden.ObjectID.String()) {
		t.Fatalf("visited corrupt record error = %v, want invalid archive for %s", err, hidden.ObjectID)
	}
}

func TestListArchivedDetectsVisitedCyclesAndHonorsCancellation(t *testing.T) {
	f := newArchiveFixture(t)
	var root, child *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		file, err := captureProofFile(ctx, writer, "leaf", []byte("data"))
		if err != nil {
			return err
		}
		child, err = writeProofDirectory(ctx, writer, "child", file)
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", child)
		return err
	})
	archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	var rootDescriptor snapshot.DirEntry
	if err := json.Unmarshal(archive.Root, &rootDescriptor); err != nil {
		t.Fatal(err)
	}
	var rootManifest snapshot.DirManifest
	if err := json.Unmarshal(archive.Directories[rootDescriptor.ObjectID.String()], &rootManifest); err != nil {
		t.Fatal(err)
	}
	rootManifest.Entries[0].ObjectID = rootDescriptor.ObjectID
	archive.Directories[rootDescriptor.ObjectID.String()], err = json.Marshal(rootManifest)
	if err != nil {
		t.Fatal(err)
	}
	all := model.Selection{}.Include(model.Path{})
	if _, _, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		return archive.Directories[id], nil
	}, all, "", 20); !errors.Is(err, lodekopia.ErrInvalidArchive) || !strings.Contains(err.Error(), "directory cycle") {
		t.Fatalf("visited cycle error = %v, want invalid archive cycle", err)
	}

	canceled, cancel := context.WithCancel(f.ctx)
	defer cancel()
	reads := 0
	_, _, err = objects.ListArchived(canceled, archive.Format, archive.Root, func(ctx context.Context, id string) ([]byte, error) {
		reads++
		cancel()
		return archive.Directories[id], nil
	}, all, "", 20)
	if !errors.Is(err, context.Canceled) || reads != 1 {
		t.Fatalf("canceled page = %v after %d record reads; want context.Canceled after root", err, reads)
	}
}

func TestListArchivedCostEvidence(t *testing.T) {
	f := newArchiveFixture(t)
	build := func(unrelatedFiles int) lodekopia.Archive {
		t.Helper()
		var root *snapshot.DirEntry
		f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
			selectedFile, err := captureProofFile(ctx, writer, "keep.bin", []byte("selected payload"))
			if err != nil {
				return err
			}
			deep, err := writeProofDirectory(ctx, writer, "deep", selectedFile)
			if err != nil {
				return err
			}
			selected, err := writeProofDirectory(ctx, writer, "selected", deep)
			if err != nil {
				return err
			}
			wideEntries := make([]*snapshot.DirEntry, 0, unrelatedFiles)
			for i := 0; i < unrelatedFiles; i++ {
				entry, err := captureProofFile(ctx, writer, fmt.Sprintf("file-%04d.bin", i), []byte("shared payload"))
				if err != nil {
					return err
				}
				wideEntries = append(wideEntries, entry)
			}
			unrelated, err := writeProofDirectory(ctx, writer, "unrelated", wideEntries...)
			if err != nil {
				return err
			}
			root, err = writeProofDirectory(ctx, writer, "", selected, unrelated)
			return err
		})
		archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
		if err != nil {
			t.Fatal(err)
		}
		return archive
	}

	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	selection := model.Selection{}.Include(testArchivePath(t, "selected/deep/keep.bin"))
	measurePage := func(archive lodekopia.Archive, pageSelection model.Selection, after string, limit int) (records, bytesRead int, elapsed time.Duration, allocated uint64, nodes int, next string) {
		var before, afterStats runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		started := time.Now()
		entries, next, err := objects.ListArchived(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
			data, ok := archive.Directories[id]
			if !ok {
				return nil, fmt.Errorf("missing test directory %q", id)
			}
			records++
			bytesRead += len(data)
			return data, nil
		}, pageSelection, after, limit)
		elapsed = time.Since(started)
		runtime.ReadMemStats(&afterStats)
		if err != nil {
			t.Fatal(err)
		}
		if afterStats.TotalAlloc >= before.TotalAlloc {
			allocated = afterStats.TotalAlloc - before.TotalAlloc
		}
		return records, bytesRead, elapsed, allocated, len(entries), next
	}

	for _, unrelatedFiles := range []int{1, 1024} {
		archive := build(unrelatedFiles)
		records, bytesRead, elapsed, allocated, nodes, _ := measurePage(archive, selection, "", 1000)
		if records != 3 || nodes != 3 {
			t.Fatalf("selected page with %d unrelated files read %d records and returned %d nodes; want selected path only", unrelatedFiles, records, nodes)
		}
		t.Logf("selected nested page, unrelated_files=%d: records_read=%d, raw_directory_bytes=%d, returned_nodes=%d, elapsed=%s, allocated=%d bytes", unrelatedFiles, records, bytesRead, nodes, elapsed, allocated)
	}

	archive := build(768)
	closureBytes := 0
	for _, raw := range archive.Directories {
		closureBytes += len(raw)
	}
	all := model.Selection{}.Include(model.Path{})
	singleRecords, singleBytes, singleElapsed, singleAllocated, singleNodes, next := measurePage(archive, all, "", 1000)
	if next != "" {
		t.Fatalf("single page unexpectedly had next cursor %q", next)
	}
	var pageRecords, pageBytes, pages, listed int
	var pageElapsed time.Duration
	var pageAllocated uint64
	after := ""
	for pages = 0; pages < 100; pages++ {
		records, bytesRead, elapsed, allocated, nodes, next := measurePage(archive, all, after, 64)
		pageRecords += records
		pageBytes += bytesRead
		pageElapsed += elapsed
		pageAllocated += allocated
		listed += nodes
		if next == "" {
			break
		}
		after = next
	}
	pages++
	if listed != singleNodes || pageRecords <= len(archive.Directories) || pageBytes < closureBytes {
		t.Fatalf("paged traversal records=%d bytes=%d entries=%d; closure records=%d bytes=%d entries=%d", pageRecords, pageBytes, listed, len(archive.Directories), closureBytes, singleNodes)
	}
	t.Logf("full tree, one page: records_read=%d, raw_directory_bytes=%d, closure_bytes=%d, returned_nodes=%d, elapsed=%s, allocated=%d bytes", singleRecords, singleBytes, closureBytes, singleNodes, singleElapsed, singleAllocated)
	t.Logf("full tree, pages=%d x 64: records_read=%d, raw_directory_bytes=%d, closure_bytes=%d, returned_nodes=%d, elapsed_sum=%s, allocated_sum=%d bytes", pages, pageRecords, pageBytes, closureBytes, listed, pageElapsed, pageAllocated)
}

func testArchivePath(t *testing.T, value string) model.Path {
	t.Helper()
	path, err := model.ParsePath(value)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
