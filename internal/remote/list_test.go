package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestLargeHistoryCleanupPreviewAndHistoryList(t *testing.T) {
	ctx := context.Background()
	p, err := project.Create(ctx, filepath.Join(t.TempDir(), "project"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	c, err := New(srv.URL, p.Token(), p.ID())
	if err != nil {
		t.Fatal(err)
	}

	const messageSize = 6 << 20
	message := strings.Repeat("x", messageSize)
	data := []byte("a")
	var commits []project.Commit
	base := ""
	for i := 0; i < 3; i++ {
		data = []byte{byte('a' + i)}
		entry := listTestEntry("file.bin", data)
		req := project.Request{
			ID: project.NewID(), Branch: "main", Base: base,
			Author: "test", Message: message, Changes: []model.Entry{entry},
		}
		commit, err := c.Save(ctx, req, func(string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		commits = append(commits, commit)
		base = commit.ID
	}
	if err := c.CreateTag(ctx, "first-version", commits[0].ID); err != nil {
		t.Fatal(err)
	}

	history, err := c.History(ctx, "main", 10)
	if err != nil {
		t.Fatalf("history larger than 16 MiB in total: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history count = %d, want 3", len(history))
	}
	var historyBytes int
	for _, commit := range history {
		if len(commit.Message) != messageSize || commit.Message != message {
			t.Fatalf("history message length = %d, want exact %d-byte message", len(commit.Message), messageSize)
		}
		historyBytes += len(commit.Message)
	}
	if historyBytes <= maxMetadata {
		t.Fatalf("history message bytes = %d, want more than %d", historyBytes, maxMetadata)
	}

	var got []project.RetentionVersion
	token, err := c.PreviewCleanup(ctx, 0, func(v project.RetentionVersion) error {
		got = append(got, v)
		return nil
	})
	if err != nil {
		t.Fatalf("cleanup preview: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("cleanup token length = %d, want 64", len(token))
	}
	byID := make(map[string]project.RetentionVersion, len(got))
	for _, version := range got {
		byID[version.ID] = version
	}
	if len(byID) != 3 {
		t.Fatalf("cleanup IDs = %d, want 3", len(byID))
	}
	if v := byID[commits[0].ID]; v.Tags != 1 || !v.Protected() {
		t.Fatalf("tagged version summary = %#v; want one tag protection", v)
	}
	// The oldest tag and newest branch tip are protected; the middle payload is eligible.
	if v := byID[commits[2].ID]; v.Branches != 1 || !v.Protected() {
		t.Fatalf("tip version protection = %#v, want branch tip", v)
	}
	if v := byID[commits[1].ID]; v.Protected() || v.Tags != 0 || v.Branches != 0 || v.Recent != 0 {
		t.Fatalf("middle version protection = %#v, want unprotected", v)
	}
	for _, commit := range commits {
		if _, ok := byID[commit.ID]; !ok {
			t.Errorf("preview missing exact commit ID %s", commit.ID)
		}
	}
	expired, err := c.ApplyCleanup(ctx, 0, token)
	if err != nil {
		t.Fatalf("apply cleanup preview token: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired count = %d, want only the middle version", expired)
	}
}

func TestLargeListStreamsMoreThanMetadataLimitWithoutCollecting(t *testing.T) {
	const count = 150_000
	token := strings.Repeat("a", 64)
	sample, err := json.Marshal(project.RetentionVersion{ID: fmt.Sprintf("%032x", 0), Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Retained: true, Branches: 1})
	if err != nil {
		t.Fatal(err)
	}
	if count*(len(sample)+1) <= maxMetadata {
		t.Fatalf("test list estimate %d bytes does not exceed %d", count*(len(sample)+1), maxMetadata)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(projectHeader, testProjectID)
		w.Header().Set(cleanupTokenHeader, token)
		w.Header().Set("Content-Type", listContentType)
		w.Header().Set(itemCountHeader, fmt.Sprint(count))
		encoder := json.NewEncoder(w)
		created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		for i := 0; i < count; i++ {
			version := project.RetentionVersion{ID: fmt.Sprintf("%032x", i), Created: created, Retained: true, Branches: 1}
			if err := encoder.Encode(version); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	c := fakeListClient(t, srv, testProjectID)
	visited := 0
	gotToken, err := c.PreviewCleanup(context.Background(), 0, func(v project.RetentionVersion) error {
		if v.ID != fmt.Sprintf("%032x", visited) {
			t.Fatalf("record %d ID = %q", visited, v.ID)
		}
		visited++ // Retain only a count; the visitor is deliberately streaming.
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if visited != count || gotToken != token {
		t.Fatalf("visited=%d token=%q, want count=%d and validated token", visited, gotToken, count)
	}
}

func TestPreviewCleanupRejectsIncompleteOrInvalidListsWithoutToken(t *testing.T) {
	valid := project.RetentionVersion{ID: strings.Repeat("1", 32), Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Retained: true, Branches: 1}
	line, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	goodToken := strings.Repeat("a", 64)

	tests := []struct {
		name     string
		count    string
		token    string
		body     string
		content  string
		visitErr error
	}{
		{name: "truncated", count: "2", token: goodToken, body: string(line) + "\n"},
		{name: "extra record", count: "1", token: goodToken, body: string(line) + "\n" + string(line) + "\n"},
		{name: "malformed record", count: "1", token: goodToken, body: "{bad json}\n"},
		{name: "bad count", count: "not-a-number", token: goodToken},
		{name: "missing token", count: "0"},
		{name: "invalid token", count: "0", token: "not-a-hash"},
		{name: "callback error", count: "1", token: goodToken, body: string(line) + "\n", visitErr: errors.New("visitor failed")},
		{name: "wrong content type", count: "0", token: goodToken, content: "application/json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contentType := tc.content
			if contentType == "" {
				contentType = listContentType
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(projectHeader, testProjectID)
				w.Header().Set(itemCountHeader, tc.count)
				w.Header().Set(cleanupTokenHeader, tc.token)
				w.Header().Set("Content-Type", contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := fakeListClient(t, srv, testProjectID)
			gotToken, err := c.PreviewCleanup(context.Background(), 0, func(project.RetentionVersion) error {
				return tc.visitErr
			})
			if err == nil {
				t.Fatal("expected preview error")
			}
			if gotToken != "" {
				t.Fatalf("failed preview returned token %q", gotToken)
			}
		})
	}
}

func TestPreviewCleanupRejectsOversizedRecordWithoutToken(t *testing.T) {
	goodToken := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(projectHeader, testProjectID)
		w.Header().Set(cleanupTokenHeader, goodToken)
		w.Header().Set(itemCountHeader, "1")
		w.Header().Set("Content-Type", listContentType)
		_, _ = io.WriteString(w, strings.Repeat(" ", maxMetadata+1)+"\n")
	}))
	defer srv.Close()
	c := fakeListClient(t, srv, testProjectID)
	token, err := c.PreviewCleanup(context.Background(), 0, nil)
	if err == nil || token != "" {
		t.Fatalf("oversized record token=%q error=%v, want no token and an error", token, err)
	}
}

func TestEmptyPreviewListIsValid(t *testing.T) {
	goodToken := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(projectHeader, testProjectID)
		w.Header().Set(cleanupTokenHeader, goodToken)
		w.Header().Set(itemCountHeader, "0")
		w.Header().Set("Content-Type", listContentType)
	}))
	defer srv.Close()
	c := fakeListClient(t, srv, testProjectID)
	visited := 0
	token, err := c.PreviewCleanup(context.Background(), 0, func(project.RetentionVersion) error {
		visited++
		return nil
	})
	if err != nil || token != goodToken || visited != 0 {
		t.Fatalf("empty preview token=%q visited=%d error=%v", token, visited, err)
	}
}

func fakeListClient(t *testing.T, srv *httptest.Server, id string) *Client {
	t.Helper()
	c, err := New(srv.URL, "test-token", id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func listTestEntry(name string, data []byte) model.Entry {
	sum := sha256.Sum256(data)
	return model.Entry{Path: name, Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

const testProjectID = "0123456789abcdef0123456789abcdef"
