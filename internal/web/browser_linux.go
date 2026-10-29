package web

import (
	"database/sql"
	"errors"
	"github.com/alexedwards/scs/v2"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func (s *Server) initBrowser(secure bool) http.Handler {
	// Cookies ignore ports; separate instances on one host need distinct names.
	cookieName := "lode-" + s.instance.ID
	if secure {
		cookieName = "__Host-" + cookieName
	}
	// One session manager handles each browser request. Configure its SQLite
	// store directly: scs.New also starts an unused memory-store cleanup worker.
	sessions := &scs.SessionManager{
		Store: s.instance.Sessions(), Codec: scs.GobCodec{}, HashTokenInStore: true,
		Lifetime: instance.SessionLifetime,
		Cookie:   scs.SessionCookie{Name: cookieName, Path: "/", Secure: secure, HttpOnly: true, SameSite: http.SameSiteLaxMode},
		ErrorFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("browser session: %v", err)
			http.Error(w, "Unable to load session", http.StatusInternalServerError)
		},
	}
	s.sessions = sessions
	mux := http.NewServeMux()
	type handler func(http.ResponseWriter, *http.Request, instance.Principal) error
	bind := func(pattern string, h handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if sessions.GetString(r.Context(), "user") == "" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			if sessions.GetBool(r.Context(), "pending") {
				http.Redirect(w, r, "/verify", http.StatusSeeOther)
				return
			}
			user, err := s.sessionUser(r, false)
			if err == nil {
				err = h(w, r, instance.Principal{User: user})
			}
			if errors.Is(err, instance.ErrCredentials) {
				if err = sessions.Destroy(r.Context()); err == nil {
					http.Redirect(w, r, "/login", http.StatusSeeOther)
					return
				}
			}
			if err == nil {
				return
			}
			status, message := 500, "The request could not be completed."
			switch {
			case errors.Is(err, instance.ErrAccess):
				status, message = 403, "You do not have permission to perform this action."
			case errors.Is(err, instance.ErrNotFound), errors.Is(err, sql.ErrNoRows), errors.Is(err, os.ErrNotExist):
				status, message = 404, "Not found."
			case errors.Is(err, project.ErrExpired):
				status, message = 410, "This version's contents have expired."
			case errors.Is(err, project.ErrRequest):
				status, message = 400, err.Error()
			case errors.Is(err, instance.ErrMember), errors.Is(err, instance.ErrOwner), errors.Is(err, instance.ErrRole):
				status, message = 400, err.Error()
			default:
				log.Printf("browser request: %v", err)
			}
			render(w, status, "error", page{Title: http.StatusText(status), Error: message, User: user})
		})
	}
	s.initAuthentication(mux)
	bind("POST /logout", func(w http.ResponseWriter, r *http.Request, _ instance.Principal) error {
		if err := sessions.Destroy(r.Context()); err != nil {
			return err
		}
		http.Redirect(w, r, "/login", 303)
		return nil
	})
	bind("GET /{$}", s.projectsPage)
	bind("GET /{owner}", s.projectsPage)
	bind("GET /projects/new", func(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
		render(w, 200, "new-project", page{Title: "New project", User: actor.User})
		return nil
	})
	bind("POST /projects/new", func(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
		if err := r.ParseForm(); err != nil {
			return err
		}
		p, err := s.instance.CreateProject(r.Context(), actor, r.FormValue("name"))
		if errors.Is(err, instance.ErrName) || errors.Is(err, instance.ErrExists) {
			render(w, 400, "new-project", page{Title: "New project", User: actor.User, Error: err.Error()})
			return nil
		}
		if err != nil {
			return err
		}
		http.Redirect(w, r, projectURL(p), 303)
		return nil
	})
	bind("GET /settings/appearance", func(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
		render(w, 200, "appearance", page{Title: "Appearance", Section: "appearance", User: actor.User})
		return nil
	})
	bind("GET /settings/tokens", s.tokenPage)
	bind("GET /settings/usage", s.usagePage)
	bind("GET /settings/security", s.securityPage)
	bind("POST /settings/security", s.securityPage)
	bind("POST /settings/security/keys/{key}/remove", s.changeKeys)
	bind("POST /settings/security/recovery", s.changeKeys)
	bind("POST /settings/tokens", s.tokenPage)
	bind("POST /settings/tokens/{token}/revoke", func(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
		if err := s.instance.RevokeToken(r.Context(), actor.User, r.PathValue("token")); err != nil {
			return err
		}
		http.Redirect(w, r, "/settings/tokens", 303)
		return nil
	})
	bind("GET /admin/users", s.usersPage)
	bind("GET /admin/projects", s.adminProjectsPage)
	bind("GET /admin/users/new", s.newUserPage)
	bind("POST /admin/users/new", s.newUserPage)
	bind("GET /admin/users/{user}/edit", s.userPage)
	bind("POST /admin/users/{user}/edit", s.userPage)
	bind("GET /{owner}/{project}/settings/members", s.membersPage)
	bind("POST /{owner}/{project}/settings/members", s.membersPage)
	bind("POST /{owner}/{project}/settings/members/{user}/remove", s.removeMember)
	bind("GET /{owner}/{project}/settings/storage", s.storagePage)
	bind("POST /{owner}/{project}/settings/storage", s.storagePage)
	bind("GET /{owner}/{project}/settings/delete", s.deleteProjectPage)
	bind("POST /{owner}/{project}/settings/delete", s.deleteProjectPage)
	bind("GET /{owner}/{project}/tags", s.tagsPage)
	bind("POST /{owner}/{project}/tags", s.tagsPage)
	bind("GET /{owner}/{project}", s.projectPage)
	bind("GET /{owner}/{project}/usage", s.projectUsage)
	bind("GET /{owner}/{project}/history", s.historyPage)
	bind("GET /{owner}/{project}/branches", s.branchesPage)
	bind("GET /{owner}/{project}/raw/{version}/{path...}", s.download)
	bind("GET /{owner}/{project}/media/{version}/{path...}", s.media)
	protected := http.NewCrossOriginProtection().Handler(sessions.LoadAndSave(mux))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "same-origin")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		protected.ServeHTTP(&response{ResponseWriter: w}, r)
	})
}

func (s *Server) tokenPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Access tokens", Section: "tokens", User: actor.User}
	var err error
	limited := actor
	limited.Token = true
	limited.Role = instance.Maintainer
	p.Projects, err = s.instance.Projects(r.Context(), limited)
	if err != nil {
		return err
	}
	status := 200
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		days, err := strconv.Atoi(r.FormValue("days"))
		if err != nil || days < 1 || days > 365 {
			p.Error = "Expiry must be 1–365 days."
		} else {
			p.Token, err = s.instance.CreateToken(r.Context(), actor.User, instance.TokenOptions{Name: r.FormValue("name"), Project: r.FormValue("project"), Role: instance.Role(r.FormValue("role")), Expires: time.Now().Add(time.Duration(days) * 24 * time.Hour)})
			if err != nil {
				p.Error = "Token could not be created. Check its name and project access."
			}
		}
		if p.Error != "" {
			status = 400
		}
	}
	p.Tokens, err = s.instance.Tokens(r.Context(), actor.User)
	if err != nil {
		return err
	}
	render(w, status, "tokens", p)
	return nil
}
