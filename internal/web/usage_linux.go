package web

import (
	"net/http"

	"github.com/redthing1/lode/internal/instance"
)

func (s *Server) projectUsage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	sample, err := s.instance.ProjectUsage(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"))
	if err != nil {
		return err
	}
	render(w, 200, "project-size", page{Storage: sample})
	return nil
}

func (s *Server) usagePage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	refresh := r.URL.Query().Get("refresh") == "1"
	usage, err := s.instance.AccountUsage(r.Context(), actor, refresh)
	if err != nil {
		return err
	}
	name := "usage"
	if refresh {
		name = "usage-content"
	}
	render(w, 200, name, page{Title: "Usage", Section: "usage", User: actor.User, AccountUsage: usage})
	return nil
}
