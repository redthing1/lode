package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
)

func TestTagsUseIdempotentCreateAndCompareDelete(t *testing.T) {
	tp := newTestProject(t)
	a := save(t, tp.p, request(fileEntry("a", []byte("a"))), map[string][]byte{"a": []byte("a")})
	bReq := request(fileEntry("b", []byte("b")))
	bReq.Base = a.ID
	b := save(t, tp.p, bReq, map[string][]byte{"b": []byte("b")})
	if err := tp.p.CreateTag(tp.ctx, "release", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.CreateTag(tp.ctx, "release", a.ID); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if err := tp.p.CreateTag(tp.ctx, "release", b.ID); !errors.Is(err, ErrExists) {
		t.Fatalf("conflicting create = %v", err)
	}
	if err := tp.p.DeleteTag(tp.ctx, "release", b.ID); !errors.Is(err, ErrReferenceChanged) {
		t.Fatalf("wrong expected commit delete = %v", err)
	}
	if got, err := tp.p.Tags(tp.ctx); err != nil || !reflect.DeepEqual(got, []Tag{{Name: "release", Commit: a.ID}}) {
		t.Fatalf("tags = %#v, %v", got, err)
	}
	if err := tp.p.DeleteTag(tp.ctx, "release", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.DeleteTag(tp.ctx, "release", a.ID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestCleanupKeepNPreservesTipsTagsAndFirstParentHistory(t *testing.T) {
	tp := newTestProject(t)
	ctx := tp.ctx
	versions := make([]Commit, 6)
	base := ""
	for i := range versions {
		data := []byte{byte('a' + i)}
		versions[i] = saveOn(t, tp.p, "main", base, []model.Entry{fileEntry("f", data)}, map[string][]byte{"f": data})
		base = versions[i].ID
	}
	if err := tp.p.CreateBranch(ctx, "side", versions[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.CreateTag(ctx, "pinned", versions[0].ID); err != nil {
		t.Fatal(err)
	}
	plan, err := tp.p.PreviewCleanup(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]RetentionVersion{}
	for _, v := range plan.Versions {
		byID[v.Commit.ID] = v
	}
	// Keep counts the tip: Keep=2 retains the tip and one first-parent version.
	for _, id := range []string{versions[5].ID, versions[4].ID, versions[1].ID, versions[0].ID} {
		if len(byID[id].Reasons) == 0 {
			t.Errorf("expected %s protected, reasons=%v", id, byID[id].Reasons)
		}
	}
	if len(byID[versions[0].ID].Reasons) < 2 {
		t.Errorf("side branch history and tag reasons missing: %v", byID[versions[0].ID].Reasons)
	}
	for _, id := range []string{versions[3].ID, versions[2].ID} {
		if len(byID[id].Reasons) != 0 {
			t.Errorf("version outside tips, tags, and KeepN ancestry is protected: %v", byID[id].Reasons)
		}
	}
	if _, err := tp.p.ApplyCleanup(ctx, 2, plan.Token); err != nil {
		t.Fatal(err)
	}
	// Remove the side branch and tag, then KeepN=0 leaves only the current tip.
	if err := tp.p.DeleteBranch(ctx, "side", versions[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.DeleteTag(ctx, "pinned", versions[0].ID); err != nil {
		t.Fatal(err)
	}
	plan, err = tp.p.PreviewCleanup(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Versions) != len(versions) {
		t.Fatalf("versions listed=%d", len(plan.Versions))
	}
	for _, v := range plan.Versions {
		if v.Commit.ID == versions[5].ID && len(v.Reasons) == 0 {
			t.Fatal("main tip is not protected")
		}
		if v.Commit.ID != versions[5].ID && len(v.Reasons) != 0 {
			t.Fatalf("non-tip %s unexpectedly protected: %v", v.Commit.ID, v.Reasons)
		}
	}
}

func TestApplyCleanupInvalidatesOnHistoryAndReferenceChanges(t *testing.T) {
	for _, mutation := range []string{"save", "tag", "branch-create"} {
		t.Run(mutation, func(t *testing.T) {
			tp := newTestProject(t)
			first := save(t, tp.p, request(fileEntry("a", []byte("a"))), map[string][]byte{"a": []byte("a")})
			plan, err := tp.p.PreviewCleanup(tp.ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "save":
				saveOn(t, tp.p, "main", first.ID, []model.Entry{fileEntry("b", []byte("b"))}, map[string][]byte{"b": []byte("b")})
			case "tag":
				if err := tp.p.CreateTag(tp.ctx, "saved", first.ID); err != nil {
					t.Fatal(err)
				}
			case "branch-create":
				if err := tp.p.CreateBranch(tp.ctx, "side", first.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tp.p.ApplyCleanup(tp.ctx, 0, plan.Token); !errors.Is(err, ErrCleanupChanged) {
				t.Fatalf("apply after %s = %v", mutation, err)
			}
		})
	}
}

func TestCleanupExpiryKeepsLogicalHistoryAndAcceptedRetry(t *testing.T) {
	tp := newTestProject(t)
	data := []byte{0, 0xff, 0x10}
	r := request(fileEntry("opaque.bin", data))
	commit := save(t, tp.p, r, map[string][]byte{"opaque.bin": data})
	secondData := []byte("new")
	saveOn(t, tp.p, "main", commit.ID, []model.Entry{fileEntry("new", secondData)}, map[string][]byte{"new": secondData})
	plan, err := tp.p.PreviewCleanup(tp.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.p.ApplyCleanup(tp.ctx, 0, plan.Token); err != nil {
		t.Fatal(err)
	}
	if n, err := tp.p.ApplyCleanup(tp.ctx, 0, plan.Token); err != nil || n != 0 {
		t.Fatalf("same cleanup plan retry = %d, %v; want zero newly expired roots", n, err)
	}
	entries, err := tp.p.Entries(tp.ctx, commit.ID, nil)
	if err != nil || !reflect.DeepEqual(entries, []model.Entry{fileEntry("opaque.bin", data)}) {
		t.Fatalf("expired logical entries = %#v, %v", entries, err)
	}
	history, err := tp.p.History(tp.ctx, "main", 10)
	if err != nil || len(history) != 2 || history[1].ID != commit.ID || history[1].Retained {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if err := tp.p.Read(tp.ctx, commit.ID, "opaque.bin", func(model.Entry, io.Reader) error { return nil }); !errors.Is(err, ErrExpired) {
		t.Fatalf("read expired = %v", err)
	}
	if err := tp.p.CreateBranch(tp.ctx, "old", commit.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("branch expired = %v", err)
	}
	if err := tp.p.CreateTag(tp.ctx, "old", commit.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("tag expired = %v", err)
	}
	retried, err := tp.p.Save(tp.ctx, r, Source{Write: func(model.Entry, Basis, io.Writer) error { return errors.New("must not read") }})
	if err != nil || retried.ID != commit.ID || retried.Retained || retried.Retained == commit.Retained {
		t.Fatalf("accepted expired retry = %#v, %v", retried, err)
	}
}

func TestCleanupExpiresOnlyAfterApplyAndReopenReleasesRoots(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("payload")
	old := save(t, tp.p, request(fileEntry("a", data)), map[string][]byte{"a": data})
	newer := saveOn(t, tp.p, "main", old.ID, []model.Entry{fileEntry("b", []byte("b"))}, map[string][]byte{"b": []byte("b")})
	if _, err := tp.p.contents.Roots(tp.ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := tp.p.PreviewCleanup(tp.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.p.ApplyCleanup(tp.ctx, 0, plan.Token); err != nil {
		t.Fatal(err)
	}
	roots, err := tp.p.contents.Roots(tp.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots[old.ID]) == 0 {
		t.Fatal("ApplyCleanup released native root before Collect/reopen")
	}
	if got := readFile(t, tp.p, newer.ID, "a"); !bytes.Equal(got, data) {
		t.Fatalf("retained inherited bytes=%q", got)
	}
	tp.reopen()
	roots, err = tp.p.contents.Roots(tp.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots[old.ID]) != 0 {
		t.Fatalf("reopen left expired roots: %v", roots[old.ID])
	}
	if got := readFile(t, tp.p, newer.ID, "a"); !bytes.Equal(got, data) {
		t.Fatalf("reopened retained bytes=%q", got)
	}
}

func TestActiveReadBlocksCleanupAndCompletesWithOriginalBytes(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("read remains protected")
	commit := save(t, tp.p, request(fileEntry("a", data)), map[string][]byte{"a": data})
	saveOn(t, tp.p, "main", commit.ID, []model.Entry{fileEntry("b", []byte("new tip"))}, map[string][]byte{"b": []byte("new tip")})
	entered, release, readDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		readDone <- tp.p.Read(context.Background(), commit.ID, "a", func(_ model.Entry, r io.Reader) error {
			close(entered)
			<-release
			got, err := io.ReadAll(r)
			if err == nil && !bytes.Equal(got, data) {
				return errors.New("read bytes changed")
			}
			return err
		})
	}()
	<-entered
	plan, err := tp.p.PreviewCleanup(tp.ctx, 0)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	applyDone := make(chan error, 1)
	go func() { _, err := tp.p.ApplyCleanup(tp.ctx, 0, plan.Token); applyDone <- err }()
	select {
	case err := <-applyDone:
		close(release)
		t.Fatalf("cleanup passed active read gate: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
	if err := tp.p.Read(tp.ctx, commit.ID, "a", func(model.Entry, io.Reader) error { return nil }); !errors.Is(err, ErrExpired) {
		t.Fatalf("old version after blocked cleanup completes = %v, want ErrExpired", err)
	}
}

func TestMergeUsesRetainedParentsAfterCommonBaseExpires(t *testing.T) {
	tp := newTestProject(t)
	ctx := tp.ctx
	baseData := []byte("base")
	base := saveOn(t, tp.p, "main", "", []model.Entry{fileEntry("base", baseData)}, map[string][]byte{"base": baseData})
	if err := tp.p.CreateBranch(ctx, "topic", base.ID); err != nil {
		t.Fatal(err)
	}
	leftData, rightData := []byte("left"), []byte("right")
	left := saveOn(t, tp.p, "main", base.ID, []model.Entry{fileEntry("left", leftData)}, map[string][]byte{"left": leftData})
	right := saveOn(t, tp.p, "topic", base.ID, []model.Entry{fileEntry("right", rightData)}, map[string][]byte{"right": rightData})
	plan, err := tp.p.PreviewCleanup(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.p.ApplyCleanup(ctx, 0, plan.Token); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.Read(ctx, base.ID, "base", func(model.Entry, io.Reader) error { return nil }); !errors.Is(err, ErrExpired) {
		t.Fatalf("common base read = %v", err)
	}
	merged, err := tp.p.Save(ctx, mergeRequest("main", left.ID, right.ID), Source{})
	if err != nil {
		t.Fatalf("merge with expired common base: %v", err)
	}
	entries, err := tp.p.Entries(ctx, merged.ID, nil)
	want := []model.Entry{fileEntry("base", baseData), fileEntry("left", leftData), fileEntry("right", rightData)}
	if err != nil || !reflect.DeepEqual(entries, want) {
		t.Fatalf("merged entries = %#v, %v; want %#v", entries, err, want)
	}
	if got := readFile(t, tp.p, merged.ID, "base"); !bytes.Equal(got, baseData) {
		t.Fatalf("inherited base bytes = %q", got)
	}
}

func TestUnacceptedMergeRejectsExpiredSource(t *testing.T) {
	tp := newTestProject(t)
	ctx := tp.ctx
	baseData := []byte("base")
	base := saveOn(t, tp.p, "main", "", []model.Entry{fileEntry("base", baseData)}, map[string][]byte{"base": baseData})
	if err := tp.p.CreateBranch(ctx, "topic", base.ID); err != nil {
		t.Fatal(err)
	}
	mainData := []byte("main")
	main := saveOn(t, tp.p, "main", base.ID, []model.Entry{fileEntry("main", mainData)}, map[string][]byte{"main": mainData})
	sourceData := []byte("source")
	source := saveOn(t, tp.p, "topic", base.ID, []model.Entry{fileEntry("source", sourceData)}, map[string][]byte{"source": sourceData})
	if err := tp.p.DeleteBranch(ctx, "topic", source.ID); err != nil {
		t.Fatal(err)
	}
	plan, err := tp.p.PreviewCleanup(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.p.ApplyCleanup(ctx, 0, plan.Token); err != nil {
		t.Fatal(err)
	}
	r := mergeRequest("main", main.ID, source.ID)
	if _, err := tp.p.Save(ctx, r, Source{}); !errors.Is(err, ErrExpired) {
		t.Fatalf("merge from expired source = %v", err)
	}
}
