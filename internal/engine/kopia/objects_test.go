package kopia_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestObjectsSparseOverlayRetainsOmittedNativeEntriesAndPayload(t *testing.T) {
	f := newArchiveFixture(t)
	oldA, oldB := []byte("old a payload"), []byte("untouched b payload")
	base := makeObjectsFixtureArchive(t, f, map[string][]byte{"dir/a": oldA, "dir/b": oldB})
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	newA := []byte("new a")
	archive, facts, err := objects.Build(f.ctx, &base, []lodekopia.Change{{Path: proofPath(t, "dir/a"), Kind: model.File, SHA256: digestHex(newA), Size: int64(len(newA))}}, func(p model.Path) (io.ReadCloser, error) {
		if p.String() != "dir/a" {
			t.Fatalf("unexpected source path %q", p)
		}
		return io.NopCloser(bytes.NewReader(newA)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].SHA256 != digestHex(newA) || facts[0].Size != int64(len(newA)) {
		t.Fatalf("unexpected file facts: %#v", facts)
	}
	reader, err := lodekopia.OpenArchive(objects.Namespace(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]model.Entry{
		"dir/a": {Kind: model.File, ContentID: scopedForTest(objects.Namespace(), facts[0].Ref)},
		"dir/b": {Kind: model.File, ContentID: scopedForTest(objects.Namespace(), nativeRefAt(t, base, "dir/b"))},
	} {
		got, err := reader.Lookup(proofPath(t, path))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("lookup %q = %#v, want %#v", path, got, want)
		}
	}
	read, _, err := objects.OpenFile(f.ctx, archive, proofPath(t, "dir/b"))
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := io.ReadAll(read)
	closeErr := read.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read retained b: %v / %v", err, closeErr)
	}
	if !bytes.Equal(gotB, oldB) {
		t.Fatalf("retained b payload changed: %q", gotB)
	}
	oldDirID := nativeDirRefAt(t, base, "dir")
	newDirID := nativeDirRefAt(t, archive, "dir")
	if oldDirID == newDirID {
		t.Fatal("changed directory manifest reused its old object ID")
	}
}

func TestObjectsRejectIncompleteRemovalAndAllowExplicitWholeSubtreeRemoval(t *testing.T) {
	f := newArchiveFixture(t)
	base := makeObjectsFixtureArchive(t, f, map[string][]byte{"dir/a": []byte("a"), "dir/b": []byte("b")})
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = objects.Build(f.ctx, &base, []lodekopia.Change{{Path: proofPath(t, "dir"), Kind: model.Absent}}, nil)
	if !errors.Is(err, model.ErrTreeConflict) {
		t.Fatalf("directory removal with omitted children error = %v", err)
	}
	changes := []lodekopia.Change{
		{Path: proofPath(t, "dir"), Kind: model.Absent},
		{Path: proofPath(t, "dir/a"), Kind: model.Absent},
		{Path: proofPath(t, "dir/b"), Kind: model.Absent},
	}
	archive, facts, err := objects.Build(f.ctx, &base, changes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("absence changes captured content: %#v", facts)
	}
	tree, err := lodekopia.OpenArchive(objects.Namespace(), archive)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tree.Lookup(proofPath(t, "dir"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != model.Absent {
		t.Fatalf("removed directory lookup = %#v", got)
	}
}

func TestObjectsFileDirectoryTransitionsAndValidation(t *testing.T) {
	f := newArchiveFixture(t)
	base := makeObjectsFixtureArchive(t, f, map[string][]byte{"switch/child": []byte("child"), "plain": []byte("plain")})
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	newPayload := []byte("replaced")
	archive, _, err := objects.Build(f.ctx, &base, []lodekopia.Change{
		{Path: proofPath(t, "switch"), Kind: model.Absent},
		{Path: proofPath(t, "switch/child"), Kind: model.Absent},
		{Path: proofPath(t, "switch"), Kind: model.File, SHA256: digestHex(newPayload), Size: int64(len(newPayload))},
		{Path: proofPath(t, "plain"), Kind: model.Directory},
	}, func(p model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(newPayload)), nil })
	if err == nil {
		t.Fatal("duplicate change paths were accepted")
	}

	archive, _, err = objects.Build(f.ctx, &base, []lodekopia.Change{
		{Path: proofPath(t, "switch/child"), Kind: model.Absent},
		{Path: proofPath(t, "switch"), Kind: model.File, SHA256: digestHex(newPayload), Size: int64(len(newPayload))},
		{Path: proofPath(t, "plain"), Kind: model.Directory},
	}, func(p model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(newPayload)), nil })
	if err != nil {
		t.Fatal(err)
	}
	tree, err := lodekopia.OpenArchive(objects.Namespace(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := tree.Lookup(proofPath(t, "switch")); got.Kind != model.File {
		t.Fatalf("file transition: %#v", got)
	}
	if got, _ := tree.Lookup(proofPath(t, "plain")); got.Kind != model.Directory {
		t.Fatalf("directory transition: %#v", got)
	}

	for _, tc := range []struct {
		name   string
		change lodekopia.Change
		paths  []model.Path
		open   func(model.Path) (io.ReadCloser, error)
	}{
		{name: "root", change: lodekopia.Change{Path: model.Path{}, Kind: model.Absent}},
		{name: "bad hash", change: lodekopia.Change{Path: proofPath(t, "x"), Kind: model.File, SHA256: "ABC", Size: 1}, open: func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("x"))), nil }},
		{name: "negative size", change: lodekopia.Change{Path: proofPath(t, "x"), Kind: model.File, SHA256: digestHex([]byte("x")), Size: -1}, open: func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("x"))), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := objects.Build(f.ctx, nil, []lodekopia.Change{tc.change}, tc.open)
			if err == nil {
				t.Fatal("invalid change accepted")
			}
		})
	}
	if _, _, err := objects.Build(f.ctx, nil, []lodekopia.Change{{Path: proofPath(t, "size"), Kind: model.File, SHA256: digestHex([]byte("x")), Size: 2}}, func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("x"))), nil }); err == nil {
		t.Fatal("size mismatch accepted")
	}
	if _, _, err := objects.Build(f.ctx, nil, []lodekopia.Change{{Path: proofPath(t, "hash"), Kind: model.File, SHA256: digestHex([]byte("other")), Size: 1}}, func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("x"))), nil }); err == nil {
		t.Fatal("hash mismatch accepted")
	}
}

func TestObjectsDeterministicReplayAndPagedSelectionReconstructsFiles(t *testing.T) {
	f := newArchiveFixture(t)
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	dataA, dataB := []byte("first file bytes"), []byte("second file bytes")
	changes := []lodekopia.Change{
		{Path: proofPath(t, "dir/a"), Kind: model.File, SHA256: digestHex(dataA), Size: int64(len(dataA))},
		{Path: proofPath(t, "dir/b"), Kind: model.File, SHA256: digestHex(dataB), Size: int64(len(dataB))},
	}
	var opened []string
	open := func(p model.Path) (io.ReadCloser, error) {
		opened = append(opened, p.String())
		if p.String() == "dir/a" {
			return io.NopCloser(bytes.NewReader(dataA)), nil
		}
		return io.NopCloser(bytes.NewReader(dataB)), nil
	}
	a, _, err := objects.Build(f.ctx, nil, changes, open)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opened, ","); got != "dir/a,dir/b" {
		t.Fatalf("file source read order/count = %q", got)
	}
	opened = nil
	b, _, err := objects.Build(f.ctx, nil, changes, open)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opened, ","); got != "dir/a,dir/b" {
		t.Fatalf("replay source read order/count = %q", got)
	}
	if !bytes.Equal(a.Root, b.Root) || !equalRawDirectories(a.Directories, b.Directories) {
		t.Fatal("replaying identical changes changed native archive identity")
	}
	selection := model.Selection{}.Include(proofPath(t, "dir/a")).Include(proofPath(t, "dir/b"))
	page1, next, err := objects.List(a, selection, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || next != "dir/a" {
		t.Fatalf("first page = %#v next %q", page1, next)
	}
	if page1[0].Path.String() != "dir" || page1[0].Kind != model.Directory || page1[1].Path.String() != "dir/a" {
		t.Fatalf("selected results omitted their ancestor directory: %#v", page1)
	}
	page2, next2, err := objects.List(a, selection, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].Path.String() != "dir/b" || next2 != "" {
		t.Fatalf("second page = %#v next %q", page2, next2)
	}
	all := append(page1, page2...)
	for _, node := range all {
		if node.Kind != model.File {
			continue
		}
		reader, gotNode, err := objects.OpenFile(f.ctx, a, node.Path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read %q: %v / %v", node.Path, err, closeErr)
		}
		want := dataA
		if node.Path.String() == "dir/b" {
			want = dataB
		}
		if !bytes.Equal(body, want) || gotNode.Size != int64(len(want)) {
			t.Fatalf("reconstructed %q differs", node.Path)
		}
	}
	if _, _, err := objects.List(a, selection, "", 1001); !errors.Is(err, lodekopia.ErrInvalidPage) {
		t.Fatalf("large page error = %v", err)
	}
}

func TestObjectsEmptyFileRoundTrip(t *testing.T) {
	f := newArchiveFixture(t)
	objects, err := lodekopia.NewObjects(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	empty := []byte{}
	archive, facts, err := objects.Build(f.ctx, nil, []lodekopia.Change{{Path: proofPath(t, "empty"), Kind: model.File, SHA256: digestHex(empty), Size: 0}}, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(empty)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Ref == "" || facts[0].Size != 0 {
		t.Fatalf("empty file facts = %#v", facts)
	}
	reader, node, err := objects.OpenFile(f.ctx, archive, proofPath(t, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(body) != 0 || node.Size != 0 {
		t.Fatalf("empty file read = %q, node %#v, errors %v / %v", body, node, readErr, closeErr)
	}
}

func makeObjectsFixtureArchive(t *testing.T, f *archiveFixture, files map[string][]byte) lodekopia.Archive {
	t.Helper()
	var root *snapshot.DirEntry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		children := map[string][]*snapshot.DirEntry{}
		for path, data := range files {
			parts := bytes.Split([]byte(path), []byte("/"))
			if len(parts) == 1 {
				e, err := captureProofFile(ctx, w, path, data)
				if err != nil {
					return err
				}
				children[""] = append(children[""], e)
				continue
			}
			dirName := string(parts[0])
			leaf := string(parts[1])
			e, err := captureProofFile(ctx, w, leaf, data)
			if err != nil {
				return err
			}
			children[dirName] = append(children[dirName], e)
		}
		rootEntries := children[""]
		for dir, entries := range children {
			if dir == "" {
				continue
			}
			de, err := writeProofDirectory(ctx, w, dir, entries...)
			if err != nil {
				return err
			}
			rootEntries = append(rootEntries, de)
		}
		var err error
		root, err = writeProofDirectory(ctx, w, "", rootEntries...)
		return err
	})
	a, err := lodekopia.ExportTree(f.ctx, f.rep, root)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func digestHex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func scopedForTest(namespace, ref string) string {
	return "kopia:v1:" + itoa(len(namespace)) + ":" + namespace + ref
}
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
func nativeRefAt(t *testing.T, a lodekopia.Archive, path string) string {
	t.Helper()
	return nativeEntryAt(t, a, path).ObjectID.String()
}
func nativeDirRefAt(t *testing.T, a lodekopia.Archive, path string) string {
	t.Helper()
	return nativeEntryAt(t, a, path).ObjectID.String()
}
func nativeEntryAt(t *testing.T, a lodekopia.Archive, path string) *snapshot.DirEntry {
	t.Helper()
	var current snapshot.DirEntry
	if err := json.Unmarshal(a.Root, &current); err != nil {
		t.Fatal(err)
	}
	if path == "" {
		return &current
	}
	for _, part := range bytes.Split([]byte(path), []byte("/")) {
		var dir snapshot.DirManifest
		raw, ok := a.Directories[current.ObjectID.String()]
		if !ok {
			t.Fatalf("missing native directory %q", current.ObjectID)
		}
		if err := json.Unmarshal(raw, &dir); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, child := range dir.Entries {
			if child.Name == string(part) {
				current = *child
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing native path %q", path)
		}
	}
	return &current
}
func equalRawDirectories(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}
