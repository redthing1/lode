package remote

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transfer"
)

type observedRequest struct {
	method        string
	path          string
	body          []byte
	responseBytes int64
	encoding      string
}

type requestObserver struct {
	mu       sync.Mutex
	requests []*observedRequest
}

func (o *requestObserver) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Method == http.MethodPost && (r.URL.Path == "/v1/save" || r.URL.Path == "/v1/read") {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		record := &observedRequest{method: r.Method, path: r.URL.RequestURI(), body: body}
		o.mu.Lock()
		o.requests = append(o.requests, record)
		o.mu.Unlock()
		capture := &observedWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		o.mu.Lock()
		record.responseBytes = capture.bytes
		record.encoding = capture.Header().Get("Lode-Encoding")
		o.mu.Unlock()
	})
}

type observedWriter struct {
	http.ResponseWriter
	bytes int64
}

func (w *observedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (o *requestObserver) snapshot() []observedRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make([]observedRequest, len(o.requests))
	for i, request := range o.requests {
		result[i] = *request
	}
	return result
}

func transferFixture(t *testing.T) (*project.Project, *Client, *requestObserver) {
	t.Helper()
	p, err := project.Create(context.Background(), t.TempDir()+"/project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("close project: %v", err)
		}
	})
	o := &requestObserver{}
	srv := httptest.NewServer(o.handler(Handler(p)))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, p.Token(), p.ID())
	if err != nil {
		t.Fatal(err)
	}
	return p, c, o
}

func opaqueData(t *testing.T, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func transferEntry(path string, data []byte) model.Entry {
	h := sha256.Sum256(data)
	return model.Entry{Path: path, Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}
}

func saveBytes(ctx context.Context, c *Client, req project.Request, data []byte) (project.Commit, error) {
	return c.Save(ctx, req, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
}

func assertStoredBytes(t *testing.T, p *project.Project, commit string, entry model.Entry, want []byte) {
	t.Helper()
	var got []byte
	err := p.Read(context.Background(), commit, entry.Path, func(_ model.Entry, r io.Reader) error {
		var err error
		got, err = io.ReadAll(r)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stored %s differs: got %d bytes, want %d", entry.Path, len(got), len(want))
	}
}

func TestHTTPTransferUsesDeltaAndBoundsFullRewrite(t *testing.T) {
	ctx := context.Background()
	p, c, observer := transferFixture(t)
	baseBytes := opaqueData(t, 4<<20)
	baseEntry := transferEntry("opaque.bin", baseBytes)
	base, err := saveBytes(ctx, c, project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "base", Changes: []model.Entry{baseEntry}}, baseBytes)
	if err != nil {
		t.Fatal(err)
	}
	observer.mu.Lock()
	observer.requests = nil
	observer.mu.Unlock()

	target := append([]byte(nil), baseBytes...)
	copy(target[2<<20:2<<20+7], []byte{0, 0xff, 1, 0, 2, 0xfe, 3})
	entry := transferEntry("opaque.bin", target)
	req := project.Request{ID: project.NewID(), Branch: "main", Base: base.ID, Author: "test", Message: "small edit", Changes: []model.Entry{entry}}
	commit, err := saveBytes(ctx, c, req, target)
	if err != nil {
		t.Fatal(err)
	}
	seen := observer.snapshot()
	if len(seen) != 2 || seen[0].method != http.MethodGet || !strings.HasPrefix(seen[0].path, "/v1/signature?") || seen[1].method != http.MethodPost || seen[1].path != "/v1/save" {
		t.Fatalf("request order = %#v, want signature GET followed by save POST", seen)
	}
	if !bytes.Contains(seen[1].body, []byte("Lode-Encoding: rsync")) {
		t.Fatal("small edit was not sent as an rsync delta")
	}
	if int64(len(seen[1].body)) >= entry.Size/4 {
		t.Fatalf("delta multipart body is %d bytes for %d-byte target", len(seen[1].body), entry.Size)
	}
	assertStoredBytes(t, p, commit.ID, entry, target)

	observer.mu.Lock()
	observer.requests = nil
	observer.mu.Unlock()
	fullRewrite := opaqueData(t, len(target))
	rewriteEntry := transferEntry("opaque.bin", fullRewrite)
	rewriteReq := project.Request{ID: project.NewID(), Branch: "main", Base: commit.ID, Author: "test", Message: "full rewrite", Changes: []model.Entry{rewriteEntry}}
	rewrite, err := saveBytes(ctx, c, rewriteReq, fullRewrite)
	if err != nil {
		t.Fatal(err)
	}
	seen = observer.snapshot()
	var post []byte
	for _, request := range seen {
		if request.method == http.MethodPost && request.path == "/v1/save" {
			post = request.body
		}
	}
	if len(post) == 0 || int64(len(post)) > 2*rewriteEntry.Size+(64<<10) {
		t.Fatalf("full-rewrite multipart body size = %d for %d-byte target", len(post), rewriteEntry.Size)
	}
	assertStoredBytes(t, p, rewrite.ID, rewriteEntry, fullRewrite)
}

func TestDownloadUsesDeltaRawFallbackAndFullRewrite(t *testing.T) {
	ctx := context.Background()
	_, c, observer := transferFixture(t)
	baseBytes := opaqueData(t, 4<<20)
	baseEntry := transferEntry("download.bin", baseBytes)
	base, err := saveBytes(ctx, c, project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "base", Changes: []model.Entry{baseEntry}}, baseBytes)
	if err != nil {
		t.Fatal(err)
	}
	target := append([]byte(nil), baseBytes...)
	copy(target[2<<20:2<<20+8], []byte{0, 255, 1, 0, 254, 2, 0, 253})
	targetEntry := transferEntry(baseEntry.Path, target)
	commit, err := saveBytes(ctx, c, project.Request{ID: project.NewID(), Branch: "main", Base: base.ID, Author: "test", Message: "small edit", Changes: []model.Entry{targetEntry}}, target)
	if err != nil {
		t.Fatal(err)
	}
	observer.mu.Lock()
	observer.requests = nil
	observer.mu.Unlock()
	var got bytes.Buffer
	if err := c.Download(ctx, commit.ID, targetEntry, bytes.NewReader(baseBytes), int64(len(baseBytes)), &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), target) {
		t.Fatal("delta download differs from target")
	}
	seen := observer.snapshot()
	if len(seen) != 1 || seen[0].method != http.MethodPost || !strings.HasPrefix(seen[0].path, "/v1/read?") || len(seen[0].body) == 0 || seen[0].encoding != "rsync" {
		t.Fatalf("delta download request = %#v", seen)
	}
	if seen[0].responseBytes >= int64(len(target))/4 {
		t.Fatalf("delta response was not small: %d bytes", seen[0].responseBytes)
	}

	observer.mu.Lock()
	observer.requests = nil
	observer.mu.Unlock()
	got.Reset()
	if err := c.Download(ctx, commit.ID, targetEntry, nil, 0, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), target) {
		t.Fatal("raw download differs from target")
	}
	seen = observer.snapshot()
	if len(seen) != 1 || seen[0].method != http.MethodGet || !strings.HasPrefix(seen[0].path, "/v1/read?") || seen[0].encoding == "rsync" {
		t.Fatalf("raw download request = %#v", seen)
	}

	rewrite := opaqueData(t, len(target))
	rewriteEntry := transferEntry(baseEntry.Path, rewrite)
	rewritten, err := saveBytes(ctx, c, project.Request{ID: project.NewID(), Branch: "main", Base: commit.ID, Author: "test", Message: "full rewrite", Changes: []model.Entry{rewriteEntry}}, rewrite)
	if err != nil {
		t.Fatal(err)
	}
	observer.mu.Lock()
	observer.requests = nil
	observer.mu.Unlock()
	got.Reset()
	if err := c.Download(ctx, rewritten.ID, rewriteEntry, bytes.NewReader(target), int64(len(target)), &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), rewrite) {
		t.Fatal("full rewrite delta differs from target")
	}
	seen = observer.snapshot()
	if len(seen) != 1 || seen[0].method != http.MethodPost || seen[0].encoding != "rsync" || seen[0].responseBytes < int64(len(rewrite))/2 {
		t.Fatalf("full rewrite download did not use a large delta: %#v", seen)
	}
}

func TestReadRejectsOversizedSignature(t *testing.T) {
	_, c, _ := transferFixture(t)
	resp, err := c.request(context.Background(), http.MethodPost, "read?commit="+project.NewID()+"&path=file", bytes.NewReader(make([]byte, transfer.MaxSignatureSize+1)), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("oversized signature status = %s", resp.Status)
	}
}

func TestHTTPTransferUsesRawBodyWithoutSignatureAndRetryOutcomesStayStable(t *testing.T) {
	ctx := context.Background()
	p, c, observer := transferFixture(t)
	baseBytes := opaqueData(t, 4<<20)
	baseEntry := transferEntry("base.bin", baseBytes)
	base, err := saveBytes(ctx, c, project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "base", Changes: []model.Entry{baseEntry}}, baseBytes)
	if err != nil {
		t.Fatal(err)
	}
	observer.mu.Lock()
	observer.requests = nil
	observer.mu.Unlock()

	newBytes := opaqueData(t, 1<<20)
	newEntry := transferEntry("new.bin", newBytes)
	req := project.Request{ID: project.NewID(), Branch: "main", Base: base.ID, Author: "test", Message: "new file", Changes: []model.Entry{newEntry}}
	commit, err := saveBytes(ctx, c, req, newBytes)
	if err != nil {
		t.Fatal(err)
	}
	seen := observer.snapshot()
	if len(seen) != 2 || seen[0].method != http.MethodGet || !strings.HasPrefix(seen[0].path, "/v1/signature?") || seen[1].method != http.MethodPost {
		t.Fatalf("new-file requests = %#v", seen)
	}
	if bytes.Contains(seen[1].body, []byte("Lode-Encoding: rsync")) || int64(len(seen[1].body)) < newEntry.Size {
		t.Fatalf("new file was not sent raw: multipart body has %d bytes", len(seen[1].body))
	}
	assertStoredBytes(t, p, commit.ID, newEntry, newBytes)

	staleReq := project.Request{ID: project.NewID(), Branch: "main", Base: base.ID, Author: "test", Message: "stale", Changes: []model.Entry{baseEntry}}
	if _, err := saveBytes(ctx, c, staleReq, baseBytes); !errors.Is(err, project.ErrStale) {
		t.Fatalf("stale save error = %v", err)
	}
	acceptedBytes := opaqueData(t, 1<<20)
	acceptedEntry := transferEntry("base.bin", acceptedBytes)
	acceptedReq := project.Request{ID: project.NewID(), Branch: "main", Base: commit.ID, Author: "test", Message: "accepted", Changes: []model.Entry{acceptedEntry}}
	accepted, err := saveBytes(ctx, c, acceptedReq, acceptedBytes)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := saveBytes(ctx, c, acceptedReq, opaqueData(t, 1<<20))
	if err != nil || retried.ID != accepted.ID {
		t.Fatalf("accepted retry = %q, %v; want %q", retried.ID, err, accepted.ID)
	}
	assertStoredBytes(t, p, accepted.ID, acceptedEntry, acceptedBytes)
}

func fetchSignature(t *testing.T, c *Client, commit, path string) []byte {
	t.Helper()
	query := url.Values{"commit": {commit}, "path": {path}}
	resp, err := c.request(context.Background(), http.MethodGet, "signature?"+query.Encode(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signature response = %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func multipartDelta(t *testing.T, req project.Request, path string, delta []byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	metadata, err := w.CreateFormField("metadata")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(metadata).Encode(req); err != nil {
		t.Fatal(err)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="payload"`)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Lode-Path", path)
	h.Set("Lode-Encoding", "rsync")
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(delta); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), w.FormDataContentType()
}

func makeDelta(t *testing.T, signature, target []byte) []byte {
	t.Helper()
	var delta bytes.Buffer
	if err := transfer.Delta(context.Background(), bytes.NewReader(signature), bytes.NewReader(target), &delta); err != nil {
		t.Fatal(err)
	}
	return delta.Bytes()
}

func TestHTTPTransferRejectsBadDeltasAndRetriesSameIntent(t *testing.T) {
	ctx := context.Background()
	p, c, _ := transferFixture(t)
	baseBytes := opaqueData(t, 4<<20)
	baseEntry := transferEntry("opaque.bin", baseBytes)
	base, err := saveBytes(ctx, c, project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "base", Changes: []model.Entry{baseEntry}}, baseBytes)
	if err != nil {
		t.Fatal(err)
	}
	signature := fetchSignature(t, c, base.ID, baseEntry.Path)
	if len(signature) == 0 {
		t.Fatal("large retained basis returned an empty signature")
	}

	for _, mode := range []string{"forged", "truncated", "trailing", "size mismatch", "hash mismatch"} {
		t.Run(mode, func(t *testing.T) {
			want := opaqueData(t, len(baseBytes))
			entry := transferEntry(baseEntry.Path, want)
			deltaTarget := want
			switch mode {
			case "size mismatch":
				deltaTarget = append(append([]byte(nil), want...), 0x7f)
			case "hash mismatch":
				deltaTarget = opaqueData(t, len(want))
			}
			delta := makeDelta(t, signature, deltaTarget)
			switch mode {
			case "forged":
				delta = bytes.Repeat([]byte{0xff}, 128)
			case "truncated":
				delta = delta[:len(delta)-1]
			case "trailing":
				delta = append(append([]byte(nil), delta...), 0)
			}
			req := project.Request{ID: project.NewID(), Branch: "main", Base: base.ID, Author: "test", Message: "recover malformed " + mode, Changes: []model.Entry{entry}}
			body, contentType := multipartDelta(t, req, entry.Path, delta)
			resp, err := c.request(ctx, http.MethodPost, "save", bytes.NewReader(body), contentType)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Fatalf("bad delta accepted with status %s", resp.Status)
			}
			if head, err := p.Head(ctx, "main"); err != nil || head != base.ID {
				t.Fatalf("bad delta published: head=%q err=%v", head, err)
			}
			commit, err := saveBytes(ctx, c, req, want)
			if err != nil {
				t.Fatalf("retry original intent: %v", err)
			}
			assertStoredBytes(t, p, commit.ID, entry, want)
			base.ID = commit.ID
			baseEntry = entry
			signature = fetchSignature(t, c, base.ID, baseEntry.Path)
		})
	}
}
