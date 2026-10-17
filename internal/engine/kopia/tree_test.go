package kopia_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/kopia/kopia/repo"

	engine "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func fullSelection() model.Selection { return (model.Selection{}).Include(model.Path{}) }

func lookup(t *testing.T, f *fixture, root engine.Entry, name string) engine.Entry {
	t.Helper()
	e, err := engine.Lookup(f.ctx, f.rep, root, path(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestTreeChangesPreserveOmittedDescendants(t *testing.T) {
	f := newFixture(t)
	var root, hero, library, replacement engine.Entry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		hero, err = engine.CaptureFile(ctx, w, bytes.NewBufferString("hero contents"), false)
		if err != nil {
			return err
		}
		library, err = engine.CaptureFile(ctx, w, bytes.NewBufferString("omitted contents"), false)
		if err != nil {
			return err
		}
		replacement, err = engine.CaptureFile(ctx, w, bytes.NewBufferString("replacement"), false)
		if err != nil {
			return err
		}
		root, err = engine.Apply(ctx, w, engine.Entry{}, fullSelection(), []engine.Change{
			{Path: path(t, "assets/hero.blend"), After: hero},
			{Path: path(t, "assets/library.pack"), After: library},
		})
		return err
	})
	assets := lookup(t, f, root, "assets")
	partial := (model.Selection{}).Include(path(t, "assets/hero.blend"))
	var removed engine.Entry
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		var err error
		removed, err = engine.Apply(ctx, w, root, partial, []engine.Change{
			{Path: path(t, "assets"), Before: assets},
			{Path: path(t, "assets/hero.blend"), Before: hero},
		})
		return err
	})
	if lookup(t, f, removed, "assets/hero.blend").State().Kind != model.Absent {
		t.Fatal("selected deletion was not applied")
	}
	if lookup(t, f, removed, "assets/library.pack").State() != library.State() {
		t.Fatal("an omitted descendant changed")
	}
	if lookup(t, f, removed, "assets").State().Kind != model.Directory {
		t.Fatal("a parent containing omitted content was removed")
	}
	if lookup(t, f, root, "assets/hero.blend").State() != hero.State() {
		t.Fatal("editing a candidate mutated its base")
	}

	t.Run("cannot replace a parent while retaining descendants", func(t *testing.T) {
		err := repo.WriteSession(f.ctx, f.rep, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
			_, err := engine.Apply(ctx, w, root, fullSelection().Exclude(path(t, "assets/library.pack")), []engine.Change{
				{Path: path(t, "assets"), Before: assets, After: replacement},
				{Path: path(t, "assets/hero.blend"), Before: hero},
			})
			return err
		})
		if !errors.Is(err, engine.ErrStructure) {
			t.Fatalf("got %v, want structural conflict", err)
		}
	})
	t.Run("can replace a directory after exact child removals", func(t *testing.T) {
		f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
			replaced, err := engine.Apply(ctx, w, root, fullSelection(), []engine.Change{
				{Path: path(t, "assets"), Before: assets, After: replacement},
				{Path: path(t, "assets/hero.blend"), Before: hero},
				{Path: path(t, "assets/library.pack"), Before: library},
			})
			if err != nil {
				return err
			}
			entry, err := engine.Lookup(ctx, w, replaced, path(t, "assets"))
			if err == nil && entry.State() != replacement.State() {
				t.Fatal("wrong replacement")
			}
			return err
		})
	})
	for _, tc := range []struct {
		name      string
		selection model.Selection
		changes   []engine.Change
		want      error
	}{
		{"excluded file", partial, []engine.Change{{Path: path(t, "assets/library.pack"), Before: library}}, engine.ErrOutsideSelection},
		{"stale before", fullSelection(), []engine.Change{{Path: path(t, "assets/hero.blend"), After: replacement}}, model.ErrConflict},
		{"duplicate path", fullSelection(), []engine.Change{{Path: path(t, "new"), After: hero}, {Path: path(t, "new"), After: hero}}, engine.ErrInvalidChange},
		{"file ancestor", fullSelection(), []engine.Change{{Path: path(t, "assets/hero.blend/child"), After: hero}}, engine.ErrStructure},
		{"root replacement", fullSelection(), []engine.Change{{After: hero}}, engine.ErrInvalidChange},
		{"remove and add beneath", fullSelection(), []engine.Change{{Path: path(t, "assets"), Before: assets}, {Path: path(t, "assets/new"), After: hero}}, engine.ErrInvalidChange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := repo.WriteSession(f.ctx, f.rep, repo.WriteSessionOptions{}, func(ctx context.Context, w repo.RepositoryWriter) error {
				_, err := engine.Apply(ctx, w, root, tc.selection, tc.changes)
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEmptyProjectAndExplicitDirectoryIntent(t *testing.T) {
	f := newFixture(t)
	f.write(func(ctx context.Context, w repo.RepositoryWriter) error {
		empty, err := engine.Apply(ctx, w, engine.Entry{}, model.Selection{}, nil)
		if err != nil {
			return err
		}
		if empty.State().Kind != model.Directory {
			t.Fatal("empty project is not a complete directory")
		}
		root, err := engine.Apply(ctx, w, empty, fullSelection(), []engine.Change{{Path: path(t, "empty"), After: engine.Directory()}})
		if err != nil {
			return err
		}
		entry, err := engine.Lookup(ctx, w, root, path(t, "empty"))
		if err == nil && entry.State().Kind != model.Directory {
			t.Fatal("empty directory was lost")
		}
		return err
	})
}
