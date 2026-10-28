//go:build linux

package remote

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transfer"
)

// Handler returns the versioned HTTP boundary for one project.
func Handler(p *project.Project) http.Handler { return &handler{p: p} }

type handler struct {
	p         *project.Project
	authorize actorAuthorizer
}

type Access string

const (
	Read   Access = "read"
	Write  Access = "write"
	Manage Access = "manage"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
)

type actorAuthorizer func(*http.Request, Access) (project.Actor, error)

// AuthenticatedHandler delegates identity and current authorization to the instance layer.
// It does not check or fall back to the project token.
func AuthenticatedHandler(p *project.Project, authorize func(*http.Request, Access) (project.Actor, error)) http.Handler {
	if authorize == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unauthorized", http.StatusUnauthorized) })
	}
	return &handler{p: p, authorize: authorize}
}

func requiredAccess(path, method string) (Access, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	if len(parts) == 1 {
		switch parts[0] {
		case "info", "head", "history":
			if method == http.MethodGet {
				return Read, true
			}
		case "entries":
			if method == http.MethodPost {
				return Read, true
			}
		case "branches", "tags":
			if method == http.MethodGet {
				return Read, true
			}
			if method == http.MethodPost || method == http.MethodDelete {
				return Write, true
			}
		case "read":
			if method == http.MethodGet || method == http.MethodPost {
				return Read, true
			}
		case "signature":
			if method == http.MethodGet {
				return Read, true
			}
		case "save":
			if method == http.MethodPost {
				return Write, true
			}
		case "collect":
			if method == http.MethodPost {
				return Manage, true
			}
		}
	}
	if len(parts) == 2 {
		switch parts[0] {
		case "cancel":
			if method == http.MethodPost {
				return Write, true
			}
		case "cleanup":
			if method == http.MethodPost && (parts[1] == "preview" || parts[1] == "apply") {
				return Manage, true
			}
		}
	}
	return "", false
}

func codeOf(err error) string {
	var conflicts *model.Conflicts
	var mergeBases *project.MergeBaseConflict
	switch {
	case errors.As(err, &conflicts):
		return "conflicts"
	case errors.As(err, &mergeBases):
		return "merge_base"
	case errors.Is(err, project.ErrAlreadyMerged):
		return "unchanged"
	case errors.Is(err, project.ErrExpired):
		return "expired"
	case errors.Is(err, project.ErrCleanupChanged):
		return "cleanup_changed"
	case errors.Is(err, project.ErrReferenceChanged):
		return "reference_changed"
	}
	switch {
	case errors.Is(err, project.ErrNotFound):
		return "not_found"
	case errors.Is(err, project.ErrExists):
		return "exists"
	case errors.Is(err, project.ErrStale):
		return "stale"
	case errors.Is(err, project.ErrOperationOwner):
		return "operation_conflict"
	case errors.Is(err, project.ErrCancelled):
		return "cancelled"
	case errors.Is(err, project.ErrOperationConflict):
		return "operation_conflict"
	case errors.Is(err, project.ErrAlreadyAccepted):
		return "already_accepted"
	case errors.Is(err, project.ErrInvalid):
		return "invalid"
	default:
		return "internal"
	}
}
func (h *handler) fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch codeOf(err) {
	case "not_found":
		status = http.StatusNotFound
	case "exists":
		status = http.StatusConflict
	case "stale":
		status = http.StatusConflict
	case "cancelled":
		status = http.StatusGone
	case "operation_conflict", "already_accepted":
		status = http.StatusConflict
	case "invalid":
		status = http.StatusBadRequest
	case "expired":
		status = http.StatusGone
	case "cleanup_changed", "reference_changed":
		status = http.StatusConflict
	case "conflicts", "merge_base", "unchanged":
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	we := wireError{Code: codeOf(err), Message: err.Error()}
	var conflicts *model.Conflicts
	var mergeBases *project.MergeBaseConflict
	if errors.As(err, &conflicts) {
		we.Paths = append([]string(nil), conflicts.Paths...)
	}
	if errors.As(err, &mergeBases) {
		we.Bases = append([]string(nil), mergeBases.Bases...)
	}
	_ = json.NewEncoder(w).Encode(we)
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions || r.URL.Path == "/v1" || !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	var actor project.Actor
	if h.authorize != nil {
		access, ok := requiredAccess(r.URL.Path, r.Method)
		if !ok {
			http.NotFound(w, r)
			return
		}
		var err error
		actor, err = h.authorize(r, access)
		if err != nil || actor.ID == "" || actor.Name == "" {
			status := http.StatusForbidden
			if errors.Is(err, ErrUnauthenticated) {
				status = http.StatusUnauthorized
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
	} else {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(h.p.Token())) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	requestedProject := r.Header.Get(projectHeader)
	if requestedProject != "" && requestedProject != h.p.ID() {
		http.Error(w, "project identity mismatch", http.StatusMisdirectedRequest)
		return
	}
	if requestedProject == "" && !(r.Method == http.MethodGet && r.URL.Path == "/v1/info") {
		http.Error(w, "project identity is required", http.StatusBadRequest)
		return
	}
	w.Header().Set(projectHeader, h.p.ID())
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")
	ctx := r.Context()
	if len(parts) == 1 && parts[0] == "info" && r.Method == http.MethodGet {
		h.json(w, Info{ID: h.p.ID()})
		return
	}
	if len(parts) == 1 && parts[0] == "head" && r.Method == http.MethodGet {
		id, err := h.p.Head(ctx, r.URL.Query().Get("branch"))
		if err != nil {
			h.fail(w, err)
			return
		}
		h.json(w, id)
		return
	}
	if len(parts) == 1 && parts[0] == "branches" {
		switch r.Method {
		case http.MethodGet:
			items, err := h.p.Branches(ctx)
			if err != nil {
				h.fail(w, err)
				return
			}
			writeList(w, items)
			return
		case http.MethodPost:
			var req project.Branch
			if !h.decode(w, r, &req) {
				return
			}
			if err := h.p.CreateBranch(ctx, req.Name, req.Tip); err != nil {
				h.fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodDelete:
			if !h.decodeEmpty(w, r) {
				return
			}
			if err := h.p.DeleteBranch(ctx, r.URL.Query().Get("name"), r.URL.Query().Get("expected")); err != nil {
				h.fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if len(parts) == 1 && parts[0] == "tags" {
		switch r.Method {
		case http.MethodGet:
			items, err := h.p.Tags(ctx)
			if err != nil {
				h.fail(w, err)
				return
			}
			writeList(w, items)
			return
		case http.MethodPost:
			var req project.Tag
			if !h.decode(w, r, &req) {
				return
			}
			if err := h.p.CreateTag(ctx, req.Name, req.Commit); err != nil {
				h.fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodDelete:
			var req struct {
				Name     string `json:"name"`
				Expected string `json:"expected"`
			}
			if !h.decode(w, r, &req) {
				return
			}
			if err := h.p.DeleteTag(ctx, req.Name, req.Expected); err != nil {
				h.fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if len(parts) == 2 && parts[0] == "cleanup" {
		switch parts[1] {
		case "preview":
			if r.Method != http.MethodPost {
				break
			}
			var req struct {
				Keep int `json:"keep"`
			}
			if !h.decode(w, r, &req) {
				return
			}
			plan, err := h.p.PreviewCleanup(ctx, req.Keep)
			if err != nil {
				h.fail(w, err)
				return
			}
			w.Header().Set(cleanupTokenHeader, plan.Token)
			writeList(w, plan.Versions)
			return
		case "apply":
			if r.Method != http.MethodPost {
				break
			}
			var req struct {
				Keep  int    `json:"keep"`
				Token string `json:"token"`
			}
			if !h.decode(w, r, &req) {
				return
			}
			n, err := h.p.ApplyCleanup(ctx, req.Keep, req.Token)
			if err != nil {
				h.fail(w, err)
				return
			}
			h.json(w, n)
			return
		}
	}
	if len(parts) == 1 && parts[0] == "collect" && r.Method == http.MethodPost {
		if !h.decodeEmpty(w, r) {
			return
		}
		result, err := h.p.Collect(ctx)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.json(w, result)
		return
	}
	if len(parts) == 1 && parts[0] == "entries" && r.Method == http.MethodPost {
		var req struct {
			ID        string          `json:"id"`
			Selection model.Selection `json:"selection"`
		}
		if !h.decode(w, r, &req) {
			return
		}
		entries, err := h.p.Entries(ctx, req.ID, req.Selection)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeList(w, entries)
		return
	}
	if len(parts) == 1 && parts[0] == "history" && r.Method == http.MethodGet {
		var limit int
		_, _ = fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		items, err := h.p.History(ctx, r.URL.Query().Get("branch"), limit)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeList(w, items)
		return
	}
	if len(parts) == 2 && parts[0] == "cancel" && r.Method == http.MethodPost {
		if !h.decodeEmpty(w, r) {
			return
		}
		var err error
		if actor.ID != "" {
			err = h.p.CancelAs(ctx, actor, parts[1])
		} else {
			err = h.p.Cancel(ctx, parts[1])
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 1 && parts[0] == "read" && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		h.read(w, r)
		return
	}
	if len(parts) == 1 && parts[0] == "signature" && r.Method == http.MethodGet {
		h.signature(w, r)
		return
	}
	if len(parts) == 1 && parts[0] == "save" && r.Method == http.MethodPost {
		h.save(w, r, actor)
		return
	}
	http.NotFound(w, r)
}
func (h *handler) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (h *handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxMetadata)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		h.fail(w, project.ErrInvalid)
		return false
	}
	var x any
	if d.Decode(&x) != io.EOF {
		h.fail(w, project.ErrInvalid)
		return false
	}
	return true
}
func (h *handler) decodeEmpty(w http.ResponseWriter, r *http.Request) bool {
	var v struct{}
	return h.decode(w, r, &v)
}
func (h *handler) save(w http.ResponseWriter, r *http.Request, actor project.Actor) {
	mr, err := r.MultipartReader()
	if err != nil {
		h.fail(w, project.ErrInvalid)
		return
	}
	first, err := mr.NextPart()
	if err != nil || first.FormName() != "metadata" {
		panic(http.ErrAbortHandler)
	}
	metadata, err := io.ReadAll(io.LimitReader(first, maxMetadata+1))
	if err != nil || len(metadata) > maxMetadata {
		panic(http.ErrAbortHandler)
	}
	var req project.Request
	dec := json.NewDecoder(bytes.NewReader(metadata))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil {
		h.fail(w, project.ErrInvalid)
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		h.fail(w, project.ErrInvalid)
		return
	}
	_ = first.Close()
	source := project.Source{Write: func(entry model.Entry, basis project.Basis, out io.Writer) error {
		p, err := mr.NextPart()
		if err != nil {
			return fmt.Errorf("%w: missing file part", project.ErrInvalid)
		}
		name, err := url.PathUnescape(p.Header.Get("Lode-Path"))
		if err != nil || p.FormName() != "file" || name != entry.Path {
			panic(http.ErrAbortHandler)
		}
		switch p.Header.Get("Lode-Encoding") {
		case "":
		case "rsync":
			previous, rd, err := basis()
			if err != nil {
				return err
			}
			defer rd.Close()
			limited := &io.LimitedReader{R: p, N: deltaLimit(entry.Size)}
			if err := transfer.Patch(r.Context(), rd, previous.Size, limited, entry.Size, out); err != nil {
				return err
			}
			if limited.N == 0 {
				return fmt.Errorf("%w: delta exceeds encoded size limit", project.ErrInvalid)
			}
			return nil
		default:
			return project.ErrInvalid
		}
		// Do not use Part.Close: a failed capture must not drain an unbounded body.
		_, err = io.Copy(out, io.LimitReader(p, entry.Size+1))
		return err
	}, Finish: func() error {
		_, err := mr.NextPart()
		if err == nil {
			panic(http.ErrAbortHandler)
		}
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: malformed multipart body", project.ErrInvalid)
		}
		return nil
	}}
	var c project.Commit
	if actor.ID != "" {
		c, err = h.p.SaveAs(r.Context(), actor, req, source)
	} else {
		c, err = h.p.Save(r.Context(), req, source)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	h.json(w, c)
}

// Client carries an immutable expected project identity. An empty identity is
// useful only for the initial Info request; construct a bound client from its result.

// read holds the normal project read protection for either representation.
// POST supplies a bounded client signature; GET requests the complete file.
func (h *handler) read(w http.ResponseWriter, r *http.Request) {
	var sig []byte
	if r.Method == http.MethodPost {
		var err error
		sig, err = io.ReadAll(http.MaxBytesReader(w, r.Body, transfer.MaxSignatureSize))
		if err != nil || len(sig) == 0 {
			h.fail(w, project.ErrInvalid)
			return
		}
	}
	out := &downloadResponse{ResponseWriter: w}
	err := h.p.Read(r.Context(), r.URL.Query().Get("commit"), r.URL.Query().Get("path"), func(e model.Entry, rd io.Reader) error {
		w.Header().Set("Content-Type", "application/octet-stream")
		if sig != nil {
			w.Header().Set("Lode-Encoding", "rsync")
			if err := transfer.Delta(r.Context(), bytes.NewReader(sig), rd, out); err != nil {
				return fmt.Errorf("%w: delta download: %v", project.ErrInvalid, err)
			}
			return nil
		}
		w.Header().Set("Content-Length", fmt.Sprint(e.Size))
		_, err := io.Copy(out, rd)
		return err
	})
	if err != nil {
		if out.started {
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Lode-Encoding")
		w.Header().Del("Content-Length")
		h.fail(w, err)
	}
}

type downloadResponse struct {
	http.ResponseWriter
	started bool
}

func (w *downloadResponse) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}

func (h *handler) signature(w http.ResponseWriter, r *http.Request) {
	var sig bytes.Buffer
	err := h.p.Read(r.Context(), r.URL.Query().Get("commit"), r.URL.Query().Get("path"), func(e model.Entry, rd io.Reader) error {
		if e.Size < minDeltaSize || e.Size > transfer.MaxBasisSize {
			return nil
		}
		return transfer.Signature(r.Context(), rd, e.Size, &sig)
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	if sig.Len() == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(sig.Len()))
	_, _ = w.Write(sig.Bytes())
}
