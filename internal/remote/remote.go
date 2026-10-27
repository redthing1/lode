// Package remote carries project operations over a small authenticated HTTP API.
package remote

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transfer"
)

const maxMetadata = 16 << 20
const projectHeader = "Lode-Project-ID"

type Info struct {
	ID string `json:"id"`
}

// Handler returns the versioned HTTP boundary for one project.
func Handler(p *project.Project) http.Handler { return &handler{p: p} }

type handler struct{ p *project.Project }

type wireError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Paths   []string `json:"paths,omitempty"`
	Bases   []string `json:"bases,omitempty"`
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
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	auth := strings.TrimPrefix(header, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(auth), []byte(h.p.Token())) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set(projectHeader, h.p.ID())
	requestedProject := r.Header.Get(projectHeader)
	if requestedProject != "" && requestedProject != h.p.ID() {
		http.Error(w, "project identity mismatch", http.StatusMisdirectedRequest)
		return
	}
	if requestedProject == "" && !(r.Method == http.MethodGet && r.URL.Path == "/v1/info") {
		http.Error(w, "project identity is required", http.StatusBadRequest)
		return
	}
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
		err := h.p.Cancel(ctx, parts[1])
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
		h.save(w, r)
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
func (h *handler) save(w http.ResponseWriter, r *http.Request) {
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
	c, err := h.p.Save(r.Context(), req, source)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.json(w, c)
}

// Client carries an immutable expected project identity. An empty identity is
// useful only for the initial Info request; construct a bound client from its result.
type Client struct {
	base      *url.URL
	token     string
	projectID string
	http      *http.Client
}

func New(baseURL, token, projectID string) (*Client, error) {
	if projectID != "" && !project.ValidID(projectID) {
		return nil, errors.New("invalid remote project identity")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("invalid remote base URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && localHost(u.Hostname())) {
		return nil, errors.New("remote HTTP requires a loopback host")
	}
	if u.Host == "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("remote base URL must not contain a path")
	}
	if token == "" {
		return nil, errors.New("empty bearer token")
	}
	u.Path = ""
	return &Client{base: u, token: token, projectID: projectID, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func localHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (c *Client) request(ctx context.Context, method, p string, body io.Reader, contentType string) (*http.Response, error) {
	u := *c.base
	pathPart, q, _ := strings.Cut(p, "?")
	decodedPath, decodeErr := url.PathUnescape(strings.TrimPrefix(pathPart, "/"))
	if decodeErr != nil {
		return nil, decodeErr
	}
	u.Path = "/v1/" + decodedPath
	u.RawQuery = q
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.projectID != "" {
		req.Header.Set(projectHeader, c.projectID)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.http.Do(req)
}
func (c *Client) verify(resp *http.Response) error {
	if c.projectID != "" && resp.Header.Get(projectHeader) != c.projectID {
		return errors.New("remote project identity mismatch")
	}
	if c.projectID == "" && resp.Header.Get(projectHeader) == "" {
		return errors.New("remote omitted project identity")
	}
	return nil
}
func (c *Client) result(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if err := c.verify(resp); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseError(resp)
	}
	if out == nil {
		return nil
	}
	return decodeMetadata(resp.Body, out)
}
func decodeMetadata(r io.Reader, out any) error {
	limited := &io.LimitedReader{R: r, N: maxMetadata + 1}
	d := json.NewDecoder(limited)
	d.DisallowUnknownFields()
	err := d.Decode(out)
	if err == nil {
		var extra any
		if trailing := d.Decode(&extra); trailing != io.EOF {
			err = errors.New("invalid trailing metadata")
		}
	}
	if limited.N == 0 {
		return errors.New("remote metadata exceeds 16 MiB")
	}
	return err
}
func parseError(resp *http.Response) error {
	var w wireError
	if decodeMetadata(resp.Body, &w) != nil {
		return fmt.Errorf("remote HTTP %d", resp.StatusCode)
	}
	switch w.Code {
	case "not_found":
		return fmt.Errorf("%s: %w", w.Message, project.ErrNotFound)
	case "exists":
		return fmt.Errorf("%s: %w", w.Message, project.ErrExists)
	case "stale":
		return fmt.Errorf("%s: %w", w.Message, project.ErrStale)
	case "cancelled":
		return fmt.Errorf("%s: %w", w.Message, project.ErrCancelled)
	case "operation_conflict":
		return fmt.Errorf("%s: %w", w.Message, project.ErrOperationConflict)
	case "already_accepted":
		return fmt.Errorf("%s: %w", w.Message, project.ErrAlreadyAccepted)
	case "expired":
		return fmt.Errorf("%s: %w", w.Message, project.ErrExpired)
	case "cleanup_changed":
		return fmt.Errorf("%s: %w", w.Message, project.ErrCleanupChanged)
	case "reference_changed":
		return fmt.Errorf("%s: %w", w.Message, project.ErrReferenceChanged)
	case "invalid":
		return fmt.Errorf("%s: %w", w.Message, project.ErrInvalid)
	case "conflicts":
		return &model.Conflicts{Paths: append([]string(nil), w.Paths...)}
	case "merge_base":
		return &project.MergeBaseConflict{Bases: append([]string(nil), w.Bases...)}
	case "unchanged":
		return fmt.Errorf("%s: %w", w.Message, project.ErrAlreadyMerged)
	default:
		return errors.New(w.Message)
	}
}
func (c *Client) Info(ctx context.Context) (Info, error) {
	resp, err := c.request(ctx, "GET", "info", nil, "")
	if err != nil {
		return Info{}, err
	}
	var x Info
	headerID := resp.Header.Get(projectHeader)
	if err := c.result(resp, &x); err != nil {
		return x, err
	}
	if headerID == "" || x.ID != headerID {
		return Info{}, errors.New("remote project identity mismatch")
	}
	return x, nil
}
func (c *Client) Branches(ctx context.Context) ([]project.Branch, error) {
	resp, err := c.request(ctx, "GET", "branches", nil, "")
	if err != nil {
		return nil, err
	}
	return collectList[project.Branch](c, resp)
}
func (c *Client) CreateBranch(ctx context.Context, name, tip string) error {
	body, _ := json.Marshal(project.Branch{Name: name, Tip: tip})
	resp, err := c.request(ctx, "POST", "branches", bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	return c.result(resp, nil)
}
func (c *Client) DeleteBranch(ctx context.Context, name, expected string) error {
	query := url.Values{"name": {name}, "expected": {expected}}
	resp, err := c.request(ctx, "DELETE", "branches?"+query.Encode(), bytes.NewReader([]byte("{}")), "application/json")
	if err != nil {
		return err
	}
	return c.result(resp, nil)
}
func (c *Client) Tags(ctx context.Context) ([]project.Tag, error) {
	resp, err := c.request(ctx, http.MethodGet, "tags", nil, "")
	if err != nil {
		return nil, err
	}
	return collectList[project.Tag](c, resp)
}
func (c *Client) CreateTag(ctx context.Context, name, commit string) error {
	body, _ := json.Marshal(project.Tag{Name: name, Commit: commit})
	resp, err := c.request(ctx, http.MethodPost, "tags", bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	return c.result(resp, nil)
}
func (c *Client) DeleteTag(ctx context.Context, name, expected string) error {
	body, _ := json.Marshal(struct {
		Name     string `json:"name"`
		Expected string `json:"expected"`
	}{name, expected})
	resp, err := c.request(ctx, http.MethodDelete, "tags", bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	return c.result(resp, nil)
}

// PreviewCleanup visits every decision and returns an apply token only after
// the complete response has been validated. A failed preview changes no state.
func (c *Client) PreviewCleanup(ctx context.Context, keep int, visit func(project.RetentionVersion) error) (string, error) {
	body, _ := json.Marshal(struct {
		Keep int `json:"keep"`
	}{keep})
	resp, err := c.request(ctx, http.MethodPost, "cleanup/preview", bytes.NewReader(body), "application/json")
	if err != nil {
		return "", err
	}
	if err := readList(c, resp, visit); err != nil {
		return "", fmt.Errorf("cleanup preview incomplete: %w", err)
	}
	token := resp.Header.Get(cleanupTokenHeader)
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != token {
		return "", errors.New("invalid cleanup preview token")
	}
	return token, nil
}
func (c *Client) ApplyCleanup(ctx context.Context, keep int, token string) (int, error) {
	body, _ := json.Marshal(struct {
		Keep  int    `json:"keep"`
		Token string `json:"token"`
	}{keep, token})
	resp, err := c.request(ctx, http.MethodPost, "cleanup/apply", bytes.NewReader(body), "application/json")
	if err != nil {
		return 0, err
	}
	var out int
	err = c.result(resp, &out)
	return out, err
}
func (c *Client) Collect(ctx context.Context) (content.Collection, error) {
	resp, err := c.request(ctx, http.MethodPost, "collect", bytes.NewReader([]byte("{}")), "application/json")
	if err != nil {
		return content.Collection{}, err
	}
	var out content.Collection
	err = c.result(resp, &out)
	return out, err
}
func (c *Client) Head(ctx context.Context, branch string) (string, error) {
	resp, e := c.request(ctx, "GET", "head?branch="+url.QueryEscape(branch), nil, "")
	if e != nil {
		return "", e
	}
	var x string
	e = c.result(resp, &x)
	return x, e
}
func (c *Client) Entries(ctx context.Context, id string, s model.Selection) ([]model.Entry, error) {
	b, _ := json.Marshal(struct {
		ID        string          `json:"id"`
		Selection model.Selection `json:"selection"`
	}{id, s})
	resp, e := c.request(ctx, "POST", "entries", bytes.NewReader(b), "application/json")
	if e != nil {
		return nil, e
	}
	return collectList[model.Entry](c, resp)
}
func (c *Client) History(ctx context.Context, branch string, limit int) ([]project.Commit, error) {
	q := "history?branch=" + url.QueryEscape(branch) + "&limit=" + fmt.Sprint(limit)
	resp, e := c.request(ctx, "GET", q, nil, "")
	if e != nil {
		return nil, e
	}
	return collectList[project.Commit](c, resp)
}
func (c *Client) Cancel(ctx context.Context, id string) error {
	resp, e := c.request(ctx, "POST", "cancel/"+id, bytes.NewReader([]byte("{}")), "application/json")
	if e != nil {
		return e
	}
	return c.result(resp, nil)
}
func (c *Client) Read(ctx context.Context, id, name string) (io.ReadCloser, error) {
	query := url.Values{"commit": {id}, "path": {name}}
	resp, e := c.request(ctx, "GET", "read?"+query.Encode(), nil, "")
	if e != nil {
		return nil, e
	}
	if err := c.verify(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := parseError(resp)
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}
func (c *Client) Save(ctx context.Context, req project.Request, open func(string) (io.ReadCloser, error)) (project.Commit, error) {
	signatures, err := c.saveSignatures(ctx, req)
	if err != nil {
		return project.Commit{}, err
	}
	defer signatures.close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	done := make(chan error, 1)
	go func() {
		err := func() error {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", `form-data; name="metadata"`)
			h.Set("Content-Type", "application/json")
			part, e := mw.CreatePart(h)
			if e != nil {
				return e
			}
			if e = json.NewEncoder(part).Encode(req); e != nil {
				return e
			}
			changes := append([]model.Entry(nil), req.Changes...)
			model.Sort(changes)
			for _, entry := range changes {
				if entry.Kind != model.File {
					continue
				}
				if open == nil {
					return errors.New("save source has no open function")
				}
				r, e := open(entry.Path)
				if e != nil {
					return e
				}
				fh := make(textproto.MIMEHeader)
				fh.Set("Content-Disposition", `form-data; name="file"; filename="payload"`)
				fh.Set("Lode-Path", url.PathEscape(entry.Path))
				fh.Set("Content-Type", "application/octet-stream")
				sig := signatures.reader(entry.Path)
				if sig != nil {
					fh.Set("Lode-Encoding", "rsync")
				}
				fp, e := mw.CreatePart(fh)
				if e == nil {
					if sig != nil {
						e = transfer.Delta(ctx, sig, io.LimitReader(r, entry.Size+1), fp)
					} else {
						_, e = io.Copy(fp, r)
					}
				}
				e = errors.Join(e, r.Close())
				if e != nil {
					return e
				}
			}
			return nil
		}()
		if err == nil {
			err = mw.Close()
		}
		if err == nil {
			err = pw.Close()
		} else {
			_ = pw.CloseWithError(err)
		}
		done <- err
	}()
	resp, err := c.request(ctx, "POST", "save", pr, mw.FormDataContentType())
	if err != nil {
		_ = pr.CloseWithError(err)
		<-done
		return project.Commit{}, err
	}
	_ = pr.CloseWithError(io.ErrClosedPipe)
	defer resp.Body.Close()
	if e := c.verify(resp); e != nil {
		<-done
		return project.Commit{}, e
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		remoteErr := parseError(resp)
		<-done
		return project.Commit{}, remoteErr
	}
	var out project.Commit
	e := decodeMetadata(resp.Body, &out)
	<-done
	if e != nil {
		return out, e
	}
	if out.ID != req.ID {
		return project.Commit{}, errors.New("save reply does not match the requested operation")
	}
	return out, nil
}
