package kopia_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestObjectsComposeCombinesNativeMetadataWithoutReadingOldPayloads(t *testing.T) {
	f := newArchiveFixture(t)
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	p := func(value string) model.Path { return proofPath(t, value) }
	baseData := map[string][]byte{
		"dir/a": []byte("base-a"), "dir/b": []byte("base-b"),
		"folder/child": []byte("base-child"), "plain": []byte("base-plain"), "empty-file": {},
	}
	baseChanges := make([]lodekopia.Change, 0, len(baseData)+1)
	for path, data := range baseData {
		baseChanges = append(baseChanges, lodekopia.Change{Path: p(path), Kind: model.File, SHA256: digestHex(data), Size: int64(len(data))})
	}
	baseChanges = append(baseChanges, lodekopia.Change{Path: p("empty-dir"), Kind: model.Directory})
	base, _, err := objects.Build(f.ctx, nil, baseChanges, func(path model.Path) (io.ReadCloser, error) {
		data, ok := baseData[path.String()]
		if !ok {
			return nil, errors.New("unexpected base file request")
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	baseEntries, err := objects.Entries(base)
	if err != nil {
		t.Fatal(err)
	}
	basePayloadIDs := map[string]bool{}
	for path := range baseData {
		basePayloadIDs[nativeRefAt(t, base, path)] = true
	}
	localA, alternateB := []byte("local-a"), []byte("alternate-b")
	local, _, err := objects.Build(f.ctx, &base, []lodekopia.Change{
		{Path: p("dir/a"), Kind: model.File, SHA256: digestHex(localA), Size: int64(len(localA))},
		{Path: p("plain"), Kind: model.Directory},
		{Path: p("plain/inside"), Kind: model.File, SHA256: digestHex([]byte("inside")), Size: 6},
	}, func(path model.Path) (io.ReadCloser, error) {
		data := localA
		if path.String() == "plain/inside" {
			data = []byte("inside")
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	alternate, _, err := objects.Build(f.ctx, &base, []lodekopia.Change{
		{Path: p("dir/b"), Kind: model.File, SHA256: digestHex(alternateB), Size: int64(len(alternateB))},
		{Path: p("folder/child"), Kind: model.Absent},
		{Path: p("folder"), Kind: model.File, SHA256: digestHex([]byte("folder-file")), Size: 11},
	}, func(path model.Path) (io.ReadCloser, error) {
		data := alternateB
		if path.String() == "folder" {
			data = []byte("folder-file")
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	primaryEntries, err := objects.Entries(local)
	if err != nil {
		t.Fatal(err)
	}
	alternateEntries, err := objects.Entries(alternate)
	if err != nil {
		t.Fatal(err)
	}
	merged := make(map[model.Path]model.Entry, len(primaryEntries)+1)
	for path, entry := range primaryEntries {
		merged[path] = entry
	}
	merged[p("dir/b")] = alternateEntries[p("dir/b")]
	merged[p("folder")] = alternateEntries[p("folder")]
	delete(merged, p("folder/child"))
	beforeLocal := cloneComposeArchive(local)
	beforeAlternate := cloneComposeArchive(alternate)
	guardedRepo := &objectOpenGuard{DirectRepository: f.rep, forbidden: basePayloadIDs}
	guardedObjects, err := lodekopia.NewObjects(guardedRepo)
	if err != nil {
		t.Fatal(err)
	}
	composed, err := guardedObjects.Compose(f.ctx, local, alternate, merged)
	if err != nil {
		t.Fatalf("Compose unexpectedly read an old payload or failed: %v", err)
	}
	if guardedRepo.forbiddenOpens != 0 {
		t.Fatalf("Compose attempted %d reads of obsolete source payloads", guardedRepo.forbiddenOpens)
	}
	if !equalArchive(local, beforeLocal) || !equalArchive(alternate, beforeAlternate) {
		t.Fatal("Compose mutated an input archive")
	}
	composedEntries, err := objects.Entries(composed)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{
		"dir/a": localA, "dir/b": alternateB, "folder": []byte("folder-file"),
		"plain/inside": []byte("inside"), "empty-file": {},
	} {
		if composedEntries[p(path)].Kind != model.File {
			t.Fatalf("composed path %q is not a file: %#v", path, composedEntries[p(path)])
		}
		reader, _, openErr := objects.OpenFile(f.ctx, composed, p(path))
		if openErr != nil {
			t.Fatalf("open composed %q: %v", path, openErr)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("composed %q = %q; want %q; errors %v / %v", path, got, want, readErr, closeErr)
		}
	}
	if composedEntries[p("plain")].Kind != model.Directory || composedEntries[p("empty-dir")].Kind != model.Directory {
		t.Fatalf("directory transition/empty directory lost: plain=%#v empty-dir=%#v", composedEntries[p("plain")], composedEntries[p("empty-dir")])
	}
	if _, ok := composedEntries[p("folder/child")]; ok {
		t.Fatalf("file transition retained child: %#v", composedEntries[p("folder/child")])
	}
	if baseEntries[p("empty-file")].Kind != model.File || baseEntries[p("empty-file")].ContentID != composedEntries[p("empty-file")].ContentID {
		t.Fatal("unchanged empty-file metadata was not retained")
	}
	if nativeDirRefAt(t, composed, "dir") == nativeDirRefAt(t, local, "dir") {
		t.Fatal("merged directory metadata reused the primary directory ref despite changed child")
	}

	forged := make(map[model.Path]model.Entry, len(merged))
	for path, entry := range merged {
		forged[path] = entry
	}
	forged[p("dir/a")] = model.Entry{Kind: model.File, ContentID: "forged-reference"}
	if _, err := guardedObjects.Compose(f.ctx, local, alternate, forged); err == nil {
		t.Fatal("Compose accepted a file reference absent from both source archives")
	}
}

type objectOpenGuard struct {
	repo.DirectRepository
	forbidden      map[string]bool
	forbiddenOpens int
}

func (g *objectOpenGuard) OpenObject(ctx context.Context, id object.ID) (object.Reader, error) {
	if g.forbidden[id.String()] {
		g.forbiddenOpens++
		return nil, errors.New("attempted to read forbidden base payload")
	}
	return g.DirectRepository.OpenObject(ctx, id)
}

func cloneComposeArchive(input lodekopia.Archive) lodekopia.Archive {
	result := lodekopia.Archive{Format: input.Format, Root: append([]byte(nil), input.Root...), Directories: make(map[string][]byte, len(input.Directories))}
	for key, value := range input.Directories {
		result.Directories[key] = append([]byte(nil), value...)
	}
	return result
}

func equalArchive(a, b lodekopia.Archive) bool {
	return a.Format == b.Format && bytes.Equal(a.Root, b.Root) && equalRawDirectories(a.Directories, b.Directories)
}
