package project

import (
	"errors"
	"io"
	"reflect"
	"sort"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func saveOn(t *testing.T, p *Project, branch, base string, changes []model.Entry, files map[string][]byte) Commit {
	t.Helper()
	r := Request{ID: NewID(), Branch: branch, Base: base, Author: "tester", Message: "save", Changes: changes}
	return save(t, p, r, files)
}

func mergeRequest(branch, base, source string, choices ...model.Choice) Request {
	return Request{
		ID: NewID(), Branch: branch, Base: base, SecondParent: source,
		Author: "tester", Message: "merge", Choices: choices,
	}
}

func merge(t *testing.T, p *Project, r Request) Commit {
	t.Helper()
	return save(t, p, r, nil)
}

func TestMergePublishesCompleteTreeWithReusedParentObjects(t *testing.T) {
	tp := newTestProject(t)
	ctx := tp.ctx
	rootFiles := map[string][]byte{"common": []byte("common"), "left": []byte("left-0"), "right": []byte("right-0")}
	rootChanges := []model.Entry{fileEntry("common", rootFiles["common"]), fileEntry("left", rootFiles["left"]), fileEntry("right", rootFiles["right"])}
	root := saveOn(t, tp.p, "main", "", rootChanges, rootFiles)
	if err := tp.p.CreateBranch(ctx, "topic", root.ID); err != nil {
		t.Fatal(err)
	}
	leftBytes := []byte("left-1")
	ours := saveOn(t, tp.p, "main", root.ID, []model.Entry{fileEntry("left", leftBytes)}, map[string][]byte{"left": leftBytes})
	rightBytes := []byte("right-1")
	theirs := saveOn(t, tp.p, "topic", root.ID, []model.Entry{fileEntry("right", rightBytes)}, map[string][]byte{"right": rightBytes})

	opened := 0
	r := mergeRequest("main", ours.ID, theirs.ID)
	commit, err := tp.p.Save(ctx, r, Source{
		Write: func(model.Entry, Basis, io.Writer) error {
			opened++
			return errors.New("merge must reuse parent objects")
		},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if opened != 0 {
		t.Fatalf("merge source callbacks = %d, want none", opened)
	}
	if !reflect.DeepEqual(commit.Parents, []string{ours.ID, theirs.ID}) {
		t.Fatalf("merge parents = %v", commit.Parents)
	}
	if got, err := tp.p.Head(ctx, "main"); err != nil || got != commit.ID {
		t.Fatalf("main head = %q, %v; want %q", got, err, commit.ID)
	}
	tip, err := tp.p.Entries(ctx, commit.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Entry{
		fileEntry("common", rootFiles["common"]),
		fileEntry("left", leftBytes),
		fileEntry("right", rightBytes),
	}
	if !reflect.DeepEqual(tip, want) {
		t.Fatalf("merged inventory = %#v, want %#v", tip, want)
	}

	// Accepted retries remain bound to the same result after branch movement and
	// source-branch deletion; no historical payload read or recapture is needed.
	newBytes := []byte("later")
	saveOn(t, tp.p, "main", commit.ID, []model.Entry{fileEntry("later", newBytes)}, map[string][]byte{"later": newBytes})
	if err := tp.p.DeleteBranch(ctx, "topic", theirs.ID); err != nil {
		t.Fatalf("delete source branch: %v", err)
	}
	retried, err := tp.p.Save(ctx, r, Source{Write: func(model.Entry, Basis, io.Writer) error {
		opened++
		return errors.New("accepted retry reopened source")
	}})
	if err != nil || retried.ID != commit.ID || opened != 0 {
		t.Fatalf("accepted merge retry = %#v, %v; source callbacks=%d", retried, err, opened)
	}
	tp.reopen()
	persisted, err := tp.p.commit(ctx, commit.ID)
	if err != nil || !reflect.DeepEqual(persisted.Parents, []string{ours.ID, theirs.ID}) {
		t.Fatalf("reopened merge parents = %v, %v", persisted.Parents, err)
	}
	got, err := tp.p.Entries(ctx, commit.ID, nil)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened merged inventory = %#v, %v", got, err)
	}
	if !reflect.DeepEqual(readFile(t, tp.p, commit.ID, "left"), leftBytes) || !reflect.DeepEqual(readFile(t, tp.p, commit.ID, "right"), rightBytes) {
		t.Fatal("merged content did not retain exact parent bytes after reopen")
	}
}

func TestMergeConflictChoicesCanForceFileAndWholeSubtree(t *testing.T) {
	tp := newTestProject(t)
	rootData := map[string][]byte{"conflict": []byte("base"), "assets/a": []byte("a-base"), "assets/b": []byte("b-base")}
	var changes []model.Entry
	for name, data := range rootData {
		changes = append(changes, fileEntry(name, data))
	}
	root := saveOn(t, tp.p, "main", "", changes, rootData)
	if err := tp.p.CreateBranch(tp.ctx, "topic", root.ID); err != nil {
		t.Fatal(err)
	}
	oursData := map[string][]byte{"conflict": []byte("ours"), "assets/a": []byte("a-ours")}
	var oursChanges []model.Entry
	for name, data := range oursData {
		oursChanges = append(oursChanges, fileEntry(name, data))
	}
	ours := saveOn(t, tp.p, "main", root.ID, oursChanges, oursData)
	theirsData := map[string][]byte{"conflict": []byte("theirs"), "assets/a": []byte("a-theirs"), "assets/b": []byte("b-theirs")}
	var theirsChanges []model.Entry
	for name, data := range theirsData {
		theirsChanges = append(theirsChanges, fileEntry(name, data))
	}
	theirs := saveOn(t, tp.p, "topic", root.ID, theirsChanges, theirsData)

	_, err := tp.p.Save(tp.ctx, mergeRequest("main", ours.ID, theirs.ID), Source{})
	var conflicts *model.Conflicts
	if !errors.As(err, &conflicts) || !reflect.DeepEqual(conflicts.Paths, []string{"assets/a", "conflict"}) {
		t.Fatalf("unresolved merge error = %v, want conflicts assets/a and conflict", err)
	}

	// Exact-file and subtree choices deliberately force the selected side over
	// the entire matching path. The subtree choice therefore discards b-theirs.
	r := mergeRequest("main", ours.ID, theirs.ID,
		model.Choice{Path: "conflict", Ours: true},
		model.Choice{Path: "assets", Ours: true},
	)
	commit, err := tp.p.Save(tp.ctx, r, Source{Write: func(model.Entry, Basis, io.Writer) error {
		t.Fatal("merge choice must reuse parent object references")
		return nil
	}})
	if err != nil {
		t.Fatalf("chosen merge: %v", err)
	}
	if !reflect.DeepEqual(commit.Parents, []string{ours.ID, theirs.ID}) {
		t.Fatalf("chosen merge parents = %v", commit.Parents)
	}
	want := []model.Entry{
		{Path: "assets", Kind: model.Directory},
		fileEntry("assets/a", oursData["assets/a"]),
		fileEntry("assets/b", rootData["assets/b"]),
		fileEntry("conflict", oursData["conflict"]),
	}
	got, err := tp.p.Entries(tp.ctx, commit.ID, nil)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("chosen inventory = %#v, %v; want %#v", got, err, want)
	}
}

func TestMergeIntoEmptyBranchUsesSourceAsFirstParent(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("source tree")
	source := saveOn(t, tp.p, "main", "", []model.Entry{fileEntry("a", data)}, map[string][]byte{"a": data})
	if err := tp.p.CreateBranch(tp.ctx, "empty", ""); err != nil {
		t.Fatal(err)
	}
	r := mergeRequest("empty", "", source.ID)
	commit, err := tp.p.Save(tp.ctx, r, Source{})
	if err != nil {
		t.Fatalf("merge into empty branch: %v", err)
	}
	if !reflect.DeepEqual(commit.Parents, []string{source.ID}) {
		t.Fatalf("empty-branch merge parents = %v, want [%s]", commit.Parents, source.ID)
	}
	got, err := tp.p.Entries(tp.ctx, commit.ID, nil)
	if err != nil || !reflect.DeepEqual(got, []model.Entry{fileEntry("a", data)}) {
		t.Fatalf("empty-branch merge inventory = %#v, %v", got, err)
	}
	tp.reopen()
	persisted, err := tp.p.commit(tp.ctx, commit.ID)
	if err != nil || !reflect.DeepEqual(persisted.Parents, []string{source.ID}) {
		t.Fatalf("reopened empty-branch merge parents = %v, %v", persisted.Parents, err)
	}
	got, err = tp.p.Entries(tp.ctx, commit.ID, nil)
	if err != nil || !reflect.DeepEqual(got, []model.Entry{fileEntry("a", data)}) {
		t.Fatalf("reopened empty-branch merge inventory = %#v, %v", got, err)
	}
}

func TestMergeOperationFingerprintNormalizesChoicesAndBindsAllFields(t *testing.T) {
	tp := newTestProject(t)
	baseData := []byte("base")
	base := saveOn(t, tp.p, "main", "", []model.Entry{fileEntry("a", baseData), fileEntry("b", baseData)}, map[string][]byte{"a": baseData, "b": baseData})
	if err := tp.p.CreateBranch(tp.ctx, "topic", base.ID); err != nil {
		t.Fatal(err)
	}
	oursData := []byte("ours")
	ours := saveOn(t, tp.p, "main", base.ID, []model.Entry{fileEntry("a", oursData)}, map[string][]byte{"a": oursData})
	theirsData := []byte("theirs")
	theirs := saveOn(t, tp.p, "topic", base.ID, []model.Entry{fileEntry("a", theirsData)}, map[string][]byte{"a": theirsData})
	r := mergeRequest("main", ours.ID, theirs.ID,
		model.Choice{Path: "b", Ours: true}, model.Choice{Path: "a", Ours: true})
	accepted := merge(t, tp.p, r)

	reordered := r
	reordered.Choices = []model.Choice{r.Choices[1], r.Choices[0]}
	retried, err := tp.p.Save(tp.ctx, reordered, Source{})
	if err != nil || retried.ID != accepted.ID {
		t.Fatalf("reordered choice retry = %#v, %v", retried, err)
	}
	variants := []struct {
		name string
		edit func(*Request)
	}{
		{"choices", func(q *Request) { q.Choices[0].Ours = false }},
		{"source parent", func(q *Request) { q.SecondParent = ours.ID }},
		{"base", func(q *Request) { q.Base = base.ID }},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			q := r
			q.Choices = append([]model.Choice(nil), r.Choices...)
			variant.edit(&q)
			if _, err := tp.p.Save(tp.ctx, q, Source{}); !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("changed %s error = %v, want ErrOperationConflict", variant.name, err)
			}
		})
	}
}

func TestAlreadyMergedOutcomeSurvivesBranchMovement(t *testing.T) {
	tp := newTestProject(t)
	firstData := []byte("first")
	first := saveOn(t, tp.p, "main", "", []model.Entry{fileEntry("a", firstData)}, map[string][]byte{"a": firstData})
	if err := tp.p.CreateBranch(tp.ctx, "older", first.ID); err != nil {
		t.Fatal(err)
	}
	nextData := []byte("next")
	second := saveOn(t, tp.p, "main", first.ID, []model.Entry{fileEntry("b", nextData)}, map[string][]byte{"b": nextData})
	r := mergeRequest("main", second.ID, first.ID)
	if _, err := tp.p.Save(tp.ctx, r, Source{}); !errors.Is(err, ErrAlreadyMerged) {
		t.Fatalf("already merged result = %v", err)
	}
	laterData := []byte("later")
	saveOn(t, tp.p, "main", second.ID, []model.Entry{fileEntry("c", laterData)}, map[string][]byte{"c": laterData})
	if _, err := tp.p.Save(tp.ctx, r, Source{}); !errors.Is(err, ErrAlreadyMerged) {
		t.Fatalf("already merged retry after head movement = %v", err)
	}
}

func TestMergeStaleBranchCASIsDurable(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("base")
	base := saveOn(t, tp.p, "main", "", []model.Entry{fileEntry("a", data)}, map[string][]byte{"a": data})
	if err := tp.p.CreateBranch(tp.ctx, "topic", base.ID); err != nil {
		t.Fatal(err)
	}
	topicData := []byte("topic")
	topic := saveOn(t, tp.p, "topic", base.ID, []model.Entry{fileEntry("b", topicData)}, map[string][]byte{"b": topicData})
	mainData := []byte("main")
	main := saveOn(t, tp.p, "main", base.ID, []model.Entry{fileEntry("c", mainData)}, map[string][]byte{"c": mainData})
	r := mergeRequest("main", base.ID, topic.ID)
	if _, err := tp.p.Save(tp.ctx, r, Source{}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale merge error = %v", err)
	}
	latestData := []byte("latest")
	latest := saveOn(t, tp.p, "main", main.ID, []model.Entry{fileEntry("d", latestData)}, map[string][]byte{"d": latestData})
	if _, err := tp.p.Save(tp.ctx, r, Source{}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale merge retry after further advance = %v", err)
	}
	if got, err := tp.p.Head(tp.ctx, "main"); err != nil || got != latest.ID {
		t.Fatalf("stale merge changed main tip to %q, %v", got, err)
	}
}

func TestMergeBaseFindsAllParentsAndRequiresExplicitCrissCrossBase(t *testing.T) {
	tp := newTestProject(t)
	rootData := []byte("root")
	root := saveOn(t, tp.p, "main", "", []model.Entry{fileEntry("root", rootData)}, map[string][]byte{"root": rootData})
	if err := tp.p.CreateBranch(tp.ctx, "side", root.ID); err != nil {
		t.Fatal(err)
	}
	aData := []byte("a")
	a := saveOn(t, tp.p, "main", root.ID, []model.Entry{fileEntry("a", aData)}, map[string][]byte{"a": aData})
	bData := []byte("b")
	b := saveOn(t, tp.p, "side", root.ID, []model.Entry{fileEntry("b", bData)}, map[string][]byte{"b": bData})
	if err := tp.p.CreateBranch(tp.ctx, "at-a", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.CreateBranch(tp.ctx, "at-b", b.ID); err != nil {
		t.Fatal(err)
	}
	m1 := merge(t, tp.p, mergeRequest("at-a", a.ID, b.ID))
	m2 := merge(t, tp.p, mergeRequest("at-b", b.ID, a.ID))
	if !reflect.DeepEqual(m1.Parents, []string{a.ID, b.ID}) || !reflect.DeepEqual(m2.Parents, []string{b.ID, a.ID}) {
		t.Fatalf("criss-cross parent lists: m1=%v m2=%v", m1.Parents, m2.Parents)
	}

	r := mergeRequest("at-a", m1.ID, m2.ID)
	_, err := tp.p.Save(tp.ctx, r, Source{})
	var multi *MergeBaseConflict
	if !errors.As(err, &multi) {
		t.Fatalf("criss-cross merge error = %v, want MergeBaseConflict", err)
	}
	wantBases := []string{a.ID, b.ID}
	sort.Strings(wantBases)
	if !reflect.DeepEqual(multi.Bases, wantBases) {
		t.Fatalf("best merge bases = %v, want %v", multi.Bases, wantBases)
	}

	if err := tp.p.CreateBranch(tp.ctx, "unrelated", ""); err != nil {
		t.Fatal(err)
	}
	unrelatedData := []byte("unrelated")
	unrelated := saveOn(t, tp.p, "unrelated", "", []model.Entry{fileEntry("unrelated", unrelatedData)}, map[string][]byte{"unrelated": unrelatedData})
	bad := mergeRequest("at-a", m1.ID, m2.ID)
	bad.MergeBase = unrelated.ID
	if _, err := tp.p.Save(tp.ctx, bad, Source{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("noncommon selected merge base error = %v", err)
	}
	valid := mergeRequest("at-a", m1.ID, m2.ID, model.Choice{Path: "", Ours: true})
	valid.MergeBase = a.ID
	commit, err := tp.p.Save(tp.ctx, valid, Source{})
	if err != nil {
		t.Fatalf("merge with explicit common base: %v", err)
	}
	if !reflect.DeepEqual(commit.Parents, []string{m1.ID, m2.ID}) {
		t.Fatalf("explicit-base merge parents = %v", commit.Parents)
	}
}
