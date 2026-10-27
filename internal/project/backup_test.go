package project

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestBackupPreservesHistoryOutcomesAndIndependentStores(t *testing.T) {
	ctx := context.Background()
	tp := newTestProject(t)
	versions := make([]Commit, 4)
	base := ""
	for i := range versions {
		data := []byte{0, byte(i), 0xff}
		versions[i] = saveOn(t, tp.p, "main", base, []model.Entry{fileEntry("opaque.bin", data)}, map[string][]byte{"opaque.bin": data})
		base = versions[i].ID
	}
	if err := tp.p.CreateBranch(ctx, "release", versions[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.CreateTag(ctx, "pinned", versions[0].ID); err != nil {
		t.Fatal(err)
	}
	acceptedReq := request(fileEntry("accepted.bin", []byte("accepted")))
	acceptedReq.Base = versions[3].ID
	accepted, err := tp.p.Save(ctx, acceptedReq, sourceFor(map[string][]byte{"accepted.bin": []byte("accepted")}))
	if err != nil {
		t.Fatal(err)
	}
	cancelledReq := request(fileEntry("cancelled.bin", []byte("never accepted")))
	if err := tp.p.Cancel(ctx, cancelledReq.ID); err != nil {
		t.Fatal(err)
	}
	plan, err := tp.p.PreviewCleanup(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.p.ApplyCleanup(ctx, 0, plan.Token); err != nil {
		t.Fatal(err)
	}
	projectID, projectToken := tp.p.ID(), tp.p.Token()

	backup := filepath.Join(t.TempDir(), "backup")
	if err := tp.p.Close(); err != nil {
		t.Fatal(err)
	}
	tp.p = nil
	if err := Backup(ctx, tp.dir, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	assertBackupHasNoDerivedArtifacts(t, backup)
	restored := filepath.Join(t.TempDir(), "restored")
	if err := Backup(ctx, backup, restored); err != nil {
		t.Fatalf("restore by copying backup: %v", err)
	}
	assertBackupHasNoDerivedArtifacts(t, restored)

	for _, dir := range []string{backup, restored} {
		p, err := Open(ctx, dir)
		if err != nil {
			t.Fatalf("open %s: %v", dir, err)
		}
		if p.ID() != projectID || p.Token() != projectToken {
			t.Errorf("project identity changed in %s: id=%q token=%q", dir, p.ID(), p.Token())
		}
		history, err := p.History(ctx, "main", 20)
		if err != nil || len(history) != 5 {
			t.Fatalf("history in %s: %d commits, %v", dir, len(history), err)
		}
		branches, err := p.Branches(ctx)
		if err != nil || len(branches) != 2 || branches[0].Name != "main" || branches[1].Name != "release" {
			t.Fatalf("branches in %s: %#v, %v", dir, branches, err)
		}
		tags, err := p.Tags(ctx)
		if err != nil || len(tags) != 1 || tags[0].Name != "pinned" || tags[0].Commit != versions[0].ID {
			t.Fatalf("tags in %s: %#v, %v", dir, tags, err)
		}
		if history[1].Retained || history[2].Retained {
			t.Errorf("expired versions became retained in %s: %#v %#v", dir, history[1], history[2])
		}
		got := readFile(t, p, accepted.ID, "accepted.bin")
		if !bytes.Equal(got, []byte("accepted")) {
			t.Errorf("accepted outcome contents in %s = %q", dir, got)
		}
		if _, err := p.Save(ctx, acceptedReq, Source{}); err != nil {
			t.Errorf("accepted retry in %s: %v", dir, err)
		}
		if _, err := p.Save(ctx, cancelledReq, sourceFor(map[string][]byte{"cancelled.bin": []byte("never accepted")})); !errors.Is(err, ErrCancelled) {
			t.Errorf("cancelled outcome in %s = %v", dir, err)
		}
		if dir == restored {
			want := []byte{0, 0, 0xff}
			if got := readFile(t, p, versions[0].ID, "opaque.bin"); !bytes.Equal(got, want) {
				t.Errorf("oldest tagged payload in restored copy = %v, want %v", got, want)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.RemoveAll(tp.dir); err != nil {
		t.Fatal(err)
	}
	original, err := Open(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	copy, err := Open(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if got := readFile(t, original, accepted.ID, "accepted.bin"); !bytes.Equal(got, []byte("accepted")) {
		t.Fatalf("bytes after original removal = %q", got)
	}
	left := []byte("backup fork")
	right := []byte("restored fork")
	leftReq := request(fileEntry("fork.bin", left))
	leftReq.Base = accepted.ID
	rightReq := request(fileEntry("fork.bin", right))
	rightReq.Base = accepted.ID
	if _, err := original.Save(ctx, leftReq, sourceFor(map[string][]byte{"fork.bin": left})); err != nil {
		t.Fatal(err)
	}
	if _, err := copy.Save(ctx, rightReq, sourceFor(map[string][]byte{"fork.bin": right})); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, original, leftReq.ID, "fork.bin"); !bytes.Equal(got, left) {
		t.Fatalf("backup fork = %q", got)
	}
	if got := readFile(t, copy, rightReq.ID, "fork.bin"); !bytes.Equal(got, right) {
		t.Fatalf("restored fork = %q", got)
	}
}

func assertBackupHasNoDerivedArtifacts(t *testing.T, dir string) {
	t.Helper()
	for _, path := range []string{"lock", "content/cache", "content/kopia.config"} {
		if _, err := os.Lstat(filepath.Join(dir, path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("derived artifact %s exists in %s: %v", path, dir, err)
		}
	}
}

func TestBackupRejectsUnsafeSourcesAndDestinations(t *testing.T) {
	ctx := context.Background()
	tp := newTestProject(t)
	if err := Backup(ctx, tp.dir, filepath.Join(t.TempDir(), "busy")); err == nil {
		t.Fatal("backup of open project succeeded")
	}
	inside := filepath.Join(tp.dir, "nested")
	if err := Backup(ctx, tp.dir, inside); err == nil {
		t.Fatal("backup inside source succeeded")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(tp.dir, alias); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, alias, filepath.Join(t.TempDir(), "alias-copy")); err == nil {
		t.Fatal("backup through source alias succeeded while original is open")
	}

	if err := tp.p.Close(); err != nil {
		t.Fatal(err)
	}
	tp.p = nil
	if err := Backup(ctx, alias, filepath.Join(t.TempDir(), "alias-copy")); err != nil {
		t.Fatalf("backup through source alias: %v", err)
	}
	destinationAlias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(tp.dir, destinationAlias); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, tp.dir, filepath.Join(destinationAlias, "copy")); err == nil {
		t.Fatal("backup destination through an alias inside the source succeeded")
	}
	destination := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "keep")
	if err := os.WriteFile(marker, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, tp.dir, destination); err == nil {
		t.Fatal("backup replaced existing destination")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "untouched" {
		t.Fatalf("existing destination marker = %q, %v", got, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cancelDest := filepath.Join(t.TempDir(), "cancelled")
	if err := Backup(cancelled, tp.dir, cancelDest); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled backup error = %v", err)
	}
	if _, err := os.Lstat(cancelDest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled final destination exists: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(cancelDest), ".cancelled.partial")); len(matches) != 0 {
		t.Fatalf("cancelled staging directory remains: %v", matches)
	}

	if err := os.Symlink("missing-target", filepath.Join(tp.dir, "content", "repository", "unsupported-link")); err != nil {
		t.Fatal(err)
	}
	badDest := filepath.Join(t.TempDir(), "unsupported")
	if err := Backup(ctx, tp.dir, badDest); err == nil || !strings.Contains(err.Error(), "unsupported backup entry") {
		t.Fatalf("native symlink backup error = %v", err)
	}
	if _, err := os.Lstat(badDest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported source published destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tp.dir, "project.json")); err != nil {
		t.Fatalf("source damaged by failed backup: %v", err)
	}
}
