package kopia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/repo/blob"
)

type localTestBytes []byte

func (b localTestBytes) Length() int { return len(b) }

func (b localTestBytes) Reader() io.ReadSeekCloser {
	return &localTestReadSeeker{Reader: *bytes.NewReader(b)}
}

func (b localTestBytes) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(b)
	return int64(n), err
}

type localTestReadSeeker struct{ bytes.Reader }

func (localTestReadSeeker) Close() error { return nil }

type localTestOutput struct{ bytes.Buffer }

func (b *localTestOutput) Reset()      { b.Buffer.Reset() }
func (b *localTestOutput) Length() int { return b.Buffer.Len() }

func TestLocalStorageRoundTripAndDurableLifecycle(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repository")
	st, err := NewLocalStorage(ctx, root, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.PutBlob(ctx, "manifest-001", localTestBytes("first contents"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(ctx); err != nil {
		t.Fatal(err)
	}

	connectionInfo, err := json.Marshal(blob.ConnectionInfo{Type: localFilesystemStorageType, Config: st.ConnectionInfo().Config})
	if err != nil {
		t.Fatal(err)
	}
	var reopenedInfo blob.ConnectionInfo
	if err := json.Unmarshal(connectionInfo, &reopenedInfo); err != nil {
		t.Fatal(err)
	}
	reopened, err := blob.NewStorage(ctx, reopenedInfo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)

	var output localTestOutput
	if err := reopened.GetBlob(ctx, "manifest-001", 0, -1, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "first contents" {
		t.Fatalf("GetBlob() = %q, want first contents", got)
	}

	if err := reopened.DeleteBlob(ctx, "manifest-001"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.GetBlob(ctx, "manifest-001", 0, -1, &output); !errors.Is(err, blob.ErrBlobNotFound) {
		t.Fatalf("GetBlob after deletion = %v, want blob not found", err)
	}
}

func TestLocalStoragePutSyncFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	st, err := NewLocalStorage(ctx, filepath.Join(t.TempDir(), "repository"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)

	local := st.(*localStorage)
	actualSyncDir := local.syncDir
	syncAttempts := 0
	sentinel := errors.New("injected directory sync failure")
	local.syncDir = func(path string) error {
		syncAttempts++
		if syncAttempts == 1 {
			return sentinel
		}
		return actualSyncDir(path)
	}

	if err := st.PutBlob(ctx, "manifest-002", localTestBytes("durable contents"), blob.PutOptions{}); !errors.Is(err, sentinel) {
		t.Fatalf("PutBlob() error = %v, want injected sync failure", err)
	}
	if syncAttempts != 1 {
		t.Fatalf("directory sync attempts = %d, want 1", syncAttempts)
	}

	if err := st.PutBlob(ctx, "manifest-002", localTestBytes("durable contents"), blob.PutOptions{}); err != nil {
		t.Fatalf("retry PutBlob(): %v", err)
	}
	if syncAttempts <= 1 {
		t.Fatalf("retry did not repeat directory sync, attempts = %d", syncAttempts)
	}

	var output localTestOutput
	if err := st.GetBlob(ctx, "manifest-002", 0, -1, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "durable contents" {
		t.Fatalf("GetBlob() = %q after retry", got)
	}
}

func TestLocalStorageFileSyncFailureRetriesWithFileThenNestedDirectorySync(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repository")
	st, err := NewLocalStorage(ctx, root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	local := st.(*localStorage)
	pathProvider := local.Storage.(shardedPathProvider)
	dir, file, err := pathProvider.GetShardedPathAndFilePath(ctx, "manifest-004")
	if err != nil {
		t.Fatal(err)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wantDirs []string
	for current, stop := dirAbs, filepath.Dir(rootAbs); ; current = filepath.Dir(current) {
		wantDirs = append(wantDirs, current)
		if current == stop {
			break
		}
	}
	var events []string
	fileAttempts := 0
	syncFile := local.syncFile
	syncDir := local.syncDir
	sentinel := errors.New("injected payload fsync failure")
	local.syncFile = func(path string) error {
		fileAttempts++
		events = append(events, "file:"+path)
		if fileAttempts == 1 {
			return sentinel
		}
		return syncFile(path)
	}
	local.syncDir = func(path string) error {
		events = append(events, "dir:"+path)
		return syncDir(path)
	}
	data := localTestBytes("payload present before durability acknowledgment")
	if err := st.PutBlob(ctx, "manifest-004", data, blob.PutOptions{}); !errors.Is(err, sentinel) {
		t.Fatalf("PutBlob after file sync failure = %v, want injected error", err)
	}
	if len(events) != 1 || events[0] != "file:"+file {
		t.Fatalf("first durability ordering = %#v; want payload fsync only", events)
	}
	var output localTestOutput
	if err := st.GetBlob(ctx, "manifest-004", 0, -1, &output); err != nil {
		t.Fatalf("native PutBlob did not leave retriable bytes: %v", err)
	}
	if output.String() != string(data) {
		t.Fatalf("bytes after failed sync = %q", output.String())
	}
	events = nil
	if err := st.PutBlob(ctx, "manifest-004", data, blob.PutOptions{}); err != nil {
		t.Fatalf("retry identical PutBlob: %v", err)
	}
	if len(events) != len(wantDirs)+1 || events[0] != "file:"+file {
		t.Fatalf("retry durability ordering = %#v", events)
	}
	for i, want := range wantDirs {
		if got := events[i+1]; got != "dir:"+want {
			t.Fatalf("directory sync %d = %q, want %q; full order %#v", i, got, "dir:"+want, events)
		}
	}
	output.Reset()
	if err := st.GetBlob(ctx, "manifest-004", 0, -1, &output); err != nil || output.String() != string(data) {
		t.Fatalf("GetBlob after durable retry = %q, %v", output.String(), err)
	}
}

func TestLocalStorageDeleteSyncFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	st, err := NewLocalStorage(ctx, filepath.Join(t.TempDir(), "repository"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)

	if err := st.PutBlob(ctx, "manifest-003", localTestBytes("contents"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	local := st.(*localStorage)
	actualSyncDir := local.syncDir
	failed := false
	sentinel := errors.New("injected directory sync failure")
	local.syncDir = func(path string) error {
		if !failed {
			failed = true
			return sentinel
		}
		return actualSyncDir(path)
	}

	if err := st.DeleteBlob(ctx, "manifest-003"); !errors.Is(err, sentinel) {
		t.Fatalf("DeleteBlob() error = %v, want injected sync failure", err)
	}
	if err := st.DeleteBlob(ctx, "manifest-003"); err != nil {
		t.Fatalf("retry DeleteBlob(): %v", err)
	}
	var output localTestOutput
	if err := st.GetBlob(ctx, "manifest-003", 0, -1, &output); !errors.Is(err, blob.ErrBlobNotFound) {
		t.Fatalf("GetBlob after deletion = %v, want blob not found", err)
	}
}
