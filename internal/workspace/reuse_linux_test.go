package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/progress"
)

func TestSaveCaseRenameReusesContentsAndRecovers(t *testing.T) {
	for _, mode := range []string{"normal", "unaccepted", "accepted"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			w := attach(t, f, t.TempDir())
			data := strings.Repeat("unchanged ISO contents", 100000)
			writeFile(t, w.Directory(), "FILE.iso", data)
			first := save(t, f, w, "original")
			if err := os.Rename(filepath.Join(w.Directory(), "FILE.iso"), filepath.Join(w.Directory(), "FILE.ISO")); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var transfers []progress.Event
			ctx := progress.WithReporter(context.Background(), func(e progress.Event) {
				if e.Phase == "Uploading" || e.Phase == "Encoding delta" || e.Phase == "Preparing transfer" {
					mu.Lock()
					defer mu.Unlock()
					transfers = append(transfers, e)
				}
			})
			f.refuseSave.Store(mode == "unaccepted")
			f.dropSave.Store(mode == "accepted")
			version, err := w.Save(ctx, f.token, "rename")
			if mode == "normal" {
				if err != nil {
					t.Fatal(err)
				}
				if version.Parent != first.ID {
					t.Fatal("rename lost its parent")
				}
			} else {
				if err == nil {
					t.Fatal("save response was not interrupted")
				}
				var pending pendingSave
				if err := w.readJSON("save.json", &pending); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(pending.Request.Reuse, map[string]string{"FILE.ISO": "FILE.iso"}) {
					t.Fatalf("pending save lost content reuse: %+v", pending)
				}
				// Resume uses the captured identity, preserving newer local edits.
				writeFile(t, w.Directory(), "FILE.ISO", "later edit")
				dir := w.Directory()
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				w, err = Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				if err := w.Resume(ctx, f.token); err != nil {
					t.Fatal(err)
				}
				if got := readFile(t, dir, "FILE.ISO"); got != "later edit" {
					t.Fatal("recovery replaced later work")
				}
				changes, err := w.Status(ctx)
				if err != nil || len(changes) != 1 || changes[0].Path != "FILE.ISO" || changes[0].Kind != model.File {
					t.Fatalf("later edit was not left unsaved: %v, %v", changes, err)
				}
			}
			if got := remoteFiles(t, f, w.State().Version); !reflect.DeepEqual(got, map[string]string{"FILE.ISO": data}) {
				t.Fatal("rename did not preserve remote contents and remove the old path")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(transfers) != 0 {
				t.Fatalf("unchanged rename transferred contents: %+v", transfers)
			}
		})
	}
}
