package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

type countedUpload struct {
	io.ReadCloser
	bytes *atomic.Int64
}

func (r *countedUpload) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}

func clientWithHandler(t *testing.T, token string, handler http.Handler, http2 bool) *Client {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	if http2 {
		server.EnableHTTP2 = true
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if http2 {
		client.http.Transport = server.Client().Transport
	}
	t.Cleanup(client.Close)
	return client
}

func assertTransportFile(t *testing.T, client *Client, commit, path string, want []byte) {
	t.Helper()
	r, node, err := client.OpenFile(context.Background(), commit, path)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, want) || node.SHA256 != digestBytes(want) {
		t.Fatalf("%s reconstruction: bytes=%d read=%v close=%v digest=%s", path, len(got), readErr, closeErr, node.SHA256)
	}
}

func TestClientDeltaSparseMixedUploadAndReplay(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			basis := patternedBytes(2 << 20)
			baseRequest := deltaRequest("client-base", "", basis)
			baseRequest.Changes = append(baseRequest.Changes, change("omitted.bin", "keep omitted bytes"), change("small", "old"))
			base, err := f.client.Apply(ctx, baseRequest, opener(map[string]string{"asset.bin": string(basis), "omitted.bin": "keep omitted bytes", "small": "old"}, nil))
			if err != nil {
				t.Fatal(err)
			}
			var uploaded, signatures, prepares, sourceBytes atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if http2 && r.ProtoMajor != 2 {
					t.Errorf("request negotiated %s, expected HTTP/2", r.Proto)
				}
				switch r.URL.Path {
				case ChangesPreparePath:
					prepares.Add(1)
				case ChangesSignaturePath:
					signatures.Add(1)
				case "/v1/changes":
					r.Body = &countedUpload{ReadCloser: r.Body, bytes: &uploaded}
				}
				f.server.Config.Handler.ServeHTTP(w, r)
			})
			client := clientWithHandler(t, f.local.Token, handler, http2)
			target := bytes.Clone(basis)
			target[700_000] ^= 0x55
			added := patternedBytes(1 << 20)
			request := deltaRequest("client-delta", base.ID, target)
			request.Changes = append(request.Changes, changeBytes("new.bin", added), change("small", "new"))
			contents := map[string][]byte{"asset.bin": target, "new.bin": added, "small": []byte("new")}
			commit, err := client.Apply(ctx, request, func(path model.Path) (io.ReadCloser, error) {
				return &countedUpload{ReadCloser: io.NopCloser(bytes.NewReader(contents[path.String()])), bytes: &sourceBytes}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if uploaded.Load() >= int64(len(added))+(128<<10) || uploaded.Load() <= int64(len(added)) {
				t.Fatalf("upload bytes=%d: expected raw new file plus small delta/manifest", uploaded.Load())
			}
			if signatures.Load() != 2 || prepares.Load() != 1 || sourceBytes.Load() != int64(len(target)+len(added)+3) {
				t.Fatalf("signatures=%d prepares=%d source bytes=%d", signatures.Load(), prepares.Load(), sourceBytes.Load())
			}
			for path, want := range contents {
				assertTransportFile(t, client, commit.ID, path, want)
			}
			assertTransportFile(t, client, commit.ID, "omitted.bin", []byte("keep omitted bytes"))
			noOpen := func(model.Path) (io.ReadCloser, error) {
				t.Error("accepted replay reopened a source")
				return nil, errors.New("unexpected open")
			}
			replayed, err := client.Apply(ctx, request, noOpen)
			if err != nil || replayed.ID != commit.ID || signatures.Load() != 2 || prepares.Load() != 1 {
				t.Fatalf("replay=%+v error=%v signatures=%d prepares=%d", replayed, err, signatures.Load(), prepares.Load())
			}
			request.Message = "different sealed input"
			if _, err := client.Apply(ctx, request, noOpen); !errors.Is(err, project.ErrOperationConflict) {
				t.Fatalf("modified replay error=%v", err)
			}
		})
	}
}

func TestClientDeltaFallsBackOnlyForUnavailablePrepareRoute(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFixture(t)
			basis := patternedBytes(1 << 20)
			base := createTransportBase(t, f, basis)
			var uploads atomic.Int64
			client := clientWithHandler(t, f.local.Token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == ChangesPreparePath {
					w.WriteHeader(status)
					return
				}
				if r.URL.Path == ChangesSignaturePath {
					t.Error("fallback requested a signature")
				}
				if r.URL.Path == "/v1/changes" {
					r.Body = &countedUpload{ReadCloser: r.Body, bytes: &uploads}
				}
				f.server.Config.Handler.ServeHTTP(w, r)
			}), false)
			target := bytes.Clone(basis)
			target[5] ^= 1
			request := deltaRequest("old-server", base.ID, target)
			opens := 0
			commit, err := client.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) {
				opens++
				return io.NopCloser(bytes.NewReader(target)), nil
			})
			if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
				if err != nil || opens != 1 || uploads.Load() < int64(len(target)) {
					t.Fatalf("fallback commit=%+v err=%v opens=%d upload=%d", commit, err, opens, uploads.Load())
				}
				assertTransportFile(t, client, commit.ID, "asset.bin", target)
			} else if err == nil || opens != 0 || uploads.Load() != 0 {
				t.Fatalf("unexpected fallback after failure: err=%v opens=%d upload=%d", err, opens, uploads.Load())
			}
		})
	}
}

func TestClientDeltaDoesNotTreatMissingBaseAsMissingRoute(t *testing.T) {
	request := deltaRequest("missing-base", "missing", make([]byte, 1<<20))
	client := clientWithHandler(t, "token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/operation":
			writeServiceError(w, project.ErrOperationNotFound)
		case ChangesPreparePath:
			writeServiceError(w, project.ErrCommitNotFound)
		default:
			t.Errorf("unexpected request after prepare failure: %s", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}), false)
	_, err := client.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) {
		t.Error("opened source for missing base")
		return nil, errors.New("unexpected open")
	})
	if !errors.Is(err, project.ErrCommitNotFound) {
		t.Fatalf("missing base error=%v", err)
	}
}

type failingUpload struct{ io.Reader }

func (r failingUpload) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestClientDeltaRetriesPartialUploadAndRejectsChangedSource(t *testing.T) {
	f := newFixture(t)
	basis := patternedBytes(2 << 20)
	base := createTransportBase(t, f, basis)
	target := bytes.Clone(basis)
	target[900_000] ^= 0x7a
	request := deltaRequest("interrupted-delta", base.ID, target)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := f.client.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(failingUpload{bytes.NewReader(target[:len(target)/2])}), nil
	})
	if err == nil {
		t.Fatal("partial source accepted")
	}
	operation, err := f.client.Operation(ctx, request.OperationID)
	if err != nil || operation.State != project.OperationCapturing {
		t.Fatalf("interrupted operation=%+v err=%v", operation, err)
	}
	mutated := bytes.Clone(target)
	mutated[3] ^= 1
	if _, err := f.client.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(mutated)), nil
	}); !errors.Is(err, project.ErrInvalidRequest) {
		t.Fatalf("mutated source error=%v", err)
	}
	if head, err := f.client.Head(ctx, "main"); err != nil || head != base.ID {
		t.Fatalf("failed uploads changed head=%q err=%v", head, err)
	}
	commit, err := f.client.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(target)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertTransportFile(t, f.client, commit.ID, "asset.bin", target)
}

type dropAcceptedReply struct {
	base http.RoundTripper
	drop atomic.Bool
}

func (r *dropAcceptedReply) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	if err == nil && request.URL.Path == "/v1/changes" && response.StatusCode == http.StatusOK && r.drop.Swap(false) {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil, errors.New("injected lost accepted reply")
	}
	return response, err
}

func TestClientDeltaLostAcceptedReplyDoesNotReopenSource(t *testing.T) {
	f := newFixture(t)
	basis := patternedBytes(1 << 20)
	base := createTransportBase(t, f, basis)
	target := bytes.Clone(basis)
	target[100] ^= 1
	request := deltaRequest("lost-delta-reply", base.ID, target)
	wrapped := &dropAcceptedReply{base: f.client.http.Transport}
	wrapped.drop.Store(true)
	f.client.http.Transport = wrapped
	if _, err := f.client.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(target)), nil
	}); err == nil {
		t.Fatal("expected lost response")
	}
	operation, err := f.client.Operation(context.Background(), request.OperationID)
	if err != nil || operation.State != project.OperationAccepted {
		t.Fatalf("operation=%+v err=%v", operation, err)
	}
	commit, err := f.client.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) {
		t.Error("reopened source after acceptance")
		return nil, errors.New("source no longer exists")
	})
	if err != nil || commit.ID != operation.CommitID {
		t.Fatalf("recovered commit=%+v err=%v", commit, err)
	}
	assertTransportFile(t, f.client, commit.ID, "asset.bin", target)
}

func TestClientCancelsSignatureWhenUploadStops(t *testing.T) {
	for _, outcome := range []string{"rejected", "accepted", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started, stopped := make(chan struct{}), make(chan struct{})
			request := deltaRequest("cancel-signature", "base", make([]byte, 1<<20))
			var sourceOpens atomic.Int64
			client := clientWithHandler(t, "token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/operation":
					writeServiceError(w, project.ErrOperationNotFound)
				case ChangesPreparePath:
					writeJSON(w, project.Operation{ID: request.OperationID, Branch: request.Branch, Base: request.Base, State: project.OperationCapturing})
				case ChangesSignaturePath:
					close(started)
					<-r.Context().Done()
					close(stopped)
				case "/v1/changes":
					if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
						t.Error(err)
						return
					}
					select {
					case <-started:
					case <-r.Context().Done():
						return
					}
					switch outcome {
					case "accepted":
						writeJSON(w, project.Commit{ID: "already-accepted"})
					case "rejected":
						writeServiceError(w, project.ErrStaleHead)
					case "canceled":
						cancel()
					}
				default:
					http.NotFound(w, r)
				}
			}), false)
			commit, err := client.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
				sourceOpens.Add(1)
				return nil, errors.New("unexpected source open")
			})
			switch outcome {
			case "accepted":
				if err != nil || commit.ID != "already-accepted" {
					t.Fatalf("accepted outcome=%+v err=%v", commit, err)
				}
			case "rejected":
				if !errors.Is(err, project.ErrStaleHead) {
					t.Fatalf("rejection=%v", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation=%v", err)
				}
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("secondary signature request remained live")
			}
			if sourceOpens.Load() != 0 {
				t.Fatal("source opened while signature was canceled")
			}
		})
	}
}

func TestClientRejectsMismatchedAndMalformedSignature(t *testing.T) {
	f := newFixture(t)
	basis := patternedBytes(1 << 20)
	base := createTransportBase(t, f, basis)
	target := bytes.Clone(basis)
	target[2] ^= 1
	mutations := []struct {
		name  string
		apply func(*project.FileSignature)
	}{
		{"operation", func(s *project.FileSignature) { s.OperationID = "another-operation" }},
		{"base", func(s *project.FileSignature) { s.Base = "another-base" }},
		{"path", func(s *project.FileSignature) { s.Path = "another-path" }},
		{"basis-path", func(s *project.FileSignature) { s.Basis.Path = "another-path" }},
		{"basis-size", func(s *project.FileSignature) { s.Basis.Size = -1 }},
		{"basis-digest", func(s *project.FileSignature) { s.Basis.SHA256 = strings.Repeat("x", 64) }},
		{"header", func(s *project.FileSignature) { s.Data[4] = 0xff }},
		{"truncated", func(s *project.FileSignature) { s.Data = s.Data[:len(s.Data)-1] }},
	}
	for i, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			var sourceBytes atomic.Int64
			request := deltaRequest(fmt.Sprintf("invalid-signature-%d", i), base.ID, target)
			client := clientWithHandler(t, f.local.Token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != ChangesSignaturePath {
					f.server.Config.Handler.ServeHTTP(w, r)
					return
				}
				signature, err := f.local.Service.Signature(r.Context(), request.OperationID, "asset.bin")
				if err != nil {
					writeServiceError(w, err)
					return
				}
				mutation.apply(&signature)
				_ = json.NewEncoder(w).Encode(signature)
			}), false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := client.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
				return &countedUpload{ReadCloser: io.NopCloser(bytes.NewReader(target)), bytes: &sourceBytes}, nil
			})
			if err == nil || sourceBytes.Load() != 0 {
				t.Fatalf("bad signature err=%v source reads=%d", err, sourceBytes.Load())
			}
			if head, err := f.client.Head(ctx, "main"); err != nil || head != base.ID {
				t.Fatalf("bad signature changed head=%q err=%v", head, err)
			}
		})
	}
}
