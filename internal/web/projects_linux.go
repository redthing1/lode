package web

import (
	"errors"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"net/http"
	"os"
	"path"
)

func (s *Server) projectsPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Projects", User: actor.User, Owner: r.PathValue("owner")}
	projects, err := s.instance.Projects(r.Context(), actor)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if p.Owner == "" || project.Owner == p.Owner {
			p.Projects = append(p.Projects, project)
		}
	}
	if p.Owner != "" {
		if len(p.Projects) == 0 && p.Owner != actor.User.Name {
			return instance.ErrNotFound
		}
		p.Title = p.Owner
	}
	render(w, 200, "projects", p)
	return nil
}

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{User: actor.User, Path: r.URL.Query().Get("path"), Origin: s.instance.Config.URL, Branch: branch(r), Ref: branch(r)}
	var err error
	err = s.instance.WithProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), instance.Reader, func(info instance.Project, store *project.Store) error {
		p.Project = info
		p.Title = info.Owner + "/" + info.Name
		p.Branches, _, err = store.Branches(r.Context(), "")
		if err != nil {
			return err
		}
		id := r.URL.Query().Get("version")
		if id != "" {
			p.Ref = model.ShortVersion(id)
			p.Branch = ""
		}
		if id == "" {
			var err error
			id, err = store.Head(r.Context(), p.Branch)
			if err != nil {
				return err
			}
		}
		if id == "" {
			return nil
		}
		var err error
		p.Version, err = store.Version(r.Context(), id)
		if err != nil {
			return err
		}
		if p.Path != "" {
			e, err := store.Entry(r.Context(), id, p.Path)
			if err != nil {
				return err
			}
			p.File = e.Kind == model.File
			if p.File {
				return preview(r, store, e, &p)
			}
		}
		p.Entries, p.Next, err = store.Directory(r.Context(), id, p.Path, r.URL.Query().Get("after"), 100)
		if err != nil {
			return err
		}
		if p.Version.Retained {
			for _, name := range []string{"README.md", "readme.md"} {
				e, err := store.Entry(r.Context(), id, path.Join(p.Path, name))
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				if e.Kind == model.File {
					return preview(r, store, e, &p)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if p.Version.Actor != "" {
		p.Authors, err = s.instance.UserNames(r.Context(), []string{p.Version.Actor})
		if err != nil {
			return err
		}
	}
	render(w, 200, "project", p)
	return nil
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "History", User: actor.User, Branch: branch(r), Ref: branch(r)}
	if from := r.URL.Query().Get("from"); from != "" && r.URL.Query().Get("branch") == "" {
		p.Branch, p.Ref = "", model.ShortVersion(from)
	}
	var err error
	err = s.instance.WithProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), instance.Reader, func(info instance.Project, store *project.Store) error {
		p.Project = info
		id := r.URL.Query().Get("from")
		if id == "" {
			var err error
			id, err = store.Head(r.Context(), p.Branch)
			if err != nil {
				return err
			}
		}
		var err error
		p.Version.ID = id
		history, err := store.History(r.Context(), id, 50)
		p.History, p.Next, p.Boundary = history.Versions, history.Next, history.Boundary
		return err
	})
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(p.History))
	for _, v := range p.History {
		ids = append(ids, v.Actor)
	}
	p.Authors, err = s.instance.UserNames(r.Context(), ids)
	if err != nil {
		return err
	}
	render(w, 200, "history", p)
	return nil
}

func (s *Server) branchesPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Branches", User: actor.User}
	var err error
	err = s.instance.WithProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), instance.Reader, func(info instance.Project, store *project.Store) error {
		p.Project = info
		p.Branches, p.Next, err = store.Branches(r.Context(), r.URL.Query().Get("after"))
		return err
	})
	if err != nil {
		return err
	}
	render(w, 200, "branches", p)
	return nil
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.serveFile(w, r, actor, false)
}
