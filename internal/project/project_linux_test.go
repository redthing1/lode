package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
	_ "modernc.org/sqlite"
)

type fixture struct {
	store     *Store
	content   *content.Store
	db        *sql.DB
	directory string
}

func openFixture(t *testing.T, dir string, create bool) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(dir, "instance.sqlite")+"?_pragma=synchronous(EXTRA)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if create {
		if err := Initialize(ctx, db); err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := Create(ctx, tx, "project"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	c, err := content.Open(ctx, filepath.Join(dir, "content"), "test-only", create)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, db, "project", c)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s, c, db, dir}
	t.Cleanup(func() { f.close(t) })
	return f
}
func (f *fixture) close(t *testing.T) {
	t.Helper()
	if f.content != nil {
		if err := f.content.Close(context.Background()); err != nil {
			t.Error(err)
		}
		f.content = nil
	}
	if f.db != nil {
		if err := f.db.Close(); err != nil {
			t.Error(err)
		}
		f.db = nil
	}
}
func operation(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
func file(name, value string) model.Entry {
	h := sha256.Sum256([]byte(value))
	return model.Entry{Path: name, Kind: model.File, Size: int64(len(value)), SHA256: hex.EncodeToString(h[:])}
}
func publish(t *testing.T, s *Store, base string, changes []model.Entry, data map[string]string) model.Version {
	t.Helper()
	v, err := s.Save(context.Background(), "alice", model.Save{ID: operation(t), Branch: "main", Base: base, Message: "save", Changes: changes}, Source{Write: func(e model.Entry, w io.Writer, _ *Basis) error {
		_, err := io.WriteString(w, data[e.Path])
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func read(t *testing.T, s *Store, id, name string) string {
	t.Helper()
	_, r, err := s.File(context.Background(), id, name)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPartialPublicationAndExactDeletion(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	v1 := publish(t, s, "", []model.Entry{file("assets/a", "a1"), file("references/b", "b1")}, map[string]string{"assets/a": "a1", "references/b": "b1"})
	v2 := publish(t, s, v1.ID, []model.Entry{file("assets/a", "a2")}, map[string]string{"assets/a": "a2"})
	if read(t, s, v2.ID, "references/b") != "b1" || read(t, s, v1.ID, "assets/a") != "a1" {
		t.Fatal("version contents changed")
	}
	var selected []model.Entry
	if err := s.Inventory(ctx, v2.ID, model.Selection{{Path: "assets/a", Include: true}}, func(e model.Entry) error { selected = append(selected, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0] != file("assets/a", "a2") {
		t.Fatalf("selected: %v", selected)
	}
	_, err := s.Save(ctx, "alice", model.Save{ID: operation(t), Branch: "main", Base: v2.ID, Changes: []model.Entry{{Path: "references", Kind: model.Absent}}}, Source{})
	if err == nil {
		t.Fatal("erased unseen descendants")
	}
	v3 := publish(t, s, v2.ID, []model.Entry{file("references", "replacement"), {Path: "references/b", Kind: model.Absent}}, map[string]string{"references": "replacement"})
	if read(t, s, v3.ID, "references") != "replacement" || read(t, s, v3.ID, "assets/a") != "a2" {
		t.Fatal("type change lost contents")
	}
	_, _, err = s.File(ctx, v3.ID, "references/child")
	if err == nil {
		t.Fatal("file traversed as directory")
	}
}

func TestRetryAndCompetingSave(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	r := model.Save{ID: operation(t), Branch: "main", Message: "first", Changes: []model.Entry{file("a", "first")}}
	v, err := s.Save(ctx, "alice", r, Source{Write: func(e model.Entry, w io.Writer, _ *Basis) error { _, err := io.WriteString(w, "first"); return err }})
	if err != nil {
		t.Fatal(err)
	}
	publish(t, s, v.ID, []model.Entry{file("b", "later")}, map[string]string{"b": "later"})
	retry, err := s.Save(ctx, "alice", r, Source{Write: func(model.Entry, io.Writer, *Basis) error { t.Fatal("retry read source"); return nil }})
	if err != nil || retry != v {
		t.Fatalf("retry: %v %v", retry, err)
	}
	if _, err := s.Save(ctx, "bob", r, Source{}); !errors.Is(err, ErrIntent) {
		t.Fatalf("actor reused operation: %v", err)
	}
	r.ID = operation(t)
	r.Base = v.ID
	if _, err := s.Save(ctx, "alice", r, Source{}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale save: %v", err)
	}
}

func TestCaptureFailureAndMissingMetadata(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	v := publish(t, s, "", []model.Entry{file("keep", "keep")}, map[string]string{"keep": "keep"})
	_, err := s.Save(ctx, "alice", model.Save{ID: operation(t), Branch: "main", Base: v.ID, Changes: []model.Entry{file("new", "good")}}, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error { _, err := io.WriteString(w, "bad"); return err }})
	if err == nil {
		t.Fatal("bad identity accepted")
	}
	head, err := s.Head(ctx, "main")
	if err != nil || head != v.ID {
		t.Fatal("failed capture advanced branch")
	}
	if _, err := f.db.Exec("DELETE FROM groups WHERE project='project'"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Save(ctx, "alice", model.Save{ID: operation(t), Branch: "main", Base: v.ID, Changes: []model.Entry{file("new", "new")}}, Source{})
	if !errors.Is(err, ErrMetadata) {
		t.Fatalf("missing metadata was not refused: %v", err)
	}
}

func TestOperationValidityWindow(t *testing.T) {
	u, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	sec, nsec := u.Time().UnixTime()
	created := time.Unix(sec, nsec)
	if err := validOperation(u.String(), created.Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if err := validOperation(u.String(), created.Add(operationLifetime), ""); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("old operation: %v", err)
	}
	if err := validOperation(u.String(), created.Add(-6*time.Minute), ""); err == nil {
		t.Fatal("future operation accepted")
	}
	if err := validOperation(strings.ToUpper(u.String()), created, ""); err == nil {
		t.Fatal("noncanonical operation accepted")
	}
}

func TestConcurrentSavesHaveOneWinner(t *testing.T) {
	f := openFixture(t, t.TempDir(), true)
	start := make(chan struct{})
	type result struct {
		version model.Version
		err     error
	}
	results := make(chan result, 2)
	for _, name := range []string{"alice", "bob"} {
		id := operation(t)
		go func() {
			<-start
			v, err := f.store.Save(context.Background(), name, model.Save{ID: id, Branch: "main", Changes: []model.Entry{file(name, name)}}, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error { _, err := io.WriteString(w, name); return err }})
			results <- result{v, err}
		}()
	}
	close(start)
	winners, stale := 0, 0
	for range 2 {
		r := <-results
		if r.err == nil {
			winners++
			if read(t, f.store, r.version.ID, r.version.Actor) != r.version.Actor {
				t.Fatal("winner contents differ")
			}
		} else if errors.Is(r.err, ErrStale) {
			stale++
		} else {
			t.Fatal(r.err)
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("winners=%d stale=%d", winners, stale)
	}
}

func TestCancelledSaveCannotPublishAfterReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := openFixture(t, dir, true)
	r := model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{file("a", "a")}}
	for range 2 {
		if v, err := f.store.Cancel(ctx, "alice", r); err != nil || v.ID != "" {
			t.Fatalf("cancel: %v %v", v, err)
		}
	}
	f.close(t)
	f = openFixture(t, dir, false)
	_, err := f.store.Save(ctx, "alice", r, Source{Write: func(model.Entry, io.Writer, *Basis) error {
		t.Fatal("cancelled save read source")
		return nil
	}})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("late save: %v", err)
	}
	if _, err := f.store.Resolve(ctx, "alice", r); !errors.Is(err, ErrCancelled) {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := f.store.Cancel(ctx, "bob", r); err != nil {
		t.Fatalf("independent actor cancellation: %v", err)
	}
	r.Message = "different request"
	if _, err := f.store.Cancel(ctx, "alice", r); !errors.Is(err, ErrIntent) {
		t.Fatalf("changed intent: %v", err)
	}
	if head, err := f.store.Head(ctx, "main"); err != nil || head != "" {
		t.Fatalf("head: %s %v", head, err)
	}
}

func TestCancelReturnsAcceptedVersion(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	r := model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{file("a", "a")}}
	if _, err := f.store.Cancel(ctx, "bob", r); err != nil {
		t.Fatal(err)
	}
	v, err := f.store.Save(ctx, "alice", r, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error {
		_, err := io.WriteString(w, "a")
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Cancel(ctx, "alice", r)
	if err != nil || got != v {
		t.Fatalf("accepted cancellation: %v %v", got, err)
	}
	if got := read(t, f.store, v.ID, "a"); got != "a" {
		t.Fatal(got)
	}
	if v, err := f.store.Cancel(ctx, "bob", r); err != nil || v.ID != "" {
		t.Fatalf("actor cancellation changed: %v %v", v, err)
	}
	if _, err := f.store.Cancel(ctx, "eve", r); !errors.Is(err, ErrIntent) {
		t.Fatalf("different actor adopted acceptance: %v", err)
	}
}

func TestDirectoryPaginationKeepsFoldersFirst(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	changes := []model.Entry{file("z-directory/child", "child")}
	data := map[string]string{"z-directory/child": "child"}
	for i := range 105 {
		name := fmt.Sprintf("a-%03d", i)
		changes = append(changes, file(name, name))
		data[name] = name
	}
	v := publish(t, f.store, "", changes, data)
	var got []model.Entry
	after := ""
	for {
		entries, next, err := f.store.Directory(ctx, v.ID, "", after, 17)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 17 || next != "" && next == after {
			t.Fatal("invalid page boundary")
		}
		got = append(got, entries...)
		if next == "" {
			break
		}
		after = next
	}
	if len(got) != 106 || got[0].Path != "z-directory" {
		t.Fatalf("directory listing: %v", got)
	}
	for i, e := range got[1:] {
		if e.Path != fmt.Sprintf("a-%03d", i) {
			t.Fatalf("page lost or repeated entry %d: %v", i, e)
		}
	}
	children, next, err := f.store.Directory(ctx, v.ID, "z-directory", "", 100)
	if err != nil || len(children) != 1 || children[0].Path != "z-directory/child" || next != "" {
		t.Fatalf("children: %v %s %v", children, next, err)
	}
}
