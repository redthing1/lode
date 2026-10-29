package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/progress"
)

func TestFileProgressAcrossScanUploadBatchesAndDeltas(t *testing.T) {
	f := newFixture(t)
	w := attach(t, f, t.TempDir())
	const count = 135
	positions := map[string]int{}
	for i := range count {
		name := fmt.Sprintf("assets/%03d", i)
		data := name
		if i == 0 {
			data = ""
		} else if i == count-1 {
			data = strings.Repeat("x", 2<<20)
		}
		writeFile(t, w.Directory(), name, data)
		positions[name] = i + 1
	}
	writeFile(t, w.Directory(), "omitted", "not selected for clone")
	writeFile(t, w.Directory(), "assets/.DS_Store", "ignored")
	positions["omitted"] = count + 1
	var mu sync.Mutex
	var events []progress.Event
	ctx := progress.WithReporter(context.Background(), func(e progress.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	take := func() []progress.Event {
		mu.Lock()
		defer mu.Unlock()
		out := events
		events = nil
		return out
	}
	check := func(events []progress.Event, phase string, total, wantSeen int) {
		t.Helper()
		seen := map[string]bool{}
		for _, e := range events {
			if e.Phase != phase || e.Path == "" {
				continue
			}
			if e.Path == "assets" && e.Files == 0 && e.TotalFiles == 0 {
				continue // The directory itself is not a file.
			}
			position := positions[e.Path]
			if total == 1 {
				position = 1
			}
			if position == 0 || e.Files != position || e.TotalFiles != total {
				t.Fatalf("unexpected file progress: %+v; want %d/%d", e, position, total)
			}
			seen[e.Path] = true
		}
		if len(seen) != wantSeen {
			t.Fatalf("%s reported %d files, want %d", phase, len(seen), wantSeen)
		}
	}
	if _, err := w.Save(ctx, f.token, "files"); err != nil {
		t.Fatal(err)
	}
	upload := take()
	check(upload, "Checking", 0, count+1)
	check(upload, "Uploading", count+1, count+1)
	clone, err := Clone(ctx, f.url, f.client, filepath.Join(t.TempDir(), "clone"), ConnectOptions{Selection: model.Selection{{Path: "assets", Include: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	download := take()
	check(download, "Downloading", count, count)
	check(download, "Updating", count, count)
	// Staged-file verification keeps the public path and the same file position.
	check(download, "Checking", count, count-1)
	if got := readFile(t, clone.Directory(), "assets/000"); got != "" {
		t.Fatalf("empty file changed: %q", got)
	}
	writeFile(t, w.Directory(), "assets/134", strings.Repeat("y", 2<<20))
	if _, err := w.Save(ctx, f.token, "delta"); err != nil {
		t.Fatal(err)
	}
	check(take(), "Encoding delta", 1, 1)
	if err := clone.Update(ctx, f.token, nil); err != nil {
		t.Fatal(err)
	}
	delta := take()
	check(delta, "Preparing download", 1, 1)
	check(delta, "Reconstructing", 1, 1)
	if got := readFile(t, clone.Directory(), "assets/134"); got != strings.Repeat("y", 2<<20) {
		t.Fatal("delta file changed")
	}
}
