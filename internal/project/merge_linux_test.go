package project

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func branchSave(t *testing.T, s *Store, branch, base string, changes []model.Entry, data map[string]string) model.Version {
	t.Helper()
	v, err := s.Save(context.Background(), "alice", model.Save{ID: operation(t), Branch: branch, Base: base, Changes: changes}, Source{Write: func(e model.Entry, out io.Writer, _ *Basis) error {
		_, err := io.WriteString(out, data[e.Path])
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMergeReusesCompleteTreesAndRecordsBothParents(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	first := publish(t, s, "", []model.Entry{file("assets/a", "original"), file("references/keep", "omitted")}, map[string]string{"assets/a": "original", "references/keep": "omitted"})
	if _, err := s.CreateBranch(ctx, "experiment", first.ID); err != nil {
		t.Fatal(err)
	}
	source := branchSave(t, s, "experiment", first.ID, []model.Entry{file("assets/a", "experiment"), file("references/extra", "never downloaded")}, map[string]string{"assets/a": "experiment", "references/extra": "never downloaded"})
	target := publish(t, s, first.ID, []model.Entry{file("local", "independent")}, map[string]string{"local": "independent"})
	// Expired ancestor contents are unnecessary while comparison metadata exists.
	if _, err := f.db.Exec("UPDATE versions SET protection='' WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	r := model.Save{ID: operation(t), Branch: "main", Base: target.ID, Merge: source.ID, Message: "Integrate experiment"}
	complete := false
	v, err := s.Save(ctx, "alice", r, Source{Write: func(model.Entry, io.Writer, *Basis) error { t.Fatal("merge requested payload"); return nil }, Complete: func() error { complete = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !complete || v.Parent != target.ID || v.MergeParent != source.ID {
		t.Fatalf("receipt: %+v, complete %v", v, complete)
	}
	for name, want := range map[string]string{"assets/a": "experiment", "references/keep": "omitted", "references/extra": "never downloaded", "local": "independent"} {
		if got := read(t, s, v.ID, name); got != want {
			t.Fatalf("%s = %q", name, got)
		}
	}
	a, _ := s.version(ctx, source.ID)
	b, _ := s.version(ctx, v.ID)
	for _, name := range []string{"assets/a", "references/extra"} {
		old, err := s.lookup(ctx, a.root, name)
		if err != nil {
			t.Fatal(err)
		}
		next, err := s.lookup(ctx, b.root, name)
		if err != nil || old.Object != next.Object {
			t.Fatalf("object not reused: %v", err)
		}
	}
	// The next merge must use the second parent as its base, retaining a target
	// reversal instead of silently reintroducing already integrated work.
	reverted := publish(t, s, v.ID, []model.Entry{file("assets/a", "original")}, map[string]string{"assets/a": "original"})
	advanced := branchSave(t, s, "experiment", source.ID, []model.Entry{file("new", "next experiment")}, map[string]string{"new": "next experiment"})
	r2 := model.Save{ID: operation(t), Branch: "main", Base: reverted.ID, Merge: advanced.ID}
	v2, err := s.Save(ctx, "alice", r2, Source{})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, s, v2.ID, "assets/a") != "original" || read(t, s, v2.ID, "new") != "next experiment" {
		t.Fatal("second parent ignored")
	}
	if retry, err := s.Save(ctx, "alice", r, Source{}); err != nil || retry != v {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	changed := r
	changed.Merge = advanced.ID
	if _, err := s.Save(ctx, "alice", changed, Source{}); !errors.Is(err, ErrIntent) {
		t.Fatalf("changed source: %v", err)
	}
	if _, err := s.Save(ctx, "bob", r, Source{}); !errors.Is(err, ErrIntent) {
		t.Fatalf("changed actor: %v", err)
	}
	r2.ID = operation(t)
	r2.Base = v2.ID
	if _, err := s.Save(ctx, "alice", r2, Source{}); !errors.Is(err, ErrAlreadyMerged) {
		t.Fatalf("repeated integration: %v", err)
	}
	f.close(t)
	f = openFixture(t, f.directory, false)
	if got, err := f.store.Resolve(ctx, "alice", r); err != nil || got != v {
		t.Fatalf("reopened receipt: %+v %v", got, err)
	}
	if read(t, f.store, v2.ID, "assets/a") != "original" {
		t.Fatal("reopened contents")
	}
}

func TestMergeWholeSubtreeChoicesAndCancellation(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	first := publish(t, s, "", []model.Entry{file("assets/a", "original")}, map[string]string{"assets/a": "original"})
	if _, err := s.CreateBranch(ctx, "experiment", first.ID); err != nil {
		t.Fatal(err)
	}
	source := branchSave(t, s, "experiment", first.ID, []model.Entry{file("assets/a", "changed"), file("assets/b", "new")}, map[string]string{"assets/a": "changed", "assets/b": "new"})
	target := publish(t, s, first.ID, []model.Entry{{Path: "assets", Kind: model.Absent}, {Path: "assets/a", Kind: model.Absent}}, nil)
	r := model.Save{ID: operation(t), Branch: "main", Base: target.ID, Merge: source.ID}
	_, err := s.Save(ctx, "alice", r, Source{})
	var conflicts *ConflictsError
	if !errors.As(err, &conflicts) || len(conflicts.Conflicts) != 1 || conflicts.Conflicts[0].Path != "assets" {
		t.Fatalf("conflicts: %v", err)
	}
	if head, _ := s.Head(ctx, "main"); head != target.ID {
		t.Fatal("conflict published")
	}
	if _, err := s.Cancel(ctx, "alice", r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "alice", r, Source{}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("delayed request: %v", err)
	}
	r.ID = operation(t)
	r.Choices = map[string]model.Side{"assets": model.Local}
	v, err := s.Save(ctx, "alice", r, Source{})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.version(ctx, target.ID)
	next, _ := s.version(ctx, v.ID)
	if next.root != old.root || v.MergeParent != source.ID {
		t.Fatal("identical tree lost merge history")
	}
	if _, err := s.CreateBranch(ctx, "incoming", target.ID); err != nil {
		t.Fatal(err)
	}
	r.ID = operation(t)
	r.Branch = "incoming"
	r.Choices["assets"] = model.Incoming
	v, err = s.Save(ctx, "alice", r, Source{})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, s, v.ID, "assets/a") != "changed" || read(t, s, v.ID, "assets/b") != "new" {
		t.Fatal("incoming subtree incomplete")
	}
	if _, err := s.CreateBranch(ctx, "empty", ""); err != nil {
		t.Fatal(err)
	}
	r = model.Save{ID: operation(t), Branch: "empty", Merge: source.ID}
	v, err = s.Save(ctx, "alice", r, Source{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Parent != "" || v.MergeParent != source.ID || read(t, s, v.ID, "assets/b") != "new" {
		t.Fatal("empty target merge")
	}
	// Extra bytes must be rejected even though integration consumes no files.
	if _, err := s.CreateBranch(ctx, "bad-stream", first.ID); err != nil {
		t.Fatal(err)
	}
	r = model.Save{ID: operation(t), Branch: "bad-stream", Base: first.ID, Merge: source.ID}
	bad := errors.New("extra upload part")
	if _, err := s.Save(ctx, "alice", r, Source{Complete: func() error { return bad }}); !errors.Is(err, bad) {
		t.Fatalf("extra contents: %v", err)
	}
	if head, _ := s.Head(ctx, "bad-stream"); head != first.ID {
		t.Fatal("bad stream published")
	}
	if _, err := f.db.Exec("UPDATE versions SET protection='' WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "alice", r, Source{}); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired source: %v", err)
	}
}

func TestMergeBasePrunedAndAmbiguousAncestry(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	type node struct{ id, parent, merge string }
	for _, tc := range []struct {
		name                 string
		nodes                []node
		target, source, want string
		failure              error
	}{
		{"independent roots", []node{{"a", "", ""}, {"b", "", ""}}, "a", "b", "", nil},
		{"second parent", []node{{"a", "", ""}, {"b", "a", ""}, {"c", "a", ""}, {"m", "b", "c"}, {"d", "c", ""}}, "m", "d", "c", nil},
		{"already integrated", []node{{"a", "", ""}, {"b", "a", ""}, {"c", "a", ""}, {"m", "b", "c"}}, "m", "c", "", ErrAlreadyMerged},
		{"criss cross", []node{{"a", "", ""}, {"b", "a", ""}, {"c", "a", ""}, {"m", "b", "c"}, {"n", "c", "b"}}, "m", "n", "", ErrMergeBase},
		{"missing base", []node{{"a", "gone", ""}, {"b", "gone", ""}}, "a", "b", "", ErrHistoryUnavailable},
		{"missing before base", []node{{"b", "gone", ""}, {"a", "b", ""}, {"c", "b", ""}}, "a", "c", "b", nil},
		{"missing competing path", []node{{"b", "", ""}, {"a", "b", "gone"}, {"c", "b", ""}}, "a", "c", "", ErrHistoryUnavailable},
		{"known descendant past boundary", []node{{"b", "gone", ""}, {"a", "b", ""}}, "b", "a", "b", nil},
		{"disjoint unknown history", []node{{"a", "x", ""}, {"b", "y", ""}}, "a", "b", "", ErrHistoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.db.Exec("DELETE FROM versions"); err != nil {
				t.Fatal(err)
			}
			for _, n := range tc.nodes {
				if _, err := f.db.Exec("INSERT INTO versions VALUES('project',?,?,?,'alice','',0,'','','')", n.id, n.parent, n.merge); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.store.mergeBase(ctx, tc.target, tc.source)
			if !errors.Is(err, tc.failure) || got != tc.want {
				t.Fatalf("base %q, error %v; want %q, %v", got, err, tc.want, tc.failure)
			}
		})
	}
}
