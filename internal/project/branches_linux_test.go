package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestBranchesKeepIndependentTipsAndResolveVersions(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	first := publish(t, s, "", []model.Entry{file("asset", "first")}, map[string]string{"asset": "first"})
	if _, err := s.CreateBranch(ctx, "experiment/model", first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.Save(ctx, "alice", model.Save{ID: operation(t), Branch: "experiment/model", Base: first.ID, Changes: []model.Entry{file("asset", "experiment")}}, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error {
		_, err := io.WriteString(w, "experiment")
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if head, err := s.Head(ctx, "main"); err != nil || head != first.ID {
		t.Fatalf("main advanced: %s %v", head, err)
	}
	for _, ref := range []string{second.ID, model.ShortVersion(second.ID), "experiment/model"} {
		if got, err := s.Reference(ctx, ref); err != nil || got != second {
			t.Fatalf("resolve %s: %v %v", ref, got, err)
		}
	}
	if _, err := s.CreateBranch(ctx, "experiment/model", first.ID); !errors.Is(err, ErrBranchExists) {
		t.Fatalf("creation reset a branch: %v", err)
	}
	if _, err := s.CreateBranch(ctx, model.ShortVersion(second.ID), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reference(ctx, model.ShortVersion(second.ID)); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous reference: %v", err)
	}
	if got, err := s.Reference(ctx, second.ID); err != nil || got.ID != second.ID {
		t.Fatalf("complete ID: %v %v", got, err)
	}
	if err := s.DeleteBranch(ctx, "experiment/model", first.ID); !errors.Is(err, ErrBranchChanged) {
		t.Fatalf("stale deletion: %v", err)
	}
	if err := s.DeleteBranch(ctx, "main", first.ID); !errors.Is(err, ErrRequest) {
		t.Fatalf("deleted default: %v", err)
	}
	for range 2 {
		if err := s.DeleteBranch(ctx, "experiment/model", second.ID); err != nil {
			t.Fatal(err)
		}
	}
	if read(t, s, second.ID, "asset") != "experiment" {
		t.Fatal("deleting a branch removed its contents")
	}
	if _, err := f.db.Exec("UPDATE versions SET protection='' WHERE project=? AND id=?", s.id, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBranch(ctx, "expired", second.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired branch source: %v", err)
	}
	for _, name := range []string{"", "../escape", "a/../b", "space name", "a//b", "-option"} {
		if _, err := s.CreateBranch(ctx, name, first.ID); !errors.Is(err, ErrRequest) {
			t.Fatalf("invalid branch %q: %v", name, err)
		}
	}
}

func TestBranchListPagesAndEmptyReferences(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	for i := 0; i < 104; i++ {
		if _, err := s.CreateBranch(ctx, fmt.Sprintf("branch-%03d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	after := ""
	for {
		branches, next, err := s.Branches(ctx, after)
		if err != nil || len(branches) > 100 {
			t.Fatalf("page: %d %v", len(branches), err)
		}
		for _, b := range branches {
			if seen[b.Name] || b.Version != "" {
				t.Fatalf("unexpected branch: %v", b)
			}
			seen[b.Name] = true
		}
		if next == "" {
			break
		}
		if next <= after {
			t.Fatal("cursor did not advance")
		}
		after = next
	}
	if len(seen) != 105 {
		t.Fatalf("listed %d branches", len(seen))
	}
	if v, err := s.Reference(ctx, "main"); err != nil || v.ID != "" {
		t.Fatalf("empty branch: %v %v", v, err)
	}
}
