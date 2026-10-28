//go:build linux

package web

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	items, err := s.i.Projects(r.Context(), u.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, "projects", page{Title: "Your projects", Section: "projects", User: u, Data: items}, 200)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	p := page{Title: "New project", Section: "projects", User: u}
	if r.Method == http.MethodPost {
		created, err := s.i.CreateProject(r.Context(), u.ID, r.FormValue("slug"))
		if err != nil {
			p.Error = "Choose an unused project name with 1–64 lowercase letters, digits or internal hyphens."
		} else {
			redirect(w, r, "/"+created.Path())
			return
		}
	}
	s.render(w, r, "new-project", p, 200)
}

type fileRow struct {
	model.Entry
	Name string
}
type projectView struct {
	Info                                 instance.Project
	URL, Tab, Branch, Path, Parent, Next string
	Commit                               project.Commit
	CanWrite, CanManage, CanDelete       bool
	Branches                             []project.Branch
	Files                                []fileRow
	History                              []project.Commit
	Members                              []instance.Member
	Notice                               string
	Keep, Protected, Eligible, Expired   int
	PlanToken                            string
	ReadmeName                           string
	ReadmeBody                           template.HTML
}

func (s *Server) access(w http.ResponseWriter, r *http.Request, role instance.Role) (instance.User, instance.Project, bool) {
	u, ok := s.require(w, r)
	if !ok {
		return u, instance.Project{}, false
	}
	info, err := s.i.Project(r.Context(), r.PathValue("owner"), r.PathValue("slug"))
	if err != nil {
		if errors.Is(err, instance.ErrNotFound) {
			s.fail(w, r, 404, "This project is unavailable.")
		} else {
			s.internal(w, r, err)
		}
		return u, info, false
	}
	if err := s.i.Authorize(r.Context(), principal(u), info.ID, role); err != nil {
		s.fail(w, r, 403, "Your account does not have permission for this project or action.")
		return u, info, false
	}
	return u, info, true
}

func (s *Server) projectPage(info instance.Project, tab string) projectView {
	return projectView{Info: info, URL: s.i.Config.URL + "/" + info.Path(), Tab: tab, Branch: "main", Keep: 10}
}

func (s *Server) renderProject(w http.ResponseWriter, r *http.Request, u instance.User, v projectView, message string) {
	v.CanWrite = s.i.Authorize(r.Context(), principal(u), v.Info.ID, instance.RoleWriter) == nil
	v.CanManage = s.i.Authorize(r.Context(), principal(u), v.Info.ID, instance.RoleMaintainer) == nil
	v.CanDelete = u.Admin || v.Info.OwnerID == u.ID
	s.render(w, r, "project-"+v.Tab, page{Title: v.Info.Path(), Section: "projects", User: u, Error: message, Data: v}, 200)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	info, err := s.i.Project(r.Context(), r.PathValue("owner"), r.PathValue("slug"))
	if err != nil {
		s.fail(w, r, 404, "This project is unavailable.")
		return
	}
	allowed, err := s.i.CanDeleteProject(r.Context(), principal(u), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !allowed {
		s.fail(w, r, 403, "Only the owner or an instance administrator can delete this project.")
		return
	}
	message := ""
	if r.Method == http.MethodPost {
		if !s.fresh(w, r) {
			return
		}
		if r.FormValue("confirm") != info.Path() {
			message = "Enter the full project name to confirm deletion."
		} else if err := s.i.DeleteProject(r.Context(), principal(u), info.ID); err != nil {
			if errors.Is(err, instance.ErrConflict) {
				message = "This project has active requests. Try again when they finish."
			} else {
				s.internal(w, r, err)
				return
			}
		} else {
			redirect(w, r, "/")
			return
		}
	}
	s.renderProject(w, r, u, s.projectPage(info, "delete"), message)
}

func (s *Server) projectFiles(w http.ResponseWriter, r *http.Request) {
	u, info, ok := s.access(w, r, instance.RoleReader)
	if !ok {
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer release()
	v := s.projectPage(info, "files")
	if b := r.URL.Query().Get("branch"); b != "" {
		v.Branch = b
	}
	v.Branches, err = p.Branches(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	id := r.URL.Query().Get("version")
	if id == "" {
		id, err = p.Head(r.Context(), v.Branch)
		if err != nil {
			s.fail(w, r, 404, "This branch is unavailable.")
			return
		}
	}
	if id != "" {
		v.Commit, err = p.Version(r.Context(), id)
		if err != nil {
			s.fail(w, r, 404, "This version is unavailable.")
			return
		}
		v.Path = r.URL.Query().Get("path")
		v.Parent = path.Dir(v.Path)
		if v.Parent == "." {
			v.Parent = ""
		}
		var entries []model.Entry
		entries, v.Next, err = p.Directory(r.Context(), id, v.Path, r.URL.Query().Get("after"), 100)
		if err != nil {
			s.fail(w, r, 404, "This directory is unavailable in the selected version.")
			return
		}
		for _, e := range entries {
			v.Files = append(v.Files, fileRow{e, path.Base(e.Path)})
		}
		if v.Commit.Retained {
			v.ReadmeName, _, v.ReadmeBody, err = readDirectoryReadme(r.Context(), p, info.Path(), id, v.Path)
			if err != nil && !errors.Is(err, project.ErrExpired) {
				s.internal(w, r, err)
				return
			}
		}
	}
	s.renderProject(w, r, u, v, "")
}

func (s *Server) projectHistory(w http.ResponseWriter, r *http.Request) {
	u, info, ok := s.access(w, r, instance.RoleReader)
	if !ok {
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer release()
	v := s.projectPage(info, "history")
	if b := r.URL.Query().Get("branch"); b != "" {
		v.Branch = b
	}
	v.Branches, err = p.Branches(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	v.History, v.Next, err = p.HistoryPage(r.Context(), v.Branch, r.URL.Query().Get("before"), 50)
	if err != nil {
		s.fail(w, r, 404, "This history is unavailable.")
		return
	}
	s.renderProject(w, r, u, v, "")
}

func (s *Server) projectBranches(w http.ResponseWriter, r *http.Request) {
	role := instance.RoleReader
	if r.Method == http.MethodPost {
		role = instance.RoleWriter
	}
	u, info, ok := s.access(w, r, role)
	if !ok {
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer release()
	v := s.projectPage(info, "branches")
	message := ""
	if r.Method == http.MethodPost {
		if r.FormValue("action") == "delete" {
			err = p.DeleteBranch(r.Context(), r.FormValue("name"), r.FormValue("tip"))
		} else {
			err = p.CreateBranch(r.Context(), r.FormValue("name"), r.FormValue("tip"))
		}
		if err != nil {
			message = "The branch could not be changed. Check its name and version, then refresh and try again."
		} else {
			redirect(w, r, "/"+info.Path()+"/branches")
			return
		}
	}
	v.Branches, err = p.Branches(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.renderProject(w, r, u, v, message)
}

func (s *Server) projectMembers(w http.ResponseWriter, r *http.Request) {
	u, info, ok := s.access(w, r, instance.RoleMaintainer)
	if !ok {
		return
	}
	v := s.projectPage(info, "members")
	message := ""
	if r.Method == http.MethodPost {
		var err error
		if r.FormValue("action") == "remove" {
			err = s.i.RemoveMembership(r.Context(), r.FormValue("user"), info.ID)
		} else {
			member, lookupErr := s.i.LookupUser(r.Context(), r.FormValue("username"))
			err = lookupErr
			if err == nil {
				err = s.i.PutMembership(r.Context(), member.ID, info.ID, instance.Role(r.FormValue("role")))
			}
		}
		if err != nil {
			message = "Use an existing username and choose a project role."
		} else {
			redirect(w, r, "/"+info.Path()+"/members")
			return
		}
	}
	var err error
	v.Members, err = s.i.Members(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.renderProject(w, r, u, v, message)
}

func (s *Server) projectStorage(w http.ResponseWriter, r *http.Request) {
	u, info, ok := s.access(w, r, instance.RoleMaintainer)
	if !ok {
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer release()
	v := s.projectPage(info, "storage")
	message := ""
	if r.Method == http.MethodPost {
		v.Keep, err = strconv.Atoi(r.FormValue("keep"))
		if err != nil || v.Keep < 0 {
			s.fail(w, r, 400, "Choose a non-negative number of versions to keep.")
			return
		}
		switch r.FormValue("action") {
		case "apply":
			var count int
			count, err = p.ApplyCleanup(r.Context(), v.Keep, r.FormValue("token"))
			if err != nil {
				message = "History or its protection changed. Preview cleanup again."
			} else {
				v.Notice = fmt.Sprintf("Released contents for %d historical versions. Their metadata remains available.", count)
			}
		case "collect":
			result, collectErr := p.Collect(r.Context())
			err = collectErr
			if err != nil {
				message = "Collection could not finish. Try again later."
			} else {
				v.Notice = fmt.Sprintf("Repository storage: %s before, %s after. Native safety delays can defer physical reclamation.", formatBytes(result.BeforeBytes), formatBytes(result.AfterBytes))
			}
		case "preview":
			plan, previewErr := p.PreviewCleanup(r.Context(), v.Keep)
			err = previewErr
			if err != nil {
				s.internal(w, r, err)
				return
			}
			v.PlanToken = plan.Token
			for _, version := range plan.Versions {
				if !version.Retained {
					v.Expired++
				} else if version.Protected() {
					v.Protected++
				} else {
					v.Eligible++
				}
			}
		default:
			s.fail(w, r, 400, "Choose a storage action.")
			return
		}
	}
	s.renderProject(w, r, u, v, message)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	_, info, ok := s.access(w, r, instance.RoleReader)
	if !ok {
		return
	}
	p, release, err := s.i.Acquire(r.Context(), info.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer release()
	started := false
	err = p.Read(r.Context(), r.URL.Query().Get("version"), r.URL.Query().Get("path"), func(e model.Entry, rd io.Reader) error {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(e.Path)}))
		w.Header().Set("Content-Length", strconv.FormatInt(e.Size, 10))
		started = true
		_, err := io.Copy(w, rd)
		return err
	})
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		if errors.Is(err, project.ErrExpired) {
			s.fail(w, r, 410, "This version's contents have been released. Its history and file listing are still available.")
		} else {
			s.fail(w, r, 404, "This file is unavailable.")
		}
	}
}
