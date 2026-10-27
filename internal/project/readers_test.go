package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestActiveFileReaderProtectsExpiredLeaseAndGatesExclusiveContentWork(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	oldBytes := []byte("stream held while retention lease elapses")
	old := mergeApply(t, local.Service, "reader-old", "main", "", fileChanges(map[string][]byte{"stream.bin": oldBytes}), map[string][]byte{"stream.bin": oldBytes})
	tipBytes := []byte("new branch tip")
	tip := mergeApply(t, local.Service, "reader-tip", "main", old.ID, fileChanges(map[string][]byte{"stream.bin": tipBytes}), map[string][]byte{"stream.bin": tipBytes})
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	late := tip.CreatedAt.Add(48 * time.Hour)
	local.Service.now = func() time.Time { return late }
	lease, err := local.Service.HoldRead(ctx, "reader-lease", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	readerCtx, cancelReader := context.WithCancel(ctx)
	reader, _, err := local.Service.OpenFile(readerCtx, old.ID, "stream.bin")
	if err != nil {
		t.Fatal(err)
	}
	if lease.ExpiresAt.Sub(late) <= 0 {
		t.Fatalf("read hold expiry = %s, now=%s", lease.ExpiresAt, late)
	}
	// The durable lease expires, but the open stream continues to protect the
	// payload until its native content reader is closed.
	local.Service.now = func() time.Time { return lease.ExpiresAt.Add(time.Minute) }
	result, err := local.Service.Expire(ctx, []string{old.ID})
	if err != nil || len(result.Protected) != 1 || !containsString(result.Protected[0].Reasons, "active file stream") {
		t.Fatalf("expiry with active stream and elapsed lease = %+v, %v", result, err)
	}

	// Native collection takes this exclusive gate. It must wait for the stream,
	// then proceed when request cancellation closes the tracked reader.
	entered := startContentExclusive(t, local.Service)
	select {
	case <-entered:
		t.Fatal("exclusive content gate passed an open reader")
	case <-time.After(50 * time.Millisecond):
	}
	cancelReader()
	var one [1]byte
	if _, readErr := reader.Read(one[:]); !errors.Is(readErr, context.Canceled) {
		t.Errorf("read after request cancellation = %v, want context.Canceled", readErr)
	}
	waitContentExclusive(t, entered)

	result, err = local.Service.Expire(ctx, []string{old.ID})
	if err != nil || len(result.Expired) != 1 || result.Expired[0] != old.ID {
		t.Fatalf("expiry after stream cancellation and lease expiry = %+v, %v", result, err)
	}
	if err := local.Service.ReleaseRead(ctx, "reader-lease"); err != nil {
		t.Fatalf("release elapsed lease: %v", err)
	}
}

func TestExplicitReaderCloseReleasesExclusiveContentGate(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	data := []byte("native stream")
	commit := mergeApply(t, local.Service, "reader-close", "main", "", fileChanges(map[string][]byte{"file.bin": data}), map[string][]byte{"file.bin": data})
	reader, _, err := local.Service.OpenFile(ctx, commit.ID, "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, reader); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("reader bytes=%q, want %q", got.Bytes(), data)
	}
	entered := startContentExclusive(t, local.Service)
	select {
	case <-entered:
		t.Fatal("exclusive content gate passed before reader Close")
	case <-time.After(50 * time.Millisecond):
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	waitContentExclusive(t, entered)
}

func startContentExclusive(t *testing.T, service *Service) <-chan struct{} {
	t.Helper()
	entered := make(chan struct{})
	go func() {
		service.contentMu.Lock()
		close(entered)
		service.contentMu.Unlock()
	}()
	return entered
}

func waitContentExclusive(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("exclusive content gate did not proceed after reader release")
	}
}
