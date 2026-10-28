//go:build linux

// Package web serves the embedded interface and authenticated project API.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

//go:embed templates/*.html assets/*
var files embed.FS

type Server struct {
	i         *instance.Instance
	security  *instance.Security
	sessions  *scs.SessionManager
	templates *template.Template
	attempts  attempts
}

type page struct {
	Title   string
	Section string
	User    instance.User
	Error   string
	Data    any
}

func New(i *instance.Instance, version string) (http.Handler, error) {
	u, _ := url.Parse(i.Config.URL)
	session := sessions(i.DB(), u.Scheme == "https")
	security, err := instance.NewSecurity(i.Store, i.Config.URL)
	if err != nil {
		return nil, err
	}
	t, err := template.New("lode").Funcs(template.FuncMap{
		"short": func(s string) string {
			if len(s) > 10 {
				return s[:10]
			}
			return s
		},
		"version": func() string { return version },
		"date":    func(t time.Time) string { return t.UTC().Format("02 Jan 2006 · 15:04 UTC") },
		"bytes":   formatBytes,
		"base":    path.Base,
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{i: i, security: security, sessions: session, templates: t}
	mux := http.NewServeMux()
	assets, _ := fs.Sub(files, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /api/me", s.me)
	// Separate wildcard project routes from reserved system paths, and keep
	// streamed API requests outside browser sessions and the form body limit.
	api := http.NewServeMux()
	api.HandleFunc("/{owner}/{slug}/v1/{rest...}", s.api)
	web := http.NewServeMux()
	web.HandleFunc("GET /{$}", s.projects)
	web.HandleFunc("GET /setup", s.setup)
	web.HandleFunc("POST /setup", s.setup)
	web.HandleFunc("GET /login", s.login)
	web.HandleFunc("POST /login", s.login)
	web.HandleFunc("GET /register", s.register)
	web.HandleFunc("POST /register", s.register)
	web.HandleFunc("POST /passkey/{step}", s.passkey)
	web.HandleFunc("GET /verify", s.verify)
	web.HandleFunc("POST /verify/recovery", s.recover)
	web.HandleFunc("POST /logout", s.logout)
	web.HandleFunc("POST /security/{purpose}/{step}", s.ceremony)
	web.HandleFunc("GET /settings/appearance", func(w http.ResponseWriter, r *http.Request) {
		if u, ok := s.require(w, r); ok {
			s.render(w, r, "appearance", page{Title: "Appearance", Section: "appearance", User: u}, http.StatusOK)
		}
	})
	web.HandleFunc("GET /settings/security", s.accountSecurity)
	web.HandleFunc("POST /settings/password", s.password)
	web.HandleFunc("POST /settings/keys/remove", s.removeKey)
	web.HandleFunc("POST /settings/recovery", s.recoveryCodes)
	web.HandleFunc("GET /settings/tokens", s.tokens)
	web.HandleFunc("POST /settings/tokens", s.tokens)
	web.HandleFunc("POST /settings/tokens/revoke", s.revokeToken)
	web.HandleFunc("GET /projects/new", s.createProject)
	web.HandleFunc("POST /projects/new", s.createProject)
	projects := http.NewServeMux()
	projects.HandleFunc("GET /{owner}/{slug}", s.projectFiles)
	projects.HandleFunc("GET /{owner}/{slug}/history", s.projectHistory)
	projects.HandleFunc("GET /{owner}/{slug}/branches", s.projectBranches)
	projects.HandleFunc("POST /{owner}/{slug}/branches", s.projectBranches)
	projects.HandleFunc("GET /{owner}/{slug}/storage", s.projectStorage)
	projects.HandleFunc("POST /{owner}/{slug}/storage", s.projectStorage)
	projects.HandleFunc("GET /{owner}/{slug}/members", s.projectMembers)
	projects.HandleFunc("POST /{owner}/{slug}/members", s.projectMembers)
	projects.HandleFunc("GET /{owner}/{slug}/delete", s.deleteProject)
	projects.HandleFunc("POST /{owner}/{slug}/delete", s.deleteProject)
	projects.HandleFunc("GET /{owner}/{slug}/download", s.download)
	projects.HandleFunc("GET /{owner}/{slug}/file", s.projectFile)
	projects.HandleFunc("GET /{owner}/{slug}/raw", s.rawFile)
	web.Handle("/", projects)
	web.HandleFunc("GET /-/admin/users", s.users)
	web.HandleFunc("POST /-/admin/users", s.users)
	web.HandleFunc("GET /-/admin/users/{username}", s.adminUser)
	web.HandleFunc("POST /-/admin/users/{username}", s.adminUser)
	web.HandleFunc("GET /-/admin/projects", s.adminProjects)
	browser := session.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		web.ServeHTTP(w, r)
	}))
	api.Handle("/", browser)
	mux.Handle("/", api)
	protected := http.NewCrossOriginProtection().Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		w.Header().Set("Cache-Control", "no-store")
		if !strings.EqualFold(r.Host, u.Host) {
			http.Error(w, "use the configured instance address", http.StatusMisdirectedRequest)
			return
		}
		protected.ServeHTTP(w, r)
	}), nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page, status int) {
	if p.User.ID == "" {
		p.User, _ = s.current(r, false)
	}
	var b bytes.Buffer
	if err := s.templates.ExecuteTemplate(&b, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "Unable to render this page.", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = b.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, "error", page{Title: http.StatusText(status), Error: message}, status)
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	s.fail(w, r, 500, "Something went wrong. Try again; the server log has the details.")
}

func redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}
func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func principal(u instance.User) instance.Principal {
	return instance.Principal{UserID: u.ID, Username: u.Username, Admin: u.Admin, AuthVersion: u.AuthVersion, ForcePasswordChange: u.ForcePasswordChange}
}

func (s *Server) current(r *http.Request, allowPending bool) (instance.User, bool) {
	id := s.sessions.GetString(r.Context(), "user")
	if id == "" {
		return instance.User{}, false
	}
	u, err := s.i.GetUser(r.Context(), id)
	if err != nil || u.Disabled || u.AuthVersion != s.sessions.GetInt64(r.Context(), "version") {
		return instance.User{}, false
	}
	if !allowPending && s.sessions.GetBool(r.Context(), "pending") {
		return instance.User{}, false
	}
	return u, true
}

func (s *Server) require(w http.ResponseWriter, r *http.Request) (instance.User, bool) {
	u, ok := s.current(r, false)
	if !ok {
		redirect(w, r, "/login")
		return u, false
	}
	if u.ForcePasswordChange && r.URL.Path != "/settings/security" && r.URL.Path != "/settings/password" {
		redirect(w, r, "/settings/security")
		return u, false
	}
	return u, true
}

func (s *Server) fresh(w http.ResponseWriter, r *http.Request) bool {
	if time.Now().Unix()-s.sessions.GetInt64(r.Context(), "authenticated") > 600 {
		s.fail(w, r, 403, "Sign in again to continue.")
		return false
	}
	return true
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) (instance.User, bool) {
	u, ok := s.require(w, r)
	if !ok {
		return u, false
	}
	if !u.Admin {
		s.fail(w, r, 403, "Instance administrator access is required.")
		return u, false
	}
	return u, true
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "bearer access token required", 401)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	actor, err := s.i.AuthenticateToken(r.Context(), token)
	if err != nil {
		http.Error(w, "invalid or expired access token", 401)
		return
	}
	info, err := s.i.Project(r.Context(), r.PathValue("owner"), r.PathValue("slug"))
	if err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			log.Printf("resolve project: %v", err)
			http.Error(w, "project unavailable", 500)
		}
		return
	}
	if err := s.i.Authorize(r.Context(), actor, info.ID, instance.RoleReader); err != nil {
		http.Error(w, "access denied", 403)
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		http.Error(w, "project unavailable; retry shortly", 503)
		return
	}
	defer release()
	h := remote.AuthenticatedHandler(p, func(req *http.Request, access remote.Access) (project.Actor, error) {
		role := instance.RoleReader
		if access == remote.Write {
			role = instance.RoleWriter
		}
		if access == remote.Manage {
			role = instance.RoleMaintainer
		}
		if err := s.i.Authorize(req.Context(), actor, info.ID, role); err != nil {
			return project.Actor{}, err
		}
		return project.Actor{ID: actor.UserID, Name: actor.Username}, nil
	})
	request := r.Clone(r.Context())
	u := *r.URL
	u.Path = "/v1/" + r.PathValue("rest")
	u.RawPath = ""
	request.URL = &u
	h.ServeHTTP(w, request)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "bearer access token required", 401)
		return
	}
	p, err := s.i.AuthenticateToken(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		http.Error(w, "invalid or expired access token", 401)
		return
	}
	jsonReply(w, struct{ ID, Username, Instance string }{p.UserID, p.Username, s.i.Config.ID})
}

func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n) / 1024
	for j, u := range units {
		if v < 1024 || j == len(units)-1 {
			return fmt.Sprintf("%.1f %s", v, u)
		}
		v /= 1024
	}
	return ""
}
