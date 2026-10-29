package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

type deltaTransport struct {
	http.RoundTripper
	posts, gets    int
	truncate       bool
	afterSignature func()
}

func (d *deltaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	file := strings.HasSuffix(r.URL.Path, "/file")
	if file {
		if r.Method == "POST" {
			d.posts++
			if d.afterSignature != nil {
				d.afterSignature()
			}
		} else {
			d.gets++
		}
	}
	response, err := d.RoundTripper.RoundTrip(r)
	if err == nil && file && d.truncate {
		response.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(response.Body, 128), response.Body}
		d.truncate = false
	}
	return response, err
}

func largeDownloadFixture(t *testing.T) (*fixture, *Workspace, []byte, []byte) {
	t.Helper()
	f := newFixture(t)
	dir := t.TempDir()
	old := make([]byte, 2<<20)
	rand.New(rand.NewSource(29)).Read(old)
	writeFile(t, dir, "asset.bin", string(old))
	writeFile(t, dir, "omitted", "unselected")
	seed := attach(t, f, dir)
	save(t, f, seed, "initial")
	local := cloneWorkspace(t, f, model.Selection{{Path: "asset.bin", Include: true}})
	target := append([]byte(nil), old...)
	for i := 1 << 20; i < (1<<20)+(64<<10); i++ {
		target[i] ^= 0x47
	}
	writeFile(t, dir, "asset.bin", string(target))
	save(t, f, seed, "small edit")
	return f, local, old, target
}

func TestLargeUpdateUsesLocalBasisAfterExpiryAndConflictChoice(t *testing.T) {
	ctx := context.Background()
	f, w, old, target := largeDownloadFixture(t)
	err := f.store.WithProject(ctx, instance.Principal{User: f.user}, "alice", "project", instance.Maintainer, func(_ instance.Project, p *project.Store) error {
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
	old[0] ^= 0x3f
	writeFile(t, w.Directory(), "asset.bin", string(old))
	counts := &deltaTransport{RoundTripper: f.client.HTTP.Transport}
	f.client.HTTP.Transport = counts
	var conflict *ConflictsError
	if err := w.update(ctx, f.client, nil); !errors.As(err, &conflict) || counts.posts != 0 {
		t.Fatalf("expected conflict before downloading: %v posts=%d", err, counts.posts)
	}
	if err := w.update(ctx, f.client, map[string]model.Side{"asset.bin": model.Incoming}); err != nil {
		t.Fatal(err)
	}
	if counts.posts != 1 || counts.gets != 0 || readFile(t, w.Directory(), "asset.bin") != string(target) {
		t.Fatalf("update: POST=%d GET=%d", counts.posts, counts.gets)
	}
	if _, err := os.Stat(filepath.Join(w.Directory(), "omitted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("downloaded omitted contents")
	}
	if changes, err := w.Status(ctx); err != nil || len(changes) != 0 {
		t.Fatalf("updated status: %v %v", changes, err)
	}
}

func TestInterruptedDeltaDownloadPreservesLocalFiles(t *testing.T) {
	ctx := context.Background()
	f, original, old, target := largeDownloadFixture(t)
	transport := f.client.HTTP.Transport
	for _, mode := range []string{"resume", "cancel", "later-edit", "basis-changed-during-download"} {
		t.Run(mode, func(t *testing.T) {
			w := copyWorkspace(t, original.Directory())
			counts := &deltaTransport{RoundTripper: transport, truncate: true}
			f.client.HTTP.Transport = counts
			want := string(old)
			if mode == "basis-changed-during-download" {
				changed := append([]byte(nil), old...)
				changed[0] ^= 0x4f
				want = string(changed)
				counts.truncate = false
				counts.afterSignature = func() { writeFile(t, w.Directory(), "asset.bin", want) }
			}
			if err := w.update(ctx, f.client, nil); err == nil || counts.posts != 1 {
				t.Fatalf("invalid download accepted: %v POST=%d", err, counts.posts)
			}
			if readFile(t, w.Directory(), "asset.bin") != want || w.State().Version != original.State().Version {
				t.Fatal("failed download changed local contents or baseline")
			}
			if exists, err := w.pending(stageName(0, "new")); err != nil || exists {
				t.Fatalf("unverified replacement was staged: %v %v", exists, err)
			}
			directory := w.Directory()
			w.Close()
			w, err := Open(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if mode == "resume" {
				if err := w.Resume(ctx, f.token); err != nil {
					t.Fatal(err)
				}
				want = string(target)
			} else {
				if mode == "later-edit" {
					want = "later local work"
					writeFile(t, directory, "asset.bin", want)
					if err := w.Resume(ctx, f.token); err == nil {
						t.Fatal("resume overwrote later local work")
					}
				}
				if err := w.Cancel(ctx, ""); err != nil {
					t.Fatal(err)
				}
			}
			if readFile(t, directory, "asset.bin") != want {
				t.Fatal("recovery lost expected contents")
			}
			if exists, err := w.pending("transition.json"); err != nil || exists {
				t.Fatalf("recovery remains pending: %v %v", exists, err)
			}
		})
	}
}

func TestDeltaReplacementRecoversAfterProcessExit(t *testing.T) {
	f, original, old, target := largeDownloadFixture(t)
	for _, mode := range []string{"resume", "cancel", "missing-replacement"} {
		t.Run(mode, func(t *testing.T) {
			w := copyWorkspace(t, original.Directory())
			planUpdate(t, f, w)
			t.Setenv("LODE_TEST_DETACHED", "1")
			w = interruptAction(t, f, w, 0)
			if readFile(t, w.Directory(), stagePath(0, "old")) != string(old) || readFile(t, w.Directory(), stagePath(0, "new")) != string(target) {
				t.Fatal("interrupted action did not retain original and verified replacement")
			}
			want := target
			if mode == "cancel" {
				if err := w.Cancel(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				want = old
			} else {
				counts := &deltaTransport{RoundTripper: f.client.HTTP.Transport}
				if mode == "missing-replacement" {
					if err := w.meta.Remove(stageName(0, "new")); err != nil {
						t.Fatal(err)
					}
					f.client.HTTP.Transport = counts
				}
				if err := w.resumeTransition(context.Background(), f.client, false); err != nil {
					t.Fatal(err)
				}
				if mode == "missing-replacement" && (counts.posts != 0 || counts.gets != 1) {
					t.Fatalf("missing local basis: POST=%d GET=%d", counts.posts, counts.gets)
				}
			}
			if !bytes.Equal([]byte(readFile(t, w.Directory(), "asset.bin")), want) {
				t.Fatal("process-exit recovery changed contents")
			}
		})
	}
}

func TestLargeSwitchAndRestoreUseExactContentSource(t *testing.T) {
	ctx := context.Background()
	f, w, old, target := largeDownloadFixture(t)
	base := w.State().Version
	if _, err := f.client.CreateBranch(ctx, "alice", "project", model.Reference{Name: "original", Version: base}); err != nil {
		t.Fatal(err)
	}
	if err := w.Update(ctx, f.token, nil); err != nil {
		t.Fatal(err)
	}
	latest := w.State().Version
	if err := w.Switch(ctx, f.token, "original"); err != nil {
		t.Fatal(err)
	}
	if readFile(t, w.Directory(), "asset.bin") != string(old) || w.State().Version != base {
		t.Fatal("switch used wrong version")
	}
	if err := w.Restore(ctx, f.token, latest, []string{"asset.bin"}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, w.Directory(), "asset.bin") != string(target) || w.State().Version != base || w.State().Branch != "original" {
		t.Fatal("restore changed baseline or used wrong download source")
	}
}
