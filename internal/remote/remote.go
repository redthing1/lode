// Package remote carries project operations over a small authenticated HTTP API.
package remote

import (
	"bytes"
	"context"
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

type Identity struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Instance string `json:"instance"`
}

func WhoAmI(ctx context.Context, baseURL, token string) (Identity, error) {
	if token == "" {
		return Identity{}, errors.New("empty bearer token")
	}
	canonical, err := CanonicalURL(baseURL)
	if err != nil {
		return Identity{}, err
	}
	u, _ := url.Parse(canonical)
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Path = "/api/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Identity{}, errors.New("authentication failed; check the access token")
	}
	if resp.StatusCode == http.StatusForbidden {
		return Identity{}, errors.New("permission denied")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Identity{}, fmt.Errorf("identity check returned HTTP %d", resp.StatusCode)
	}
	var identity Identity
	if err := decodeMetadata(resp.Body, &identity); err != nil {
		return Identity{}, err
	}
	if identity.ID == "" || identity.Username == "" || identity.Instance == "" {
		return Identity{}, errors.New("identity response is incomplete")
	}
	return identity, nil
}

type wireError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Paths   []string `json:"paths,omitempty"`
	Bases   []string `json:"bases,omitempty"`
}

type Client struct {
	base      *url.URL
	token     string
	projectID string
	http      *http.Client
}

func ValidateURL(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("invalid remote base URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && localHost(u.Hostname())) {
		return errors.New("remote HTTP requires a loopback host")
	}
	if u.Host == "" {
		return errors.New("invalid remote base URL")
	}
	if u.RawPath != "" {
		return errors.New("remote URL path must be canonical")
	}
	if u.Path != "" && u.Path != "/" && !validProjectPath(u.Path) {
		return errors.New("remote URL path must be /USERNAME/PROJECT")
	}
	return nil
}

func validProjectPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.Count(path[1:], "/") != 1 {
		return false
	}
	owner, slug, ok := strings.Cut(path[1:], "/")
	if !ok || len(owner) < 3 || len(owner) > 32 || len(slug) < 1 || len(slug) > 64 || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return false
	}
	for _, c := range owner {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	for _, c := range slug {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func HasProjectPath(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && validProjectPath(u.Path)
}

// CanonicalURL validates and normalizes the instance or project URL.
func CanonicalURL(baseURL string) (string, error) {
	if err := ValidateURL(baseURL); err != nil {
		return "", err
	}
	u, _ := url.Parse(baseURL)
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.RawPath = ""
	if u.Path == "/" {
		u.Path = ""
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func New(baseURL, token, projectID string) (*Client, error) {
	if projectID != "" && !project.ValidID(projectID) {
		return nil, errors.New("invalid remote project identity")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("empty bearer token")
	}
	if err := ValidateURL(baseURL); err != nil {
		return nil, err
	}
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
	u.Path = strings.TrimSuffix(c.base.Path, "/") + "/v1/" + decodedPath
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
	// Authentication fails before the service reveals its project identity.
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("authentication failed; check the access token with lode login or LODE_TOKEN")
	}
	if resp.StatusCode == http.StatusForbidden {
		return errors.New("permission denied; your account lacks access to this action")
	}
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
	case "conflicts":
		return &model.Conflicts{Paths: append([]string(nil), w.Paths...)}
	case "merge_base":
		return &project.MergeBaseConflict{Bases: append([]string(nil), w.Bases...)}
	default:
		return &w
	}
}

// Preserve the service's explanation once while retaining typed error checks.
func (w *wireError) Error() string { return w.Message }

func (w *wireError) Unwrap() error {
	switch w.Code {
	case "not_found":
		return project.ErrNotFound
	case "exists":
		return project.ErrExists
	case "stale":
		return project.ErrStale
	case "cancelled":
		return project.ErrCancelled
	case "operation_conflict":
		return project.ErrOperationConflict
	case "already_accepted":
		return project.ErrAlreadyAccepted
	case "expired":
		return project.ErrExpired
	case "cleanup_changed":
		return project.ErrCleanupChanged
	case "reference_changed":
		return project.ErrReferenceChanged
	case "invalid":
		return project.ErrInvalid
	case "unchanged":
		return project.ErrAlreadyMerged
	default:
		return nil
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
