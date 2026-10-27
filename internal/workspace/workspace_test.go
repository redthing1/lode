//go:build linux

package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

type workspaceServer struct {
	p   *project.Project
	srv *httptest.Server
}

func newWorkspaceServer(t *testing.T, wrap func(http.Handler) http.Handler) *workspaceServer {
	t.Helper()
	p, err := project.Create(context.Background(), filepath.Join(t.TempDir(), "project"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("close project: %v", err)
		}
	})
	h := http.Handler(remote.Handler(p))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &workspaceServer{p: p, srv: srv}
}

func (s *workspaceServer) attach(t *testing.T, dir string) *Workspace {
	t.Helper()
	w, err := Attach(context.Background(), s.srv.URL, s.p.Token(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func write(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func save(t *testing.T, w *Workspace) project.Commit {
	t.Helper()
	c, err := w.Save(context.Background(), "tester", "save")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func closeWorkspace(t *testing.T, w *Workspace) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachAndPartialCloneSavePreservesOmittedFile(t *testing.T) {
	var reads atomic.Int32
	var entryRequests atomic.Int32
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" {
				reads.Add(1)
			}
			if r.Method == http.MethodPost && r.URL.Path == "/v1/entries" {
				entryRequests.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	fullDir := filepath.Join(t.TempDir(), "full")
	if err := os.Mkdir(fullDir, 0700); err != nil {
		t.Fatal(err)
	}
	a, b := []byte{0, 1, 0xff}, []byte("omitted bytes")
	write(t, fullDir, "assets/a.bin", a)
	write(t, fullDir, "data/b.bin", b)
	full := s.attach(t, fullDir)
	first := save(t, full)
	closeWorkspace(t, full)

	partialDir := filepath.Join(t.TempDir(), "partial")
	partial, err := Clone(context.Background(), s.srv.URL, s.p.Token(), partialDir, model.Selection{{Include: true}, {Path: "data", Include: false}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, partial)
	if reads.Load() != 1 {
		t.Fatalf("clone requested %d file contents, want only selected a.bin", reads.Load())
	}
	if entryRequests.Load() != 1 {
		t.Fatalf("clone made %d entry requests, want one selected metadata request", entryRequests.Load())
	}
	if got := read(t, partialDir, "assets/a.bin"); !bytes.Equal(got, a) {
		t.Fatalf("selected bytes = %v", got)
	}
	if _, err := os.Stat(filepath.Join(partialDir, "data/b.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("omitted path exists or stat failed unexpectedly: %v", err)
	}
	updated := []byte("edited locally")
	write(t, partialDir, "assets/a.bin", updated)
	second := save(t, partial)
	if got := readProjectFile(t, s.p, second.ID, "assets/a.bin"); !bytes.Equal(got, updated) {
		t.Fatalf("published selected bytes = %q", got)
	}
	if got := readProjectFile(t, s.p, second.ID, "data/b.bin"); !bytes.Equal(got, b) {
		t.Fatalf("omitted bytes after save = %q", got)
	}
	if first.ID == second.ID {
		t.Fatal("save did not create a new version")
	}
}

func readProjectFile(t *testing.T, p *project.Project, id, name string) []byte {
	t.Helper()
	var got []byte
	err := p.Read(context.Background(), id, name, func(_ model.Entry, r io.Reader) error { var e error; got, e = io.ReadAll(r); return e })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLostAcceptedReplyResumeKeepsLaterLocalEdit(t *testing.T) {
	var lose atomic.Bool
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/save" && lose.Swap(false) {
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				if recorder.Code >= 200 && recorder.Code < 300 {
					http.Error(w, "simulated lost reply", http.StatusBadGateway)
					return
				}
				for key, values := range recorder.Header() {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	dir := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	w := s.attach(t, dir)
	defer closeWorkspace(t, w)
	initial := []byte("initial")
	write(t, dir, "a.bin", initial)
	base := save(t, w)
	intended := []byte("intended bytes")
	write(t, dir, "a.bin", intended)
	lose.Store(true)
	if _, err := w.Save(context.Background(), "tester", "uncertain"); err == nil {
		t.Fatal("expected simulated lost reply")
	}
	if _, err := os.Stat(filepath.Join(dir, ".lode/save.json")); err != nil {
		t.Fatalf("save intent was not retained: %v", err)
	}
	later := []byte("user edit after the request")
	write(t, dir, "a.bin", later)
	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("resume accepted save: %v", err)
	}
	if !bytes.Equal(read(t, dir, "a.bin"), later) {
		t.Fatal("resume overwrote later local edit")
	}
	changes, err := w.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "a.bin" || changes[0].Kind != model.File {
		t.Fatalf("status after resume = %#v", changes)
	}
	if w.State().Base == base.ID {
		t.Fatal("resume did not advance workspace baseline")
	}
	if got := readProjectFile(t, s.p, w.State().Base, "a.bin"); !bytes.Equal(got, intended) {
		t.Fatalf("accepted bytes = %q", got)
	}
}

func TestInterruptedCheckoutResumesAndPreservesInstalledFile(t *testing.T) {
	var failB atomic.Bool
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b.bin" && failB.Swap(false) {
				http.Error(w, "interrupted transfer", http.StatusBadGateway)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	dir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.bin", []byte("first"))
	write(t, dir, "b.bin", []byte("second"))
	source := s.attach(t, dir)
	save(t, source)
	closeWorkspace(t, source)
	cloneDir := filepath.Join(t.TempDir(), "clone")
	failB.Store(true)
	if _, err := Clone(context.Background(), s.srv.URL, s.p.Token(), cloneDir, nil); err == nil {
		t.Fatal("expected interrupted checkout")
	}
	if got := read(t, cloneDir, "a.bin"); string(got) != "first" {
		t.Fatalf("first installed file = %q", got)
	}
	w, err := Open(cloneDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	if err := w.Resume(context.Background()); err != nil {
		t.Fatalf("resume checkout: %v", err)
	}
	if got := read(t, cloneDir, "a.bin"); string(got) != "first" {
		t.Fatalf("first file after resume = %q", got)
	}
	if got := read(t, cloneDir, "b.bin"); string(got) != "second" {
		t.Fatalf("second installed file = %q", got)
	}
}

func TestResumeDoesNotTruncateInstalledHardlinkAndPreservesUnexpectedTarget(t *testing.T) {
	var failB atomic.Bool
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b.bin" && failB.Swap(false) {
				http.Error(w, "interrupted transfer", http.StatusBadGateway)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	dir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.bin", []byte("first contents"))
	write(t, dir, "b.bin", []byte("second contents"))
	source := s.attach(t, dir)
	save(t, source)
	closeWorkspace(t, source)
	cloneDir := filepath.Join(t.TempDir(), "clone")
	failB.Store(true)
	if _, err := Clone(context.Background(), s.srv.URL, s.p.Token(), cloneDir, nil); err == nil {
		t.Fatal("expected interrupted checkout")
	}
	plan := transitionJournal(t, cloneDir)
	var staged string
	for i, change := range plan.Changes {
		if change.After.Path == "a.bin" {
			staged = filepath.Join(cloneDir, filepath.FromSlash(recoveryName(i, "new")))
			break
		}
	}
	if staged == "" {
		t.Fatal("transition has no staged a.bin")
	}
	installed, err := os.Stat(filepath.Join(cloneDir, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(installed, stagedInfo) {
		t.Fatal("installed file is not linked to its per-action staged inode")
	}
	w, err := Open(cloneDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Resume(context.Background()); err != nil {
		closeWorkspace(t, w)
		t.Fatalf("resume after staged hardlink: %v", err)
	}
	if got := read(t, cloneDir, "a.bin"); string(got) != "first contents" {
		closeWorkspace(t, w)
		t.Fatalf("installed hardlink contents = %q", got)
	}
	if _, err := os.Stat(filepath.Join(cloneDir, ".lode", "transition")); !errors.Is(err, os.ErrNotExist) {
		closeWorkspace(t, w)
		t.Fatalf("transition recovery metadata remains after completion: %v", err)
	}
	closeWorkspace(t, w)

	cloneDir2 := filepath.Join(t.TempDir(), "clone-unexpected")
	failB.Store(true)
	if _, err := Clone(context.Background(), s.srv.URL, s.p.Token(), cloneDir2, nil); err == nil {
		t.Fatal("expected second interrupted checkout")
	}
	unexpected := []byte("user-owned target")
	write(t, cloneDir2, "b.bin", unexpected)
	w, err = Open(cloneDir2)
	if err != nil {
		t.Fatal(err)
	}
	err = w.Resume(context.Background())
	if err == nil {
		closeWorkspace(t, w)
		t.Fatal("resume overwrote unexpected target")
	}
	if got := read(t, cloneDir2, "b.bin"); !bytes.Equal(got, unexpected) {
		closeWorkspace(t, w)
		t.Fatalf("unexpected target changed: %q", got)
	}
	if err := os.Rename(filepath.Join(cloneDir2, "b.bin"), filepath.Join(cloneDir2, "b.saved")); err != nil {
		closeWorkspace(t, w)
		t.Fatal(err)
	}
	if err := w.Resume(context.Background()); err != nil {
		closeWorkspace(t, w)
		t.Fatalf("resume after moving unexpected target: %v", err)
	}
	if got := read(t, cloneDir2, "b.bin"); string(got) != "second contents" {
		closeWorkspace(t, w)
		t.Fatalf("installed target = %q", got)
	}
	closeWorkspace(t, w)
}

func TestStatusErrorsForSelectedSymlinkButIgnoresOmittedSymlink(t *testing.T) {
	s := newWorkspaceServer(t, nil)
	dir := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	w := s.attach(t, dir)
	defer closeWorkspace(t, w)
	w.state.Selection = model.Selection{{Include: true}, {Path: "ignored", Include: false}}
	if err := w.store("state.json", w.state); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "target", []byte("target"))
	if err := os.MkdirAll(filepath.Join(dir, "ignored"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(dir, "ignored", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Status(context.Background()); err != nil {
		t.Fatalf("omitted symlink affected status: %v", err)
	}
	if err := os.Symlink("../target", filepath.Join(dir, "selected-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Status(context.Background()); err == nil {
		t.Fatal("status accepted a selected symlink")
	}
}
