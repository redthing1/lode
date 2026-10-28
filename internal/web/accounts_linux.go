//go:build linux

package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
)

type tokenView struct {
	Tokens       []instance.Token
	Projects     []instance.Project
	ProjectNames map[string]string
	Secret       string
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	v := tokenView{}
	message := ""
	if r.Method == http.MethodPost {
		if !s.fresh(w, r) {
			return
		}
		days, err := strconv.Atoi(r.FormValue("days"))
		if err != nil || days < 1 || days > 365 {
			message = "Choose an expiry between 1 and 365 days."
		} else {
			v.Secret, _, err = s.i.CreateToken(r.Context(), u.ID, r.FormValue("name"), instance.Role(r.FormValue("role")), r.FormValue("project"), time.Now().Add(time.Duration(days)*24*time.Hour))
			if err != nil {
				message = "The token could not be created. Check its name, project and permissions."
			}
		}
	}
	var err error
	v.Tokens, err = s.i.ListTokens(r.Context(), u.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	v.Projects, err = s.i.Projects(r.Context(), u.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	v.ProjectNames = make(map[string]string, len(v.Projects))
	for _, p := range v.Projects {
		v.ProjectNames[p.ID] = p.Slug
	}
	s.render(w, r, "tokens", page{Title: "Access tokens", Section: "tokens", User: u, Error: message, Data: v}, 200)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	if err := s.i.RevokeToken(r.Context(), u.ID, r.FormValue("id")); err != nil {
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/settings/tokens")
}

type usersView struct {
	Users             []instance.User
	Created, Password string
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	u, ok := s.admin(w, r)
	if !ok {
		return
	}
	v := usersView{}
	message := ""
	if r.Method == http.MethodPost {
		if !s.fresh(w, r) {
			return
		}
		password := project.NewID()
		created, err := s.i.CreateUser(r.Context(), r.FormValue("username"), password, r.FormValue("admin") == "on")
		if err != nil {
			message = "Choose an unused username with 3–32 lowercase letters, digits, underscores or hyphens."
		} else {
			v.Created, v.Password = created.Username, password
		}
	}
	var err error
	v.Users, err = s.i.ListUsers(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, "users", page{Title: "People", Section: "admin", User: u, Error: message, Data: v}, 200)
}

func (s *Server) userState(w http.ResponseWriter, r *http.Request) {
	_, ok := s.admin(w, r)
	if !ok {
		return
	}
	if !s.fresh(w, r) {
		return
	}
	if _, err := s.i.SetUserState(r.Context(), r.FormValue("id"), r.FormValue("disabled") == "on", r.FormValue("admin") == "on"); err != nil {
		s.fail(w, r, 400, "Keep at least one enabled administrator. The account could not be changed.")
		return
	}
	redirect(w, r, "/admin/users")
}
