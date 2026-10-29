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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/transfer"
)

func TestMissingCredentialsDoNotSendRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client, err := New(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTP.CloseIdleConnections()
	if _, err := client.Me(context.Background()); !errors.Is(err, ErrCredentialsRequired) {
		t.Fatalf("missing credentials: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("sent an unauthenticated request")
	}
}

func TestIncompleteOrUnexpectedInventoryIsRejected(t *testing.T) {
	h := sha256.Sum256(nil)
	e := model.Entry{Path: "selected/a", Kind: model.File, SHA256: hex.EncodeToString(h[:])}
	b, err := json.Marshal(InventoryRecord{Entry: &e})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"missing count":   string(b) + "\n",
		"wrong count":     string(b) + "\n{\"count\":2}\n",
		"duplicate":       string(b) + "\n" + string(b) + "\n{\"count\":2}\n",
		"extra count":     string(b) + "\n{\"count\":1}\n{\"count\":1}\n",
		"truncated entry": string(b[:len(b)-4]),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			client, err := New(server.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			defer client.HTTP.CloseIdleConnections()
			entries, err := client.Inventory(context.Background(), "owner", "project", "version", model.Selection{{Path: "selected", Include: true}})
			if err == nil || entries != nil {
				t.Fatalf("accepted %v: %v", entries, err)
			}
		})
	}
}

func TestDownloadIdentityIsVerified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "wrong") }))
	defer server.Close()
	client, err := New(server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTP.CloseIdleConnections()
	h := sha256.Sum256([]byte("right"))
	e := model.Entry{Path: "file", Kind: model.File, Size: 5, SHA256: hex.EncodeToString(h[:])}
	var out bytes.Buffer
	if err := client.Download(context.Background(), "owner", "project", "version", e, nil, 0, &out); err == nil {
		t.Fatal("wrong bytes accepted")
	}
}

func TestDeltaDownloadVerifiesReconstructionAndCompletion(t *testing.T) {
	ctx := context.Background()
	basis := bytes.Repeat([]byte("basis"), 1<<19)
	target := append([]byte(nil), basis...)
	copy(target[1<<20:], bytes.Repeat([]byte("change"), 1000))
	h := sha256.Sum256(target)
	e := model.Entry{Path: "file", Kind: model.File, Size: int64(len(target)), SHA256: hex.EncodeToString(h[:])}
	for _, mode := range []string{"complete", "truncated", "extra", "wrong-identity", "wrong-encoding", "incomplete-response"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Error("local basis did not request a delta")
				}
				var delta bytes.Buffer
				if err := transfer.Delta(ctx, r.Body, bytes.NewReader(target), &delta); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", DeltaType)
				data := delta.Bytes()
				switch mode {
				case "truncated":
					data = data[:len(data)-1]
				case "extra":
					data = append(data, 0)
				case "wrong-encoding":
					w.Header().Set("Content-Type", "application/octet-stream")
				case "incomplete-response":
					w.Header().Set("Content-Length", fmt.Sprint(len(data)+1))
				}
				w.Write(data)
			}))
			defer server.Close()
			c, err := New(server.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			defer c.HTTP.CloseIdleConnections()
			expected := e
			if mode == "wrong-identity" {
				h := sha256.Sum256(basis)
				expected.SHA256 = hex.EncodeToString(h[:])
			}
			var out bytes.Buffer
			err = c.Download(ctx, "owner", "project", "version", expected, bytes.NewReader(basis), int64(len(basis)), &out)
			if mode == "complete" {
				if err != nil || !bytes.Equal(out.Bytes(), target) {
					t.Fatalf("reconstruction: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid download accepted")
			}
		})
	}
}

func TestDeltaDownloadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	basis := bytes.Repeat([]byte("x"), 1<<20)
	h := sha256.Sum256(basis)
	e := model.Entry{Path: "file", Kind: model.File, Size: int64(len(basis)), SHA256: hex.EncodeToString(h[:])}
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", DeltaType)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	c, err := New(server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	err = c.Download(ctx, "owner", "project", "version", e, bytes.NewReader(basis), e.Size, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled delta: %v", err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled delta kept server request open")
	}
}

func TestDownloadBatchVerifiesFilesAndCompletion(t *testing.T) {
	var files []model.Entry
	for i, data := range []string{"abc", "", "defg"} {
		h := sha256.Sum256([]byte(data))
		files = append(files, model.Entry{Path: fmt.Sprint(i), Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])})
	}
	for _, tc := range []struct {
		name, body string
		verified   int
		ok         bool
	}{
		{"complete", "abcdefg", 3, true},
		{"truncated", "abcde", 2, false},
		{"wrong-file", "abXdefg", 0, false},
		{"reordered", "defgabc", 0, false},
		{"extra", "abcdefgX", 3, false},
		{"incomplete-response", "abcdefg", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "incomplete-response" {
					w.Header().Set("Content-Length", "100")
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			c, err := New(server.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			defer c.HTTP.CloseIdleConnections()
			verified := 0
			err = c.DownloadBatch(context.Background(), "owner", "project", "version", files, func(i int, write func(io.Writer) error) error {
				if err := write(io.Discard); err != nil {
					return err
				}
				verified++
				return nil
			})
			if (err == nil) != tc.ok || verified != tc.verified {
				t.Fatalf("verified=%d err=%v", verified, err)
			}
		})
	}
}

func TestDownloadBatchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "a")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c, err := New(server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	h := sha256.Sum256([]byte("a"))
	files := []model.Entry{{Path: "a", Kind: model.File, Size: 1, SHA256: hex.EncodeToString(h[:])}, {Path: "b", Kind: model.File, Size: 1, SHA256: hex.EncodeToString(h[:])}}
	err = c.DownloadBatch(ctx, "owner", "project", "version", files, func(i int, write func(io.Writer) error) error {
		err := write(io.Discard)
		if i == 0 {
			cancel()
		}
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled download: %v", err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled batch kept the server request open")
	}
}

func TestRedirectDoesNotSendCredentials(t *testing.T) {
	var requests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer source.Close()
	client, err := New(source.URL, "private-test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTP.CloseIdleConnections()
	_, err = client.Me(context.Background())
	var response *Error
	if !errors.As(err, &response) || response.Status != 302 || requests.Load() != 0 {
		t.Fatalf("redirect: %v; requests=%d", err, requests.Load())
	}
}

func TestCollectionWaitsForResponseAndHonorsCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRequest), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if r.Method != "POST" || r.URL.Path != "/api/projects/owner/project/collect" || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Lode-Instance") != "instance" || r.Header.Get("Lode-Project") != "project-id" {
					t.Error("collection lost request identity")
				}
				close(entered)
				select {
				case <-release:
					fmt.Fprint(w, `{"before":{"allocated":20},"after":{"allocated":10}}`)
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			client, err := New(server.URL, "test-token")
			if err != nil {
				t.Fatal(err)
			}
			defer client.HTTP.CloseIdleConnections()
			client.InstanceID, client.ProjectID = "instance", "project-id"
			transport := client.HTTP.Transport.(*http.Transport)
			transport.ResponseHeaderTimeout = 5 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				result, err := client.Collect(ctx, "owner", "project")
				if err == nil && (result.Before.Allocated != 20 || result.After.Allocated != 10) {
					err = errors.New("incorrect collection result")
				}
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("request did not reach server: %v", err)
			}
			select {
			case err := <-done:
				t.Fatalf("collection retained the ordinary header timeout: %v", err)
			case <-time.After(40 * time.Millisecond):
			}
			if cancelRequest {
				cancel()
			} else {
				close(release)
			}
			err = <-done
			if cancelRequest && !errors.Is(err, context.Canceled) || !cancelRequest && err != nil {
				t.Fatalf("collection: %v", err)
			}
			if transport.ResponseHeaderTimeout != 5*time.Millisecond {
				t.Fatal("collection changed the shared transport")
			}
		})
	}
}
