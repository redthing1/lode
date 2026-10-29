package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func (s *Server) storagePage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Storage", Section: "storage", User: actor.User}
	var err error
	status := 200
	err = s.instance.WithProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), instance.Maintainer, func(info instance.Project, store *project.Store) error {
		p.Project = info
		p.Policy, err = store.Retention(r.Context())
		if err != nil {
			return err
		}
		p.Usage, err = store.Usage(r.Context())
		if err != nil {
			return err
		}
		if r.Method != "POST" {
			return nil
		}
		if err := r.ParseForm(); err != nil {
			return err
		}
		switch r.FormValue("action") {
		case "policy":
			var a, b error
			p.Policy.Versions, a = strconv.Atoi(r.FormValue("versions"))
			p.Policy.HistoryDays, b = strconv.Atoi(r.FormValue("history_days"))
			if a != nil || b != nil {
				err = fmt.Errorf("%w: enter whole numbers for retention", project.ErrRequest)
			} else {
				err = store.SetRetention(r.Context(), p.Policy)
			}
		case "preview":
			var preview model.CleanupPreview
			preview, err = store.PreviewCleanup(r.Context())
			p.Cleanup = &preview
		case "apply":
			var preview model.CleanupPreview
			preview, err = store.Cleanup(r.Context(), r.FormValue("token"))
			if err == nil {
				p.Cleanup = &preview
				p.Applied = true
			}
		case "collect":
			var result model.Collection
			result, err = store.Collect(r.Context())
			if err == nil {
				p.Collection, p.Usage = &result, result.After
			}
		default:
			err = project.ErrRequest
		}
		if errors.Is(err, project.ErrRequest) || errors.Is(err, project.ErrCleanupChanged) {
			p.Error = err.Error()
			status = 400
			if errors.Is(err, project.ErrCleanupChanged) {
				status = 409
			}
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	render(w, status, "storage", p)
	return nil
}

func (s *Server) tagsPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Tags", User: actor.User, Ref: "main"}
	var err error
	required := instance.Reader
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		required = instance.Writer
		if r.FormValue("action") == "delete" {
			required = instance.Maintainer
		}
	}
	status := 200
	err = s.instance.WithProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), required, func(info instance.Project, store *project.Store) error {
		p.Project = info
		if r.Method == "POST" {
			p.Tag = model.Reference{Name: r.FormValue("name"), Version: r.FormValue("version")}
			p.Ref = r.FormValue("from")
			if r.FormValue("action") == "delete" {
				err = store.DeleteTag(r.Context(), p.Tag.Name, p.Tag.Version)
			} else {
				var v model.Version
				v, err = store.Reference(r.Context(), p.Ref)
				if err == nil {
					_, err = store.CreateTag(r.Context(), p.Tag.Name, v.ID)
				}
			}
			if errors.Is(err, project.ErrRequest) || errors.Is(err, project.ErrAmbiguous) || errors.Is(err, project.ErrExpired) || errors.Is(err, project.ErrTagExists) || errors.Is(err, project.ErrTagChanged) || errors.Is(err, sql.ErrNoRows) {
				p.Error = err.Error()
				if errors.Is(err, sql.ErrNoRows) {
					p.Error = "Version not found."
				}
				status = 400
			} else if err != nil {
				return err
			} else {
				return nil
			}
		}
		p.Tags, p.Next, err = store.Tags(r.Context(), r.URL.Query().Get("after"))
		return err
	})
	if err != nil {
		return err
	}
	if r.Method == "POST" && status == 200 {
		http.Redirect(w, r, projectURL(p.Project)+"/tags", 303)
		return nil
	}
	render(w, status, "tags", p)
	return nil
}
