package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	kopiaengine "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transfer"
)

var ErrRemote = errors.New("remote project service error")

// Client is an authenticated connection to a project service. Plain HTTP is
// restricted to loopback; HTTPS may use a remote host.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient accepts only loopback HTTP or HTTPS URLs, suitable for direct local
// access or an SSH tunnel. It does not install a request timeout because file
// transfers may be large.
func NewClient(baseURL, token string) (*Client, error) {
	if token == "" {
		return nil, errors.New("transport: bearer token is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(baseURL, "#") || u.Path != "" && u.Path != "/" || u.Opaque != "" {
		return nil, errors.New("transport: base URL must be a loopback HTTP(S) origin without credentials, query, fragment, or path")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("transport: base URL host must be loopback")
	}
	u.Path = ""
	u.RawPath = ""
	return &Client{
		base: u, token: token,
		http: &http.Client{
			Transport: &http.Transport{
				// A streaming upload requests each file signature on another
				// connection or stream. Keep per-host connections unrestricted.
				Proxy:               nil,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Close releases idle keep-alive connections. In-flight responses remain owned
// by their callers and are not interrupted.
func (c *Client) Close() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) Info(ctx context.Context) (ProjectInfo, error) {
	var result ProjectInfo
	err := c.getJSON(ctx, "/v1/info", nil, &result)
	return result, err
}

func (c *Client) Head(ctx context.Context, branch string) (string, error) {
	var result struct {
		Head string `json:"head"`
	}
	err := c.getJSON(ctx, "/v1/head", url.Values{"branch": {branch}}, &result)
	return result.Head, err
}

func (c *Client) CreateBranch(ctx context.Context, name, fromCommitID string) error {
	return c.postJSON(ctx, "/v1/branch/create", struct {
		Name         string `json:"name"`
		FromCommitID string `json:"fromCommitID"`
	}{name, fromCommitID}, nil)
}

func (c *Client) DeleteBranch(ctx context.Context, name, expectedTip string) error {
	return c.postJSON(ctx, "/v1/branch/delete", struct {
		Name        string `json:"name"`
		ExpectedTip string `json:"expectedTip"`
	}{name, expectedTip}, nil)
}

func (c *Client) Branch(ctx context.Context, name string) (project.Branch, error) {
	var result branchResponse
	err := c.getJSON(ctx, "/v1/branch", url.Values{"name": {name}}, &result)
	return project.Branch{Name: result.Name, Tip: result.Tip}, err
}

func (c *Client) Branches(ctx context.Context, after string, limit int) ([]project.Branch, string, error) {
	var result branchesResponse
	err := c.getJSON(ctx, "/v1/branches", url.Values{"after": {after}, "limit": {strconv.Itoa(limit)}}, &result)
	branches := make([]project.Branch, 0, len(result.Branches))
	for _, branch := range result.Branches {
		branches = append(branches, project.Branch{Name: branch.Name, Tip: branch.Tip})
	}
	return branches, result.Next, err
}

func (c *Client) Commit(ctx context.Context, id string) (project.Commit, error) {
	var result project.Commit
	err := c.getJSON(ctx, "/v1/commit", url.Values{"id": {id}}, &result)
	return result, err
}

func (c *Client) Operation(ctx context.Context, id string) (project.Operation, error) {
	var result project.Operation
	err := c.getJSON(ctx, "/v1/operation", url.Values{"id": {id}}, &result)
	return result, err
}

func (c *Client) Resume(ctx context.Context, id string) (project.Commit, error) {
	var result project.Commit
	err := c.postJSON(ctx, "/v1/resume", struct {
		OperationID string `json:"operationID"`
	}{id}, &result)
	return result, err
}

func (c *Client) Abort(ctx context.Context, id string) error {
	return c.postJSON(ctx, "/v1/abort", struct {
		OperationID string `json:"operationID"`
	}{id}, nil)
}

func (c *Client) Entries(ctx context.Context, commit string, rules []project.SelectionRule, after string, limit int) ([]project.Node, string, error) {
	var result entriesResponse
	err := c.postJSON(ctx, "/v1/entries", entriesRequest{Commit: commit, Rules: rules, After: after, Limit: limit}, &result)
	return result.Nodes, result.Next, err
}

func (c *Client) History(ctx context.Context, start string, limit int) ([]project.Commit, string, error) {
	var result historyResponse
	err := c.getJSON(ctx, "/v1/history", url.Values{"start": {start}, "limit": {strconv.Itoa(limit)}}, &result)
	return result.Commits, result.Next, err
}

func (c *Client) PreviewMerge(ctx context.Context, spec project.MergeSpec, after string, limit int) (project.MergePreview, error) {
	var result project.MergePreview
	err := c.postJSON(ctx, "/v1/merge/preview", struct {
		Spec  project.MergeSpec `json:"spec"`
		After string            `json:"after"`
		Limit int               `json:"limit"`
	}{spec, after, limit}, &result)
	return result, err
}

func (c *Client) Merge(ctx context.Context, request project.MergeRequest) (project.Commit, error) {
	var result project.Commit
	err := c.postJSON(ctx, "/v1/merge", request, &result)
	return result, err
}

func (c *Client) OpenFile(ctx context.Context, commit, path string) (io.ReadCloser, project.Node, error) {
	query := url.Values{"commit": {commit}, "path": {path}}
	response, err := c.do(ctx, http.MethodGet, "/v1/file", query, nil, "")
	if err != nil {
		return nil, project.Node{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, project.Node{}, decodeWireError(response)
	}
	encoded := response.Header.Get("X-Lode-Node")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		response.Body.Close()
		return nil, project.Node{}, fmt.Errorf("decode file metadata: %w", ErrRemote)
	}
	var node project.Node
	if err := strictJSON(raw, &node); err != nil {
		response.Body.Close()
		return nil, project.Node{}, fmt.Errorf("decode file metadata: %w", ErrRemote)
	}
	if node.Path != path || node.Kind != model.File {
		response.Body.Close()
		return nil, project.Node{}, fmt.Errorf("unexpected file metadata: %w", ErrRemote)
	}
	length, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	if err != nil || length != node.Size || response.Header.Get("Content-Type") != "application/octet-stream" {
		response.Body.Close()
		return nil, project.Node{}, fmt.Errorf("invalid file response metadata: %w", ErrRemote)
	}
	return response.Body, node, nil
}

// HoldRead keeps a commit's contents available for a bounded workspace
// materialization. The service binds idempotent retries to both identifiers.
func (c *Client) HoldRead(ctx context.Context, id, commitID string) (project.ReadLease, error) {
	var result project.ReadLease
	err := c.postJSON(ctx, "/v1/read/hold", struct {
		ID       string `json:"id"`
		CommitID string `json:"commitID"`
	}{ID: id, CommitID: commitID}, &result)
	return result, err
}

// ReleaseRead ends a bounded workspace materialization hold. Repeated release
// requests are safe.
func (c *Client) ReleaseRead(ctx context.Context, id string) error {
	return c.postJSON(ctx, "/v1/read/release", struct {
		ID string `json:"id"`
	}{ID: id}, nil)
}

// Apply streams changed file payloads in sorted manifest order. An exact
// retry of a stable non-capturing operation sends only its manifest; the
// server still compares the sealed request before returning that result.
func (c *Client) Apply(ctx context.Context, request project.ChangeRequest, openFile func(model.Path) (io.ReadCloser, error)) (project.Commit, error) {
	request.Changes = append([]project.Change(nil), request.Changes...)
	sort.Slice(request.Changes, func(i, j int) bool { return request.Changes[i].Path < request.Changes[j].Path })
	includeFiles := false
	if len(request.Changes) != 0 {
		op, err := c.Operation(ctx, request.OperationID)
		switch {
		case err == nil:
			includeFiles = op.State == project.OperationCapturing
		case errors.Is(err, project.ErrOperationNotFound):
			includeFiles = true
		default:
			return project.Commit{}, err
		}
	} else {
		// Still query operation so malformed retries cannot use a local cached
		// outcome to avoid the server's fingerprint comparison.
		if _, err := c.Operation(ctx, request.OperationID); err != nil && !errors.Is(err, project.ErrOperationNotFound) {
			return project.Commit{}, err
		}
	}
	fileCount := 0
	for _, change := range request.Changes {
		if change.Kind == model.File {
			fileCount++
		}
	}
	if includeFiles && fileCount != 0 && openFile == nil {
		return project.Commit{}, errors.New("transport: file opener is required for file changes")
	}
	manifest, err := json.Marshal(request)
	if err != nil {
		return project.Commit{}, err
	}
	var signature fileSignature
	if includeFiles && hasDeltaCandidate(request) {
		operation, supported, err := c.prepareTransfer(ctx, request, manifest)
		if err != nil {
			return project.Commit{}, err
		}
		if supported {
			includeFiles = operation.State == project.OperationCapturing
			signature = func(ctx context.Context, change project.Change) (project.FileSignature, error) {
				return c.fileSignature(ctx, request, change)
			}
		}
	}

	var body io.ReadCloser
	var contentType string
	var writerDone <-chan error
	var cancelWriter func()
	if includeFiles && fileCount != 0 {
		body, contentType, writerDone, cancelWriter = streamMultipartFiles(ctx, manifest, request.Changes, openFile, signature)
	} else {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		part, err := writer.CreateFormField("manifest")
		if err != nil {
			return project.Commit{}, err
		}
		if _, err := part.Write(manifest); err != nil {
			return project.Commit{}, err
		}
		if err := writer.Close(); err != nil {
			return project.Commit{}, err
		}
		body = io.NopCloser(bytes.NewReader(buffer.Bytes()))
		contentType = writer.FormDataContentType()
	}
	response, requestErr := c.do(ctx, http.MethodPost, "/v1/changes", nil, body, contentType)
	var writerErr error
	if cancelWriter != nil {
		cancelWriter()
		writerErr = <-writerDone
	}
	if response != nil {
		defer response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			var result project.Commit
			if err := decodeResponse(response, &result); err != nil {
				return project.Commit{}, err
			}
			// A successful response is authoritative: the server may have
			// already accepted an idempotent operation and stopped reading this
			// retry's upload. The writer error is then just the expected pipe
			// cancellation after the response arrived.
			return result, nil
		}
	}
	if response == nil {
		if requestErr != nil || writerErr != nil {
			return project.Commit{}, errors.Join(requestErr, writerErr)
		}
		return project.Commit{}, errors.New("transport: missing HTTP response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// A server may reject a manifest before consuming payload bytes. The
		// upload writer is stopped above; retain the useful server error instead
		// of replacing it with the expected broken-pipe error.
		return project.Commit{}, decodeWireError(response)
	}
	return project.Commit{}, errors.Join(requestErr, writerErr, errors.New("transport: unreachable response state"))
}

func streamMultipart(ctx context.Context, manifest []byte, changes []project.Change, openFile func(model.Path) (io.ReadCloser, error)) (io.ReadCloser, string, <-chan error, func()) {
	return streamMultipartFiles(ctx, manifest, changes, openFile, nil)
}

type fileSignature func(context.Context, project.Change) (project.FileSignature, error)

func streamMultipartFiles(ctx context.Context, manifest []byte, changes []project.Change, openFile func(model.Path) (io.ReadCloser, error), signature fileSignature) (io.ReadCloser, string, <-chan error, func()) {
	ctx, cancelContext := context.WithCancel(ctx)
	reader, writerPipe := io.Pipe()
	writer := multipart.NewWriter(writerPipe)
	done := make(chan error, 1)
	state := &streamState{reader: reader, writer: writerPipe}
	go func() {
		defer cancelContext()
		fail := func(err error) { _ = writerPipe.CloseWithError(err); done <- err }
		manifestPart, err := writer.CreateFormField("manifest")
		if err != nil {
			fail(err)
			return
		}
		if _, err := manifestPart.Write(manifest); err != nil {
			fail(err)
			return
		}
		fileIndex := 0
		for _, change := range changes {
			if change.Kind != model.File {
				continue
			}
			path, err := model.ParsePath(change.Path)
			if err != nil {
				fail(err)
				return
			}
			var basis project.FileSignature
			if signature != nil && change.Size >= project.MinDeltaFileSize {
				basis, err = signature(ctx, change)
				if err != nil {
					fail(err)
					return
				}
			}
			if err := ctx.Err(); err != nil {
				fail(err)
				return
			}
			file, err := openFile(path)
			if err != nil {
				if file != nil {
					if closeErr := file.Close(); closeErr != nil {
						err = errors.Join(err, closeErr)
					}
				}
				fail(err)
				return
			}
			if file == nil {
				fail(errors.New("transport: file opener returned nil reader"))
				return
			}
			if err := state.setActive(file); err != nil {
				fail(err)
				return
			}
			mediaType := "application/octet-stream"
			if len(basis.Data) != 0 {
				mediaType = DeltaMediaType
			}
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", `form-data; name="file:`+strconv.Itoa(fileIndex)+`"; filename="data"`)
			header.Set("Content-Type", mediaType)
			part, err := writer.CreatePart(header)
			if err != nil {
				state.closeActive()
				fail(err)
				return
			}
			var copyErr error
			if len(basis.Data) != 0 {
				copyErr = transfer.Delta(ctx, basis.Data, basis.Basis.Size, change.Size, file, part)
			} else {
				var copied int64
				copied, copyErr = io.CopyN(part, &contextReader{ctx: ctx, reader: file}, change.Size+1)
				if errors.Is(copyErr, io.EOF) && copied == change.Size {
					copyErr = nil
				} else if copyErr == nil {
					copyErr = fmt.Errorf("transport: file %q size differs from manifest", change.Path)
				}
			}
			if err := errors.Join(copyErr, state.closeActive()); err != nil {
				fail(err)
				return
			}
			fileIndex++
		}
		if err := writer.Close(); err != nil {
			fail(err)
			return
		}
		_ = writerPipe.Close()
		done <- nil
	}()
	return reader, writer.FormDataContentType(), done, func() {
		// The body producer can be waiting on a signature request as well as a
		// source read. Stop both before the caller waits for writer completion.
		cancelContext()
		state.cancel()
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type streamState struct {
	mu       sync.Mutex
	reader   *io.PipeReader
	writer   *io.PipeWriter
	active   io.ReadCloser
	canceled bool
}

func (s *streamState) setActive(reader io.ReadCloser) error {
	s.mu.Lock()
	if s.canceled {
		s.mu.Unlock()
		return errors.Join(context.Canceled, reader.Close())
	}
	s.active = reader
	s.mu.Unlock()
	return nil
}
func (s *streamState) closeActive() error {
	s.mu.Lock()
	active := s.active
	s.active = nil
	s.mu.Unlock()
	if active != nil {
		return active.Close()
	}
	return nil
}
func (s *streamState) cancel() {
	s.mu.Lock()
	s.canceled = true
	s.mu.Unlock()
	_ = s.reader.CloseWithError(io.ErrClosedPipe)
	_ = s.writer.CloseWithError(io.ErrClosedPipe)
	_ = s.closeActive()
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, dst any) error {
	response, err := c.do(ctx, http.MethodGet, path, query, nil, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeWireError(response)
	}
	return decodeResponse(response, dst)
}

func (c *Client) postJSON(ctx context.Context, path string, value any, dst any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	response, err := c.do(ctx, http.MethodPost, path, nil, io.NopCloser(bytes.NewReader(data)), "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeWireError(response)
	}
	if dst == nil {
		return nil
	}
	return decodeResponse(response, dst)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.ReadCloser, contentType string) (*http.Response, error) {
	u := *c.base
	u.Path = path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, err
	}
	return response, nil
}

func decodeResponse(response *http.Response, dst any) error {
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBody+1))
	if err != nil || len(data) > maxJSONBody {
		return fmt.Errorf("read remote response: %w", ErrRemote)
	}
	if err := strictJSON(data, dst); err != nil {
		return fmt.Errorf("decode remote response: %w", ErrRemote)
	}
	return nil
}

func strictJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func decodeWireError(response *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBody+1))
	if err == nil && len(data) <= maxJSONBody {
		var wire wireError
		if strictJSON(data, &wire) == nil {
			if sentinel := errorSentinel(wire.Code); sentinel != nil {
				return sentinel
			}
		}
	}
	return fmt.Errorf("HTTP %d: %w", response.StatusCode, ErrRemote)
}

func errorSentinel(code string) error {
	switch code {
	case "invalid_request":
		return project.ErrInvalidRequest
	case "invalid_upload":
		return project.ErrInvalidRequest
	case "commit_not_found":
		return project.ErrCommitNotFound
	case "payload_expired":
		return project.ErrPayloadExpired
	case "read_lease_conflict":
		return project.ErrReadLeaseConflict
	case "tag_not_found":
		return project.ErrTagNotFound
	case "tag_exists":
		return project.ErrTagExists
	case "branch_not_found":
		return project.ErrBranchNotFound
	case "branch_exists":
		return project.ErrBranchExists
	case "operation_not_found":
		return project.ErrOperationNotFound
	case "path_not_found":
		return kopiaengine.ErrPathNotFound
	case "path_not_file":
		return kopiaengine.ErrPathNotFile
	case "tree_conflict":
		return model.ErrTreeConflict
	case "stale_head":
		return project.ErrStaleHead
	case "operation_conflict":
		return project.ErrOperationConflict
	case "merge_conflict":
		return project.ErrMergeConflict
	case "unrelated_history":
		return project.ErrUnrelatedHistory
	case "ambiguous_merge_base":
		return project.ErrAmbiguousMergeBase
	case "history_limit":
		return project.ErrHistoryLimit
	case "invalid_choice":
		return model.ErrInvalidChoice
	case "operation_aborted":
		return project.ErrOperationAborted
	case "operation_expired":
		return project.ErrOperationExpired
	case "already_accepted":
		return project.ErrAlreadyAccepted
	case "upload_required":
		return project.ErrUploadRequired
	case "repository_mismatch":
		return project.ErrRepositoryMismatch
	case "capacity_unknown":
		return project.ErrCapacityUnknown
	case "insufficient_capacity":
		return project.ErrInsufficientCapacity
	case "project_busy":
		return project.ErrProjectBusy
	case "service_closed":
		return project.ErrServiceClosed
	default:
		return nil
	}
}
