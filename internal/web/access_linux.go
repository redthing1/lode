package web

import (
	"errors"
	"net/http"

	"github.com/redthing1/lode/internal/instance"
)

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Users", User: actor.User}
	var err error
	p.Users, p.Next, err = s.instance.Users(r.Context(), actor, r.URL.Query().Get("after"))
	if err != nil {
		return err
	}
	render(w, 200, "users", p)
	return nil
}

func (s *Server) newUserPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	if !actor.User.Admin {
		return instance.ErrAccess
	}
	p := page{Title: "New user", User: actor.User}
	status := 200
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		p.Username = r.FormValue("username")
		_, err := s.instance.CreateUser(r.Context(), actor, p.Username, r.FormValue("password"))
		if errors.Is(err, instance.ErrUsername) || errors.Is(err, instance.ErrUserExists) || errors.Is(err, instance.ErrPassword) {
			p.Error = err.Error()
			status = 400
		} else if err != nil {
			return err
		} else {
			http.Redirect(w, r, "/admin/users", 303)
			return nil
		}
	}
	render(w, status, "new-user", p)
	return nil
}

func (s *Server) membersPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Collaborators", Section: "members", User: actor.User}
	var err error
	p.Project, p.Members, err = s.instance.Members(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"))
	if err != nil {
		return err
	}
	status := 200
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		p.Username = r.FormValue("username")
		role := instance.Role(r.FormValue("role"))
		if !role.Allows(instance.Reader) {
			err = instance.ErrRole
		} else {
			err = s.instance.SetMember(r.Context(), actor, p.Project.Owner, p.Project.Name, p.Username, role)
		}
		if errors.Is(err, instance.ErrMember) || errors.Is(err, instance.ErrOwner) || errors.Is(err, instance.ErrRole) {
			p.Error = err.Error()
			status = 400
		} else if err != nil {
			return err
		} else {
			http.Redirect(w, r, projectURL(p.Project)+"/settings/members", 303)
			return nil
		}
	}
	render(w, status, "members", p)
	return nil
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	err := s.instance.SetMember(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), r.PathValue("user"), "")
	if err != nil {
		return err
	}
	http.Redirect(w, r, "/"+r.PathValue("owner")+"/"+r.PathValue("project")+"/settings/members", 303)
	return nil
}
