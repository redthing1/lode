package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/web"
)

type fixture struct {
	url, token                     string
	client                         *remote.Client
	server                         *httptest.Server
	store                          *instance.Store
	user                           instance.User
	dropSave, dropFile, refuseSave atomic.Bool
}

type discardResponse struct{ http.ResponseWriter }

func (w discardResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w discardResponse) Write(p []byte) (int, error) { return len(p), nil }
func (w discardResponse) WriteHeader(int)             {}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	origin := "http://" + strings.Replace(listener.Addr().String(), "127.0.0.1", "localhost", 1)
	dir := filepath.Join(t.TempDir(), "instance")
	code, err := instance.Initialize(ctx, dir, instance.Config{URL: origin, Listen: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := instance.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	user, err := store.Setup(ctx, code, "alice", "a sufficiently long test password")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateToken(ctx, user, instance.TokenOptions{Name: "test", Role: instance.Writer})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{url: origin + "/alice/project", token: token, store: store, user: user}
	handler := web.New(store)
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/save") && f.refuseSave.Swap(false) {
			panic(http.ErrAbortHandler)
		}
		if strings.HasSuffix(r.URL.Path, "/save") && f.dropSave.Swap(false) {
			handler.ServeHTTP(discardResponse{w}, r)
			panic(http.ErrAbortHandler)
		}
		if (strings.HasSuffix(r.URL.Path, "/file") || strings.HasSuffix(r.URL.Path, "/files")) && f.dropFile.Swap(false) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("truncated"))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	f.server.Listener.Close()
	f.server.Listener = listener
	f.server.Start()
	t.Cleanup(f.server.Close)
	f.client, err = remote.New(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.client.HTTP.CloseIdleConnections)
	f.client.InstanceID = store.ID
	p, err := f.client.CreateProject(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	f.client.ProjectID = p.ID
	return f
}

func writeFile(t *testing.T, dir, name, data string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func attach(t *testing.T, f *fixture, dir string) *Workspace {
	t.Helper()
	w, err := Attach(context.Background(), f.url, f.client, dir, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}
func save(t *testing.T, f *fixture, w *Workspace, message string) model.Version {
	t.Helper()
	v, err := w.Save(context.Background(), f.token, message)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func remoteFiles(t *testing.T, f *fixture, v string) map[string]string {
	t.Helper()
	entries, err := f.client.Inventory(context.Background(), "alice", "project", v, model.Selection{{Include: true}})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.Kind != model.File {
			continue
		}
		var data bytes.Buffer
		if err := f.client.Download(context.Background(), "alice", "project", v, e, nil, 0, &data); err != nil {
			t.Fatal(err)
		}
		files[e.Path] = data.String()
	}
	return files
}

func TestTwoSelectedWorkspacesAndOfflineStatus(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "assets/a", "first")
	writeFile(t, dir, "assets/hidden/b", "omitted child")
	writeFile(t, dir, "references/c", "omitted subtree")
	w := attach(t, f, dir)
	save(t, f, w, "initial")
	if other, err := Open(dir); err == nil {
		other.Close()
		t.Fatal("concurrent workspace opened")
	}
	if other, err := Open(filepath.Join(dir, "missing")); err == nil {
		other.Close()
		t.Fatal("opened a nonexistent directory")
	}
	if other, err := Attach(ctx, f.url, f.client, t.TempDir(), ConnectOptions{}); err == nil {
		other.Close()
		t.Fatal("attached to populated remote")
	}
	second := filepath.Join(t.TempDir(), "second")
	selected := model.Selection{{Path: "assets", Include: true}, {Path: "assets/hidden", Include: false}}
	b, err := Clone(ctx, f.url, f.client, second, ConnectOptions{Selection: selected})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if got := readFile(t, second, "assets/a"); got != "first" {
		t.Fatal(got)
	}
	if len(b.State().Baseline) != 2 {
		t.Fatalf("selected baseline: %v", b.State().Baseline)
	}
	if _, err := os.Stat(filepath.Join(second, "references")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unselected download: %v", err)
	}
	writeFile(t, second, "assets/a", "second")
	v := save(t, f, b, "selected edit")
	got := remoteFiles(t, f, v.ID)
	if got["assets/a"] != "second" || got["assets/hidden/b"] != "omitted child" || got["references/c"] != "omitted subtree" {
		t.Fatal(got)
	}
	// Removing a partially selected directory only removes its selected contents.
	if err := os.RemoveAll(filepath.Join(second, "assets")); err != nil {
		t.Fatal(err)
	}
	v = save(t, f, b, "remove selected contents")
	got = remoteFiles(t, f, v.ID)
	if _, exists := got["assets/a"]; exists || len(got) != 2 {
		t.Fatal(got)
	}
	f.server.Close()
	writeFile(t, second, "assets/new", "offline")
	changes, err := b.Status(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "assets/new" {
		t.Fatalf("offline status: %v %v", changes, err)
	}
}

type batchTransport struct {
	http.RoundTripper
	requests    [][]model.Entry
	singles     int
	truncate    int64
	beforeBatch func(int) error
}

func (b *batchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/file") {
		b.singles++
	}
	batch := strings.HasSuffix(r.URL.Path, "/files")
	if batch {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		var files []model.Entry
		err = json.NewDecoder(body).Decode(&files)
		body.Close()
		if err != nil {
			return nil, err
		}
		b.requests = append(b.requests, files)
		if b.beforeBatch != nil {
			if err := b.beforeBatch(len(b.requests)); err != nil {
				return nil, err
			}
		}
	}
	response, err := b.RoundTripper.RoundTrip(r)
	if err == nil && batch && b.truncate != 0 {
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(response.Body, b.truncate), response.Body}
		b.truncate = 0
	}
	return response, err
}

func TestSelectedCloneBatchesSmallFiles(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	for i := range 300 {
		writeFile(t, dir, fmt.Sprintf("chosen/%03d", i), fmt.Sprint(i))
	}
	writeFile(t, dir, "omitted", "do not download")
	w := attach(t, f, dir)
	save(t, f, w, "wide")
	counts := &batchTransport{RoundTripper: f.client.HTTP.Transport}
	f.client.HTTP.Transport = counts
	clone := cloneWorkspace(t, f, model.Selection{{Path: "chosen", Include: true}})
	if len(counts.requests) != 3 || counts.singles != 0 {
		t.Fatalf("batch requests=%d single=%d", len(counts.requests), counts.singles)
	}
	for i := range 300 {
		if readFile(t, clone.Directory(), fmt.Sprintf("chosen/%03d", i)) != fmt.Sprint(i) {
			t.Fatal("batch contents differ")
		}
	}
	if _, err := os.Stat(filepath.Join(clone.Directory(), "omitted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("downloaded excluded contents")
	}
}

func TestTruncatedBatchResumesWithoutRedownloadingVerifiedFiles(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, dir, name, strings.Repeat(name, 3))
	}
	w := attach(t, f, dir)
	save(t, f, w, "initial")
	counts := &batchTransport{RoundTripper: f.client.HTTP.Transport, truncate: 4}
	f.client.HTTP.Transport = counts
	destination := filepath.Join(t.TempDir(), "partial")
	if _, err := Clone(context.Background(), f.url, f.client, destination, ConnectOptions{}); err == nil {
		t.Fatal("accepted truncated batch")
	}
	partial, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	if readFile(t, destination, stagePath(0, "new")) != "aaa" {
		t.Fatal("verified first file was not staged")
	}
	if exists, err := partial.pending(stageName(1, "new")); err != nil || exists {
		t.Fatalf("unverified second file was published: %v %v", exists, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("batch failure changed working files")
	}
	if err := partial.resumeTransition(context.Background(), f.client, false); err != nil {
		t.Fatal(err)
	}
	if len(counts.requests) != 2 || len(counts.requests[1]) != 2 || counts.requests[1][0].Path != "b" || counts.requests[1][1].Path != "c" {
		t.Fatalf("retry downloads: %v", counts.requests)
	}
	for _, name := range []string{"a", "b", "c"} {
		if readFile(t, destination, name) != strings.Repeat(name, 3) {
			t.Fatal("resumed contents differ")
		}
	}
}

func TestSourceExpiryBetweenBatchesCanCancelOffline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	for i := range 140 {
		writeFile(t, dir, fmt.Sprintf("file-%03d", i), "original")
	}
	seed := attach(t, f, dir)
	save(t, f, seed, "initial")
	counts := &batchTransport{RoundTripper: f.client.HTTP.Transport}
	counts.beforeBatch = func(n int) error {
		if n == 1 {
			// Clone has already chosen the original version and inventory.
			writeFile(t, dir, "file-000", "new branch tip")
			_, err := seed.Save(ctx, f.token, "new tip")
			return err
		}
		return f.store.WithProject(ctx, instance.Principal{User: f.user}, "alice", "project", instance.Maintainer, func(_ instance.Project, p *project.Store) error {
			if err := p.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 90}); err != nil {
				return err
			}
			preview, err := p.PreviewCleanup(ctx)
			if err != nil {
				return err
			}
			_, err = p.Cleanup(ctx, preview.Token)
			return err
		})
	}
	f.client.HTTP.Transport = counts
	destination := filepath.Join(t.TempDir(), "partial")
	_, err := Clone(ctx, f.url, f.client, destination, ConnectOptions{})
	var failure *remote.Error
	if !errors.As(err, &failure) || failure.Code != "expired" || len(counts.requests) != 2 {
		t.Fatalf("expiry after first batch: %v, requests=%d", err, len(counts.requests))
	}
	if readFile(t, destination, "file-000") != "original" {
		t.Fatal("first batch did not use the chosen version")
	}
	f.server.Close()
	w, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Cancel(ctx, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lode" {
		t.Fatalf("cancel left installed contents: %v %v", entries, err)
	}
	if exists, err := w.pending("transition.json"); err != nil || exists {
		t.Fatalf("cancel remains pending: %v %v", exists, err)
	}
}

func TestAcceptedSaveResumePreservesLaterEdits(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "a", "published")
	w := attach(t, f, dir)
	f.dropSave.Store(true)
	if _, err := w.Save(ctx, f.token, "first"); err == nil {
		t.Fatal("save response was not lost")
	}
	if w.State().Version != "" {
		t.Fatal("baseline advanced without a response")
	}
	w.Close()
	writeFile(t, dir, "a", "new local work")
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Resume(ctx, f.token); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dir, "a"); got != "new local work" {
		t.Fatal(got)
	}
	changes, err := w.Status(ctx)
	if err != nil || len(changes) != 1 {
		t.Fatalf("later edit: %v %v", changes, err)
	}
	versions, err := f.client.History(ctx, "alice", "project", "main", 10)
	if err != nil || len(versions.Versions) != 1 || versions.Versions[0].ID != w.State().Version {
		t.Fatalf("history: %v %v", versions, err)
	}
	if got := remoteFiles(t, f, versions.Versions[0].ID)["a"]; got != "published" {
		t.Fatal(got)
	}
}

func TestInterruptedClonePreservesUnexpectedFiles(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprint(large), func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			dir := t.TempDir()
			data := "complete remote file"
			if large {
				data = strings.Repeat("x", remote.DownloadBatchFileSize+1)
			}
			writeFile(t, dir, "d/a", data)
			w := attach(t, f, dir)
			save(t, f, w, "initial")
			second := filepath.Join(t.TempDir(), "second")
			f.dropFile.Store(true)
			if w, err := Clone(ctx, f.url, f.client, second, ConnectOptions{}); err == nil {
				w.Close()
				t.Fatal("truncated clone succeeded")
			}
			if _, err := os.Stat(filepath.Join(second, "d/a")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial file installed: %v", err)
			}
			writeFile(t, second, "d/a", "local work")
			b, err := Open(second)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if err := b.Resume(ctx, f.token); err == nil {
				t.Fatal("overwrote unexpected local contents")
			}
			if got := readFile(t, second, "d/a"); got != "local work" {
				t.Fatal(got)
			}
			// Small-file prefetch failed before creating d, so both the local
			// directory and file are unexpected. Single-file transfer failed
			// after creating Lode's directory; only its local child is new.
			obstruction, saved := "d", "saved-local/a"
			if large {
				obstruction, saved = "d/a", "saved-local"
			}
			if err := os.Rename(filepath.Join(second, obstruction), filepath.Join(second, "saved-local")); err != nil {
				t.Fatal(err)
			}
			if err := b.Resume(ctx, f.token); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, second, "d/a"); got != data {
				t.Fatal("remote contents differ")
			}
			if got := readFile(t, second, saved); got != "local work" {
				t.Fatal(got)
			}
			if _, err := os.Stat(filepath.Join(second, ".lode/staging")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging cleanup: %v", err)
			}
		})
	}
}

func TestObservationRefusesUnsupportedSelectedEntries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "d/a", "initial")
	w := attach(t, f, dir)
	v := save(t, f, w, "initial")
	second := filepath.Join(t.TempDir(), "second")
	b, err := Clone(ctx, f.url, f.client, second, ConnectOptions{Selection: model.Selection{{Path: "d/a", Include: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := os.RemoveAll(filepath.Join(second, "d")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, second, "d", "cannot inspect child")
	if _, err := b.Status(ctx); err == nil {
		t.Fatal("failed observation became deletion")
	}
	if err := os.Remove(filepath.Join(second, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "d"), filepath.Join(second, "d")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Save(ctx, f.token, "bad observation"); err == nil {
		t.Fatal("symlink observation accepted")
	}
	head, err := f.client.Head(ctx, "alice", "project", "main")
	if err != nil || head != v.ID {
		t.Fatalf("head: %s %v", head, err)
	}
}

func TestCancelFailedSaveAllowsNewWork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "a", "old local state")
	w := attach(t, f, dir)
	f.refuseSave.Store(true)
	if _, err := w.Save(ctx, f.token, "interrupted"); err == nil {
		t.Fatal("save was not interrupted")
	}
	var pending pendingSave
	if err := w.readJSON("save.json", &pending); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a", "later work")
	if err := w.Cancel(ctx, f.token); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dir, "a"); got != "later work" {
		t.Fatal(got)
	}
	_, err := f.client.Save(ctx, "alice", "project", pending.Request, nil, nil)
	var failure *remote.Error
	if !errors.As(err, &failure) || failure.Code != "cancelled" {
		t.Fatalf("delayed save: %v", err)
	}
	v := save(t, f, w, "new save")
	if got := remoteFiles(t, f, v.ID)["a"]; got != "later work" {
		t.Fatal(got)
	}
	if exists, err := w.pending("save.json"); err != nil || exists {
		t.Fatalf("pending save: %v %v", exists, err)
	}
}

func TestCancelAcceptedSaveKeepsPublishedBaseline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := t.TempDir()
	writeFile(t, dir, "a", "published")
	w := attach(t, f, dir)
	f.dropSave.Store(true)
	if _, err := w.Save(ctx, f.token, "accepted"); err == nil {
		t.Fatal("response was not lost")
	}
	writeFile(t, dir, "a", "later work")
	if err := w.Cancel(ctx, f.token); err != nil {
		t.Fatal(err)
	}
	if w.State().Version == "" {
		t.Fatal("accepted save forgotten")
	}
	if got := readFile(t, dir, "a"); got != "later work" {
		t.Fatal(got)
	}
	changes, err := w.Status(ctx)
	if err != nil || len(changes) != 1 {
		t.Fatalf("later work: %v %v", changes, err)
	}
}
