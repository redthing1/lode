package workspace

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func expireHistory(t *testing.T, p *project.Project) {
	t.Helper()
	plan, err := p.PreviewCleanup(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ApplyCleanup(context.Background(), 0, plan.Token); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredBaseStillSupportsPartialUpdateAndSave(t *testing.T) {
	s, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{
		"selected": []byte("original"), "unloaded": []byte("original unloaded"),
	}, model.Selection{{Path: "selected", Include: true}})
	base := w.State().Base
	write(t, localDir, "selected", []byte("local work"))
	write(t, sourceDir, "unloaded", []byte("new unloaded"))
	current := publishWorkspace(t, sourceDir)
	expireHistory(t, s.p)
	if _, err := s.p.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := w.Status(context.Background())
	if err != nil || len(status) != 1 || status[0].Path != "selected" {
		t.Fatalf("status with expired base = %v, %v", status, err)
	}
	if w.State().Base != base {
		t.Fatal("cleanup changed workspace state")
	}
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if w.State().Base != current || string(read(t, localDir, "selected")) != "local work" {
		t.Fatal("update lost local work or did not advance baseline")
	}
	c := save(t, w)
	if got := string(readProjectFile(t, s.p, c.ID, "unloaded")); got != "new unloaded" {
		t.Fatalf("save changed unloaded contents: %q", got)
	}
	if _, err := os.Stat(filepath.Join(localDir, "unloaded")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update materialized unloaded file: %v", err)
	}
}

func TestRestoreSourceExpiryBetweenReadsCanBeCancelled(t *testing.T) {
	var expire atomic.Bool
	var expireSource func() error
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b" && expire.Swap(false) {
				if err := expireSource(); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	expireSource = func() error {
		plan, err := s.p.PreviewCleanup(context.Background(), 0)
		if err != nil {
			return err
		}
		_, err = s.p.ApplyCleanup(context.Background(), 0, plan.Token)
		return err
	}
	dir := t.TempDir()
	write(t, dir, "a", []byte("old a"))
	write(t, dir, "b", []byte("old b"))
	source := s.attach(t, dir)
	historical := save(t, source).ID
	write(t, dir, "a", []byte("current a"))
	write(t, dir, "b", []byte("current b"))
	current := save(t, source).ID
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	expire.Store(true)
	if err := w.Restore(context.Background(), historical, nil, false); !errors.Is(err, project.ErrExpired) {
		t.Fatalf("restore after source expiry = %v", err)
	}
	if string(read(t, localDir, "a")) != "old a" || w.State().Base != current {
		t.Fatal("fixture did not expire source between applied actions")
	}
	if err := w.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(read(t, localDir, "a")) != "current a" || string(read(t, localDir, "b")) != "current b" || w.State().Base != current {
		t.Fatal("cancellation did not preserve pre-restore contents and baseline")
	}
	if status, err := w.Status(context.Background()); err != nil || len(status) != 0 {
		t.Fatalf("cancelled workspace = %v, %v", status, err)
	}
}
