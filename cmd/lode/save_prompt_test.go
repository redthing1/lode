package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSaveMessagePrompt(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"Update assets\n", "Update assets", true},
		{"\n", "", true},
		{"Windows line\r\n", "Windows line", true},
		{strings.Repeat("a", 4096) + "\n", strings.Repeat("a", 4096), true},
		{strings.Repeat("a", 4097) + "\n", "", false},
		{"", "", false},
	} {
		var out bytes.Buffer
		got, err := promptSaveMessage(context.Background(), strings.NewReader(tc.input), &out)
		if (err == nil) != tc.valid || tc.valid && got != tc.want || !strings.HasPrefix(out.String(), "Message (optional): ") {
			t.Fatalf("input length %d: message %q, error %v, output %q", len(tc.input), got, err, out.String())
		}
	}
}

func TestSaveMessagePromptCancellation(t *testing.T) {
	in, writer := io.Pipe()
	defer in.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := promptSaveMessage(ctx, in, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("prompt ignored cancellation: %v", err)
	}
}
