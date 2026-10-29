package web

import (
	"errors"
	"log"
	"net/http"

	"github.com/redthing1/lode/internal/instance"
)

func (s *Server) adminProjectsPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Projects", Section: "admin-projects", User: actor.User}
	var err error
	p.Projects, p.Next, err = s.instance.AdminProjects(r.Context(), actor, r.URL.Query().Get("after"))
	if err != nil {
		return err
	}
	render(w, 200, "admin-projects", p)
	return nil
}

func (s *Server) deleteProjectPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Delete project", Section: "delete", User: actor.User}
	var err error
	p.Project, err = s.instance.OwnedProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"))
	if err != nil {
		return err
	}
	status := 200
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		if r.FormValue("confirm") != p.Project.Owner+"/"+p.Project.Name {
			p.Error, status = "Enter the full project name to confirm deletion.", 400
		} else {
			err := s.instance.DeleteProject(r.Context(), actor, p.Project.Owner, p.Project.Name, r.FormValue("id"))
			switch {
			case errors.Is(err, instance.ErrProjectBusy), errors.Is(err, instance.ErrProjectChanged):
				p.Error, status = err.Error(), 409
			case errors.Is(err, instance.ErrDeletionPending):
				log.Printf("delete project %s: %v", p.Project.ID, err)
				p.Error, status = instance.ErrDeletionPending.Error(), 503
				p.Project.State = "deleting"
			case err != nil:
				return err
			default:
				destination := "/"
				if actor.User.Admin {
					destination = "/admin/projects"
				}
				http.Redirect(w, r, destination, 303)
				return nil
			}
		}
	}
	render(w, status, "delete-project", p)
	return nil
}
