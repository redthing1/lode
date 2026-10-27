package project

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestAbortBeforeArrivalTombstoneBlocksApplyAndPublishAcrossRestart(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "project")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	local := openInitializedLocal(t, directory)
	if err := local.Service.Abort(ctx, "cancel-before-apply"); err != nil {
		t.Fatal(err)
	}
	before, err := local.Service.Operation(ctx, "cancel-before-apply")
	if err != nil || before.State != OperationAborted || before.CommitID != "" || before.Branch != "" {
		t.Fatalf("abort tombstone operation = %#v, %v", before, err)
	}
	applyRequest := ChangeRequest{
		OperationID: "cancel-before-apply", Branch: "main", Author: "tester", Message: "late upload",
		Changes: []Change{{Path: "late.bin", Kind: model.File, SHA256: sha256Hex([]byte("late")), Size: 4}},
	}
	opened := false
	if _, err := local.Service.Apply(ctx, applyRequest, func(model.Path) (io.ReadCloser, error) {
		opened = true
		return nil, errors.New("tombstoned Apply must stop before payload IO")
	}); !errors.Is(err, ErrOperationAborted) {
		t.Fatalf("late Apply error = %v, want ErrOperationAborted", err)
	}
	if opened {
		t.Fatal("late Apply opened its payload")
	}

	archive, _, err := local.Service.objects.Build(ctx, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootID := rootDescriptorID(archive.Root)
	if err := local.Service.Abort(ctx, "cancel-before-arrival"); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Service.Publish(ctx, PublishRequest{
		OperationID: "cancel-before-arrival", Branch: "another-branch", Author: "tester", Message: "late publication", Tree: archive,
	}); !errors.Is(err, ErrOperationAborted) {
		t.Fatalf("late Publish error = %v, want ErrOperationAborted", err)
	}
	if _, err := local.metadata.Root(ctx, local.Service.namespace, archive.Format, rootID); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("late Publish imported root before tombstone rejection: %v", err)
	}
	if tip, err := local.Service.Head(ctx, "main"); err != nil || tip != "" {
		t.Fatalf("Abort-before-arrival created a branch head: %q, %v", tip, err)
	}
	history, _, err := local.Service.History(ctx, "", 10)
	if err != nil || len(history) != 0 {
		t.Fatalf("Abort-before-arrival created history: %#v, %v", history, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local = nil
	local = openInitializedLocal(t, directory)
	after, err := local.Service.Operation(ctx, "cancel-before-arrival")
	if err != nil || after.State != OperationAborted || after.CommitID != "" {
		t.Fatalf("reopened tombstone = %#v, %v", after, err)
	}
	if _, err := local.Service.Apply(ctx, applyRequest, func(model.Path) (io.ReadCloser, error) {
		t.Fatal("reopened tombstone opened payload")
		return nil, nil
	}); !errors.Is(err, ErrOperationAborted) {
		t.Fatalf("reopened late Apply error = %v", err)
	}
	if err := local.Service.Abort(ctx, "cancel-before-arrival"); err != nil {
		t.Fatalf("repeat tombstone Abort: %v", err)
	}
}

func TestAbortStillRejectsAcceptedPublication(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	archive, _, err := local.Service.objects.Build(ctx, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := local.Service.Publish(ctx, PublishRequest{
		OperationID: "accepted-cannot-abort", Branch: "main", Author: "tester", Message: "accepted tree", Tree: archive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Service.Abort(ctx, "accepted-cannot-abort"); !errors.Is(err, ErrAlreadyAccepted) {
		t.Fatalf("Abort accepted publication %q = %v, want ErrAlreadyAccepted", commit.ID, err)
	}
}

func TestAbortBeforeArrivalRejectsInvalidOperationIDs(t *testing.T) {
	service, metadata := openTombstoneTestService(t, testRepositoryNamespace+"-invalid-id")
	defer func() {
		_ = service.Close()
		_ = metadata.Close()
	}()
	for _, id := range []string{"", "contains/slash", "has space", string(make([]byte, 129))} {
		if err := service.Abort(context.Background(), id); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Abort(%q) error = %v, want ErrInvalidRequest", id, err)
		}
	}
	if _, err := service.Operation(context.Background(), "valid-but-not-aborted"); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("unrelated operation lookup = %v", err)
	}
}

func TestHeadRejectsInvalidBranchNames(t *testing.T) {
	service, metadata := openTombstoneTestService(t, testRepositoryNamespace+"-invalid-branch")
	defer func() {
		_ = service.Close()
		_ = metadata.Close()
	}()
	for _, branch := range []string{"", "has space", "/leading", "trailing/", "line\nbreak"} {
		if _, err := service.Head(context.Background(), branch); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Head(%q) error = %v, want ErrInvalidRequest", branch, err)
		}
	}
	if head, err := service.Head(context.Background(), "main"); err != nil || head != "" {
		t.Fatalf("Head(valid absent branch) = %q, %v; want empty head", head, err)
	}
}

func TestAbortBeforeArrivalSerializesWithInFlightPublication(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	roots := &tombstoneRoots{namespace: service.namespace, entered: make(chan struct{}), resume: make(chan struct{}), delegate: service.roots}
	service.roots = roots
	archive, _, err := service.objects.Build(ctx, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := PublishRequest{OperationID: "ordered-publish", Branch: "main", Author: "tester", Message: "publish owns ordering", Tree: archive}
	publishResult := make(chan error, 1)
	go func() {
		_, err := service.Publish(ctx, request)
		publishResult <- err
	}()
	<-roots.entered
	abortResult := make(chan error, 1)
	go func() { abortResult <- service.Abort(ctx, request.OperationID) }()
	close(roots.resume)
	if err := <-publishResult; err != nil {
		t.Fatalf("in-flight publication error: %v", err)
	}
	if err := <-abortResult; !errors.Is(err, ErrAlreadyAccepted) {
		t.Fatalf("Abort after serialized acceptance = %v, want ErrAlreadyAccepted", err)
	}
	if got, err := service.Operation(ctx, request.OperationID); err != nil || got.State != OperationAccepted {
		t.Fatalf("serialized publication result = %#v, %v", got, err)
	}
}

func TestAbortBeforeArrivalTombstoneSurvivesMetadataReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	roots := &tombstoneRoots{namespace: testRepositoryNamespace + "-restart"}
	service, err := NewService(ctx, metadata, roots, Options{})
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	projectID := service.ID()
	if err := service.Abort(ctx, "cancel-before-process-restart"); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	service, err = NewService(ctx, metadata, roots, Options{})
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = service.Close()
		_ = metadata.Close()
	}()
	if service.ID() != projectID {
		t.Fatalf("project ID changed after reopening: %q != %q", service.ID(), projectID)
	}
	if err := service.Abort(ctx, "cancel-before-process-restart"); err != nil {
		t.Fatalf("repeat Abort after restart: %v", err)
	}
	if _, err := service.Publish(ctx, PublishRequest{OperationID: "cancel-before-process-restart", Branch: "main", Tree: kopia.Archive{Format: "f", Root: []byte("r")}}); !errors.Is(err, ErrOperationAborted) {
		t.Fatalf("Publish after restart = %v, want ErrOperationAborted", err)
	}
	if roots.validate.Load() != 0 {
		t.Fatalf("tombstoned Publish performed native validation %d times", roots.validate.Load())
	}
}

type tombstoneRoots struct {
	namespace string
	entered   chan struct{}
	resume    chan struct{}
	validate  atomic.Int32
	delegate  ContentRoots
}

func (r *tombstoneRoots) Namespace() string { return r.namespace }
func (r *tombstoneRoots) Validate(ctx context.Context, archive kopia.Archive) error {
	if r.validate.Add(1) == 1 && r.entered != nil {
		close(r.entered)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.delegate != nil {
		return r.delegate.Validate(ctx, archive)
	}
	return nil
}
func (r *tombstoneRoots) Protect(ctx context.Context, key string, archive kopia.Archive) error {
	if r.delegate != nil {
		return r.delegate.Protect(ctx, key, archive)
	}
	return nil
}
func (r *tombstoneRoots) Release(ctx context.Context, key string) error {
	if r.delegate != nil {
		return r.delegate.Release(ctx, key)
	}
	return nil
}

func openTombstoneTestService(t *testing.T, namespace string) (*Service, *Metadata) {
	t.Helper()
	return openTombstoneTestServiceWithRoots(t, &tombstoneRoots{namespace: namespace})
}

func openTombstoneTestServiceWithRoots(t *testing.T, roots *tombstoneRoots) (*Service, *Metadata) {
	t.Helper()
	metadata, err := OpenMetadata(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(context.Background(), metadata, roots, Options{})
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	return service, metadata
}
