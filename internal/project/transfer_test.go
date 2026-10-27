package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/transfer"
)

func TestSignatureRunsBesideBlockedApplyAndReconstructsIntoNativeWriter(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	basis := transferTestBytes(5, 2<<20)
	baseRequest := makeChangeRequest("transfer-base", "", "basis", map[string][]byte{"large.bin": basis})
	baseCommit, err := service.Apply(ctx, baseRequest, func(path model.Path) (io.ReadCloser, error) {
		if path.String() != "large.bin" {
			t.Fatalf("unexpected base path %q", path)
		}
		return io.NopCloser(bytes.NewReader(basis)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	target := append([]byte(nil), basis...)
	for i := 31 << 10; i < 35<<10; i++ {
		target[i] ^= byte(i)
	}
	request := makeChangeRequest("transfer-delta", baseCommit.ID, "small edit", map[string][]byte{"large.bin": target})
	prepared, err := service.PrepareChanges(ctx, request)
	if err != nil || prepared.State != OperationCapturing || prepared.Base != baseCommit.ID {
		t.Fatalf("prepare operation = %+v, %v", prepared, err)
	}
	preparedAgain, err := service.PrepareChanges(ctx, request)
	if err != nil || preparedAgain.ID != prepared.ID || preparedAgain.State != OperationCapturing {
		t.Fatalf("exact prepare retry = %+v, %v", preparedAgain, err)
	}

	commit, err := service.ApplyWithBasis(ctx, request, func(path model.Path, openBasis BasisOpener) (io.ReadCloser, error) {
		if path.String() != "large.bin" {
			return nil, errors.New("unexpected changed path")
		}
		type signatureResult struct {
			signature FileSignature
			err       error
		}
		signatureDone := make(chan signatureResult, 1)
		go func() {
			signature, err := service.Signature(ctx, request.OperationID, path.String())
			signatureDone <- signatureResult{signature, err}
		}()
		var signed FileSignature
		select {
		case result := <-signatureDone:
			if result.err != nil {
				return nil, result.err
			}
			signed = result.signature
		case <-time.After(5 * time.Second):
			return nil, errors.New("Signature deadlocked behind the in-flight Apply")
		}
		if signed.OperationID != request.OperationID || signed.Base != baseCommit.ID || signed.Path != path.String() ||
			signed.Basis.Kind != model.File || signed.Basis.Size != int64(len(basis)) || signed.Basis.SHA256 != sha256Hex(basis) || len(signed.Data) == 0 {
			return nil, errors.New("signature response did not describe the sealed base file")
		}

		var encoded bytes.Buffer
		if err := transfer.Delta(ctx, signed.Data, signed.Basis.Size, int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
			return nil, err
		}
		reader, node, err := openBasis()
		if err != nil {
			return nil, err
		}
		if node.SHA256 != signed.Basis.SHA256 || node.Size != signed.Basis.Size {
			_ = reader.Close()
			return nil, errors.New("Apply basis differs from signed basis")
		}
		reconstructed, err := transfer.NewReader(ctx, reader, node.Size, int64(len(target)), bytes.NewReader(encoded.Bytes()))
		if err != nil {
			_ = reader.Close()
			return nil, err
		}
		return io.NopCloser(reconstructed), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, node, err := service.OpenFile(ctx, commit.ID, "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, target) || node.SHA256 != sha256Hex(target) {
		t.Fatalf("reconstructed native file mismatch: read=%v close=%v size=%d sha=%s", readErr, closeErr, len(got), node.SHA256)
	}
	if _, err := service.Signature(ctx, request.OperationID, "large.bin"); !errors.Is(err, ErrAlreadyAccepted) {
		t.Fatalf("signature after accepted capture = %v, want ErrAlreadyAccepted", err)
	}
}

func TestSignatureFallbacksAndAbortBeforeSignature(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	large := transferTestBytes(11, 2<<20)
	small := []byte("small basis")
	baseRequest := makeChangeRequest("transfer-fallback-base", "", "basis", map[string][]byte{
		"large.bin": large,
		"small.bin": small,
	})
	baseRequest.Changes = append(baseRequest.Changes, Change{Path: "folder", Kind: model.Directory})
	baseCommit, err := service.Apply(ctx, baseRequest, func(path model.Path) (io.ReadCloser, error) {
		data := map[string][]byte{"large.bin": large, "small.bin": small}[path.String()]
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	targetLarge := transferTestBytes(12, 2<<20)
	targetSmall := []byte("tiny new content")
	request := ChangeRequest{OperationID: "transfer-fallback", Branch: "main", Base: baseCommit.ID, Author: "tester", Message: "fallback cases", Changes: []Change{
		{Path: "large.bin", Kind: model.File, SHA256: sha256Hex(targetSmall), Size: int64(len(targetSmall))},
		{Path: "small.bin", Kind: model.File, SHA256: sha256Hex(targetLarge), Size: int64(len(targetLarge))},
		{Path: "folder", Kind: model.File, SHA256: sha256Hex(targetLarge), Size: int64(len(targetLarge))},
		{Path: "new.bin", Kind: model.File, SHA256: sha256Hex(targetLarge), Size: int64(len(targetLarge))},
	}}
	if _, err := service.PrepareChanges(ctx, request); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"large.bin", "small.bin", "folder", "new.bin"} {
		result, err := service.Signature(ctx, request.OperationID, path)
		if err != nil {
			t.Fatalf("Signature(%s): %v", path, err)
		}
		if len(result.Data) != 0 {
			t.Errorf("Signature(%s) returned a delta signature for an ineligible basis: %d bytes", path, len(result.Data))
		}
		switch path {
		case "large.bin":
			if result.Basis.Kind != model.File || result.Basis.Size != int64(len(large)) {
				t.Errorf("large basis metadata for small target = %+v", result.Basis)
			}
		case "small.bin":
			if result.Basis.Kind != model.File || result.Basis.Size != int64(len(small)) {
				t.Errorf("small basis metadata = %+v", result.Basis)
			}
		case "folder":
			if result.Basis.Kind != model.Directory {
				t.Errorf("directory basis metadata = %+v", result.Basis)
			}
		case "new.bin":
			if result.Basis.Kind != model.Absent {
				t.Errorf("missing basis metadata = %+v", result.Basis)
			}
		}
	}
	if _, err := service.Signature(ctx, request.OperationID, "not-changed.bin"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("signature for unsealed path = %v, want invalid request", err)
	}
	if err := service.Abort(ctx, request.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Signature(ctx, request.OperationID, "small.bin"); !errors.Is(err, ErrOperationAborted) {
		t.Fatalf("signature after abort = %v, want ErrOperationAborted", err)
	}
}

func TestPreparedSignatureSurvivesReopenAndExpiresReadOnly(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "project")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	local, err := OpenLocal(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error(err)
		}
	})
	basis := transferTestBytes(19, 2<<20)
	baseCommit, err := local.Service.Apply(ctx, makeChangeRequest("transfer-reopen-base", "", "basis", map[string][]byte{"file.bin": basis}), func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(basis)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	target := transferTestBytes(20, 2<<20)
	request := makeChangeRequest("transfer-reopen", baseCommit.ID, "reopen", map[string][]byte{"file.bin": target})
	prepared, err := local.Service.PrepareChanges(ctx, request)
	if err != nil || prepared.State != OperationCapturing {
		t.Fatalf("prepare = %+v, %v", prepared, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local, err = OpenLocal(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := local.Service.Signature(ctx, request.OperationID, "file.bin")
	if err != nil || len(signature.Data) == 0 || signature.Basis.SHA256 != sha256Hex(basis) {
		t.Fatalf("signature after reopen = %+v, %v", signature, err)
	}
	local.Service.now = func() time.Time { return prepared.ExpiresAt }
	if _, err := local.Service.Signature(ctx, request.OperationID, "file.bin"); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("signature at expiry = %v, want ErrOperationExpired", err)
	}
	operation, err := local.Service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != OperationCapturing {
		t.Fatalf("read-only signature check changed capture state: %+v, %v", operation, err)
	}
	if _, err := local.Service.Resume(ctx, request.OperationID); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("Resume after expiry = %v, want ErrOperationExpired", err)
	}
	operation, err = local.Service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != OperationExpired {
		t.Fatalf("capture expiry was not made durable by Resume: %+v, %v", operation, err)
	}
}

func TestSignatureDetectsStaleBaseWithoutChangingCapture(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	service := local.Service
	basis := transferTestBytes(23, 2<<20)
	base, err := service.Apply(ctx, makeChangeRequest("transfer-stale-base", "", "base", map[string][]byte{"file.bin": basis}), func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(basis)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	target := transferTestBytes(24, 2<<20)
	request := makeChangeRequest("transfer-stale", base.ID, "stale capture", map[string][]byte{"file.bin": target})
	if _, err := service.PrepareChanges(ctx, request); err != nil {
		t.Fatal(err)
	}
	advance := []byte("branch advanced")
	if _, err := service.Apply(ctx, makeChangeRequest("transfer-advance", base.ID, "advance", map[string][]byte{"file.bin": advance}), func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(advance)), nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Signature(ctx, request.OperationID, "file.bin"); !errors.Is(err, ErrStaleHead) {
		t.Fatalf("signature with stale basis = %v, want ErrStaleHead", err)
	}
	operation, err := service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != OperationCapturing {
		t.Fatalf("read-only stale check changed capture state: %+v, %v", operation, err)
	}
	if _, err := service.Apply(ctx, request, nil); !errors.Is(err, ErrStaleHead) {
		t.Fatalf("Apply after stale signature = %v, want ErrStaleHead", err)
	}
	operation, err = service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != OperationStale {
		t.Fatalf("Apply did not persist stale outcome: %+v, %v", operation, err)
	}
}

func transferTestBytes(seed byte, size int) []byte {
	result := make([]byte, size)
	state := uint32(seed) + 1
	for i := range result {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		result[i] = byte(state)
	}
	return result
}
