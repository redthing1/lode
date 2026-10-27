package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"reflect"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestStreamSaveAndRead(t *testing.T) {
	ctx := context.Background()
	p, err := project.Create(ctx, t.TempDir()+"/project")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	bootstrap, err := New(srv.URL, p.Token(), "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := bootstrap.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != p.ID() {
		t.Fatalf("project ID %q", info.ID)
	}
	c, err := New(srv.URL, p.Token(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBranch(ctx, "topic", ""); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if branches, err := c.Branches(ctx); err != nil || len(branches) != 2 || branches[0].Name != "main" || branches[1].Name != "topic" {
		t.Fatalf("branches %#v: %v", branches, err)
	}
	data := []byte("opaque file bytes")
	hash := sha256.Sum256(data)
	e := model.Entry{Path: "nested/é%data.bin", Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	req := project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "save", Changes: []model.Entry{e}}
	commit, err := c.Save(ctx, req, project.Source{Open: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteBranch(ctx, "topic", ""); err != nil {
		t.Fatalf("delete empty branch: %v", err)
	}
	if err := c.CreateBranch(ctx, "topic", commit.ID); err != nil {
		t.Fatalf("create branch at commit: %v", err)
	}
	if err := c.CreateBranch(ctx, "topic", commit.ID); err != nil {
		t.Fatalf("same-tip create retry: %v", err)
	}
	if err := c.CreateBranch(ctx, "topic", ""); !errors.Is(err, project.ErrExists) {
		t.Fatalf("branch collision: %v", err)
	}
	if err := c.DeleteBranch(ctx, "topic", project.NewID()); !errors.Is(err, project.ErrStale) {
		t.Fatalf("stale branch delete: %v", err)
	}
	if err := c.DeleteBranch(ctx, "topic", commit.ID); err != nil {
		t.Fatalf("delete branch: %v", err)
	}
	entries, err := c.Entries(ctx, commit.ID, model.Selection{{Include: true}})
	if err != nil {
		t.Fatalf("commit=%#v err=%v", commit, err)
	}
	if len(entries) != 2 || entries[1].Path != "nested/é%data.bin" {
		t.Fatalf("entries: %#v", entries)
	}
	r, err := c.Read(ctx, commit.ID, "nested/é%data.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if string(got) != string(data) {
		t.Fatalf("read %q", got)
	}
	acceptedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	accepted, err := c.Save(acceptedCtx, req, project.Source{Open: func(string) (io.ReadCloser, error) { return io.NopCloser(&endlessReader{}), nil }})
	if err != nil || accepted.ID != commit.ID {
		t.Fatalf("accepted retry %#v: %v", accepted, err)
	}
	stale := req
	stale.ID = project.NewID()
	if _, err := c.Save(ctx, stale, project.Source{Open: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}); !errors.Is(err, project.ErrStale) {
		t.Fatalf("stale save error: %v", err)
	}
	if head, err := c.Head(ctx, "main"); err != nil || head != commit.ID {
		t.Fatalf("head %q: %v", head, err)
	}
}

type endlessReader struct{}

func (*endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestClientRejectsNonLocalHTTP(t *testing.T) {
	if _, err := New("http://example.com", "token", ""); err == nil {
		t.Fatal("accepted non-loopback HTTP")
	}
}

func TestSaveRejectsUnrelatedSuccessfulReply(t *testing.T) {
	id := project.NewID()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(projectHeader, id)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, "token", id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Save(context.Background(), project.Request{ID: project.NewID(), Branch: "main", Message: "mkdir", Changes: []model.Entry{{Path: "empty", Kind: model.Directory}}}, project.Source{})
	if err == nil {
		t.Fatal("accepted a successful reply without the original operation identity")
	}
}

func TestUnauthorizedRequest(t *testing.T) {
	p, err := project.Create(context.Background(), t.TempDir()+"/project")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	c, err := New(srv.URL, "wrong-token", p.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("unauthorized request succeeded")
	}
}

func TestBoundIdentityMismatchCannotPublish(t *testing.T) {
	p, err := project.Create(context.Background(), t.TempDir()+"/project")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	c, err := New(srv.URL, p.Token(), project.NewID())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("must not publish")
	hash := sha256.Sum256(data)
	e := model.Entry{Path: "file.bin", Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	req := project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "wrong project", Changes: []model.Entry{e}}
	_, err = c.Save(context.Background(), req, project.Source{Open: func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }})
	if err == nil {
		t.Fatal("wrong project identity was accepted")
	}
	head, err := p.Head(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if head != "" {
		t.Fatalf("wrong-project request advanced branch to %s", head)
	}
}

func TestMalformedMultipartParts(t *testing.T) {
	p, err := project.Create(context.Background(), t.TempDir()+"/project")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	data := []byte("file")
	hash := sha256.Sum256(data)
	entry := model.Entry{Path: "file.bin", Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	req := project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "malformed probe", Changes: []model.Entry{entry}}

	missing, contentType := multipartBody(t, req, data, false)
	response, err := postMultipart(srv.URL, p.Token(), p.ID(), contentType, missing)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing part status %d", response.StatusCode)
	}

	extra, contentType := multipartBody(t, req, data, true)
	response, err = postMultipart(srv.URL, p.Token(), p.ID(), contentType, extra)
	if err == nil {
		response.Body.Close()
		t.Fatal("extra part was not aborted")
	}
}

func TestOversizedPartAbortsWithoutDraining(t *testing.T) {
	p, err := project.Create(context.Background(), t.TempDir()+"/project")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	data := []byte("x")
	hash := sha256.Sum256(data)
	entry := model.Entry{Path: "file.bin", Kind: model.File, Size: 1, SHA256: hex.EncodeToString(hash[:])}
	req := project.Request{ID: project.NewID(), Branch: "main", Author: "test", Message: "oversize probe", Changes: []model.Entry{entry}}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	meta, err := w.CreateFormField("metadata")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(meta).Encode(req); err != nil {
		t.Fatal(err)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="payload"`)
	h.Set("Lode-Path", entry.Path)
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("z"), 2<<20)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	reader := &countingReader{r: bytes.NewReader(body.Bytes())}
	request := httptest.NewRequest(http.MethodPost, "/v1/save", reader)
	request.Header.Set("Authorization", "Bearer "+p.Token())
	request.Header.Set(projectHeader, p.ID())
	request.Header.Set("Content-Type", w.FormDataContentType())
	response := httptest.NewRecorder()
	Handler(p).ServeHTTP(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("oversized content accepted")
	}
	if reader.n >= int64(body.Len()/2) {
		t.Fatalf("read %d bytes from %d byte upload before abort", reader.n, body.Len())
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.n += int64(n)
	return n, err
}

func multipartBody(t *testing.T, req project.Request, data []byte, includeFiles bool) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	meta, err := w.CreateFormField("metadata")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(meta).Encode(req); err != nil {
		t.Fatal(err)
	}
	if includeFiles {
		add := func(name string, contents []byte) {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="file"; filename="payload"`)
			h.Set("Lode-Path", name)
			h.Set("Content-Type", "application/octet-stream")
			part, err := w.CreatePart(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(contents); err != nil {
				t.Fatal(err)
			}
		}
		add("file.bin", data)
		add("extra.bin", []byte("extra"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), w.FormDataContentType()
}

func postMultipart(baseURL, token, projectID, contentType string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/save", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(projectHeader, projectID)
	req.Header.Set("Content-Type", contentType)
	return http.DefaultClient.Do(req)
}

func TestTypedErrorWireRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"conflicts", fmt.Errorf("wrapped: %w", &model.Conflicts{Paths: []string{"a.bin", "dir/b.bin"}}), "conflicts"},
		{"merge bases", fmt.Errorf("wrapped: %w", &project.MergeBaseConflict{Bases: []string{"abc", "def"}}), "merge_base"},
		{"already merged", fmt.Errorf("wrapped: %w", project.ErrAlreadyMerged), "unchanged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&handler{}).fail(recorder, tc.err)
			var payload wireError
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Code != tc.code {
				t.Fatalf("code %q, want %q", payload.Code, tc.code)
			}
			got := parseError(recorder.Result())
			switch tc.code {
			case "conflicts":
				var conflicts *model.Conflicts
				if !errors.As(got, &conflicts) || !reflect.DeepEqual(conflicts.Paths, []string{"a.bin", "dir/b.bin"}) {
					t.Fatalf("round-trip conflicts: %#v (%v)", conflicts, got)
				}
			case "merge_base":
				var mergeBases *project.MergeBaseConflict
				if !errors.As(got, &mergeBases) || !reflect.DeepEqual(mergeBases.Bases, []string{"abc", "def"}) {
					t.Fatalf("round-trip bases: %#v (%v)", mergeBases, got)
				}
			case "unchanged":
				if !errors.Is(got, project.ErrAlreadyMerged) {
					t.Fatalf("round-trip already merged: %v", got)
				}
			}
		})
	}
}
