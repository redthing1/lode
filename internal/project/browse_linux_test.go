//go:build linux

package project

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestVersionAndHistoryPagesUseImmutableFirstParentCursor(t *testing.T) {
	tp := newTestProject(t)
	versions := make([]Commit, 7)
	base := ""
	for i := range versions {
		name := fmt.Sprintf("file-%d", i)
		data := []byte(name)
		versions[i] = saveOn(t, tp.p, "main", base, []model.Entry{fileEntry(name, data)}, map[string][]byte{name: data})
		base = versions[i].ID
	}

	gotVersion, err := tp.p.Version(tp.ctx, versions[3].ID)
	if err != nil || gotVersion.ID != versions[3].ID || !gotVersion.Retained {
		t.Fatalf("Version = %+v, %v", gotVersion, err)
	}
	if _, err := tp.p.Version(tp.ctx, "not-an-id"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid version error = %v, want ErrInvalid", err)
	}

	var got []string
	cursor := ""
	for {
		page, next, err := tp.p.HistoryPage(tp.ctx, "main", cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, commit := range page {
			got = append(got, commit.ID)
		}
		if next == "" {
			break
		}
		if len(page) != 3 || next != page[len(page)-1].ID {
			t.Fatalf("page cursor = %q for page %#v", next, page)
		}
		cursor = next
	}
	want := make([]string, len(versions))
	for i := range versions {
		want[len(versions)-1-i] = versions[i].ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history IDs = %v, want %v", got, want)
	}
	if _, _, err := tp.p.HistoryPage(tp.ctx, "main", "", 101); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized history page error = %v, want ErrInvalid", err)
	}

	// A valid cursor from another branch continues by immutable commit ID; it
	// does not require reachability from the named branch.
	if err := tp.p.CreateBranch(tp.ctx, "side", versions[2].ID); err != nil {
		t.Fatal(err)
	}
	sideData := []byte("side")
	side := saveOn(t, tp.p, "side", versions[2].ID, []model.Entry{fileEntry("side", sideData)}, map[string][]byte{"side": sideData})
	continued, _, err := tp.p.HistoryPage(tp.ctx, "main", side.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{versions[2].ID, versions[1].ID}
	if len(continued) != len(want) || continued[0].ID != want[0] || continued[1].ID != want[1] {
		t.Fatalf("unreachable cursor continuation = %#v, want IDs %v", continued, want)
	}
}

func TestDirectoryPagesWideRootAndSkipsDeepSubtrees(t *testing.T) {
	tp := newTestProject(t)
	const wide = 235
	entries := make([]model.Entry, 0, wide+220)
	files := make(map[string][]byte)
	wantRoot := make([]model.Entry, 0, wide+1)
	for i := 0; i < wide; i++ {
		name := fmt.Sprintf("item-%03d", i)
		if i%3 == 0 {
			entries = append(entries, model.Entry{Path: name, Kind: model.Directory})
			wantRoot = append(wantRoot, model.Entry{Path: name, Kind: model.Directory})
			child := name + "/nested"
			entries = append(entries, model.Entry{Path: child, Kind: model.Directory})
			leaf := child + "/leaf"
			data := []byte(name)
			entries = append(entries, fileEntry(leaf, data))
			files[leaf] = data
		} else {
			data := []byte(name)
			entry := fileEntry(name, data)
			entries = append(entries, entry)
			wantRoot = append(wantRoot, entry)
			files[name] = data
		}
	}
	itemBoundaryData := []byte("boundary sibling")
	itemBoundary := fileEntry("item-0000", itemBoundaryData)
	entries = append(entries, itemBoundary)
	wantRoot = append(wantRoot, itemBoundary)
	files[itemBoundary.Path] = itemBoundaryData
	deep := "deep"
	entries = append(entries, model.Entry{Path: deep, Kind: model.Directory})
	wantRoot = append(wantRoot, model.Entry{Path: deep, Kind: model.Directory})
	entries = append(entries, model.Entry{Path: "deep/d0000", Kind: model.Directory})
	for i := 0; i < 220; i++ {
		deep += fmt.Sprintf("/d%03d", i)
		entries = append(entries, model.Entry{Path: deep, Kind: model.Directory})
	}
	deepLeaf := deep + "/leaf"
	deepData := []byte("deep leaf")
	entries = append(entries, fileEntry(deepLeaf, deepData))
	files[deepLeaf] = deepData
	model.Sort(entries)
	r := request(entries...)
	commit, err := tp.p.Save(tp.ctx, r, Source{Write: func(entry model.Entry, _ Basis, out io.Writer) error {
		data, ok := files[entry.Path]
		if !ok {
			return fmt.Errorf("missing test payload for %q", entry.Path)
		}
		_, err := out.Write(data)
		return err
	}})
	if err != nil {
		t.Fatalf("save large tree: %v", err)
	}
	model.Sort(wantRoot)

	var actual []model.Entry
	after := ""
	for {
		page, next, err := tp.p.Directory(tp.ctx, commit.ID, "", after, 31)
		if err != nil {
			t.Fatalf("root page after %q: %v", after, err)
		}
		if len(page) > 31 {
			t.Fatalf("root page has %d entries, limit is 31", len(page))
		}
		actual = append(actual, page...)
		if next == "" {
			break
		}
		if len(page) == 0 || next != page[len(page)-1].Path {
			t.Fatalf("root cursor %q does not match last item %#v", next, page)
		}
		after = next
	}
	if !reflect.DeepEqual(actual, wantRoot) {
		t.Fatalf("root directory entries (%d) differ from expected (%d)", len(actual), len(wantRoot))
	}

	deepChildren, next, err := tp.p.Directory(tp.ctx, commit.ID, "deep", "", 2)
	if err != nil || len(deepChildren) != 2 || deepChildren[0].Path != "deep/d000" || deepChildren[1].Path != "deep/d0000" || next != "" {
		t.Fatalf("deep direct children = %#v, %q, %v", deepChildren, next, err)
	}
	if _, _, err := tp.p.Directory(tp.ctx, commit.ID, "item-001", "", 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("file directory error = %v, want ErrInvalid", err)
	}
	if _, _, err := tp.p.Directory(tp.ctx, commit.ID, "missing", "", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing directory error = %v, want ErrNotFound", err)
	}
}

func TestDirectoryMergesDeltasAndReadsExpiredMetadata(t *testing.T) {
	tp := newTestProject(t)
	one, two, three := []byte("one"), []byte("two"), []byte("three")
	first := save(t, tp.p, request(
		model.Entry{Path: "folder", Kind: model.Directory},
		fileEntry("folder/a", one), fileEntry("folder/b", two),
		model.Entry{Path: "folder/sub", Kind: model.Directory},
		fileEntry("folder/sub/c", three),
	), map[string][]byte{"folder/a": one, "folder/b": two, "folder/sub/c": three})
	secondRequest := request(fileEntry("folder/a", []byte("changed")), fileEntry("folder/0", []byte("new")))
	secondRequest.Base = first.ID
	second := save(t, tp.p, secondRequest, map[string][]byte{"folder/a": []byte("changed"), "folder/0": []byte("new")})
	thirdRequest := request(model.Entry{Path: "folder/b", Kind: model.Absent})
	thirdRequest.Base = second.ID
	third := save(t, tp.p, thirdRequest, nil)

	page, next, err := tp.p.Directory(tp.ctx, third.ID, "folder", "", 10)
	if err != nil || next != "" {
		t.Fatalf("current folder page = %#v, %q, %v", page, next, err)
	}
	want := []model.Entry{
		fileEntry("folder/0", []byte("new")),
		fileEntry("folder/a", []byte("changed")),
		model.Entry{Path: "folder/sub", Kind: model.Directory},
	}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("current folder entries = %#v, want %#v", page, want)
	}

	plan, err := tp.p.PreviewCleanup(tp.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.p.ApplyCleanup(tp.ctx, 0, plan.Token); err != nil {
		t.Fatal(err)
	}
	metadata, err := tp.p.Version(tp.ctx, first.ID)
	if err != nil || metadata.Retained {
		t.Fatalf("expired Version = %+v, %v", metadata, err)
	}
	expired, _, err := tp.p.Directory(tp.ctx, first.ID, "folder", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	wantExpired := []model.Entry{
		fileEntry("folder/a", one),
		fileEntry("folder/b", two),
		model.Entry{Path: "folder/sub", Kind: model.Directory},
	}
	if !reflect.DeepEqual(expired, wantExpired) {
		t.Fatalf("expired version directory = %#v, want %#v", expired, wantExpired)
	}
}

func TestDirectoryRejectsInvalidPageAndCursor(t *testing.T) {
	tp := newTestProject(t)
	commit := save(t, tp.p, request(fileEntry("folder/file", []byte("x")), model.Entry{Path: "folder", Kind: model.Directory}), map[string][]byte{"folder/file": []byte("x")})
	for _, test := range []struct {
		name  string
		dir   string
		after string
		limit int
	}{
		{name: "zero limit", limit: 0},
		{name: "large limit", limit: 101},
		{name: "invalid cursor", dir: "folder", after: "../bad", limit: 10},
		{name: "wrong parent cursor", dir: "folder", after: "other/file", limit: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := tp.p.Directory(tp.ctx, commit.ID, test.dir, test.after, test.limit); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Directory error = %v, want ErrInvalid", err)
			}
		})
	}
}
