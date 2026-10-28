//go:build linux

package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const maxTextPreview = 1 << 20

type previewKind string

const (
	previewMarkdown previewKind = "markdown"
	previewText     previewKind = "text"
	previewImage    previewKind = "image"
	previewAudio    previewKind = "audio"
	previewVideo    previewKind = "video"
	previewDownload previewKind = "download"
)

// filePreview describes how a whole opaque project entry may be presented.
// It deliberately classifies from the name and bounded content prefix only.
type filePreview struct {
	Kind      previewKind
	MediaType string
	SizeLimit int64
}

func classifyPreview(name string, prefix []byte) filePreview {
	ext := strings.ToLower(path.Ext(name))
	if isMarkdownName(name) {
		return filePreview{Kind: previewMarkdown, MediaType: "text/html; charset=utf-8", SizeLimit: maxTextPreview}
	}
	media := map[string]struct {
		kind     previewKind
		typeName string
	}{
		".png": {previewImage, "image/png"}, ".jpg": {previewImage, "image/jpeg"}, ".jpeg": {previewImage, "image/jpeg"},
		".gif": {previewImage, "image/gif"}, ".webp": {previewImage, "image/webp"},
		".mp3": {previewAudio, "audio/mpeg"}, ".ogg": {previewAudio, "audio/ogg"}, ".wav": {previewAudio, "audio/wav"},
		".m4a": {previewAudio, "audio/mp4"}, ".mp4": {previewVideo, "video/mp4"}, ".webm": {previewVideo, "video/webm"},
	}
	if m, ok := media[ext]; ok {
		return filePreview{Kind: m.kind, MediaType: m.typeName}
	}
	// HTML and SVG are displayed only as escaped text; raw media serving never
	// includes either type. Text previews require valid UTF-8 with no NUL bytes.
	if textExtension(ext) || textFilename(path.Base(name)) {
		if bytes.Contains(prefix, []byte{0}) || !utf8.Valid(prefix) {
			return filePreview{Kind: previewDownload, MediaType: "application/octet-stream"}
		}
		return filePreview{Kind: previewText, MediaType: "text/plain; charset=utf-8", SizeLimit: maxTextPreview}
	}
	return filePreview{Kind: previewDownload, MediaType: "application/octet-stream"}
}

func isMarkdownName(name string) bool {
	base := strings.ToLower(path.Base(name))
	if base == "readme" {
		return true
	}
	switch strings.ToLower(path.Ext(base)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return true
	default:
		return false
	}
}

func textExtension(ext string) bool {
	switch ext {
	case ".txt", ".go", ".c", ".h", ".cc", ".cpp", ".hpp", ".rs", ".py", ".js", ".ts", ".jsx", ".tsx", ".css", ".json", ".yaml", ".yml", ".toml", ".ini", ".sh", ".bash", ".zsh", ".md", ".markdown", ".csv", ".xml", ".obj", ".log", ".conf", ".html", ".htm", ".svg":
		return true
	default:
		return false
	}
}

func textFilename(name string) bool {
	switch strings.ToLower(name) {
	case "makefile", "dockerfile", "gemfile", "license", "licence", "notice":
		return true
	default:
		return false
	}
}

// readPreviewBytes reads at most maxTextPreview+1 bytes while the project
// service holds its retained-content read gate. The extra byte detects an
// oversized preview without allocating in proportion to the file size.
func readPreviewBytes(read func(func(model.Entry, io.Reader) error) error) (model.Entry, []byte, error) {
	var entry model.Entry
	var data []byte
	err := read(func(e model.Entry, src io.Reader) error {
		entry = e
		var readErr error
		data, readErr = io.ReadAll(io.LimitReader(src, maxTextPreview+1))
		return readErr
	})
	return entry, data, err
}

type projectFileView struct {
	projectView
	Name   string
	Entry  model.Entry
	Kind   previewKind
	Body   template.HTML
	Notice string
}

func (s *Server) projectFile(w http.ResponseWriter, r *http.Request) {
	u, info, ok := s.access(w, r, instance.RoleReader)
	if !ok {
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer release()
	id, name := r.URL.Query().Get("version"), r.URL.Query().Get("path")
	if id == "" || !model.ValidPath(name) {
		s.fail(w, r, 404, "This file is unavailable.")
		return
	}
	commit, err := p.Version(r.Context(), id)
	if err != nil {
		s.fail(w, r, 404, "This version is unavailable.")
		return
	}
	if _, _, err := p.Directory(r.Context(), id, name, "", 1); err == nil {
		query := url.Values{"version": {id}, "path": {name}}
		if branch := r.URL.Query().Get("branch"); branch != "" {
			query.Set("branch", branch)
		}
		http.Redirect(w, r, "/p/"+info.Slug+"?"+query.Encode(), http.StatusSeeOther)
		return
	}
	entry, data, err := readPreviewBytes(func(consume func(model.Entry, io.Reader) error) error { return p.Read(r.Context(), id, name, consume) })
	if err != nil {
		s.previewReadError(w, r, err)
		return
	}
	presentation := classifyPreview(name, data)
	notice := ""
	if presentation.SizeLimit > 0 && int64(len(data)) > presentation.SizeLimit {
		presentation.Kind = previewDownload
		notice = "This file is too large to preview."
	}
	if presentation.Kind == previewDownload && notice == "" {
		notice = "This file type is available as a download."
	}
	body := template.HTML("")
	if presentation.Kind == previewMarkdown {
		rendered, err := renderMarkdown(data, info.Slug, id, name)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		body = template.HTML(rendered)
	} else if presentation.Kind == previewText {
		body = template.HTML("<pre><code>" + template.HTMLEscapeString(string(data)) + "</code></pre>")
	}
	v := s.projectPage(info, "files")
	v.Commit = commit
	v.Path = path.Dir(name)
	if v.Path == "." {
		v.Path = ""
	}
	if branch := r.URL.Query().Get("branch"); branch != "" {
		v.Branch = branch
	}
	v.CanWrite = s.i.Authorize(r.Context(), principal(u), info.ID, instance.RoleWriter) == nil
	v.CanManage = s.i.Authorize(r.Context(), principal(u), info.ID, instance.RoleMaintainer) == nil
	dataView := projectFileView{projectView: v, Name: name, Entry: entry, Kind: presentation.Kind, Body: body, Notice: notice}
	s.render(w, r, "project-file", page{Title: path.Base(name), Section: "projects", User: u, Data: dataView}, 200)
}

func (s *Server) rawFile(w http.ResponseWriter, r *http.Request) {
	_, info, ok := s.access(w, r, instance.RoleReader)
	if !ok {
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer release()
	id, name := r.URL.Query().Get("version"), r.URL.Query().Get("path")
	if id == "" || !model.ValidPath(name) {
		s.fail(w, r, 404, "This media file is unavailable.")
		return
	}
	var started bool
	err = p.Read(r.Context(), id, name, func(e model.Entry, rd io.Reader) error {
		presentation := classifyPreview(name, nil)
		if presentation.Kind != previewImage && presentation.Kind != previewAudio && presentation.Kind != previewVideo {
			return project.ErrNotFound
		}
		start, length, status, rangeErr := parseSingleRange(r.Header.Get("Range"), e.Size)
		if rangeErr != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", e.Size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			started = true
			return nil
		}
		if start > 0 {
			if _, err := io.CopyN(io.Discard, rd, start); err != nil {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", e.Size))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				started = true
				return nil
			}
		}
		w.Header().Set("Content-Type", presentation.MediaType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": path.Base(name)}))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if status == http.StatusPartialContent {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, e.Size))
		}
		w.WriteHeader(status)
		started = true
		_, err := io.CopyN(w, rd, length)
		return err
	})
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		s.previewReadError(w, r, err)
	}
}

func (s *Server) previewReadError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, project.ErrExpired) {
		s.fail(w, r, 410, "This version's contents have been released. Its history and file listing are still available.")
		return
	}
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrInvalid) {
		s.fail(w, r, 404, "This file is unavailable.")
		return
	}
	s.internal(w, r, err)
}

func parseSingleRange(header string, size int64) (start, length int64, status int, err error) {
	if header == "" {
		return 0, size, http.StatusOK, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, 0, errors.New("invalid range")
	}
	parts := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
	if len(parts) != 2 || size <= 0 {
		return 0, 0, 0, errors.New("invalid range")
	}
	if parts[0] == "" {
		suffix, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || suffix <= 0 {
			return 0, 0, 0, errors.New("invalid suffix range")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, http.StatusPartialContent, nil
	}
	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, 0, errors.New("invalid start range")
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, 0, errors.New("invalid end range")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, http.StatusPartialContent, nil
}

func renderMarkdown(source []byte, slug, version, current string) (string, error) {
	engine := goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithParserOptions(parser.WithAutoHeadingID()))
	doc := engine.Parser().Parse(text.NewReader(source))
	base := path.Dir(current)
	if base == "." {
		base = ""
	}
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var dest *[]byte
		switch node := n.(type) {
		case *ast.Link:
			dest = &node.Destination
		case *ast.Image:
			dest = &node.Destination
		}
		if dest == nil {
			return ast.WalkContinue, nil
		}
		raw := string(*dest)
		parsed, err := url.Parse(raw)
		if err != nil {
			*dest = nil
			return ast.WalkContinue, nil
		}
		if parsed.IsAbs() || strings.HasPrefix(raw, "//") {
			if _, isImage := n.(*ast.Image); isImage {
				*dest = []byte("data:,")
			}
			return ast.WalkContinue, nil
		}
		if strings.HasPrefix(raw, "#") {
			return ast.WalkContinue, nil
		}
		resolved := path.Clean(path.Join(base, parsed.Path))
		if resolved == ".." || strings.HasPrefix(resolved, "../") || strings.HasPrefix(parsed.Path, "/") {
			*dest = nil
			return ast.WalkContinue, nil
		}
		if resolved == "." {
			resolved = ""
		}
		endpoint := "/p/" + url.PathEscape(slug) + "/file"
		if _, image := n.(*ast.Image); image {
			endpoint = "/p/" + url.PathEscape(slug) + "/raw"
		} else if strings.HasSuffix(parsed.Path, "/") {
			endpoint = "/p/" + url.PathEscape(slug)
		}
		q := url.Values{"version": {version}, "path": {resolved}}
		*dest = []byte(endpoint + "?" + q.Encode())
		return ast.WalkContinue, nil
	})
	var out bytes.Buffer
	if err := engine.Renderer().Render(&out, source, doc); err != nil {
		return "", err
	}
	return out.String(), nil
}

// readDirectoryReadme uses an explicit set of possible root README names, so it
// remains bounded even when a directory has more entries than one listing page.
func readDirectoryReadme(ctx context.Context, p *project.Project, slug, version, directory string) (string, model.Entry, template.HTML, error) {
	names := []string{"README.md", "readme.md", "Readme.md", "README.markdown", "readme.markdown", "README.mdown", "README.mkd", "README", "readme"}
	for _, name := range names {
		candidate := path.Join(directory, name)
		if directory == "" {
			candidate = name
		}
		// Directory checks inspect the candidate's exact metadata and return
		// before listing its children. In particular, never call Read with a
		// directory named README.md: Read selection is prefix based.
		_, _, dirErr := p.Directory(ctx, version, candidate, "", 1)
		if dirErr == nil || errors.Is(dirErr, project.ErrNotFound) {
			continue
		}
		if !errors.Is(dirErr, project.ErrInvalid) {
			return "", model.Entry{}, "", dirErr
		}
		var e model.Entry
		_, data, err := readPreviewBytes(func(consume func(model.Entry, io.Reader) error) error {
			return p.Read(ctx, version, candidate, func(entry model.Entry, reader io.Reader) error {
				e = entry
				return consume(entry, reader)
			})
		})
		if err != nil {
			return candidate, e, "", err
		}
		if int64(len(data)) > maxTextPreview {
			return candidate, e, "", nil
		}
		rendered, err := renderMarkdown(data, slug, version, candidate)
		return candidate, e, template.HTML(rendered), err
	}
	return "", model.Entry{}, "", nil
}
