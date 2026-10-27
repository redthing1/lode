package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
)

func TestLocalServiceApplySparseTreeAndAcceptedRetry(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	firstFiles := map[string][]byte{
		"visible/a.bin":     []byte("visible A, base"),
		"visible/b.bin":     []byte("visible B remains omitted"),
		"hidden/secret.bin": []byte("hidden path remains present"),
		"empty.bin":         {},
	}
	firstRequest := makeChangeRequest("initial", "", "initial complete tree", firstFiles)
	reads := 0
	first, err := local.Service.Apply(ctx, firstRequest, func(path model.Path) (io.ReadCloser, error) {
		reads++
		data, ok := firstFiles[path.String()]
		if !ok {
			t.Fatalf("unexpected initial capture path %q", path)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != len(firstFiles) {
		t.Fatalf("initial read count %d, want %d", reads, len(firstFiles))
	}
	if len(first.Parents) != 0 {
		t.Fatalf("initial commit parents = %#v", first.Parents)
	}

	allRules := []SelectionRule{{Path: "", Included: true}}
	initialNodes, next, err := local.Service.Entries(ctx, first.ID, allRules, "", 100)
	if err != nil || next != "" {
		t.Fatalf("initial Entries() next=%q err=%v", next, err)
	}
	assertFileNodes(t, initialNodes, firstFiles)
	if nodeByPath(initialNodes)["empty.bin"].Size != 0 || nodeByPath(initialNodes)["empty.bin"].SHA256 != sha256Hex(nil) {
		t.Fatalf("empty file facts = %#v", nodeByPath(initialNodes)["empty.bin"])
	}

	// A byte-identical retry returns the accepted commit without requesting any
	// source readers. Changing sealed input under the same operation ID conflicts.
	readsBeforeRetry := reads
	replayed, err := local.Service.Apply(ctx, firstRequest, func(model.Path) (io.ReadCloser, error) {
		reads++
		return nil, errors.New("accepted retry must not reopen source files")
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID || reads != readsBeforeRetry {
		t.Fatalf("accepted replay changed result/read sources: %#v reads=%d", replayed, reads)
	}
	changedRequest := firstRequest
	changedRequest.Message = "changed message"
	if _, err := local.Service.Apply(ctx, changedRequest, nil); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("changed accepted replay error = %v, want conflict", err)
	}

	changedA := []byte("visible A, updated")
	update := ChangeRequest{
		OperationID: "sparse-update", Branch: "main", Base: first.ID,
		Author: "tester", Message: "update just one visible file",
		Changes: []Change{{Path: "visible/a.bin", Kind: model.File, SHA256: sha256Hex(changedA), Size: int64(len(changedA))}},
	}
	opened := make([]string, 0, 1)
	second, err := local.Service.Apply(ctx, update, func(path model.Path) (io.ReadCloser, error) {
		opened = append(opened, path.String())
		return io.NopCloser(bytes.NewReader(changedA)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opened, []string{"visible/a.bin"}) {
		t.Fatalf("sparse update opened %v", opened)
	}
	if !reflect.DeepEqual(second.Parents, []string{first.ID}) {
		t.Fatalf("sparse commit parents = %#v", second.Parents)
	}
	updatedNodes, _, err := local.Service.Entries(ctx, second.ID, allRules, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := map[string][]byte{
		"visible/a.bin":     changedA,
		"visible/b.bin":     firstFiles["visible/b.bin"],
		"hidden/secret.bin": firstFiles["hidden/secret.bin"],
		"empty.bin":         {},
	}
	assertFileNodes(t, updatedNodes, wantFiles)
	for path, want := range wantFiles {
		reader, node, err := local.Service.OpenFile(ctx, second.ID, path)
		if err != nil {
			t.Fatalf("open %q: %v", path, err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %q: %v / %v", path, readErr, closeErr)
		}
		if !bytes.Equal(got, want) || node.SHA256 != sha256Hex(want) || node.Size != int64(len(want)) {
			t.Fatalf("file %q readback node=%#v bytes=%q", path, node, got)
		}
	}

	// A commit changes an unselected path. A second operation based on the old
	// complete tree is rejected before its payload reader is opened.
	hiddenUpdate := []byte("hidden changed concurrently")
	hiddenRequest := ChangeRequest{
		OperationID: "hidden-update", Branch: "main", Base: second.ID,
		Author: "tester", Message: "update hidden path",
		Changes: []Change{{Path: "hidden/secret.bin", Kind: model.File, SHA256: sha256Hex(hiddenUpdate), Size: int64(len(hiddenUpdate))}},
	}
	third, err := local.Service.Apply(ctx, hiddenRequest, func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(hiddenUpdate)), nil })
	if err != nil {
		t.Fatal(err)
	}
	staleRequest := ChangeRequest{
		OperationID: "stale-visible-update", Branch: "main", Base: second.ID,
		Author: "tester", Message: "stale branch edit",
		Changes: []Change{{Path: "visible/b.bin", Kind: model.File, SHA256: sha256Hex([]byte("stale")), Size: 5}},
	}
	staleOpened := false
	if _, err := local.Service.Apply(ctx, staleRequest, func(model.Path) (io.ReadCloser, error) {
		staleOpened = true
		return io.NopCloser(bytes.NewReader([]byte("stale"))), nil
	}); !errors.Is(err, ErrStaleHead) {
		t.Fatalf("stale hidden-path base error = %v, want ErrStaleHead", err)
	}
	if staleOpened {
		t.Fatal("stale base opened file payload before head validation")
	}
	if tip, err := local.Service.Head(ctx, "main"); err != nil || tip != third.ID {
		t.Fatalf("head after stale operation = %q, %v; want %q", tip, err, third.ID)
	}

	// The selected directory still contains a hidden child, so a sparse
	// directory deletion must account for that child explicitly.
	deleteRequest := ChangeRequest{
		OperationID: "incomplete-delete", Branch: "main", Base: third.ID,
		Author: "tester", Message: "remove directory incompletely",
		Changes: []Change{{Path: "visible", Kind: model.Absent}},
	}
	if _, err := local.Service.Apply(ctx, deleteRequest, nil); !errors.Is(err, model.ErrTreeConflict) {
		t.Fatalf("directory deletion with omitted children error = %v, want tree conflict", err)
	}
	if got, err := local.Service.Operation(ctx, deleteRequest.OperationID); err != nil || got.State != OperationCapturing {
		t.Fatalf("failed incomplete deletion record = %#v, %v", got, err)
	}
	if err := local.Service.Abort(ctx, deleteRequest.OperationID); err != nil {
		t.Fatal(err)
	}
}

func TestLocalServiceInterruptedCaptureReuploadsSameCandidateAfterReopen(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "project")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	local := openInitializedLocal(t, directory)
	data := []byte("partial payload before interruption")
	request := ChangeRequest{
		OperationID: "interrupted-capture", Branch: "main", Author: "tester", Message: "upload after reconnect",
		Changes: []Change{{Path: "payload.bin", Kind: model.File, SHA256: sha256Hex(data), Size: int64(len(data))}},
	}
	readFailure := errors.New("simulated upload interruption")
	_, err := local.Service.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
		return &failingReader{Reader: bytes.NewReader(data[:8]), err: readFailure}, nil
	})
	if !errors.Is(err, readFailure) {
		t.Fatalf("interrupted capture error = %v, want reader error", err)
	}
	before, err := local.Service.Operation(ctx, request.OperationID)
	if err != nil || before.State != OperationCapturing || before.CommitID == "" {
		t.Fatalf("capturing operation after interruption = %#v, %v", before, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local = nil

	local = openInitializedLocal(t, directory)
	after, err := local.Service.Operation(ctx, request.OperationID)
	if err != nil || after.State != OperationCapturing || after.CommitID != before.CommitID {
		t.Fatalf("capture after reopen = %#v, %v; want same candidate %q", after, err, before.CommitID)
	}
	if _, err := local.Service.Resume(ctx, request.OperationID); !errors.Is(err, ErrUploadRequired) {
		t.Fatalf("Resume(capturing) error = %v, want ErrUploadRequired", err)
	}
	commit, err := local.Service.Apply(ctx, request, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if commit.ID != before.CommitID {
		t.Fatalf("successful reupload commit ID %q, want original candidate %q", commit.ID, before.CommitID)
	}
	if got, err := local.Service.Operation(ctx, request.OperationID); err != nil || got.State != OperationAccepted || got.CommitID != before.CommitID {
		t.Fatalf("reuploaded capture operation = %#v, %v", got, err)
	}
	reader, _, err := local.Service.OpenFile(ctx, commit.ID, "payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, data) {
		t.Fatalf("reuploaded file bytes = %q, errors %v / %v", got, readErr, closeErr)
	}
}

func TestLocalServiceAbortAndExpireCapturingOperations(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	data := []byte("never completed")
	makeRequest := func(id string) ChangeRequest {
		return ChangeRequest{OperationID: id, Branch: "main", Author: "tester", Message: "capture then stop", Changes: []Change{{Path: "file.bin", Kind: model.File, SHA256: sha256Hex(data), Size: int64(len(data))}}}
	}
	interrupt := func(model.Path) (io.ReadCloser, error) {
		return &failingReader{Reader: bytes.NewReader(data[:4]), err: errors.New("interrupted")}, nil
	}
	abortRequest := makeRequest("abort-capturing")
	if _, err := local.Service.Apply(ctx, abortRequest, interrupt); err == nil {
		t.Fatal("interrupted capture unexpectedly completed")
	}
	if err := local.Service.Abort(ctx, abortRequest.OperationID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.Abort(ctx, abortRequest.OperationID); err != nil {
		t.Fatalf("repeated capture abort: %v", err)
	}
	if got, err := local.Service.Operation(ctx, abortRequest.OperationID); err != nil || got.State != OperationAborted {
		t.Fatalf("aborted capture = %#v, %v", got, err)
	}
	if _, err := local.Service.Apply(ctx, abortRequest, func(model.Path) (io.ReadCloser, error) { t.Fatal("aborted capture reopened source"); return nil, nil }); !errors.Is(err, ErrOperationAborted) {
		t.Fatalf("aborted capture replay = %v", err)
	}

	expireRequest := makeRequest("expire-capturing")
	if _, err := local.Service.Apply(ctx, expireRequest, interrupt); err == nil {
		t.Fatal("interrupted capture unexpectedly completed")
	}
	if _, err := local.metadata.db.ExecContext(ctx, `UPDATE publication_ops SET expires_at = ? WHERE id = ?`, formatMetadataTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)), expireRequest.OperationID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := local.Service.Operation(ctx, expireRequest.OperationID); err != nil || got.State != OperationExpired {
		t.Fatalf("expired capture = %#v, %v", got, err)
	}
	if _, err := local.Service.Resume(ctx, expireRequest.OperationID); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("expired capture resume = %v", err)
	}
	if _, err := local.Service.Apply(ctx, expireRequest, func(model.Path) (io.ReadCloser, error) { t.Fatal("expired capture reopened source"); return nil, nil }); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("expired capture replay = %v", err)
	}
}

type failingReader struct {
	*bytes.Reader
	err error
}

func (r *failingReader) Close() error { return nil }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.Reader.Len() == 0 {
		return 0, r.err
	}
	if len(p) > r.Reader.Len() {
		p = p[:r.Reader.Len()]
	}
	return r.Reader.Read(p)
}

func openTestLocal(t *testing.T) *Local {
	t.Helper()
	if testing.Short() {
		t.Skip("local Kopia integration test")
	}
	directory := filepath.Join(t.TempDir(), "project")
	if err := InitializeLocal(context.Background(), directory); err != nil {
		t.Fatal(err)
	}
	return openInitializedLocal(t, directory)
}

func openInitializedLocal(t *testing.T, directory string) *Local {
	t.Helper()
	local, err := OpenLocal(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if local != nil {
			if err := local.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return local
}

func makeChangeRequest(operationID, base, message string, files map[string][]byte) ChangeRequest {
	request := ChangeRequest{OperationID: operationID, Branch: "main", Base: base, Author: "tester", Message: message}
	for path, data := range files {
		request.Changes = append(request.Changes, Change{Path: path, Kind: model.File, SHA256: sha256Hex(data), Size: int64(len(data))})
	}
	return request
}

func sha256Hex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func assertFileNodes(t *testing.T, nodes []Node, files map[string][]byte) {
	t.Helper()
	got := nodeByPath(nodes)
	for path, data := range files {
		node, ok := got[path]
		if !ok {
			t.Errorf("missing entry %q", path)
			continue
		}
		if node.Kind != model.File || node.Size != int64(len(data)) || node.SHA256 != sha256Hex(data) {
			t.Errorf("entry %q = %#v", path, node)
		}
	}
	for path, node := range got {
		if node.Kind == model.File {
			if _, ok := files[path]; !ok {
				t.Errorf("unexpected file entry %q", path)
			}
		}
	}
}

func nodeByPath(nodes []Node) map[string]Node {
	result := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		result[node.Path] = node
	}
	return result
}
