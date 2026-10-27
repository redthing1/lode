package kopia_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestOpenArchivedFileReadsOnlyAncestorDirectories(t *testing.T) {
	f := newArchiveFixture(t)
	fileData := []byte("the selected file bytes")
	var root, target, nested *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		selected, err := captureProofFile(ctx, writer, "file.bin", fileData)
		if err != nil {
			return err
		}
		nested, err = writeProofDirectory(ctx, writer, "nested", selected)
		if err != nil {
			return err
		}
		target, err = writeProofDirectory(ctx, writer, "target", nested)
		if err != nil {
			return err
		}
		unrelatedFile, err := captureProofFile(ctx, writer, "unrelated.bin", []byte("unrelated"))
		if err != nil {
			return err
		}
		unrelated, err := writeProofDirectory(ctx, writer, "unrelated", unrelatedFile)
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", target, unrelated)
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
	reads := map[string]int{}
	reader, node, err := objects.OpenArchivedFile(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		reads[id]++
		return archive.Directories[id], nil
	}, archivePath(t, "target/nested/file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close file: %v / %v", readErr, closeErr)
	}
	if string(got) != string(fileData) || node.Path.String() != "target/nested/file.bin" || node.Size != int64(len(fileData)) {
		t.Fatalf("opened node=%+v bytes=%q", node, got)
	}
	wantReads := map[string]int{root.ObjectID.String(): 1, target.ObjectID.String(): 1, nested.ObjectID.String(): 1}
	if !reflect.DeepEqual(reads, wantReads) {
		t.Fatalf("directory reads=%v, want only path ancestors %v", reads, wantReads)
	}
}

func TestOpenArchivedFileDefersUnvisitedCorruption(t *testing.T) {
	f := newArchiveFixture(t)
	var root, hidden *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		makeDirectory := func(name string) (*snapshot.DirEntry, error) {
			file, err := captureProofFile(ctx, writer, "file.bin", []byte(name))
			if err != nil {
				return nil, err
			}
			return writeProofDirectory(ctx, writer, name, file)
		}
		var err error
		hidden, err = makeDirectory("hidden")
		if err != nil {
			return err
		}
		visible, err := makeDirectory("visible")
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
	archive.Directories[hidden.ObjectID.String()] = []byte("corrupt unvisited directory")
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int{}
	reader, _, err := objects.OpenArchivedFile(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		reads[id]++
		return archive.Directories[id], nil
	}, archivePath(t, "visible/file.bin"))
	if err != nil {
		t.Fatalf("open with corrupt unvisited subtree: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if reads[hidden.ObjectID.String()] != 0 {
		t.Fatalf("unvisited corrupt directory was read: %v", reads)
	}

	archive.Directories[hidden.ObjectID.String()] = []byte("corrupt visited directory")
	if _, _, err := objects.OpenArchivedFile(f.ctx, archive.Format, archive.Root, func(_ context.Context, id string) ([]byte, error) {
		if id == hidden.ObjectID.String() {
			return archive.Directories[id], nil
		}
		// Route the request down the hidden subtree for the visited-corruption check.
		return archive.Directories[id], nil
	}, archivePath(t, "hidden/file.bin")); !errors.Is(err, lodekopia.ErrInvalidArchive) {
		t.Fatalf("visited corrupt directory error=%v, want invalid archive", err)
	}
}

func TestOpenArchivedFileRejectsMissingAndNonFilePaths(t *testing.T) {
	f := newArchiveFixture(t)
	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		file, err := captureProofFile(ctx, writer, "file.bin", []byte("file"))
		if err != nil {
			return err
		}
		directory, err := writeProofDirectory(ctx, writer, "directory", file)
		if err != nil {
			return err
		}
		blocker, err := captureProofFile(ctx, writer, "blocker", []byte("blocker"))
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", directory, blocker)
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
	readDirectory := func(_ context.Context, id string) ([]byte, error) { return archive.Directories[id], nil }
	for _, test := range []struct {
		path string
		want error
	}{
		{"missing", lodekopia.ErrPathNotFound},
		{"directory", lodekopia.ErrPathNotFile},
		{"blocker/child", lodekopia.ErrPathNotFound},
	} {
		if _, _, err := objects.OpenArchivedFile(f.ctx, archive.Format, archive.Root, readDirectory, archivePath(t, test.path)); !errors.Is(err, test.want) {
			t.Errorf("OpenArchivedFile(%q) error=%v, want %v", test.path, err, test.want)
		}
	}
	if _, _, err := objects.OpenArchivedFile(f.ctx, archive.Format, archive.Root, readDirectory, model.Path{}); !errors.Is(err, lodekopia.ErrPathNotFile) {
		t.Fatalf("OpenArchivedFile(root) error=%v, want path-not-file", err)
	}
}

func TestOpenArchivedFileDetectsCycleDepthAndCancellation(t *testing.T) {
	f := newArchiveFixture(t)
	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, writer repo.RepositoryWriter) error {
		file, err := captureProofFile(ctx, writer, "leaf", []byte("leaf"))
		if err != nil {
			return err
		}
		loop, err := writeProofDirectory(ctx, writer, "loop", file)
		if err != nil {
			return err
		}
		root, err = writeProofDirectory(ctx, writer, "", loop)
		return err
	})
	archive, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatal(err)
	}
	var rootManifest snapshot.DirManifest
	if err := json.Unmarshal(archive.Directories[root.ObjectID.String()], &rootManifest); err != nil {
		t.Fatal(err)
	}
	rootManifest.Entries[0].ObjectID = root.ObjectID
	archive.Directories[root.ObjectID.String()], err = json.Marshal(rootManifest)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	readDirectory := func(_ context.Context, id string) ([]byte, error) {
		reads++
		return archive.Directories[id], nil
	}
	if _, _, err := objects.OpenArchivedFile(f.ctx, archive.Format, archive.Root, readDirectory, archivePath(t, "loop/leaf")); !errors.Is(err, lodekopia.ErrInvalidArchive) || !strings.Contains(err.Error(), "directory cycle") {
		t.Fatalf("cycle error=%v, want invalid-archive cycle", err)
	}
	if reads != 1 {
		t.Fatalf("cycle lookup read %d directory records, want root only", reads)
	}

	tooDeep, err := model.ParsePath(strings.TrimSuffix(strings.Repeat("x/", 4098), "/"))
	if err != nil {
		t.Fatal(err)
	}
	reads = 0
	if _, _, err := objects.OpenArchivedFile(f.ctx, archive.Format, archive.Root, readDirectory, tooDeep); !errors.Is(err, lodekopia.ErrInvalidArchive) || !strings.Contains(err.Error(), "directory depth") {
		t.Fatalf("deep path error=%v, want invalid-archive depth", err)
	}
	if reads != 0 {
		t.Fatalf("over-depth path read %d directories, want 0", reads)
	}

	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	reads = 0
	if _, _, err := objects.OpenArchivedFile(canceled, archive.Format, archive.Root, readDirectory, archivePath(t, "loop/leaf")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled lookup error=%v, want context.Canceled", err)
	}
	if reads != 0 {
		t.Fatalf("pre-canceled lookup read %d directories, want 0", reads)
	}

	callbackCanceled, cancelCallback := context.WithCancel(f.ctx)
	if _, _, err := objects.OpenArchivedFile(callbackCanceled, archive.Format, archive.Root, func(context.Context, string) ([]byte, error) {
		cancelCallback()
		return archive.Directories[root.ObjectID.String()], nil
	}, archivePath(t, "loop/leaf")); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback-canceled lookup error=%v, want context.Canceled", err)
	}
}

func archivePath(t *testing.T, value string) model.Path {
	t.Helper()
	path, err := model.ParsePath(value)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
