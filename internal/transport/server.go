package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	kopiaengine "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

const maxJSONBody = 8 << 20
const fileWriteIdleTimeout = 2 * time.Minute

type ProjectInfo struct {
	ID string `json:"id"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type server struct {
	service *project.Service
	token   string
}

var errInvalidUpload = errors.New("invalid multipart upload")

// NewHandler builds the authenticated v1 HTTP API.
func NewHandler(service *project.Service, token string) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("transport: project service is required")
	}
	if token == "" {
		return nil, errors.New("transport: bearer token is required")
	}
	s := &server{service: service, token: token}
	return http.HandlerFunc(s.serveHTTP), nil
}

func (s *server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !authorized(r, s.token) {
		writeError(w, http.StatusUnauthorized, wireError{Code: "unauthorized", Message: "unauthorized"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, http.StatusNotFound, wireError{Code: "not_found", Message: "not found"})
		return
	}
	switch r.URL.Path {
	case "/v1/info":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		if len(r.URL.Query()) != 0 {
			invalidRequest(w)
			return
		}
		writeJSON(w, ProjectInfo{ID: s.service.ID()})
	case "/v1/head":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		q, ok := readQuery(w, r, "branch")
		if !ok {
			return
		}
		head, err := s.service.Head(r.Context(), q.Get("branch"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, struct {
			Head string `json:"head"`
		}{head})
	case "/v1/branch":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		q, ok := readQuery(w, r, "name")
		if !ok {
			return
		}
		branch, err := s.service.Branch(r.Context(), q.Get("name"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, branchResponse{Name: branch.Name, Tip: branch.Tip})
	case "/v1/branches":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		q, ok := readQuery(w, r, "after", "limit")
		if !ok {
			return
		}
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			invalidRequest(w)
			return
		}
		branches, next, err := s.service.Branches(r.Context(), q.Get("after"), limit)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		result := branchesResponse{Branches: make([]branchResponse, 0, len(branches)), Next: next}
		for _, branch := range branches {
			result.Branches = append(result.Branches, branchResponse{Name: branch.Name, Tip: branch.Tip})
		}
		writeJSON(w, result)
	case "/v1/branch/create":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			Name         string `json:"name"`
			FromCommitID string `json:"fromCommitID"`
		}
		if !decodeJSONBody(w, r, &request) {
			return
		}
		if err := s.service.CreateBranch(r.Context(), request.Name, request.FromCommitID); err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, struct{}{})
	case "/v1/branch/delete":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			Name        string `json:"name"`
			ExpectedTip string `json:"expectedTip"`
		}
		if !decodeJSONBody(w, r, &request) {
			return
		}
		if err := s.service.DeleteBranch(r.Context(), request.Name, request.ExpectedTip); err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, struct{}{})
	case "/v1/commit":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		q, ok := readQuery(w, r, "id")
		if !ok {
			return
		}
		commit, err := s.service.Commit(r.Context(), q.Get("id"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, commit)
	case "/v1/operation":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		q, ok := readQuery(w, r, "id")
		if !ok {
			return
		}
		operation, err := s.service.Operation(r.Context(), q.Get("id"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, operation)
	case "/v1/resume":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			OperationID string `json:"operationID"`
		}
		if !decodeJSONBody(w, r, &request) {
			return
		}
		commit, err := s.service.Resume(r.Context(), request.OperationID)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, commit)
	case "/v1/abort":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			OperationID string `json:"operationID"`
		}
		if !decodeJSONBody(w, r, &request) {
			return
		}
		if err := s.service.Abort(r.Context(), request.OperationID); err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, struct{}{})
	case "/v1/entries":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request entriesRequest
		if !decodeJSONBody(w, r, &request) {
			return
		}
		nodes, next, err := s.service.Entries(r.Context(), request.Commit, request.Rules, request.After, request.Limit)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, entriesResponse{Nodes: nodes, Next: next})
	case "/v1/history":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		q, ok := readQuery(w, r, "start", "limit")
		if !ok {
			return
		}
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			invalidRequest(w)
			return
		}
		commits, next, err := s.service.History(r.Context(), q.Get("start"), limit)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, historyResponse{Commits: commits, Next: next})
	case "/v1/merge/preview":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			Spec  project.MergeSpec `json:"spec"`
			After string            `json:"after"`
			Limit int               `json:"limit"`
		}
		if !decodeJSONBody(w, r, &request) {
			return
		}
		preview, err := s.service.PreviewMerge(r.Context(), request.Spec, request.After, request.Limit)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, preview)
	case "/v1/merge":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request project.MergeRequest
		if !decodeJSONBody(w, r, &request) {
			return
		}
		commit, err := s.service.Merge(r.Context(), request)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, commit)
	case "/v1/file":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.serveFile(w, r)
	case "/v1/read/hold":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			ID       string `json:"id"`
			CommitID string `json:"commitID"`
		}
		if !decodeJSONBody(w, r, &request) {
			return
		}
		lease, err := s.service.HoldRead(r.Context(), request.ID, request.CommitID)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, lease)
	case "/v1/read/release":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			ID string `json:"id"`
		}
		if !decodeJSONBody(w, r, &request) {
			return
		}
		if err := s.service.ReleaseRead(r.Context(), request.ID); err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, struct{}{})
	case "/v1/changes":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.serveChanges(w, r)
	case ChangesPreparePath:
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.servePrepareChanges(w, r)
	case ChangesSignaturePath:
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.serveChangeSignature(w, r)
	default:
		if s.collectionRoutes(w, r) {
			return
		}
		if s.retentionRoutes(w, r) {
			return
		}
		writeError(w, http.StatusNotFound, wireError{Code: "not_found", Message: "not found"})
	}
}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request) {
	q, ok := readQuery(w, r, "commit", "path")
	if !ok {
		return
	}
	file, node, err := s.service.OpenFile(r.Context(), q.Get("commit"), q.Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer file.Close()
	controller := http.NewResponseController(w)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	if err := setFileWriteDeadline(controller); err != nil {
		writeServiceError(w, err)
		return
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(node.Size, 10))
	w.Header().Set("X-Lode-Node", base64.RawURLEncoding.EncodeToString(encoded))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(fileDeadlineWriter{writer: w, controller: controller}, file)
}

type fileDeadlineWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w fileDeadlineWriter) Write(p []byte) (int, error) {
	if err := setFileWriteDeadline(w.controller); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

func setFileWriteDeadline(controller *http.ResponseController) error {
	if err := controller.SetWriteDeadline(time.Now().Add(fileWriteIdleTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (s *server) serveChanges(w http.ResponseWriter, r *http.Request) {
	// This endpoint may reject a manifest before consuming large file parts.
	// Full duplex lets the response reach the client before net/http tries to
	// drain the remainder of the upload; the client can then stop its pipe and
	// close the active source reader.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		writeError(w, http.StatusInternalServerError, wireError{Code: "internal_error", Message: "internal server error"})
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		invalidRequest(w)
		return
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	manifest, err := reader.NextPart()
	if err != nil || manifest.FormName() != "manifest" || manifest.FileName() != "" {
		invalidRequest(w)
		return
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(manifest, maxJSONBody+1))
	if err != nil || len(manifestBytes) > maxJSONBody {
		invalidRequest(w)
		return
	}
	if err := manifest.Close(); err != nil {
		invalidRequest(w)
		return
	}
	var request project.ChangeRequest
	if !decodeJSONBytes(w, manifestBytes, &request) {
		return
	}
	changes := append([]project.Change(nil), request.Changes...)
	// Project.Apply canonicalizes paths before requesting file readers. The
	// transport uses the same sorted order to bind each payload part.
	sortChanges(changes)
	filePaths := make([]model.Path, 0, len(changes))
	for _, change := range changes {
		if change.Kind != model.File {
			continue
		}
		path, err := model.ParsePath(change.Path)
		if err != nil {
			invalidRequest(w)
			return
		}
		filePaths = append(filePaths, path)
	}
	if len(filePaths) == 0 {
		if extra, err := reader.NextPart(); err != io.EOF || extra != nil {
			invalidRequest(w)
			return
		}
	}
	opened := 0
	openFile := func(path model.Path, openBasis project.BasisOpener) (io.ReadCloser, error) {
		if opened >= len(filePaths) || filePaths[opened] != path {
			return nil, fmt.Errorf("%w: unexpected service file request", errInvalidUpload)
		}
		part, err := reader.NextPart()
		if err != nil {
			return nil, fmt.Errorf("%w: missing upload part", errInvalidUpload)
		}
		wantName := "file:" + strconv.Itoa(opened)
		if part.FormName() != wantName || part.FileName() != "data" {
			return nil, fmt.Errorf("%w: unexpected upload part", errInvalidUpload)
		}
		_, digest, size := changesFileFacts(request.Changes, path.String())
		opened++
		last := opened == len(filePaths)
		mediaType, err := uploadMediaType(part)
		if err != nil {
			return nil, err
		}
		switch mediaType {
		case "", "application/octet-stream":
			return &uploadPart{Part: part, size: size, sha256: digest, hasher: sha256.New(), last: last, nextPart: reader.NextPart}, nil
		case DeltaMediaType:
			basis, basisNode, err := openBasis()
			if err != nil {
				return nil, errors.Join(errInvalidUpload, err)
			}
			if basisNode.Kind != model.File {
				return nil, fmt.Errorf("%w: delta basis is not a file", errInvalidUpload)
			}
			encoded := &deltaUploadPart{Part: part, last: last, nextPart: reader.NextPart}
			decoded, err := newDeltaUploadPart(r.Context(), basis, basisNode.Size, size, digest, encoded)
			if err != nil {
				return nil, errors.Join(errInvalidUpload, err)
			}
			return decoded, nil
		default:
			return nil, fmt.Errorf("%w: unsupported file encoding %q", errInvalidUpload, mediaType)
		}
	}
	commit, err := s.service.ApplyWithBasis(r.Context(), request, openFile)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, commit)
}

type uploadPart struct {
	*multipart.Part
	size     int64
	read     int64
	sha256   string
	hasher   hash.Hash
	last     bool
	nextPart func() (*multipart.Part, error)
	closed   bool
}

func (p *uploadPart) Read(dst []byte) (int, error) {
	remaining := p.size + 1 - p.read
	if remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(dst)) > remaining {
		dst = dst[:remaining]
	}
	n, err := p.Part.Read(dst)
	p.read += int64(n)
	if n > 0 {
		_, _ = p.hasher.Write(dst[:n])
	}
	return n, err
}

func (p *uploadPart) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	if p.read != p.size {
		return fmt.Errorf("%w: uploaded length is %d, expected %d", errInvalidUpload, p.read, p.size)
	}
	if hex.EncodeToString(p.hasher.Sum(nil)) != p.sha256 {
		return fmt.Errorf("%w: uploaded SHA256 differs from manifest", errInvalidUpload)
	}
	if closeErr := p.Part.Close(); closeErr != nil {
		return closeErr
	}
	if p.last {
		extra, err := p.nextPart()
		if extra != nil {
			return fmt.Errorf("%w: unexpected trailing multipart part", errInvalidUpload)
		}
		if err != io.EOF {
			return fmt.Errorf("%w: check multipart trailer", errInvalidUpload)
		}
	}
	return nil
}

type entriesRequest struct {
	Commit string                  `json:"commit"`
	Rules  []project.SelectionRule `json:"rules"`
	After  string                  `json:"after"`
	Limit  int                     `json:"limit"`
}
type entriesResponse struct {
	Nodes []project.Node `json:"nodes"`
	Next  string         `json:"next"`
}
type historyResponse struct {
	Commits []project.Commit `json:"commits"`
	Next    string           `json:"next"`
}
type branchResponse struct {
	Name string `json:"name"`
	Tip  string `json:"tip"`
}
type branchesResponse struct {
	Branches []branchResponse `json:"branches"`
	Next     string           `json:"next"`
}

func authorized(r *http.Request, token string) bool {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(fields[1]), []byte(token)) == 1
}

func readQuery(w http.ResponseWriter, r *http.Request, names ...string) (mapValues, bool) {
	q := r.URL.Query()
	if len(q) != len(names) {
		invalidRequest(w)
		return nil, false
	}
	for _, name := range names {
		values, ok := q[name]
		if !ok || len(values) != 1 {
			invalidRequest(w)
			return nil, false
		}
	}
	return mapValues(q), true
}

type mapValues map[string][]string

func (m mapValues) Get(key string) string {
	if len(m[key]) == 0 {
		return ""
	}
	return m[key][0]
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil || len(data) > maxJSONBody {
		invalidRequest(w)
		return false
	}
	return decodeJSONBytes(w, data, dst)
}

func decodeJSONBytes(w http.ResponseWriter, data []byte, dst any) bool {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		invalidRequest(w)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		invalidRequest(w)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxJSONBody {
		writeError(w, http.StatusInternalServerError, wireError{Code: "internal_error", Message: "internal server error"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeServiceError(w http.ResponseWriter, err error) {
	status, wire := mapError(err)
	writeError(w, status, wire)
}

func mapError(err error) (int, wireError) {
	switch {
	case errors.Is(err, project.ErrInvalidRequest):
		return http.StatusBadRequest, wireError{"invalid_request", "invalid request"}
	case errors.Is(err, errInvalidUpload):
		return http.StatusBadRequest, wireError{"invalid_upload", "invalid multipart upload"}
	case errors.Is(err, project.ErrCommitNotFound):
		return http.StatusNotFound, wireError{"commit_not_found", "commit not found"}
	case errors.Is(err, project.ErrPayloadExpired):
		return http.StatusGone, wireError{"payload_expired", "version contents have expired"}
	case errors.Is(err, project.ErrReadLeaseConflict):
		return http.StatusConflict, wireError{"read_lease_conflict", "read hold ID is bound to different inputs"}
	case errors.Is(err, project.ErrTagNotFound):
		return http.StatusNotFound, wireError{"tag_not_found", "tag not found"}
	case errors.Is(err, project.ErrTagExists):
		return http.StatusConflict, wireError{"tag_exists", "tag already protects another version"}
	case errors.Is(err, project.ErrBranchNotFound):
		return http.StatusNotFound, wireError{"branch_not_found", "branch not found"}
	case errors.Is(err, project.ErrBranchExists):
		return http.StatusConflict, wireError{"branch_exists", "branch already exists at another tip"}
	case errors.Is(err, project.ErrOperationNotFound):
		return http.StatusNotFound, wireError{"operation_not_found", "operation not found"}
	case errors.Is(err, kopiaengine.ErrPathNotFound):
		return http.StatusNotFound, wireError{"path_not_found", "path not found"}
	case errors.Is(err, kopiaengine.ErrPathNotFile):
		return http.StatusBadRequest, wireError{"path_not_file", "path is not a file"}
	case errors.Is(err, model.ErrTreeConflict):
		return http.StatusConflict, wireError{"tree_conflict", "changes conflict with the project tree"}
	case errors.Is(err, project.ErrStaleHead):
		return http.StatusConflict, wireError{"stale_head", "branch base is stale"}
	case errors.Is(err, project.ErrOperationConflict):
		return http.StatusConflict, wireError{"operation_conflict", "operation ID is bound to different inputs"}
	case errors.Is(err, project.ErrMergeConflict):
		return http.StatusConflict, wireError{"merge_conflict", "merge has unresolved conflicts"}
	case errors.Is(err, project.ErrUnrelatedHistory):
		return http.StatusConflict, wireError{"unrelated_history", "histories have no common ancestor"}
	case errors.Is(err, project.ErrAmbiguousMergeBase):
		return http.StatusConflict, wireError{"ambiguous_merge_base", "histories have multiple best common ancestors"}
	case errors.Is(err, project.ErrHistoryLimit):
		return http.StatusRequestEntityTooLarge, wireError{"history_limit", "merge history exceeds supported limit"}
	case errors.Is(err, model.ErrInvalidChoice):
		return http.StatusBadRequest, wireError{"invalid_choice", "invalid merge choice"}
	case errors.Is(err, project.ErrOperationAborted):
		return http.StatusConflict, wireError{"operation_aborted", "operation was aborted"}
	case errors.Is(err, project.ErrOperationExpired):
		return http.StatusGone, wireError{"operation_expired", "operation expired"}
	case errors.Is(err, project.ErrAlreadyAccepted):
		return http.StatusConflict, wireError{"already_accepted", "operation was already accepted"}
	case errors.Is(err, project.ErrUploadRequired):
		return http.StatusConflict, wireError{"upload_required", "operation requires file upload"}
	case errors.Is(err, project.ErrRepositoryMismatch):
		return http.StatusConflict, wireError{"repository_mismatch", "repository mismatch"}
	case errors.Is(err, project.ErrProjectBusy):
		return http.StatusConflict, wireError{"project_busy", "project is busy"}
	case errors.Is(err, project.ErrServiceClosed):
		return http.StatusServiceUnavailable, wireError{"service_closed", "project service is closed"}
	case errors.Is(err, project.ErrCapacityUnknown):
		return http.StatusServiceUnavailable, wireError{"capacity_unknown", "storage capacity is unavailable"}
	case errors.Is(err, project.ErrInsufficientCapacity):
		return http.StatusInsufficientStorage, wireError{"insufficient_capacity", "insufficient storage capacity"}
	default:
		return http.StatusInternalServerError, wireError{"internal_error", "internal server error"}
	}
}

func writeError(w http.ResponseWriter, status int, response wireError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func invalidRequest(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, wireError{Code: "invalid_request", Message: "invalid request"})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, wireError{Code: "method_not_allowed", Message: "method not allowed"})
}

func sortChanges(changes []project.Change) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
}

func changesFileFacts(changes []project.Change, path string) (string, string, int64) {
	for _, change := range changes {
		if change.Path == path {
			return change.Path, change.SHA256, change.Size
		}
	}
	return "", "", -1
}
