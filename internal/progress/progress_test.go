package progress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type partialWriter struct{}

func (partialWriter) Write(b []byte) (int, error) { return 2, io.ErrShortWrite }

func TestProgressPreservesStreamAndErrors(t *testing.T) {
	var events []Event
	ctx := WithReporter(context.Background(), func(e Event) { events = append(events, e) })
	var out bytes.Buffer
	in := Reader(ctx, "Reading", "asset", 5, strings.NewReader("bytes"))
	if _, err := io.Copy(Writer(ctx, "Writing", "asset", 5, &out), in); err != nil || out.String() != "bytes" {
		t.Fatalf("stream: %q %v", out.String(), err)
	}
	last := events[len(events)-1]
	if last.Bytes != 5 || last.Total != 5 || last.Path != "asset" {
		t.Fatalf("progress: %+v", last)
	}
	n, err := Writer(ctx, "Writing", "asset", 5, partialWriter{}).Write([]byte("bytes"))
	if n != 2 || !errors.Is(err, io.ErrShortWrite) || events[len(events)-1].Bytes != 2 {
		t.Fatal("partial write changed")
	}
	plain := strings.NewReader("unchanged")
	if Reader(context.Background(), "", "", 0, plain) != plain {
		t.Fatal("unobserved stream was wrapped")
	}
}

func TestFilePositionsFollowStreamsAndClearBetweenPhases(t *testing.T) {
	var events []Event
	ctx := WithReporter(context.Background(), func(e Event) { events = append(events, e) })
	ctx = WithFiles(ctx, []string{"first", "second", "empty"})
	var out bytes.Buffer
	if _, err := io.Copy(Writer(ctx, "Downloading", "second", 5, &out), Reader(ctx, "Preparing", "second", 5, strings.NewReader("bytes"))); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Files != 2 || event.TotalFiles != 3 {
			t.Fatalf("lost file position during IO: %+v", event)
		}
	}
	Writer(ctx, "Downloading", "empty", 0, io.Discard)
	if event := events[len(events)-1]; event.Files != 3 || event.TotalFiles != 3 {
		t.Fatalf("empty file was not counted: %+v", event)
	}
	Report(ctx, Event{Phase: "Waiting for server"})
	if event := events[len(events)-1]; event.Files != 0 || event.TotalFiles != 0 {
		t.Fatalf("file counts leaked into waiting: %+v", event)
	}
}
