//go:build linux

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestTransitionAcquiresAndReleasesReadHold(t *testing.T) {
	var holds, releases atomic.Int32
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/read/hold":
				holds.Add(1)
			case "/v1/read/release":
				releases.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "read-hold-base", "", []project.Change{makeFileChange("file.bin", "base")}, map[string]string{"file.bin": "base"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	// Initial checkout has its own bounded materialization hold.
	holds.Store(0)
	releases.Store(0)

	updated := seed(t, remote, "read-hold-target", base.ID, []project.Change{makeFileChange("file.bin", "target")}, map[string]string{"file.bin": "target"})
	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if holds.Load() != 1 || releases.Load() != 1 {
		t.Fatalf("update read holds: hold=%d release=%d", holds.Load(), releases.Load())
	}
	if w.Base() != updated.ID || w.Pending() != "" {
		t.Fatalf("base=%q pending=%q", w.Base(), w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "target")
}

func TestReadHoldRenewsWhileMaterializationRuns(t *testing.T) {
	var holds, releases atomic.Int32
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/read/hold" {
				holds.Add(1)
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				if recorder.Code >= 200 && recorder.Code < 300 {
					var lease project.ReadLease
					if err := json.Unmarshal(recorder.Body.Bytes(), &lease); err != nil {
						t.Errorf("decode read hold: %v", err)
					}
					lease.Lifetime = 30 * time.Millisecond
					encoded, err := json.Marshal(lease)
					if err != nil {
						t.Errorf("encode read hold: %v", err)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(encoded)
					return
				}
				for name, values := range recorder.Header() {
					for _, value := range values {
						w.Header().Add(name, value)
					}
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			if r.URL.Path == "/v1/read/release" {
				releases.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "read-renew-base", "", []project.Change{makeFileChange("file.bin", "contents")}, map[string]string{"file.bin": "contents"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	holds.Store(0)
	releases.Store(0)

	if err := w.withReadHold(context.Background(), randomID(), base.ID, func(context.Context) error {
		time.Sleep(85 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if holds.Load() < 3 {
		t.Fatalf("read hold renewed only %d times", holds.Load())
	}
	if releases.Load() != 1 {
		t.Fatalf("read hold releases=%d, want one", releases.Load())
	}
}

func TestPendingTransitionKeepsReadHoldUntilResumeCompletes(t *testing.T) {
	var holds, releases atomic.Int32
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/read/hold":
				holds.Add(1)
			case "/v1/read/release":
				releases.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "read-hold-recovery-base", "", []project.Change{makeFileChange("file.bin", "base")}, map[string]string{"file.bin": "base"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	holds.Store(0)
	releases.Store(0)
	updated := seed(t, remote, "read-hold-recovery-target", base.ID, []project.Change{makeFileChange("file.bin", "target")}, map[string]string{"file.bin": "target"})
	w.linkEntry = func(from, to string) error {
		if err := linkNoReplace(w.root, from, to); err != nil {
			return err
		}
		return errors.New("interrupted after install")
	}
	if err := w.Update(context.Background()); err == nil {
		t.Fatal("Update unexpectedly completed")
	}
	if w.Pending() != "transition" || holds.Load() != 1 || releases.Load() != 0 {
		t.Fatalf("pending=%q holds=%d releases=%d", w.Pending(), holds.Load(), releases.Load())
	}
	w.linkEntry = nil
	if _, err := w.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Pending() != "" || w.Base() != updated.ID || holds.Load() != 1 || releases.Load() != 1 {
		t.Fatalf("resume base=%q pending=%q holds=%d releases=%d", w.Base(), w.Pending(), holds.Load(), releases.Load())
	}
}

func TestMetadataOnlyTransitionDoesNotAcquireReadHold(t *testing.T) {
	var holds atomic.Int32
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/read/hold" {
				holds.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	seed(t, remote, "read-hold-noop-base", "", []project.Change{{Path: "empty", Kind: model.Directory}}, nil)
	w := createTestWorkspace(t, remote, filepath.Join(t.TempDir(), "workspace"), "main", nil)
	holds.Store(0)
	if err := w.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if holds.Load() != 0 {
		t.Fatalf("metadata-only update acquired %d read holds", holds.Load())
	}
}

func TestExpiredReadHoldFailsBeforeTransitionJournal(t *testing.T) {
	var expireHold atomic.Bool
	remote := newTestRemote(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/read/hold" && expireHold.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusGone)
				_, _ = w.Write([]byte(`{"code":"payload_expired","message":"version contents have expired"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	base := seed(t, remote, "read-expired-base", "", []project.Change{makeFileChange("file.bin", "base")}, map[string]string{"file.bin": "base"})
	directory := filepath.Join(t.TempDir(), "workspace")
	w := createTestWorkspace(t, remote, directory, "main", nil)
	updated := seed(t, remote, "read-expired-target", base.ID, []project.Change{makeFileChange("file.bin", "target")}, map[string]string{"file.bin": "target"})
	expireHold.Store(true)
	if err := w.Update(context.Background()); !errors.Is(err, project.ErrPayloadExpired) {
		t.Fatalf("Update()=%v, want ErrPayloadExpired", err)
	}
	if w.Base() != base.ID || w.Pending() != "" {
		t.Fatalf("expired source changed state: base=%q target=%q pending=%q", w.Base(), updated.ID, w.Pending())
	}
	assertFileContents(t, filepath.Join(directory, "file.bin"), "base")
}
