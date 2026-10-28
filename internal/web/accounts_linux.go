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
		v.ProjectNames[p.ID] = p.Path()
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
	Users            []instance.User
	Account          instance.User
	Password         string
	RegistrationOpen bool
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	u, ok := s.admin(w, r)
	if !ok {
		return
	}
	v := usersView{RegistrationOpen: s.i.Config.RegistrationOpen}
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
			v.Account, v.Password = created, password
		}
	}
	var err error
	v.Users, err = s.i.ListUsers(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, "users", page{Title: "Users", Section: "admin-users", User: u, Error: message, Data: v}, 200)
}

func (s *Server) adminUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.admin(w, r)
	if !ok {
		return
	}
	account, err := s.i.LookupUser(r.Context(), r.PathValue("username"))
	if err != nil {
		s.fail(w, r, 404, "This account is unavailable.")
		return
	}
	v := struct {
		Account  instance.User
		Password string
	}{Account: account}
	message := ""
	if r.Method == http.MethodPost {
		if !s.fresh(w, r) {
			return
		}
		switch r.FormValue("action") {
		case "reset":
			password := project.NewID()
			if account.ID == u.ID {
				message = "Use account security settings to change your own credentials."
			} else if v.Account, err = s.i.ResetAccount(r.Context(), account.Username, password); err != nil {
				message = "The account could not be reset."
			} else {
				v.Password = password
			}
		case "state":
			if _, err := s.i.SetUserState(r.Context(), account.ID, r.FormValue("disabled") == "on", r.FormValue("admin") == "on"); err != nil {
				message = "Keep at least one enabled administrator. The account could not be changed."
			} else {
				redirect(w, r, "/-/admin/users/"+account.Username)
				return
			}
		default:
			message = "Choose an account action."
		}
	}
	status := http.StatusOK
	if message != "" {
		status = http.StatusBadRequest
	}
	s.render(w, r, "admin-user", page{Title: account.Username, Section: "admin-users", User: u, Error: message, Data: v}, status)
}

func (s *Server) adminProjects(w http.ResponseWriter, r *http.Request) {
	u, ok := s.admin(w, r)
	if !ok {
		return
	}
	items, err := s.i.Projects(r.Context(), u.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, "admin-projects", page{Title: "Projects", Section: "admin-projects", User: u, Data: items}, http.StatusOK)
}
