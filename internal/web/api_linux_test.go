package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/transfer"
)

type apiFixture struct {
	store         *instance.Store
	user          instance.User
	client        *remote.Client
	origin, token string
	project       remote.Project
}

func apiTest(t *testing.T) apiFixture {
	t.Helper()
	return apiTestHandler(t, nil)
}

func apiTestHandler(t *testing.T, wrap func(http.Handler) http.Handler) apiFixture {
	t.Helper()
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
	var handler http.Handler = New(store)
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	client, err := remote.New(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.HTTP.CloseIdleConnections)
	identity, err := client.Me(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client.InstanceID = identity.Instance
	p, err := client.CreateProject(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	client.ProjectID = p.ID
	return apiFixture{store, user, client, origin, token, p}
}
func entry(name string, b []byte) model.Entry {
	hash := sha256.Sum256(b)
	return model.Entry{Path: name, Kind: model.File, Size: int64(len(b)), SHA256: hex.EncodeToString(hash[:])}
}
func operation(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

type countedBody struct {
	io.ReadCloser
	count *atomic.Int64
}

func (r countedBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.count.Add(int64(n))
	return n, err
}

type countedTransport struct {
	http.RoundTripper
	upload, signature           atomic.Int64
	downloadSignature, download atomic.Int64
}

func (t *countedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/save") && r.Body != nil {
		r.Body = countedBody{r.Body, &t.upload}
	}
	if strings.HasSuffix(r.URL.Path, "/file") && r.Method == "POST" {
		r.Body = countedBody{r.Body, &t.downloadSignature}
	}
	response, err := t.RoundTripper.RoundTrip(r)
	if err == nil && strings.HasSuffix(r.URL.Path, "/signature") {
		response.Body = countedBody{response.Body, &t.signature}
	}
	if err == nil && strings.HasSuffix(r.URL.Path, "/file") {
		response.Body = countedBody{response.Body, &t.download}
	}
	return response, err
}

func TestHTTPPartialSaveDeltaAndRetry(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	c := f.client
	counts := &countedTransport{RoundTripper: c.HTTP.Transport}
	c.HTTP.Transport = counts
	data := make([]byte, 2<<20)
	_, _ = rand.New(rand.NewSource(19)).Read(data)
	contents := map[string][]byte{"assets/a.bin": data, "assets.aux": []byte("adjacent filename"), "references/b.bin": []byte("reference\x00\xff")}
	var initial []model.Entry
	for name, b := range contents {
		initial = append(initial, entry(name, b))
	}
	open := func(e model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents[e.Path])), nil
	}
	first, err := c.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Changes: initial}, nil, open)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := c.Inventory(ctx, "alice", "project", first.ID, model.Selection{{Path: "assets", Include: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].Path != "assets" || selected[1].Path != "assets/a.bin" {
		t.Fatalf("partial inventory: %v", selected)
	}
	var cloned bytes.Buffer
	if err := c.Download(ctx, "alice", "project", first.ID, selected[1], nil, 0, &cloned); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cloned.Bytes(), data) {
		t.Fatal("cloned bytes differ")
	}
	for i := 512 << 10; i < (512+64)<<10; i++ {
		data[i] ^= 0x3f
	}
	request := model.Save{ID: operation(t), Branch: "main", Base: first.ID, Message: "small edit", Changes: []model.Entry{entry("assets/a.bin", data)}}
	counts.upload.Store(0)
	counts.signature.Store(0)
	second, err := c.Save(ctx, "alice", "project", request, selected, open)
	if err != nil {
		t.Fatal(err)
	}
	if counts.upload.Load() >= 200<<10 || counts.signature.Load() == 0 {
		t.Fatalf("delta was not used: upload=%d signature=%d", counts.upload.Load(), counts.signature.Load())
	}
	counts.download.Store(0)
	var updated bytes.Buffer
	if err := c.Download(ctx, "alice", "project", second.ID, request.Changes[0], bytes.NewReader(cloned.Bytes()), int64(cloned.Len()), &updated); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(updated.Bytes(), data) || counts.downloadSignature.Load() == 0 || counts.download.Load() >= 200<<10 {
		t.Fatalf("delta download: signature=%d download=%d exact=%v", counts.downloadSignature.Load(), counts.download.Load(), bytes.Equal(updated.Bytes(), data))
	}
	t.Logf("small-edit HTTP download=%d bytes; client signature=%d bytes", counts.download.Load(), counts.downloadSignature.Load())
	for _, name := range []string{"assets/a.bin", "references/b.bin", "assets.aux"} {
		var result bytes.Buffer
		if err := c.Download(ctx, "alice", "project", second.ID, entry(name, contents[name]), nil, 0, &result); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(result.Bytes(), contents[name]) {
			t.Fatalf("%s differs", name)
		}
	}
	retry, err := c.Save(ctx, "alice", "project", request, selected, func(model.Entry) (io.ReadCloser, error) {
		t.Fatal("accepted retry opened local contents")
		return nil, nil
	})
	if err != nil || retry.ID != second.ID {
		t.Fatalf("retry %v: %v", retry, err)
	}
	request.ID = operation(t)
	_, err = c.Save(ctx, "alice", "project", request, selected, func(model.Entry) (io.ReadCloser, error) {
		t.Fatal("known stale save opened local contents")
		return nil, nil
	})
	var failure *remote.Error
	if !errors.As(err, &failure) || failure.Code != "stale" {
		t.Fatalf("stale result: %v", err)
	}
	t.Logf("small-edit HTTP upload=%d bytes; signature=%d bytes", counts.upload.Load(), counts.signature.Load())
}

func TestHTTPDeltaDownloadAccessExpiryAndUnrelatedContents(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	data, basis := make([]byte, 2<<20), make([]byte, 2<<20)
	random := rand.New(rand.NewSource(23))
	random.Read(data)
	random.Read(basis)
	e := entry("asset.bin", data)
	v, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{e}}, nil, func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "reader", Project: f.project.ID, Role: instance.Reader})
	if err != nil {
		t.Fatal(err)
	}
	c, err := remote.New(f.origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	c.InstanceID, c.ProjectID = f.store.ID, f.project.ID
	counts := &countedTransport{RoundTripper: c.HTTP.Transport}
	c.HTTP.Transport = counts
	var out bytes.Buffer
	if err := c.Download(ctx, "alice", "project", v.ID, e, bytes.NewReader(basis), int64(len(basis)), &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) || counts.download.Load() < int64(len(data))/2 || counts.downloadSignature.Load() == 0 {
		t.Fatalf("unrelated download: exact=%v wire=%d signature=%d", bytes.Equal(out.Bytes(), data), counts.download.Load(), counts.downloadSignature.Load())
	}
	for _, signature := range [][]byte{nil, []byte("invalid"), make([]byte, transfer.MaxSignatureSize+1)} {
		r, err := http.NewRequest("POST", f.origin+"/api/projects/alice/project/versions/"+v.ID+"/file?path=asset.bin", bytes.NewReader(signature))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := c.HTTP.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("invalid signature of %d bytes: HTTP %d", len(signature), response.StatusCode)
		}
	}
	rejected := func(client *remote.Client, code int) {
		t.Helper()
		var out bytes.Buffer
		err := client.Download(ctx, "alice", "project", v.ID, e, bytes.NewReader(basis), int64(len(basis)), &out)
		var failure *remote.Error
		if !errors.As(err, &failure) || failure.Status != code || out.Len() != 0 {
			t.Fatalf("expected HTTP %d without contents; got %v bytes=%d", code, err, out.Len())
		}
	}
	c.ProjectID = "wrong-project"
	rejected(c, 412)
	c.ProjectID = f.project.ID
	other, err := f.client.CreateProject(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "other", Project: other.ID, Role: instance.Reader})
	if err != nil {
		t.Fatal(err)
	}
	otherClient, err := remote.New(f.origin, otherToken)
	if err != nil {
		t.Fatal(err)
	}
	defer otherClient.HTTP.CloseIdleConnections()
	rejected(otherClient, 403)
	_, err = f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Base: v.ID, Changes: []model.Entry{entry(e.Path, basis)}}, nil, func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(basis)), nil })
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.WithProject(ctx, instance.Principal{User: f.user}, "alice", "project", instance.Maintainer, func(_ instance.Project, p *project.Store) error {
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
	if err != nil {
		t.Fatal(err)
	}
	rejected(c, 410)
}

func TestHTTPDownloadBatchAccessIdentityAndExpiry(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	contents := map[string][]byte{"a": []byte("one"), "d/b": []byte("two"), "empty": nil}
	files := []model.Entry{entry("a", contents["a"]), entry("empty", nil), entry("d/b", contents["d/b"])}
	open := func(e model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents[e.Path])), nil
	}
	v, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Changes: files}, nil, open)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "batch reader", Project: f.project.ID, Role: instance.Reader})
	if err != nil {
		t.Fatal(err)
	}
	c, err := remote.New(f.origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	c.InstanceID, c.ProjectID = f.store.ID, f.project.ID
	receive := func(i int, write func(io.Writer) error) error {
		var data bytes.Buffer
		if err := write(&data); err != nil {
			return err
		}
		if !bytes.Equal(data.Bytes(), contents[files[i].Path]) {
			t.Fatal("batch reordered or changed file bytes")
		}
		return nil
	}
	if err := c.DownloadBatch(ctx, "alice", "project", v.ID, files, receive); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		count int
		size  int64
	}{{0, 0}, {129, 1}, {9, 1 << 20}, {1, 1<<20 + 1}} {
		requested := make([]model.Entry, test.count)
		for i := range requested {
			requested[i] = entry(fmt.Sprintf("missing-%d", i), nil)
			requested[i].Size = test.size
		}
		body, err := json.Marshal(requested)
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.NewRequest("POST", f.origin+"/api/projects/alice/project/versions/"+v.ID+"/files", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := c.HTTP.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("batch limits %+v: HTTP %d", test, response.StatusCode)
		}
	}
	rejected := func(client *remote.Client, requested []model.Entry, code int) {
		t.Helper()
		err := client.DownloadBatch(ctx, "alice", "project", v.ID, requested, func(int, func(io.Writer) error) error { t.Fatal("refused batch streamed contents"); return nil })
		var failure *remote.Error
		if !errors.As(err, &failure) || failure.Status != code {
			t.Fatalf("expected HTTP %d, got %v", code, err)
		}
	}
	wrong := append([]model.Entry(nil), files...)
	wrong[2] = entry("d/b", []byte("different"))
	rejected(c, wrong, 400)
	c.ProjectID = "wrong-project"
	rejected(c, files, 412)
	c.ProjectID = f.project.ID
	other, err := f.client.CreateProject(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "other", Project: other.ID, Role: instance.Reader})
	if err != nil {
		t.Fatal(err)
	}
	otherClient, err := remote.New(f.origin, otherToken)
	if err != nil {
		t.Fatal(err)
	}
	defer otherClient.HTTP.CloseIdleConnections()
	rejected(otherClient, files, 403)
	contents["a"] = []byte("updated")
	if _, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Base: v.ID, Changes: []model.Entry{entry("a", contents["a"])}}, files, open); err != nil {
		t.Fatal(err)
	}
	controlToken, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "cleanup", Project: f.project.ID, Role: instance.Maintainer})
	if err != nil {
		t.Fatal(err)
	}
	control, err := remote.New(f.origin, controlToken)
	if err != nil {
		t.Fatal(err)
	}
	defer control.HTTP.CloseIdleConnections()
	control.InstanceID, control.ProjectID = f.store.ID, f.project.ID
	if err := control.SetRetention(ctx, "alice", "project", model.Retention{Versions: 1, HistoryDays: 90}); err != nil {
		t.Fatal(err)
	}
	preview, err := control.Cleanup(ctx, "alice", "project", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Cleanup(ctx, "alice", "project", preview.Token); err != nil {
		t.Fatal(err)
	}
	rejected(c, files, 410)
}

func TestHTTPUploadMustFinishBeforeAcceptance(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	for _, test := range []string{"extra", "truncated"} {
		t.Run(test, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			request := model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{entry("a", []byte("a"))}}
			part, err := writer.CreateFormField("request")
			if err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(part).Encode(request); err != nil {
				t.Fatal(err)
			}
			part, err = writer.CreateFormFile("file", "a")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write([]byte("a")); err != nil {
				t.Fatal(err)
			}
			if test == "extra" {
				if _, err := writer.CreateFormField("extra"); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			data := body.Bytes()
			if test == "truncated" {
				data = data[:len(data)-10]
			}
			r, err := http.NewRequestWithContext(ctx, "POST", f.origin+"/api/projects/alice/project/save", bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+f.token)
			r.Header.Set("Lode-Instance", f.store.ID)
			r.Header.Set("Lode-Project", f.project.ID)
			r.Header.Set("Content-Type", writer.FormDataContentType())
			response, err := f.client.HTTP.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 400 {
				t.Fatalf("status=%d", response.StatusCode)
			}
			head, err := f.client.Head(ctx, "alice", "project", "main")
			if err != nil || head != "" {
				t.Fatalf("incomplete request accepted: %q %v", head, err)
			}
		})
	}
}

func TestHTTPAccessAndIdentityLimits(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	readToken, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "read", Project: f.project.ID, Role: instance.Reader})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := remote.New(f.origin, readToken)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.HTTP.CloseIdleConnections()
	reader.InstanceID = f.store.ID
	reader.ProjectID = f.project.ID
	_, err = reader.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{entry("a", []byte("a"))}}, nil, nil)
	var failure *remote.Error
	if !errors.As(err, &failure) || failure.Status != 403 {
		t.Fatalf("reader publication: %v", err)
	}
	f.client.ProjectID = "wrong-project"
	_, err = f.client.Head(ctx, "alice", "project", "main")
	if !errors.As(err, &failure) || failure.Status != 412 {
		t.Fatalf("project identity: %v", err)
	}
	f.client.ProjectID = f.project.ID
	f.client.InstanceID = "wrong-instance"
	_, err = f.client.Me(ctx)
	if !errors.As(err, &failure) || failure.Status != 412 {
		t.Fatalf("instance identity: %v", err)
	}
	bad, err := remote.New(f.origin, "invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.HTTP.CloseIdleConnections()
	_, err = bad.Me(ctx)
	if !errors.As(err, &failure) || failure.Status != 401 {
		t.Fatalf("unauthenticated: %v", err)
	}
}

type lostResponse struct {
	http.RoundTripper
	drop atomic.Bool
}

func (t *lostResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.RoundTripper.RoundTrip(r)
	if err == nil && strings.HasSuffix(r.URL.Path, "/save") && response.StatusCode == 200 && t.drop.Swap(false) {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return nil, errors.New("connection lost after acceptance")
	}
	return response, err
}

func TestHTTPLostResponseResolvesWithoutSource(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	transport := &lostResponse{RoundTripper: f.client.HTTP.Transport}
	transport.drop.Store(true)
	f.client.HTTP.Transport = transport
	request := model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{entry("a", []byte("a"))}}
	_, err := f.client.Save(ctx, "alice", "project", request, nil, func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("a")), nil })
	if err == nil {
		t.Fatal("response was not lost")
	}
	head, err := f.client.Head(ctx, "alice", "project", "main")
	if err != nil || head != request.ID {
		t.Fatalf("accepted head: %q %v", head, err)
	}
	v, err := f.client.Save(ctx, "alice", "project", request, nil, func(model.Entry) (io.ReadCloser, error) { t.Fatal("recovery read local file"); return nil, nil })
	if err != nil || v.ID != request.ID {
		t.Fatalf("recovery: %v %v", v, err)
	}
	history, err := f.client.History(ctx, "alice", "project", "main", 10)
	if err != nil || len(history.Versions) != 1 {
		t.Fatalf("duplicate history: %v %v", history, err)
	}
}

type interruptedReader struct{ sent bool }

func (r *interruptedReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("source interrupted")
	}
	r.sent = true
	return copy(p, "prefix"), nil
}

func TestHTTPInterruptedUploadCanRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := apiTest(t)
	data := []byte("prefix and the remaining file")
	request := model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{entry("a", data)}}
	_, err := f.client.Save(ctx, "alice", "project", request, nil, func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(&interruptedReader{}), nil })
	if err == nil {
		t.Fatal("interrupted upload accepted")
	}
	head, err := f.client.Head(ctx, "alice", "project", "main")
	if err != nil || head != "" {
		t.Fatalf("interrupted head: %q %v", head, err)
	}
	v, err := f.client.Save(ctx, "alice", "project", request, nil, func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := f.client.Download(ctx, "alice", "project", v.ID, request.Changes[0], nil, 0, &got); err != nil || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("retried contents: %q %v", got.Bytes(), err)
	}
}
