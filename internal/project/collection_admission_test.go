package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

type observedMaintenance struct {
	ContentMaintenance
	probe   func(context.Context) (blob.Capacity, error)
	collect func(context.Context, kopia.CollectionOptions) (kopia.CollectionReport, error)
}

func (m observedMaintenance) Capacity(ctx context.Context) (blob.Capacity, error) {
	if m.probe != nil {
		return m.probe(ctx)
	}
	return m.ContentMaintenance.Capacity(ctx)
}

func (m observedMaintenance) RunCollection(ctx context.Context, options kopia.CollectionOptions) (kopia.CollectionReport, error) {
	if m.collect != nil {
		return m.collect(ctx, options)
	}
	return m.ContentMaintenance.RunCollection(ctx, options)
}

func TestCaptureCapacityAdmissionAndAcceptedReplay(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	data := []byte("a file whose capture must leave operational headroom")
	request := makeChangeRequest("capacity-capture", "", "capacity", map[string][]byte{"file.bin": data})
	available := DefaultCapacityReserveBytes + uint64(len(data))*2 - 1
	var probes, opens int
	service.maintenance = observedMaintenance{ContentMaintenance: service.maintenance, probe: func(context.Context) (blob.Capacity, error) {
		probes++
		return blob.Capacity{FreeB: available}, nil
	}}
	open := func(model.Path) (io.ReadCloser, error) { opens++; return io.NopCloser(bytes.NewReader(data)), nil }
	if _, err := service.Apply(ctx, request, open); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("capture with insufficient headroom: %v", err)
	}
	if opens != 0 {
		t.Fatal("rejected capture opened file contents")
	}
	operation, err := service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != OperationCapturing {
		t.Fatalf("retryable capture state: %+v, %v", operation, err)
	}
	available++
	commit, err := service.Apply(ctx, request, open)
	if err != nil || opens != 1 {
		t.Fatalf("retry with headroom: %+v, %v, opens=%d", commit, err, opens)
	}
	available = 0
	previousProbes := probes
	replayed, err := service.Apply(ctx, request, nil)
	if err != nil || replayed.ID != commit.ID || probes != previousProbes {
		t.Fatalf("accepted replay must not need new capacity: %+v, %v, probes=%d/%d", replayed, err, probes, previousProbes)
	}
}

func TestCaptureRechecksCapacityBeforeEachFileAndRemainsAbortable(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	files := map[string][]byte{"a.bin": []byte("first file"), "b.bin": []byte("second file")}
	request := makeChangeRequest("capacity-between-files", "", "two files", files)
	var calls int
	service.maintenance = observedMaintenance{ContentMaintenance: service.maintenance, probe: func(context.Context) (blob.Capacity, error) {
		calls++
		if calls >= 3 {
			return blob.Capacity{FreeB: 0}, nil
		}
		return blob.Capacity{FreeB: 4 << 30}, nil
	}}
	var opened []string
	_, err := service.Apply(ctx, request, func(path model.Path) (io.ReadCloser, error) {
		opened = append(opened, path.String())
		return io.NopCloser(bytes.NewReader(files[path.String()])), nil
	})
	if !errors.Is(err, ErrInsufficientCapacity) || len(opened) != 1 || opened[0] != "a.bin" {
		t.Fatalf("capacity loss between files: opens=%v, error=%v", opened, err)
	}
	if head, err := service.Head(ctx, "main"); err != nil || head != "" {
		t.Fatalf("partial upload published a head: %q, %v", head, err)
	}
	if err := service.Abort(ctx, request.OperationID); err != nil {
		t.Fatalf("low capacity prevented abort: %v", err)
	}
	if err := service.Recover(ctx); err != nil {
		t.Fatalf("low capacity prevented recovery: %v", err)
	}
}

func TestCaptureUnknownCapacityNeverOpensPayload(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	service.maintenance = observedMaintenance{ContentMaintenance: service.maintenance, probe: func(context.Context) (blob.Capacity, error) {
		return blob.Capacity{}, errors.New("capacity provider unavailable")
	}}
	data := []byte("must not be opened")
	request := makeChangeRequest("unknown-capacity", "", "capacity", map[string][]byte{"file.bin": data})
	_, err := service.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
		t.Error("unknown capacity allowed source IO")
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if !errors.Is(err, ErrCapacityUnknown) {
		t.Fatalf("unknown capacity: %v", err)
	}
}

func TestCollectionUsesReservedHeadroomForGCWithoutAdmittingCopies(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	native := service.maintenance
	var called bool
	service.maintenance = observedMaintenance{ContentMaintenance: native,
		probe: func(context.Context) (blob.Capacity, error) {
			return blob.Capacity{FreeB: DefaultCapacityReserveBytes - 1}, nil
		},
		collect: func(ctx context.Context, options kopia.CollectionOptions) (kopia.CollectionReport, error) {
			called = true
			if options.MaxRewriteBytes != 0 {
				t.Fatalf("low-space collection admitted %d rewritten bytes", options.MaxRewriteBytes)
			}
			if err := options.BeforeRewrite(kopia.PackCandidate{ID: "p-capacity-test", IndexedLiveBytes: 1}); !errors.Is(err, ErrInsufficientCapacity) {
				t.Fatalf("native copy checkpoint bypassed capacity: %v", err)
			}
			return native.RunCollection(ctx, options)
		},
	}
	report, err := service.Collect(ctx, CollectRequest{})
	if err != nil || !called || !report.Executed || !strings.Contains(report.DeferredReason, "headroom") {
		t.Fatalf("low-space native collection: %+v, %v, called=%t", report, err, called)
	}
	if _, exists, err := service.metadata.compaction(ctx); err != nil || exists {
		t.Fatalf("refused rewrite created compaction intent: exists=%t error=%v", exists, err)
	}
}

func TestCollectionWaitsForNativeFileReaderBeforeEnteringMaintenance(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	data := []byte("a native stream is active through close")
	commit := mergeApply(t, service, "collection-reader", "main", "", fileChanges(map[string][]byte{"file.bin": data}), map[string][]byte{"file.bin": data})
	reader, _, err := service.OpenFile(ctx, commit.ID, "file.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	entered := make(chan struct{})
	native := service.maintenance
	service.maintenance = observedMaintenance{ContentMaintenance: native, collect: func(ctx context.Context, options kopia.CollectionOptions) (kopia.CollectionReport, error) {
		close(entered)
		return native.RunCollection(ctx, options)
	}}
	done := make(chan error, 1)
	go func() { _, err := service.Collect(ctx, CollectRequest{}); done <- err }()
	select {
	case <-entered:
		t.Fatal("maintenance entered with an open native stream")
	case <-time.After(30 * time.Millisecond):
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("active read during collection wait: %q, %v", got, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("collection did not proceed after reader close")
	}
}
