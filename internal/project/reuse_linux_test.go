package project

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestSaveReusesVerifiedBaseObjects(t *testing.T) {
	f := openFixture(t, t.TempDir(), true)
	s, ctx := f.store, context.Background()
	first := publish(t, s, "", []model.Entry{file("FILE.iso", "original"), {Path: "dir", Kind: model.Directory}}, map[string]string{"FILE.iso": "original"})
	changes := []model.Entry{{Path: "FILE.iso", Kind: model.Absent}, file("FILE.ISO", "original"), file("dir/copy", "original")}
	r := model.Save{ID: operation(t), Branch: "main", Base: first.ID, Changes: changes, Reuse: model.ReuseFiles([]model.Entry{file("FILE.iso", "original")}, changes)}
	source := Source{Write: func(model.Entry, io.Writer, *Basis) error {
		t.Fatal("reused file requested bytes")
		return nil
	}}
	second, err := s.Save(ctx, "alice", r, source)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.version(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.version(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.lookup(ctx, old.root, "FILE.iso")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"FILE.ISO", "dir/copy"} {
		got, err := s.lookup(ctx, next.root, name)
		if err != nil || got.Object.ID != original.Object.ID || read(t, s, second.ID, name) != "original" {
			t.Fatalf("%q did not reuse the original object: %+v, %v", name, got, err)
		}
	}
	if _, err := s.lookup(ctx, next.root, "FILE.iso"); err == nil {
		t.Fatal("rename left the old path")
	}
	if retry, err := s.Save(ctx, "alice", r, source); err != nil || retry.ID != second.ID {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	r.Reuse["FILE.ISO"] = "dir/copy"
	if _, err := s.Save(ctx, "alice", r, source); !errors.Is(err, ErrIntent) {
		t.Fatalf("retry changed its reuse source: %v", err)
	}
	if err := s.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(ctx, preview.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	f.close(t)
	f = openFixture(t, f.directory, false)
	for _, name := range []string{"FILE.ISO", "dir/copy"} {
		if read(t, f.store, second.ID, name) != "original" {
			t.Fatalf("reused file %q was lost after base expiry and collection", name)
		}
	}
}

func TestSaveRejectsUnverifiedReuse(t *testing.T) {
	f := openFixture(t, t.TempDir(), true)
	s, ctx := f.store, context.Background()
	first := publish(t, s, "", []model.Entry{file("original", "original"), {Path: "dir", Kind: model.Directory}}, map[string]string{"original": "original"})
	wrongSize := file("new", "original")
	wrongSize.Size++
	for _, tc := range []struct {
		name, source string
		entry        model.Entry
	}{
		{"missing", "missing", file("new", "original")},
		{"directory", "dir", file("new", "")},
		{"hash mismatch", "original", file("new", "modified")},
		{"size mismatch", "original", wrongSize},
		{"new content", "new", file("new", "original")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := model.Save{ID: operation(t), Branch: "main", Base: first.ID, Changes: []model.Entry{tc.entry}, Reuse: map[string]string{"new": tc.source}}
			if _, err := s.Save(ctx, "alice", r, Source{}); !errors.Is(err, ErrRequest) {
				t.Fatalf("unverified reuse: %v", err)
			}
			if head, err := s.head(ctx, "main"); err != nil || head != first.ID {
				t.Fatalf("invalid reuse advanced the branch: %q, %v", head, err)
			}
		})
	}
}
