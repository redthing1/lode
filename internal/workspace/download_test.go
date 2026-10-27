package workspace

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/transfer"
)

func downloadFixtureData(t *testing.T, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func editBytes(b []byte, offset int, marker byte) []byte {
	copy := append([]byte(nil), b...)
	copy[offset] ^= marker
	return copy
}

func TestLargeSwitchAndRestoreUseTheirExactContentSource(t *testing.T) {
	ctx := context.Background()
	old := downloadFixtureData(t, 4<<20)
	s, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"asset.bin": old}, nil)
	base := w.State().Base
	if err := s.p.CreateBranch(ctx, "old", base); err != nil {
		t.Fatal(err)
	}
	target := editBytes(old, 2<<20, 0x4a)
	write(t, sourceDir, "asset.bin", target)
	head := publishWorkspace(t, sourceDir)
	if err := w.Update(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Switch(ctx, "old", nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(t, localDir, "asset.bin"), old) || w.State().Base != base {
		t.Fatal("switch did not materialize the old branch")
	}
	if err := w.Restore(ctx, head, nil, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(t, localDir, "asset.bin"), target) || w.State().Base != base || w.State().Branch != "old" {
		t.Fatal("restore did not use its content source while preserving the baseline")
	}
	changes, err := w.Status()
	if err != nil || len(changes) != 1 || changes[0].Path != "asset.bin" {
		t.Fatalf("restored contents should be local changes: %v, %v", changes, err)
	}
}

func TestUpdateUsesLocalBasisAfterBaseExpiryAndTheirsChoice(t *testing.T) {
	var deltaReads atomic.Int32
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/read" {
				deltaReads.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	baseBytes := downloadFixtureData(t, 4<<20)
	write(t, sourceDir, "asset.bin", baseBytes)
	source := s.attach(t, sourceDir)
	base := save(t, source).ID
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	localBytes := editBytes(baseBytes, 1<<20, 0x5a)
	write(t, localDir, "asset.bin", localBytes)
	target := editBytes(baseBytes, 3<<20, 0xa5)
	write(t, sourceDir, "asset.bin", target)
	current := publishWorkspace(t, sourceDir)
	expireHistory(t, s.p)
	if _, err := s.p.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Update(context.Background(), nil); err == nil {
		t.Fatal("conflicting whole-file update unexpectedly succeeded")
	}
	if got := read(t, localDir, "asset.bin"); !bytes.Equal(got, localBytes) {
		t.Fatal("conflict changed the local basis")
	}
	if err := w.Update(context.Background(), []model.Choice{{Path: "asset.bin", Ours: false}}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, localDir, "asset.bin"); !bytes.Equal(got, target) {
		t.Fatal("theirs update did not install exact remote bytes")
	}
	if w.State().Base != current {
		t.Fatalf("base = %q, want %q", w.State().Base, current)
	}
	if deltaReads.Load() == 0 {
		t.Fatal("large update did not request a delta")
	}
	changes, err := w.Status()
	if err != nil || len(changes) != 0 {
		t.Fatalf("status after theirs update = %#v, %v", changes, err)
	}
	if base == current {
		t.Fatal("test did not advance past the original base")
	}
}

func TestUpdateBadDownloadNeverInstallsAndCanResume(t *testing.T) {
	var mode atomic.Value
	mode.Store("")
	var wrongTarget []byte
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m, _ := mode.Load().(string)
			if r.Method != http.MethodPost || r.URL.Path != "/v1/read" || m == "" {
				next.ServeHTTP(w, r)
				return
			}
			signature, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(signature))
			if m == "oversized-signature" {
				r.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{0}, transfer.MaxSignatureSize+1)))
				next.ServeHTTP(w, r)
				return
			}
			recorder := httptest.NewRecorder()
			next.ServeHTTP(recorder, r)
			body := append([]byte(nil), recorder.Body.Bytes()...)
			switch m {
			case "forged":
				body = bytes.Repeat([]byte{0xff}, 128)
			case "truncated":
				body = body[:len(body)-1]
			case "trailing":
				body = append(body, 0)
			case "wrong-target":
				var delta bytes.Buffer
				if err := transfer.Delta(r.Context(), bytes.NewReader(signature), bytes.NewReader(wrongTarget), &delta); err != nil {
					t.Error(err)
					return
				}
				body = delta.Bytes()
			}
			for key, values := range recorder.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(body)
		})
	})
	ctx := context.Background()
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	current := downloadFixtureData(t, 4<<20)
	write(t, sourceDir, "asset.bin", current)
	source := s.attach(t, sourceDir)
	if _, err := source.Save(ctx, "tester", "base"); err != nil {
		t.Fatal(err)
	}
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(ctx, s.srv.URL, s.p.Token(), localDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	for _, bad := range []string{"forged", "truncated", "trailing", "wrong-target", "oversized-signature"} {
		t.Run(bad, func(t *testing.T) {
			before := append([]byte(nil), current...)
			next := editBytes(current, len(current)/2, byte(len(bad)+1))
			write(t, sourceDir, "asset.bin", next)
			currentBase := publishWorkspace(t, sourceDir)
			// publishWorkspace opens the saved source workspace and publishes its edit.
			if currentBase == w.State().Base {
				t.Fatal("source update did not advance the server")
			}
			wrongTarget = editBytes(next, len(next)/3, 0x73)
			mode.Store(bad)
			err := w.Update(ctx, nil)
			mode.Store("")
			if err == nil {
				t.Fatal("bad download unexpectedly completed")
			}
			if got := read(t, localDir, "asset.bin"); !bytes.Equal(got, before) {
				t.Fatal("failed download changed the installed file")
			}
			if w.State().Base == currentBase {
				t.Fatal("failed download advanced the baseline")
			}
			if err := w.Resume(ctx); err != nil {
				t.Fatalf("resume after rejected download: %v", err)
			}
			if got := read(t, localDir, "asset.bin"); !bytes.Equal(got, next) {
				t.Fatal("resume did not install exact target bytes")
			}
			current = next
		})
	}
}

func TestDeltaUpdateInterruptionCanResumeOrCancel(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancel"}[cancel], func(t *testing.T) {
			var failB atomic.Bool
			var deltaReads atomic.Int32
			s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Path == "/v1/read" {
						deltaReads.Add(1)
						if r.URL.Query().Get("path") == "b.bin" && failB.Swap(false) {
							http.Error(w, "interrupted", http.StatusBadGateway)
							return
						}
					}
					next.ServeHTTP(w, r)
				})
			})
			sourceDir := filepath.Join(t.TempDir(), "source")
			if err := os.Mkdir(sourceDir, 0700); err != nil {
				t.Fatal(err)
			}
			oldA, oldB := downloadFixtureData(t, 4<<20), downloadFixtureData(t, 4<<20)
			write(t, sourceDir, "a.bin", oldA)
			write(t, sourceDir, "b.bin", oldB)
			source := s.attach(t, sourceDir)
			base := save(t, source).ID
			closeWorkspace(t, source)
			localDir := filepath.Join(t.TempDir(), "local")
			w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			newA, newB := editBytes(oldA, 1<<20, 0x31), editBytes(oldB, 2<<20, 0x72)
			write(t, sourceDir, "a.bin", newA)
			write(t, sourceDir, "b.bin", newB)
			newBase := publishWorkspace(t, sourceDir)
			failB.Store(true)
			if err := w.Update(context.Background(), nil); err == nil {
				closeWorkspace(t, w)
				t.Fatal("interrupted second delta unexpectedly completed")
			}
			if got := read(t, localDir, "a.bin"); !bytes.Equal(got, newA) {
				closeWorkspace(t, w)
				t.Fatal("first delta was not installed before interruption")
			}
			if got := read(t, localDir, "b.bin"); !bytes.Equal(got, oldB) {
				closeWorkspace(t, w)
				t.Fatal("failed second delta changed the original")
			}
			closeWorkspace(t, w)
			w, err = Open(localDir)
			if err != nil {
				t.Fatal(err)
			}
			defer closeWorkspace(t, w)
			if cancel {
				if err := w.Cancel(context.Background()); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(read(t, localDir, "a.bin"), oldA) || !bytes.Equal(read(t, localDir, "b.bin"), oldB) || w.State().Base != base {
					t.Fatal("cancel did not restore exact prior view")
				}
			} else {
				if err := w.Resume(context.Background()); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(read(t, localDir, "a.bin"), newA) || !bytes.Equal(read(t, localDir, "b.bin"), newB) || w.State().Base != newBase {
					t.Fatal("resume did not install exact target view")
				}
			}
			if deltaReads.Load() < 2 {
				t.Fatalf("only %d delta read requests", deltaReads.Load())
			}
		})
	}
}
