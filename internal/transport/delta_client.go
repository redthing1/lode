package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func hasDeltaCandidate(request project.ChangeRequest) bool {
	if request.Base == "" {
		return false
	}
	for _, change := range request.Changes {
		if change.Kind == model.File && change.Size >= project.MinDeltaFileSize {
			return true
		}
	}
	return false
}

// prepareTransfer seals the operation before the streaming body can request
// per-file signatures. Missing routes indicate an older full-upload server;
// authentication, input, and operation errors must still fail the request.
func (c *Client) prepareTransfer(ctx context.Context, request project.ChangeRequest, manifest []byte) (project.Operation, bool, error) {
	response, err := c.do(ctx, http.MethodPost, ChangesPreparePath, nil, io.NopCloser(bytes.NewReader(manifest)), "application/json")
	if err != nil {
		return project.Operation{}, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed {
		data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBody+1))
		if err != nil || len(data) > maxJSONBody {
			return project.Operation{}, false, fmt.Errorf("read prepare response: %w", ErrRemote)
		}
		var wire wireError
		if strictJSON(data, &wire) == nil {
			if sentinel := errorSentinel(wire.Code); sentinel != nil {
				return project.Operation{}, false, sentinel
			}
			if wire.Code != "not_found" && wire.Code != "method_not_allowed" {
				return project.Operation{}, false, fmt.Errorf("unexpected prepare response: %w", ErrRemote)
			}
		}
		return project.Operation{}, false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return project.Operation{}, false, decodeWireError(response)
	}
	var operation project.Operation
	if err := decodeResponse(response, &operation); err != nil {
		return project.Operation{}, false, err
	}
	if operation.ID != request.OperationID || operation.Branch != request.Branch || operation.Base != request.Base ||
		(operation.State != project.OperationCapturing && operation.State != project.OperationAccepted) ||
		(operation.State == project.OperationAccepted && operation.CommitID == "") {
		return project.Operation{}, false, fmt.Errorf("unexpected prepared operation: %w", ErrRemote)
	}
	return operation, true, nil
}

func (c *Client) fileSignature(ctx context.Context, request project.ChangeRequest, change project.Change) (project.FileSignature, error) {
	var result project.FileSignature
	query := url.Values{"operation": {request.OperationID}, "path": {change.Path}}
	if err := c.getJSON(ctx, ChangesSignaturePath, query, &result); err != nil {
		return project.FileSignature{}, err
	}
	if result.OperationID != request.OperationID || result.Base != request.Base || result.Path != change.Path || result.Basis.Path != change.Path {
		return project.FileSignature{}, fmt.Errorf("unexpected transfer basis: %w", ErrRemote)
	}
	switch result.Basis.Kind {
	case model.Absent, model.Directory:
		if result.Basis.Size != 0 || result.Basis.SHA256 != "" || len(result.Data) != 0 {
			return project.FileSignature{}, fmt.Errorf("invalid non-file transfer basis: %w", ErrRemote)
		}
	case model.File:
		digest, err := hex.DecodeString(result.Basis.SHA256)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != result.Basis.SHA256 ||
			result.Basis.Size < 0 || result.Basis.Size > project.MaxFileSize ||
			(len(result.Data) != 0 && result.Basis.Size < project.MinDeltaFileSize) {
			return project.FileSignature{}, fmt.Errorf("invalid file transfer basis: %w", ErrRemote)
		}
	default:
		return project.FileSignature{}, fmt.Errorf("invalid transfer basis kind: %w", ErrRemote)
	}
	// Delta checks the exact signature length and header parameters before
	// parsing the bounded table or reading source bytes. Only one signature is
	// retained at a time; no encoded patch is staged on the client.
	return result, nil
}
