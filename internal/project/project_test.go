package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

type testProject struct {
	t   *testing.T
	ctx context.Context
	dir string
	p   *Project
}

func newTestProject(t *testing.T) *testProject {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "project")
	p, err := Create(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	tp := &testProject{t: t, ctx: ctx, dir: dir, p: p}
	t.Cleanup(func() {
		if tp.p != nil {
			if err := tp.p.Close(); err != nil {
				t.Errorf("close project: %v", err)
			}
		}
	})
	return tp
}

func (tp *testProject) reopen() {
	tp.t.Helper()
	if err := tp.p.Close(); err != nil {
		tp.t.Fatal(err)
	}
	tp.p = nil
	var err error
	tp.p, err = Open(tp.ctx, tp.dir)
	if err != nil {
		tp.t.Fatalf("reopen project: %v", err)
	}
}

func fileEntry(name string, data []byte) model.Entry {
	sum := sha256.Sum256(data)
	return model.Entry{Path: name, Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func sourceFor(files map[string][]byte) Source {
	return Source{Write: func(entry model.Entry, _ Basis, out io.Writer) error {
		data, ok := files[entry.Path]
		if !ok {
			return os.ErrNotExist
		}
		_, err := io.Copy(out, bytes.NewReader(data))
		return err
	}}
}

func request(changes ...model.Entry) Request {
	return Request{ID: NewID(), Branch: "main", Author: "tester", Message: "save", Changes: changes}
}

func save(t *testing.T, p *Project, r Request, files map[string][]byte) Commit {
	t.Helper()
	c, err := p.Save(context.Background(), r, sourceFor(files))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	return c
}

func readFile(t *testing.T, p *Project, id, name string) []byte {
	t.Helper()
	var result []byte
	err := p.Read(context.Background(), id, name, func(_ model.Entry, r io.Reader) error {
		var err error
		result, err = io.ReadAll(r)
		return err
	})
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return result
}

func TestPartialSavePreservesOmittedFilesAndBytesAfterReopen(t *testing.T) {
	tp := newTestProject(t)
	first := save(t, tp.p, request(fileEntry("assets/a.bin", []byte{0, 1, 0xff}), fileEntry("data/b.bin", []byte("unchanged"))), map[string][]byte{
		"assets/a.bin": {0, 1, 0xff}, "data/b.bin": []byte("unchanged"),
	})
	change := fileEntry("assets/a.bin", []byte{0, 2, 0xfe})
	r := request(change)
	r.Base = first.ID
	second := save(t, tp.p, r, map[string][]byte{"assets/a.bin": {0, 2, 0xfe}})
	tp.reopen()
	if got := readFile(t, tp.p, second.ID, "assets/a.bin"); !bytes.Equal(got, []byte{0, 2, 0xfe}) {
		t.Fatalf("changed bytes = %v", got)
	}
	if got := readFile(t, tp.p, second.ID, "data/b.bin"); !bytes.Equal(got, []byte("unchanged")) {
		t.Fatalf("omitted file bytes = %q", got)
	}
}

func TestAcceptedRetryIsStableAndDoesNotReadSource(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("first")
	r := request(fileEntry("a", data))
	accepted := save(t, tp.p, r, map[string][]byte{"a": data})
	newerData := []byte("second")
	save(t, tp.p, func() Request { q := request(fileEntry("b", newerData)); q.Base = accepted.ID; return q }(), map[string][]byte{"b": newerData})
	calls := 0
	retried, err := tp.p.Save(tp.ctx, r, Source{Write: func(model.Entry, Basis, io.Writer) error { calls++; return errors.New("unexpected source read") }, Finish: func() error { calls++; return errors.New("unexpected finish") }})
	if err != nil {
		t.Fatalf("accepted retry: %v", err)
	}
	if retried.ID != accepted.ID || calls != 0 {
		t.Fatalf("retry = %q, source calls = %d", retried.ID, calls)
	}
}

func TestOperationIDCannotChangeIntent(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("accepted")
	r := request(fileEntry("a", data))
	save(t, tp.p, r, map[string][]byte{"a": data})
	r.Changes = []model.Entry{fileEntry("b", data)}
	if _, err := tp.p.Save(tp.ctx, r, sourceFor(map[string][]byte{"b": data})); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("changed intent error = %v, want ErrOperationConflict", err)
	}
}

func TestStaleOutcomeRemainsStableAfterBranchAdvances(t *testing.T) {
	tp := newTestProject(t)
	baseBytes := []byte("base")
	base := save(t, tp.p, request(fileEntry("a", baseBytes)), map[string][]byte{"a": baseBytes})
	stale := request(fileEntry("a", []byte("stale")))
	stale.Base = base.ID
	other := request(fileEntry("b", []byte("advance")))
	other.Base = base.ID
	advanced := save(t, tp.p, other, map[string][]byte{"b": []byte("advance")})
	if _, err := tp.p.Save(tp.ctx, stale, sourceFor(map[string][]byte{"a": []byte("stale")})); !errors.Is(err, ErrStale) {
		t.Fatalf("stale save error = %v", err)
	}
	newHead := request(fileEntry("c", []byte("new head")))
	newHead.Base = advanced.ID
	latest := save(t, tp.p, newHead, map[string][]byte{"c": []byte("new head")})
	if _, err := tp.p.Save(tp.ctx, stale, Source{}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale retry error = %v", err)
	}
	if got, err := tp.p.Head(tp.ctx, "main"); err != nil || got != latest.ID {
		t.Fatalf("head = %q, %v", got, err)
	}
}

func TestCancelUnknownOperationPreventsDelayedSave(t *testing.T) {
	tp := newTestProject(t)
	r := request(fileEntry("a", []byte("late")))
	if err := tp.p.Cancel(tp.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tp.p.Save(tp.ctx, r, sourceFor(map[string][]byte{"a": []byte("late")})); !errors.Is(err, ErrCancelled) {
		t.Fatalf("delayed save error = %v", err)
	}
	if head, err := tp.p.Head(tp.ctx, "main"); err != nil || head != "" {
		t.Fatalf("head = %q, %v", head, err)
	}
}

func TestCancelPendingOperationPreventsDelayedSave(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("partial")
	r := request(fileEntry("a", data))
	_, err := tp.p.Save(tp.ctx, r, Source{
		Write:  sourceFor(map[string][]byte{"a": data}).Write,
		Finish: func() error { return errors.New("interrupted upload") },
	})
	if err == nil {
		t.Fatal("expected interrupted upload to remain unresolved")
	}
	if err := tp.p.Cancel(tp.ctx, r.ID); err != nil {
		t.Fatalf("cancel pending operation: %v", err)
	}
	if _, err := tp.p.Save(tp.ctx, r, sourceFor(map[string][]byte{"a": data})); !errors.Is(err, ErrCancelled) {
		t.Fatalf("delayed save error = %v, want ErrCancelled", err)
	}
}

func TestFailedFinishIsNotAcceptedAndRestartAllowsExactRetry(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("retry")
	r := request(fileEntry("a", data))
	_, err := tp.p.Save(tp.ctx, r, Source{Write: sourceFor(map[string][]byte{"a": data}).Write, Finish: func() error { return errors.New("truncated request") }})
	if err == nil {
		t.Fatal("save accepted a source whose Finish failed")
	}
	if head, _ := tp.p.Head(tp.ctx, "main"); head != "" {
		t.Fatalf("failed upload advanced branch to %q", head)
	}
	roots, err := tp.p.contents.Roots(tp.ctx)
	if err != nil || len(roots[r.ID]) == 0 {
		t.Fatalf("failed upload did not leave an unaccepted candidate for recovery: roots=%v err=%v", roots, err)
	}
	tp.reopen()
	roots, err = tp.p.contents.Roots(tp.ctx)
	if err != nil || len(roots[r.ID]) != 0 {
		t.Fatalf("restart did not clean unaccepted candidate: roots=%v err=%v", roots, err)
	}
	accepted := save(t, tp.p, r, map[string][]byte{"a": data})
	if got := readFile(t, tp.p, accepted.ID, "a"); !bytes.Equal(got, data) {
		t.Fatalf("retried bytes = %q", got)
	}
}

func TestOpenRejectsAcceptedCommitWithMissingRoot(t *testing.T) {
	tp := newTestProject(t)
	data := []byte("retained")
	commit := save(t, tp.p, request(fileEntry("a", data)), map[string][]byte{"a": data})
	roots, err := tp.p.contents.Roots(tp.ctx)
	if err != nil || len(roots[commit.ID]) != 1 {
		t.Fatalf("accepted commit roots = %v, %v", roots, err)
	}
	if err := tp.p.contents.Release(tp.ctx, roots[commit.ID][0]); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.Close(); err != nil {
		t.Fatal(err)
	}
	tp.p = nil
	p, err := Open(tp.ctx, tp.dir)
	if err == nil {
		_ = p.Close()
		t.Fatal("Open accepted history whose retained content root is missing")
	}
}

func TestSourceIdentityErrorsAreNotAccepted(t *testing.T) {
	tp := newTestProject(t)
	good := []byte("actual")
	tests := []struct {
		name  string
		entry model.Entry
		data  []byte
	}{
		{"size", model.Entry{Path: "a", Kind: model.File, Size: 99, SHA256: sha256Hex(good)}, good},
		{"hash", fileEntry("a", []byte("expected")), good},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := request(tt.entry)
			if _, err := tp.p.Save(tp.ctx, r, sourceFor(map[string][]byte{"a": tt.data})); err == nil {
				t.Fatal("accepted bytes with incorrect identity")
			}
			if head, _ := tp.p.Head(tp.ctx, "main"); head != "" {
				t.Fatalf("failed request advanced branch to %q", head)
			}
		})
	}
}

func sha256Hex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func TestDirectoryDeletionCannotHideUnselectedDescendant(t *testing.T) {
	tp := newTestProject(t)
	child := []byte("unselected")
	first := save(t, tp.p, request(fileEntry("tree/hidden.bin", child)), map[string][]byte{"tree/hidden.bin": child})
	remove := request(model.Entry{Path: "tree", Kind: model.Absent})
	remove.Base = first.ID
	if _, err := tp.p.Save(tp.ctx, remove, Source{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("directory-only deletion error = %v, want ErrInvalid", err)
	}
	if head, _ := tp.p.Head(tp.ctx, "main"); head != first.ID {
		t.Fatalf("rejected deletion advanced head to %q", head)
	}
}

func TestFileDirectoryTransitionsRequireExplicitRemoval(t *testing.T) {
	tp := newTestProject(t)
	original := []byte("file")
	first := save(t, tp.p, request(fileEntry("node", original)), map[string][]byte{"node": original})
	child := []byte("child")
	toDirectory := request(model.Entry{Path: "node", Kind: model.Directory}, fileEntry("node/child", child))
	toDirectory.Base = first.ID
	second := save(t, tp.p, toDirectory, map[string][]byte{"node/child": child})
	toFile := request(fileEntry("node", original), model.Entry{Path: "node/child", Kind: model.Absent})
	toFile.Base = second.ID
	third := save(t, tp.p, toFile, map[string][]byte{"node": original})
	bad := request(fileEntry("node/child", child))
	bad.Base = third.ID
	if _, err := tp.p.Save(tp.ctx, bad, sourceFor(map[string][]byte{"node/child": child})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("implicit file-to-directory transition error = %v", err)
	}
}

func TestCheckpointBoundaryHistoriesReopen(t *testing.T) {
	tp := newTestProject(t)
	var head string
	var snapshots = map[int]string{}
	for i := 0; i <= 128; i++ {
		data := []byte{byte(i), 0, byte(i >> 1)}
		r := request(fileEntry("stable", data))
		r.Message = "checkpoint boundary"
		r.Base = head
		c := save(t, tp.p, r, map[string][]byte{"stable": data})
		head = c.ID
		if i == 63 || i == 64 || i == 127 || i == 128 {
			snapshots[i] = c.ID
		}
	}
	tp.reopen()
	for step, id := range snapshots {
		want := []byte{byte(step), 0, byte(step >> 1)}
		if got := readFile(t, tp.p, id, "stable"); !bytes.Equal(got, want) {
			t.Errorf("step %d bytes = %v, want %v", step, got, want)
		}
	}
}

func TestSelectedHistoryOverlappingPrefixesAndExclusions(t *testing.T) {
	tp := newTestProject(t)
	files := map[string][]byte{
		"a/one": []byte("one"), "a/n/two": []byte("two"),
		"a0/neighbor": []byte("zero"), "a!/neighbor": []byte("bang"), "other": []byte("other"),
	}
	var entries []model.Entry
	for name, data := range files {
		entries = append(entries, fileEntry(name, data))
	}
	first := save(t, tp.p, request(entries...), files)
	data := []byte("three")
	next := request(model.Entry{Path: "a/n/two", Kind: model.Absent}, fileEntry("a/n/three", data))
	next.Base = first.ID
	second := save(t, tp.p, next, map[string][]byte{"a/n/three": data})
	tp.reopen()
	selection := model.Selection{
		{Path: "a", Include: true}, {Path: "a/one", Include: true},
		{Path: "a/n", Include: false}, {Path: "a/n/three", Include: true}, {Path: "a/n/two", Include: true}, {Path: "other", Include: true},
	}
	for _, tc := range []struct {
		id   string
		want []string
	}{{first.ID, []string{"a", "a/n/two", "a/one", "other"}}, {second.ID, []string{"a", "a/n/three", "a/one", "other"}}} {
		got, err := tp.p.Entries(tp.ctx, tc.id, selection)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, e := range got {
			paths = append(paths, e.Path)
		}
		if !reflect.DeepEqual(paths, tc.want) {
			t.Fatalf("selected paths = %v, want %v", paths, tc.want)
		}
	}
}

func TestBranchMetadataCRUD(t *testing.T) {
	tp := newTestProject(t)
	ctx := tp.ctx
	if err := tp.p.CreateBranch(ctx, "zeta", ""); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.CreateBranch(ctx, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.CreateBranch(ctx, "alpha", ""); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	branches, err := tp.p.Branches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 3 || branches[0].Name != "alpha" || branches[1].Name != "main" || branches[2].Name != "zeta" {
		t.Fatalf("branches: %#v", branches)
	}
	if err := tp.p.DeleteBranch(ctx, "alpha", "nonempty"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid expected tip: %v", err)
	}
	if err := tp.p.DeleteBranch(ctx, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	if err := tp.p.DeleteBranch(ctx, "alpha", ""); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if err := tp.p.DeleteBranch(ctx, "main", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete main: %v", err)
	}
}
