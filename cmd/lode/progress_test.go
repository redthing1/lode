package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/redthing1/lode/internal/progress"
)

func TestProgressKeepsOutputReadable(t *testing.T) {
	var stderr, stdout bytes.Buffer
	d := &progressDisplay{out: &stderr, started: time.Now(), width: func() int { return 80 }}
	d.update(progress.Event{Phase: "Uploading", Path: strings.Repeat("資料", 30) + "\n\x1b[2J", Bytes: 1 << 20, Total: 2 << 20, Files: 12, TotalFiles: 140})
	d.draw()
	line := strings.TrimPrefix(stderr.String(), "\r\x1b[2K")
	if !utf8.ValidString(line) || len(line) > 80 || strings.ContainsAny(line, "\n\x1b") || !strings.Contains(line, "Uploading [12/140]") || !strings.Contains(line, "1/2 MiB") {
		t.Fatalf("unsafe or unreadable line: %q", line)
	}
	if _, err := (progressOutput{&stdout, d}).Write([]byte("Saved version\n")); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Saved version\n" || !strings.HasSuffix(stderr.String(), "\r\x1b[2K") {
		t.Fatal("progress contaminated final output")
	}
	quiet := stderr.Len()
	d.draw()
	if stderr.Len() != quiet {
		t.Fatal("progress reappeared after final output")
	}
	d.update(progress.Event{Phase: "Waiting for server"})
	d.draw()
	if !strings.Contains(stderr.String()[quiet:], "Waiting for server") {
		t.Fatal("server wait was silent")
	}
	d.update(progress.Event{})
	quiet = stderr.Len()
	d.draw()
	if stderr.Len() != quiet || d.visible {
		t.Fatal("progress interfered with user input")
	}
}

func TestProgressFileCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event progress.Event
		width int
		want  string
	}{
		{"transfer", progress.Event{Phase: "Uploading", Path: "assets/scene.blend", Files: 12, TotalFiles: 140, Bytes: 8 << 20, Total: 32 << 20}, 100, "Uploading [12/140] assets/scene.blend · 8/32 MiB"},
		{"scan", progress.Event{Phase: "Checking", Path: "asset", Files: 12}, 80, "Checking [12 files] asset"},
		{"narrow", progress.Event{Phase: "Downloading", Path: strings.Repeat("long/", 30), Files: 12, TotalFiles: 140, Bytes: 8 << 20, Total: 32 << 20}, 36, "Downloading [12/140]"},
		{"empty file", progress.Event{Phase: "Uploading", Path: "empty", Files: 1, TotalFiles: 1}, 80, "Uploading [1/1] empty"},
		{"waiting", progress.Event{Phase: "Waiting for server"}, 80, "Waiting for server ·"},
		{"different units", progress.Event{Phase: "Downloading", Bytes: 512 << 10, Total: 32 << 20}, 80, "512 KiB/32 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			d := &progressDisplay{out: &out, started: time.Now(), width: func() int { return tc.width }}
			d.update(tc.event)
			d.draw()
			line := strings.TrimPrefix(out.String(), "\r\x1b[2K")
			if !strings.Contains(line, tc.want) || len(line) > tc.width {
				t.Fatalf("got %q, want %q within %d bytes", line, tc.want, tc.width)
			}
		})
	}
}
