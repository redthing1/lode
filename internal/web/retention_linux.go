package web

import (
	"net/http"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

func (s *Server) collect(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Maintainer, func(p *project.Store) error {
		var request struct{}
		if err := decode(r.Body, 4096, &request); err != nil {
			return err
		}
		result, err := p.Collect(r.Context())
		if err != nil {
			return err
		}
		s.instance.RecordUsage(p.ID(), result.After)
		return jsonReply(w, result)
	})
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		tags, next, err := p.Tags(r.Context(), r.URL.Query().Get("after"))
		if err != nil {
			return err
		}
		return jsonReply(w, remote.TagPage{Tags: tags, Next: next})
	})
}
func (s *Server) createTag(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Writer, func(p *project.Store) error {
		var tag model.Reference
		if err := decode(r.Body, 64<<10, &tag); err != nil {
			return err
		}
		result, err := p.CreateTag(r.Context(), tag.Name, tag.Version)
		if err != nil {
			return err
		}
		return jsonReply(w, result)
	})
}
func (s *Server) deleteTag(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Maintainer, func(p *project.Store) error {
		var tag model.Reference
		if err := decode(r.Body, 64<<10, &tag); err != nil {
			return err
		}
		if err := p.DeleteTag(r.Context(), tag.Name, tag.Version); err != nil {
			return err
		}
		return jsonReply(w, struct{}{})
	})
}
func (s *Server) retention(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	required := instance.Reader
	if r.Method == "POST" {
		required = instance.Maintainer
	}
	return s.withProject(r, actor, required, func(p *project.Store) error {
		if r.Method == "POST" {
			var policy model.Retention
			if err := decode(r.Body, 64<<10, &policy); err != nil {
				return err
			}
			if err := p.SetRetention(r.Context(), policy); err != nil {
				return err
			}
			return jsonReply(w, struct{}{})
		}
		policy, err := p.Retention(r.Context())
		if err != nil {
			return err
		}
		return jsonReply(w, policy)
	})
}
func (s *Server) cleanup(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Maintainer, func(p *project.Store) error {
		var request struct {
			Token string `json:"token"`
		}
		if err := decode(r.Body, 4096, &request); err != nil {
			return err
		}
		var preview model.CleanupPreview
		var err error
		if request.Token == "" {
			preview, err = p.PreviewCleanup(r.Context())
		} else {
			preview, err = p.Cleanup(r.Context(), request.Token)
		}
		if err != nil {
			return err
		}
		return jsonReply(w, preview)
	})
}
