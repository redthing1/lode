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
	d.update(progress.Event{Phase: "Uploading", Path: strings.Repeat("資料", 30) + "\n\x1b[2J", Bytes: 1 << 20, Total: 2 << 20})
	d.draw()
	line := strings.TrimPrefix(stderr.String(), "\r\x1b[2K")
	if !utf8.ValidString(line) || len(line) > 80 || strings.ContainsAny(line, "\n\x1b") || !strings.Contains(line, "1.0 MiB / 2.0 MiB") {
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
}
