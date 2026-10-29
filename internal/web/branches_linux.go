package web

import (
	"net/http"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

func (s *Server) branches(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		branches, next, err := p.Branches(r.Context(), r.URL.Query().Get("after"))
		if err != nil {
			return err
		}
		return jsonReply(w, remote.BranchPage{Branches: branches, Next: next})
	})
}

func (s *Server) createBranch(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Writer, func(p *project.Store) error {
		var request model.Reference
		if err := decode(r.Body, 4096, &request); err != nil {
			return err
		}
		branch, err := p.CreateBranch(r.Context(), request.Name, request.Version)
		if err != nil {
			return err
		}
		return jsonReply(w, branch)
	})
}

func (s *Server) deleteBranch(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Maintainer, func(p *project.Store) error {
		var request model.Reference
		if err := decode(r.Body, 4096, &request); err != nil {
			return err
		}
		if err := p.DeleteBranch(r.Context(), request.Name, request.Version); err != nil {
			return err
		}
		return jsonReply(w, struct{}{})
	})
}

func (s *Server) reference(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		v, err := p.Reference(r.Context(), r.URL.Query().Get("ref"))
		if err != nil {
			return err
		}
		return jsonReply(w, v)
	})
}
