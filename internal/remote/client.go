package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/progress"
	"github.com/redthing1/lode/internal/transfer"
)

type Client struct {
	origin     string
	token      string
	InstanceID string
	ProjectID  string
	HTTP       *http.Client
}

var ErrCredentialsRequired = errors.New("access token required")

func (c *Client) Origin() string { return c.origin }

func New(origin, token string) (*Client, error) {
	return NewWithHTTP(origin, token, false)
}

// NewWithHTTP permits non-loopback HTTP only with explicit caller consent.
func NewWithHTTP(origin, token string, allowHTTP bool) (*Client, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("use the server origin, such as https://lode.example")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !(allowHTTP || strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()) {
			return nil, errors.New("use HTTPS; for a trusted private network, run lode auth login --allow-http <server-url>")
		}
	}
	u.Path = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &Client{origin: u.String(), token: token, HTTP: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) request(ctx context.Context, method, resource string, body io.Reader, contentType string) (*http.Response, error) {
	if c.token == "" {
		return nil, ErrCredentialsRequired
	}
	r, err := http.NewRequestWithContext(ctx, method, c.origin+resource, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Lode-Instance", c.InstanceID)
	r.Header.Set("Lode-Project", c.ProjectID)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	response, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	var failure Error
	if err := json.Unmarshal(b, &failure); err != nil || failure.Message == "" {
		failure = Error{Code: "http", Message: strings.TrimSpace(string(b))}
	}
	if failure.Message == "" {
		failure.Message = response.Status
	}
	failure.Status = response.StatusCode
	return nil, &failure
}

func decodeJSON(r io.Reader, value any) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxMetadata+1))
	if err != nil {
		return err
	}
	if len(b) > MaxMetadata {
		return errors.New("server metadata exceeds limit")
	}
	return json.Unmarshal(b, value)
}

func (c *Client) json(ctx context.Context, method, resource string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		if len(b) > MaxMetadata {
			return errors.New("request metadata exceeds limit")
		}
		body = bytes.NewReader(b)
	}
	r, err := c.request(ctx, method, resource, body, "application/json")
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return decodeJSON(r.Body, output)
}

func projectPath(owner, name string) string {
	return "/api/projects/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}
func versionPath(owner, name, version string) string {
	return projectPath(owner, name) + "/versions/" + url.PathEscape(version)
}

func (c *Client) Me(ctx context.Context) (Identity, error) {
	var identity Identity
	err := c.json(ctx, "GET", "/api/me", nil, &identity)
	return identity, err
}
func (c *Client) Project(ctx context.Context, owner, name string) (Project, error) {
	var p Project
	err := c.json(ctx, "GET", projectPath(owner, name), nil, &p)
	return p, err
}
func (c *Client) CreateProject(ctx context.Context, name string) (Project, error) {
	var p Project
	err := c.json(ctx, "POST", "/api/projects", map[string]string{"name": name}, &p)
	return p, err
}

func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var projects []Project
	err := c.json(ctx, "GET", "/api/projects", nil, &projects)
	return projects, err
}
func (c *Client) Head(ctx context.Context, owner, name, branch string) (string, error) {
	var h Head
	err := c.json(ctx, "GET", projectPath(owner, name)+"/head?branch="+url.QueryEscape(branch), nil, &h)
	return h.Version, err
}
func (c *Client) History(ctx context.Context, owner, name, branch string, limit int) (model.History, error) {
	var versions model.History
	err := c.json(ctx, "GET", projectPath(owner, name)+"/history?branch="+url.QueryEscape(branch)+fmt.Sprintf("&limit=%d", limit), nil, &versions)
	return versions, err
}

func (c *Client) Inventory(ctx context.Context, owner, name, version string, selection model.Selection) ([]model.Entry, error) {
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	if version == "" {
		return nil, nil
	}
	b, err := json.Marshal(selection)
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, errors.New("selection exceeds metadata limit")
	}
	progress.Report(ctx, progress.Event{Phase: "Loading file list"})
	r, err := c.request(ctx, "POST", versionPath(owner, name, version)+"/inventory", bytes.NewReader(b), "application/json")
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var entries []model.Entry
	seen := map[string]bool{}
	complete := false
	for scanner.Scan() {
		if complete {
			return nil, errors.New("unexpected metadata after inventory count")
		}
		var record InventoryRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		if record.Count != nil && record.Entry == nil {
			if *record.Count != len(entries) {
				return nil, errors.New("inventory count differs")
			}
			complete = true
			continue
		}
		if record.Entry == nil || record.Count != nil {
			return nil, errors.New("invalid inventory record")
		}
		e := *record.Entry
		if err := e.Validate(); err != nil {
			return nil, err
		}
		if e.Kind == model.Absent || !selection.Includes(e.Path) || seen[e.Path] {
			return nil, errors.New("unexpected inventory entry")
		}
		seen[e.Path] = true
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !complete {
		return nil, errors.New("incomplete inventory")
	}
	if err := model.ValidateInventory(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// Download verifies an exact file before returning. An eligible local basis
// reduces transfer bytes; the caller owns it and must discard output on error.
func (c *Client) Download(ctx context.Context, owner, name, version string, e model.Entry, basis io.ReadSeeker, basisSize int64, out io.Writer) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Kind != model.File || e.Size == math.MaxInt64 {
		return errors.New("invalid file download")
	}
	method, encoding := "GET", ""
	var signature bytes.Buffer
	useDelta := basis != nil && e.Size >= 1<<20 && basisSize >= 1<<20 && basisSize <= transfer.MaxBasisSize
	if useDelta {
		if _, err := basis.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := transfer.Signature(ctx, progress.Reader(ctx, "Preparing download", e.Path, basisSize, basis), basisSize, &signature); err != nil {
			return err
		}
		method, encoding = "POST", "application/octet-stream"
	}
	var body io.Reader
	if useDelta {
		body = &signature
	}
	progress.Report(ctx, progress.Event{Phase: "Downloading", Path: e.Path, Total: e.Size})
	r, err := c.request(ctx, method, versionPath(owner, name, version)+"/file?path="+url.QueryEscape(e.Path), body, encoding)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if !useDelta {
		return copyFile(progress.Writer(ctx, "Downloading", e.Path, e.Size, out), io.LimitReader(r.Body, e.Size+1), e)
	}
	if r.Header.Get("Content-Type") != DeltaType {
		return errors.New("unexpected download encoding")
	}
	limit := int64(math.MaxInt64)
	if e.Size <= (math.MaxInt64-(64<<10))/2 {
		limit = 2*e.Size + (64 << 10)
	}
	delta := &io.LimitedReader{R: r.Body, N: limit}
	h := sha256.New()
	if err := transfer.Patch(ctx, basis, basisSize, delta, e.Size, io.MultiWriter(progress.Writer(ctx, "Reconstructing", e.Path, e.Size, out), h)); err != nil {
		return err
	}
	if delta.N == 0 {
		return errors.New("download delta exceeds size limit")
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("download identity mismatch for %s", e.Path)
	}
	return nil
}

// DownloadBatch streams files in request order. The receiver must call write
// once; it verifies that file before the receiver can publish its staged copy.
func (c *Client) DownloadBatch(ctx context.Context, owner, name, version string, files []model.Entry, receive func(int, func(io.Writer) error) error) error {
	if err := ValidateDownloadBatch(files); err != nil {
		return err
	}
	b, err := json.Marshal(files)
	if err != nil {
		return err
	}
	progress.Report(ctx, progress.Event{Phase: "Downloading"})
	r, err := c.request(ctx, "POST", versionPath(owner, name, version)+"/files", bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	defer r.Body.Close()
	for i, e := range files {
		verified := false
		err := receive(i, func(out io.Writer) error {
			if err := copyFile(progress.Writer(ctx, "Downloading", e.Path, e.Size, out), io.LimitReader(r.Body, e.Size), e); err != nil {
				return err
			}
			verified = true
			return nil
		})
		if err != nil {
			return err
		}
		if !verified {
			return errors.New("batch receiver did not verify the file")
		}
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, 1))
	if err != nil {
		return err
	}
	if n != 0 {
		return errors.New("unexpected data after download batch")
	}
	return nil
}

func copyFile(out io.Writer, in io.Reader, e model.Entry) error {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		return err
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return errors.New("downloaded file identity differs")
	}
	return nil
}
