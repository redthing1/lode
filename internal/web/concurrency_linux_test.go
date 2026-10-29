package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
)

// Pause at the server's streaming boundary, after it has opened a published
// file or entered capture. Client/socket buffering cannot release this barrier.
type transferPause struct {
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	enter   sync.Once
	leave   sync.Once
}

func (p *transferPause) wait(ctx context.Context) {
	p.enter.Do(func() {
		close(p.reached)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	})
}

func (p *transferPause) resume() { p.leave.Do(func() { close(p.release) }) }

type pausedUpload struct {
	io.ReadCloser
	pause *transferPause
	ctx   context.Context
	read  int
}

func (r *pausedUpload) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	r.read += n
	// The small metadata part is already consumed; the handler is now reading
	// file contents under publication ownership.
	if r.read >= 16<<10 {
		r.pause.wait(r.ctx)
	}
	return n, err
}

type pausedDownload struct {
	http.ResponseWriter
	pause *transferPause
	ctx   context.Context
}

func (w pausedDownload) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w pausedDownload) Write(b []byte) (int, error) {
	w.pause.wait(w.ctx)
	return w.ResponseWriter.Write(b)
}

func TestHTTPPublicationAndReadsDuringTransfers(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			pause := &transferPause{reached: make(chan struct{}), release: make(chan struct{})}
			f := apiTestHandler(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if pause.armed.Load() {
						if direction == "upload" && strings.HasSuffix(r.URL.Path, "/save") {
							r.Body = &pausedUpload{ReadCloser: r.Body, pause: pause, ctx: r.Context()}
						}
						if direction == "download" && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/file") {
							w = pausedDownload{w, pause, r.Context()}
						}
					}
					next.ServeHTTP(w, r)
				})
			})
			t.Cleanup(pause.resume)
			old, incoming := bytes.Repeat([]byte("a"), 1<<20), bytes.Repeat([]byte("b"), 1<<20)
			first, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{entry("asset.bin", old)}}, nil,
				func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(old)), nil })
			if err != nil {
				t.Fatal(err)
			}
			request := model.Save{ID: operation(t), Branch: "main", Base: first.ID, Changes: []model.Entry{entry("asset.bin", incoming)}}
			saved := make(chan error, 1)
			save := func() {
				v, err := f.client.Save(ctx, "alice", "project", request, nil,
					func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(incoming)), nil })
				if err == nil && v.ID != request.ID {
					err = fmt.Errorf("accepted version %s, expected %s", v.ID, request.ID)
				}
				saved <- err
			}
			downloaded := make(chan error, 1)
			pause.armed.Store(true)
			if direction == "upload" {
				go save()
			} else {
				go func() {
					var out bytes.Buffer
					err := f.client.Download(ctx, "alice", "project", first.ID, entry("asset.bin", old), nil, 0, &out)
					if err == nil && !bytes.Equal(out.Bytes(), old) {
						err = fmt.Errorf("open reader changed after publication")
					}
					downloaded <- err
				}()
			}
			select {
			case <-pause.reached:
			case <-time.After(5 * time.Second):
				t.Fatal("transfer did not reach server streaming boundary")
			}
			if direction == "download" {
				go save()
				select {
				case err := <-saved:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("publication blocked by an open download")
				}
			}
			readCtx, stopReads := context.WithTimeout(ctx, 5*time.Second)
			defer stopReads()
			expected := first.ID
			if direction == "download" {
				expected = request.ID
			}
			if head, err := f.client.Head(readCtx, "alice", "project", "main"); err != nil || head != expected {
				t.Fatalf("head during %s: %s, %v", direction, head, err)
			}
			page, err := f.client.History(readCtx, "alice", "project", "main", 20)
			if err != nil || len(page.Versions) == 0 || page.Versions[0].ID != expected {
				t.Fatalf("history during %s: %v, %v", direction, page, err)
			}
			entries, err := f.client.Inventory(readCtx, "alice", "project", first.ID, model.Selection{{Path: "", Include: true}})
			if err != nil || len(entries) != 1 || entries[0] != entry("asset.bin", old) {
				t.Fatalf("old inventory during %s: %v, %v", direction, entries, err)
			}
			if direction == "upload" {
				var out bytes.Buffer
				if err := f.client.Download(readCtx, "alice", "project", first.ID, entries[0], nil, 0, &out); err != nil || !bytes.Equal(out.Bytes(), old) {
					t.Fatalf("published bytes during upload: %v", err)
				}
			}
			pause.resume()
			if direction == "upload" {
				if err := <-saved; err != nil {
					t.Fatal(err)
				}
			} else if err := <-downloaded; err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := f.client.Download(ctx, "alice", "project", request.ID, entry("asset.bin", incoming), nil, 0, &out); err != nil || !bytes.Equal(out.Bytes(), incoming) {
				t.Fatalf("new published bytes: %v", err)
			}
		})
	}
}
