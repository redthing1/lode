//go:build linux

package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transport"
)

type testRemote struct {
	local  *project.Local
	server *httptest.Server
	client *transport.Client
}

func newTestRemote(t *testing.T, wrap func(http.Handler) http.Handler) *testRemote {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "project")
	if err := project.InitializeLocal(ctx, dir); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := transport.NewHandler(local.Service, local.Token)
	if err != nil {
		_ = local.Close()
		t.Fatal(err)
	}
	if wrap != nil {
		handler = wrap(handler)
	}
	ts := httptest.NewServer(handler)
	client, err := transport.NewClient(ts.URL, local.Token)
	if err != nil {
		ts.Close()
		_ = local.Close()
		t.Fatal(err)
	}
	remote := &testRemote{local: local, server: ts, client: client}
	t.Cleanup(func() { remote.server.Close(); remote.client.Close(); _ = local.Close() })
	return remote
}

func makeFileChange(path, data string) project.Change {
	digest := sha256.Sum256([]byte(data))
	return project.Change{Path: path, Kind: model.File, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}
}

func seed(t *testing.T, remote *testRemote, id string, base string, changes []project.Change, files map[string]string) project.Commit {
	t.Helper()
	request := project.ChangeRequest{OperationID: id, Branch: "main", Base: base, Author: "test", Message: id, Changes: changes}
	commit, err := remote.client.Apply(context.Background(), request, func(path model.Path) (io.ReadCloser, error) {
		data, ok := files[path.String()]
		if !ok {
			return nil, errors.New("missing test payload")
		}
		return io.NopCloser(strings.NewReader(data)), nil
	})
	if err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	return commit
}

func TestAdoptExistingFolderThenCommit(t *testing.T) {
	remote := newTestRemote(t, nil)
	dir := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "adopt.txt"), []byte("local addition"), 0644); err != nil {
		t.Fatal(err)
	}
	w, err := Create(context.Background(), Options{Directory: dir, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main", Adopt: true})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	changes, err := w.Status(context.Background())
	if err != nil || len(changes) != 1 || changes[0].Path != "adopt.txt" {
		t.Fatalf("adopt Status()=%+v err=%v", changes, err)
	}
	commit, err := w.Commit(context.Background(), "author", "adopt existing folder")
	if err != nil {
		t.Fatal(err)
	}
	if commit.ID == "" || w.Base() != commit.ID || w.Pending() != "" {
		t.Fatalf("commit=%+v base=%q pending=%q", commit, w.Base(), w.Pending())
	}
	if head, err := remote.client.Head(context.Background(), "main"); err != nil || head != commit.ID {
		t.Fatalf("head=%q err=%v", head, err)
	}
	history, next, err := w.History(context.Background(), 10)
	if err != nil || len(history) != 1 || history[0].ID != commit.ID || next != "" {
		t.Fatalf("workspace History()=%+v next=%q err=%v", history, next, err)
	}
}

func TestSparseCheckoutAndCommitPreserveOmittedServerPaths(t *testing.T) {
	remote := newTestRemote(t, nil)
	emptyHash := sha256.Sum256(nil)
	base := seed(t, remote, "sparse-base", "", []project.Change{
		makeFileChange("picked/keep.txt", "selected"),
		makeFileChange("omitted/secret.txt", "server only"),
		{Path: "picked/empty.txt", Kind: model.File, SHA256: hex.EncodeToString(emptyHash[:]), Size: 0},
		{Path: "picked/empty-dir", Kind: model.Directory},
	}, map[string]string{"picked/keep.txt": "selected", "omitted/secret.txt": "server only", "picked/empty.txt": ""})

	var mu sync.Mutex
	fileDownloads := map[string]int{}
	handler, err := transport.NewHandler(remote.local.Service, remote.local.Token)
	if err != nil {
		t.Fatal(err)
	}
	remote.client.Close()
	remote.server.Close()
	remote.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/file" {
			mu.Lock()
			fileDownloads[r.URL.Query().Get("path")]++
			mu.Unlock()
		}
		handler.ServeHTTP(w, r)
	}))
	remote.client, err = transport.NewClient(remote.server.URL, remote.local.Token)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "workspace")
	selection := []project.SelectionRule{{Included: false}, {Path: "picked", Included: true}}
	w, err := Create(context.Background(), Options{Directory: dir, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main", Selection: selection})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for path, want := range map[string]string{"picked/keep.txt": "selected", "picked/empty.txt": ""} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil || string(got) != want {
			t.Fatalf("checkout %q=%q err=%v", path, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "picked", "empty-dir")); err != nil || !info.IsDir() {
		t.Fatalf("empty dir stat=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "omitted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("omitted dir stat=%v, want absent", err)
	}
	mu.Lock()
	if fileDownloads["picked/keep.txt"] != 1 || fileDownloads["picked/empty.txt"] != 1 || fileDownloads["omitted/secret.txt"] != 0 {
		t.Fatalf("file endpoint counts=%#v", fileDownloads)
	}
	mu.Unlock()
	stateBytes, err := os.ReadFile(filepath.Join(dir, ".lode", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct{ Baseline []project.Node }
	if err := json.Unmarshal(stateBytes, &saved); err != nil {
		t.Fatal(err)
	}
	for _, node := range saved.Baseline {
		if strings.Contains(node.Path, "omitted") || strings.Contains(node.Path, "secret") {
			t.Fatalf("baseline contains omitted node %q", node.Path)
		}
	}
	if w.Base() != base.ID {
		t.Fatalf("base=%q, want %q", w.Base(), base.ID)
	}

	if err := os.WriteFile(filepath.Join(dir, "picked", "keep.txt"), []byte("selected changed"), 0644); err != nil {
		t.Fatal(err)
	}
	commit, err := w.Commit(context.Background(), "author", "change selected content")
	if err != nil {
		t.Fatal(err)
	}
	all, _, err := remote.client.Entries(context.Background(), commit.ID, []project.SelectionRule{{Included: true}}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, node := range all {
		seen[node.Path] = true
	}
	for _, path := range []string{"picked/keep.txt", "picked/empty.txt", "picked/empty-dir", "omitted/secret.txt"} {
		if !seen[path] {
			t.Errorf("complete remote tree lost %q: %+v", path, all)
		}
	}
}

func TestStatusRejectsUnknownSymlinkAndSpecialFile(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "observation-base", "", []project.Change{makeFileChange("tracked.txt", "tracked")}, map[string]string{"tracked.txt": "tracked"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w, err := Create(context.Background(), Options{Directory: dir, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Base() != base.ID {
		t.Fatalf("base=%q, want %q", w.Base(), base.ID)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(dir, "unknown-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Status(context.Background()); err == nil {
		t.Fatal("Status accepted unknown symlink")
	}
	if err := os.Remove(filepath.Join(dir, "unknown-link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "unknown-pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Status(context.Background()); err == nil {
		t.Fatal("Status accepted unknown special file")
	}
	got, err := os.ReadFile(filepath.Join(dir, "tracked.txt"))
	if err != nil || string(got) != "tracked" {
		t.Fatalf("tracked file after failed observation=%q err=%v", got, err)
	}
}

func TestWorkspaceExclusiveLockAndReopen(t *testing.T) {
	remote := newTestRemote(t, nil)
	base := seed(t, remote, "lock-base", "", []project.Change{makeFileChange("a.txt", "a")}, map[string]string{"a.txt": "a"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w, err := Create(context.Background(), Options{Directory: dir, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Open()=%v, want ErrBusy", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Base() != base.ID || reopened.Pending() != "" {
		t.Fatalf("reopened base=%q pending=%q", reopened.Base(), reopened.Pending())
	}
}

func TestInterruptedCheckoutResumeAndPreserveAlteredFile(t *testing.T) {
	var bRequests atomic.Int32
	var aRequests atomic.Int32
	failB := atomic.Bool{}
	failB.Store(true)
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/file" {
				if r.URL.Query().Get("path") == "a.txt" {
					aRequests.Add(1)
				}
				if r.URL.Query().Get("path") == "b.txt" {
					bRequests.Add(1)
				}
				if r.URL.Query().Get("path") == "b.txt" && failB.CompareAndSwap(true, false) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"code":"internal_error","message":"internal server error"}`)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	seed(t, remote, "checkout-base", "", []project.Change{makeFileChange("a.txt", "first"), makeFileChange("b.txt", "second")}, map[string]string{"a.txt": "first", "b.txt": "second"})
	dir := filepath.Join(t.TempDir(), "workspace")
	if _, err := Create(context.Background(), Options{Directory: dir, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main"}); err == nil {
		t.Fatal("Create succeeded after injected second-file error")
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Pending() != "checkout" {
		t.Fatalf("pending=%q, want checkout", w.Pending())
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("user edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resume(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Resume with altered existing file=%v, want conflict", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(got) != "user edit" {
		t.Fatalf("file after conflict=%q err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("first"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatalf("retry Resume()=%v", err)
	}
	if w.Pending() != "" || aRequests.Load() != 1 || bRequests.Load() != 2 {
		t.Fatalf("pending=%q a requests=%d b requests=%d", w.Pending(), aRequests.Load(), bRequests.Load())
	}
	if got, err := os.ReadFile(filepath.Join(dir, "b.txt")); err != nil || string(got) != "second" {
		t.Fatalf("resumed b file=%q err=%v", got, err)
	}
}

type lostResponseWriter struct {
	http.ResponseWriter
	lost      bool
	status    int
	wroteBody bool
}

func (w *lostResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *lostResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if w.lost && status == http.StatusOK {
		w.ResponseWriter.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *lostResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.lost && w.status == http.StatusOK {
		if !w.wroteBody {
			w.wroteBody = true
			_, err := w.ResponseWriter.Write([]byte(`{"code":"internal_error","message":"internal server error"}`))
			if err != nil {
				return 0, err
			}
		}
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}

func TestLostCommitResponseRecoveryPreservesNewerLocalEdit(t *testing.T) {
	lose := atomic.Bool{}
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/changes" && lose.CompareAndSwap(true, false) {
				next.ServeHTTP(&lostResponseWriter{ResponseWriter: w, lost: true}, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "lost-base", "", []project.Change{makeFileChange("a.txt", "base")}, map[string]string{"a.txt": "base"})
	dir := filepath.Join(t.TempDir(), "workspace")
	w, err := Create(context.Background(), Options{Directory: dir, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("submitted edit"), 0644); err != nil {
		t.Fatal(err)
	}
	lose.Store(true)
	if _, err := w.Commit(context.Background(), "author", "submitted edit"); !errors.Is(err, transport.ErrRemote) {
		t.Fatalf("Commit after accepted response loss=%v", err)
	}
	if w.Pending() != "commit" {
		t.Fatalf("pending=%q, want commit", w.Pending())
	}
	request := *w.state.Pending.Request
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("newer local edit"), 0644); err != nil {
		t.Fatal(err)
	}
	commit, err := w.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume accepted operation=%v", err)
	}
	if commit.ID == "" || commit.ID == base.ID || w.Pending() != "" {
		t.Fatalf("resumed commit=%+v pending=%q", commit, w.Pending())
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(got) != "newer local edit" {
		t.Fatalf("local edit after Resume=%q err=%v", got, err)
	}
	changes, err := w.Status(context.Background())
	if err != nil || len(changes) != 1 || changes[0].Path != "a.txt" {
		t.Fatalf("Status after Resume=%+v err=%v", changes, err)
	}
	if _, err := remote.client.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) {
		t.Fatal("accepted replay asked for file payload")
		return nil, nil
	}); err != nil {
		t.Fatalf("accepted replay=%v", err)
	}
}

func TestAbortUncertainOperationLeavesDurableTombstone(t *testing.T) {
	dropFirst := atomic.Bool{}
	dropFirst.Store(true)
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/changes" && dropFirst.CompareAndSwap(true, false) {
				_ = http.NewResponseController(w).EnableFullDuplex()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"code":"internal_error","message":"internal server error"}`)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	dir := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	w, err := Create(context.Background(), Options{Directory: dir, Remote: remote.server.URL, Token: remote.local.Token, Branch: "main", Adopt: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new payload"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(context.Background(), "author", "uncertain operation"); !errors.Is(err, transport.ErrRemote) {
		t.Fatalf("Commit before server arrival=%v", err)
	}
	request := *w.state.Pending.Request
	if err := w.Abort(context.Background()); err != nil {
		t.Fatalf("Abort uncertain operation=%v", err)
	}
	if w.Pending() != "" {
		t.Fatalf("pending after abort=%q", w.Pending())
	}
	if _, err := remote.client.Apply(context.Background(), request, func(path model.Path) (io.ReadCloser, error) {
		return os.Open(filepath.Join(dir, filepath.FromSlash(path.String())))
	}); !errors.Is(err, project.ErrOperationAborted) {
		t.Fatalf("late replay=%v, want aborted", err)
	}
	if head, err := remote.client.Head(context.Background(), "main"); err != nil || head != "" {
		t.Fatalf("head after late replay=%q err=%v", head, err)
	}
}
