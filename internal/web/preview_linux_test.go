//go:build linux

package web

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestClassifyPreviewAllowlist(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want previewKind
	}{
		{"README.md", []byte("# hello"), previewMarkdown},
		{"src/main.go", []byte("package main"), previewText},
		{"picture.svg", []byte("<svg onload=alert(1) />"), previewText},
		{"page.html", []byte("<script>alert(1)</script>"), previewText},
		{"model.obj", []byte("v 0 0 0"), previewText},
		{"server.log", []byte("started"), previewText},
		{"config.conf", []byte("value = true"), previewText},
		{"Makefile", []byte("all:\n\tgo build"), previewText},
		{"Dockerfile", []byte("FROM scratch"), previewText},
		{"asset.png", []byte{0x89, 'P', 'N', 'G'}, previewImage},
		{"sound.ogg", []byte("OggS"), previewAudio},
		{"clip.webm", []byte{0x1a, 0x45, 0xdf, 0xa3}, previewVideo},
		{"unknown.bin", []byte("plain text"), previewDownload},
		{"src/code.go", []byte{'x', 0, 'y'}, previewDownload},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPreview(tc.name, tc.data).Kind; got != tc.want {
				t.Fatalf("classifyPreview() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMarkdownPreviewEscapesHTMLAndRewritesLocalReferences(t *testing.T) {
	got, err := renderMarkdown([]byte("<script>alert(1)</script>\n\n# Heading Name\n\n| one | two |\n| --- | --- |\n| a | b |\n\n[code](../src/main.go) ![photo](../assets/p.png) [web](https://example.test/x) ![remote](https://example.test/a.png)"), "demo", "v123", "docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"<script>", "https://example.test/a.png"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("rendered Markdown contains unsafe content %q: %s", unsafe, got)
		}
	}
	for _, want := range []string{"/p/demo/file?path=src%2Fmain.go&amp;version=v123", "/p/demo/raw?path=assets%2Fp.png&amp;version=v123", "https://example.test/x", `src=""`, `id="heading-name"`, "<table>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered Markdown lacks %q: %s", want, got)
		}
	}
}

func TestReadPreviewBytesIsBoundedAndPropagatesExpired(t *testing.T) {
	large := bytes.Repeat([]byte("a"), maxTextPreview+400)
	entry, data, err := readPreviewBytes(func(consume func(model.Entry, io.Reader) error) error {
		return consume(model.Entry{Path: "large.txt", Kind: model.File, Size: int64(len(large))}, bytes.NewReader(large))
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != int64(len(large)) || len(data) != maxTextPreview+1 {
		t.Fatalf("read size = %d; entry size = %d", len(data), entry.Size)
	}

	wantErr := errors.New("expired content")
	if _, _, err := readPreviewBytes(func(func(model.Entry, io.Reader) error) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("readPreviewBytes error = %v, want expired error", err)
	}
}

func TestParseSingleRange(t *testing.T) {
	for _, tc := range []struct {
		header        string
		start, length int64
		status        int
		bad           bool
	}{
		{"", 0, 10, 200, false},
		{"bytes=2-5", 2, 4, 206, false},
		{"bytes=8-", 8, 2, 206, false},
		{"bytes=-3", 7, 3, 206, false},
		{"bytes=0-99", 0, 10, 206, false},
		{"bytes=10-", 0, 0, 0, true},
		{"bytes=2-1", 0, 0, 0, true},
		{"bytes=0-1,4-5", 0, 0, 0, true},
	} {
		start, length, status, err := parseSingleRange(tc.header, 10)
		if (err != nil) != tc.bad || !tc.bad && (start != tc.start || length != tc.length || status != tc.status) {
			t.Errorf("parseSingleRange(%q) = (%d,%d,%d,%v)", tc.header, start, length, status, err)
		}
	}
}
