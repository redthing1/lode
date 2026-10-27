package model

import (
	"errors"
	"reflect"
	"testing"
)

func treePath(t *testing.T, value string) Path {
	t.Helper()
	path, err := ParsePath(value)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyChangesPreservesOmittedEntriesAndCreatesParents(t *testing.T) {
	root := Path{}
	dir, loaded, omitted := treePath(t, "assets"), treePath(t, "assets/edit.bin"), treePath(t, "assets/omitted.bin")
	added := treePath(t, "new/nested/data.bin")
	base := map[Path]Entry{
		root: {Kind: Directory}, dir: {Kind: Directory},
		loaded: {Kind: File, ContentID: "base"}, omitted: {Kind: File, ContentID: "keep"},
	}
	changes := map[Path]Entry{loaded: {Kind: File, ContentID: "changed"}, added: {Kind: File, ContentID: "new"}}
	got, err := ApplyChanges(base, changes)
	if err != nil {
		t.Fatal(err)
	}
	if got[omitted] != base[omitted] || got[loaded] != changes[loaded] || got[added] != changes[added] {
		t.Fatalf("incorrect complete result: %v", got)
	}
	if got[added.Parent()].Kind != Directory || got[added.Parent().Parent()].Kind != Directory {
		t.Fatal("missing synthesized parents")
	}
	delete(got, omitted)
	if base[omitted].ContentID != "keep" || base[loaded].ContentID != "base" || len(changes) != 2 {
		t.Fatal("applying changes mutated inputs")
	}
}

func TestApplyChangesRequiresExplicitDescendantRemoval(t *testing.T) {
	dir, loaded, omitted := treePath(t, "assets"), treePath(t, "assets/edit.bin"), treePath(t, "assets/omitted.bin")
	base := map[Path]Entry{Path{}: {Kind: Directory}, dir: {Kind: Directory}, loaded: {Kind: File, ContentID: "a"}, omitted: {Kind: File, ContentID: "b"}}
	for _, replacement := range []Entry{{Kind: Absent}, {Kind: File, ContentID: "replacement"}} {
		changes := map[Path]Entry{dir: replacement, loaded: {Kind: Absent}}
		if _, err := ApplyChanges(base, changes); !errors.Is(err, ErrTreeConflict) {
			t.Fatalf("omitted descendant lost under %+v: %v", replacement, err)
		}
		changes[omitted] = Entry{Kind: Absent}
		got, err := ApplyChanges(base, changes)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got[loaded]; ok {
			t.Fatal("explicitly removed child remains")
		}
		if replacement.Kind == Absent {
			if _, ok := got[dir]; ok {
				t.Fatal("explicitly removed empty directory remains")
			}
		} else if got[dir] != replacement {
			t.Fatal("empty directory not replaced")
		}
	}
}

func TestApplyChangesStructuralTransitionsAndInvalidFacts(t *testing.T) {
	file, child := treePath(t, "item"), treePath(t, "item/child")
	base := map[Path]Entry{Path{}: {Kind: Directory}, file: {Kind: File, ContentID: "old"}}
	got, err := ApplyChanges(base, map[Path]Entry{file: {Kind: Directory}, child: {Kind: File, ContentID: "new"}})
	if err != nil || got[child].ContentID != "new" {
		t.Fatalf("explicit file-to-directory transition: %v, %v", got, err)
	}
	for name, changes := range map[string]map[Path]Entry{
		"root deletion":    {Path{}: {Kind: Absent}},
		"root replacement": {Path{}: {Kind: File, ContentID: "x"}},
		"below file":       {child: {Kind: File, ContentID: "x"}},
		"below deletion":   {file: {Kind: Absent}, child: {Kind: Directory}},
		"unknown fact":     {file: {}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ApplyChanges(base, changes); err == nil {
				t.Fatal("invalid change accepted")
			}
		})
	}
	for _, invalid := range []map[Path]Entry{
		{file: {Kind: File, ContentID: "x"}},
		{Path{}: {Kind: Directory}, child: {Kind: Directory}},
		{Path{}: {Kind: Directory}, file: {Kind: Absent}},
	} {
		if _, err := ApplyChanges(invalid, nil); err == nil {
			t.Fatal("incomplete base accepted")
		}
	}
	empty, err := ApplyChanges(nil, nil)
	if err != nil || !reflect.DeepEqual(empty, map[Path]Entry{Path{}: {Kind: Directory}}) {
		t.Fatalf("empty project: %v, %v", empty, err)
	}
}
