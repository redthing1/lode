package web

import (
	"bytes"
	"embed"
	"fmt"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/version"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed assets templates
var browserFiles embed.FS

type page struct {
	Title, Error, Notice, Section, Origin string
	User                                  instance.User
	Subject                               instance.User
	Setup                                 bool
	Projects                              []instance.Project
	Project                               instance.Project
	Version                               model.Version
	History                               []model.Version
	Boundary                              string
	Entries                               []model.Entry
	Path, Next, Text, Ref                 string
	Markdown                              template.HTML
	Media                                 string
	File                                  bool
	Token                                 string
	Tokens                                []instance.TokenInfo
	Keys                                  []instance.Key
	Codes                                 []string
	Users                                 []instance.User
	Members                               []instance.Member
	Username                              string
	Authors                               map[string]string
	Branches                              []model.Reference
	Branch                                string
	Tags                                  []model.Reference
	Tag                                   model.Reference
	Policy                                model.Retention
	Cleanup                               *model.CleanupPreview
	Applied                               bool
	Usage                                 model.StorageUsage
	Collection                            *model.Collection
}

func projectURL(p instance.Project) string { return "/" + p.Owner + "/" + p.Name }
func treeURL(p instance.Project, version, name, after string) string {
	q := url.Values{}
	if version != "" {
		q.Set("version", version)
	}
	if name != "" {
		q.Set("path", name)
	}
	if after != "" {
		q.Set("after", after)
	}
	u := projectURL(p)
	if len(q) != 0 {
		u += "?" + q.Encode()
	}
	return u
}
func size(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1<<20 {
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	}
	if n < 1<<30 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

var views = template.Must(template.New("pages").Funcs(template.FuncMap{
	"version":    func() string { return version.Current },
	"projectURL": projectURL, "tree": treeURL, "base": path.Base,
	"media": mediaURL,
	"branchURL": func(p instance.Project, branch string) string {
		return projectURL(p) + "?branch=" + url.QueryEscape(branch)
	},
	"historyURL": func(p instance.Project, branch, from string) string {
		q := url.Values{"branch": {branch}}
		if from != "" {
			q.Set("from", from)
		}
		return projectURL(p) + "/history?" + q.Encode()
	},
	"raw": func(p instance.Project, version, name string) string {
		u := url.URL{Path: projectURL(p) + "/raw/" + version + "/" + name}
		return u.String()
	},
	"parent": func(s string) string {
		p := path.Dir(s)
		if p == "." {
			return ""
		}
		return p
	},
	"size":  size,
	"short": model.ShortVersion,
	"date": func(t time.Time) string {
		if t.IsZero() {
			return "Never"
		}
		return t.UTC().Format("2006-01-02")
	},
	"time": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
}).ParseFS(browserFiles, "templates/*.html"))

func render(w http.ResponseWriter, status int, name string, p page) {
	var body bytes.Buffer
	if err := views.ExecuteTemplate(&body, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "Unable to display this page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}

func preview(r *http.Request, store *project.Store, e model.Entry, p *page) error {
	if !p.Version.Retained {
		return nil
	}
	p.Media, _ = mediaType(e.Path)
	if p.Media != "" || e.Size > 1<<20 {
		return nil
	}
	_, reader, err := store.File(r.Context(), p.Version.ID, e.Path)
	if err != nil {
		return err
	}
	defer reader.Close()
	b, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 1<<20 || !utf8.Valid(b) || bytes.ContainsRune(b, 0) {
		return nil
	}
	if !strings.EqualFold(path.Ext(e.Path), ".md") {
		p.Text = string(b)
		return nil
	}
	md := goldmark.New()
	doc := md.Parser().Parse(text.NewReader(b))
	var images []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if image, ok := n.(*ast.Image); ok {
			u, err := url.Parse(string(image.Destination))
			name, local := "", false
			if err == nil && u.RawQuery == "" && u.Fragment == "" {
				name, local = markdownPath(e.Path, u)
			}
			kind, _ := mediaType(name)
			if local && kind == "image" {
				image.Destination = []byte(mediaURL(p.Project, p.Version.ID, name))
			} else {
				images = append(images, n)
			}
			return ast.WalkSkipChildren, nil
		}
		if link, ok := n.(*ast.Link); ok {
			u, err := url.Parse(string(link.Destination))
			if err == nil && !u.IsAbs() && u.Host == "" && u.Path != "" {
				if name, ok := markdownPath(e.Path, u); ok {
					link.Destination = []byte(treeURL(p.Project, p.Version.ID, name, ""))
				} else {
					link.Destination = []byte("#")
				}
			}
		}
		return ast.WalkContinue, nil
	})
	for _, image := range images {
		image.Parent().RemoveChild(image.Parent(), image)
	}
	var html bytes.Buffer
	if err := md.Renderer().Render(&html, b, doc); err != nil {
		return err
	}
	p.Markdown = template.HTML(html.String())
	return nil
}

func markdownPath(source string, u *url.URL) (string, bool) {
	if u.IsAbs() || u.Host != "" || u.Path == "" {
		return "", false
	}
	name := path.Join(path.Dir(source), u.Path)
	if strings.HasPrefix(u.Path, "/") {
		name = path.Clean(strings.TrimPrefix(u.Path, "/"))
	}
	return name, model.ValidPath(name)
}
