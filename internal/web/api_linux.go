package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"

	"encoding/json"
	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/transfer"
)

func (s *Server) me(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return jsonReply(w, remote.Identity{Instance: s.instance.ID, User: actor.User.ID, Username: actor.User.Name})
}
func (s *Server) projects(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	projects, err := s.instance.Projects(r.Context(), actor)
	if err != nil {
		return err
	}
	return jsonReply(w, projects)
}
func (s *Server) createProject(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	var input struct {
		Name string `json:"name"`
	}
	if err := decode(r.Body, 1024, &input); err != nil {
		return err
	}
	p, err := s.instance.CreateProject(r.Context(), actor, input.Name)
	if err != nil {
		return err
	}
	return jsonReply(w, p)
}
func (s *Server) projectInfo(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p, err := s.instance.Project(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), instance.Reader)
	if err != nil {
		return err
	}
	if expected := r.Header.Get("Lode-Project"); expected != "" && expected != p.ID {
		return &remote.Error{Code: "identity", Message: "project identity changed", Status: 412}
	}
	return jsonReply(w, p)
}

func (s *Server) withProject(r *http.Request, actor instance.Principal, required instance.Role, fn func(*project.Store) error) error {
	if required != instance.Reader && (r.Header.Get("Lode-Instance") == "" || r.Header.Get("Lode-Project") == "") {
		return badRequest(errors.New("save requires instance and project identities"))
	}
	return s.instance.WithProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), required, func(p *project.Store) error {
		if expected := r.Header.Get("Lode-Project"); expected != "" && expected != p.ID() {
			return &remote.Error{Code: "identity", Message: "project identity changed", Status: http.StatusPreconditionFailed}
		}
		return fn(p)
	})
}

func branch(r *http.Request) string {
	b := r.URL.Query().Get("branch")
	if b == "" {
		return "main"
	}
	return b
}
func (s *Server) head(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		id, err := p.Head(r.Context(), branch(r))
		if err != nil {
			return err
		}
		return jsonReply(w, remote.Head{Version: id})
	})
}
func (s *Server) history(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		start := r.URL.Query().Get("from")
		if start == "" {
			var err error
			start, err = p.Head(r.Context(), branch(r))
			if err != nil {
				return err
			}
		}
		limit := 20
		if q := r.URL.Query().Get("limit"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil || n < 1 || n > 100 {
				return badRequest(errors.New("history limit must be 1–100"))
			}
			limit = n
		}
		versions, err := p.History(r.Context(), start, limit)
		if err != nil {
			return err
		}
		return jsonReply(w, versions)
	})
}
func (s *Server) inventory(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		var selection model.Selection
		if err := decode(r.Body, 64<<10, &selection); err != nil {
			return err
		}
		if err := selection.Validate(); err != nil {
			return badRequest(err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		encoder := json.NewEncoder(w)
		count := 0
		err := p.Inventory(r.Context(), r.PathValue("version"), selection, func(e model.Entry) error { count++; return encoder.Encode(remote.InventoryRecord{Entry: &e}) })
		if err != nil {
			return err
		}
		return encoder.Encode(remote.InventoryRecord{Count: &count})
	})
}
func (s *Server) file(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		var signature []byte
		if r.Method == "POST" {
			var err error
			signature, err = io.ReadAll(io.LimitReader(r.Body, transfer.MaxSignatureSize+1))
			if err != nil {
				return badRequest(err)
			}
			if len(signature) == 0 || len(signature) > transfer.MaxSignatureSize {
				return badRequest(errors.New("invalid signature size"))
			}
		}
		e, reader, err := p.File(r.Context(), r.PathValue("version"), r.URL.Query().Get("path"))
		if err != nil {
			return err
		}
		defer reader.Close()
		if signature != nil {
			w.Header().Set("Content-Type", remote.DeltaType)
			if err := transfer.Delta(r.Context(), bytes.NewReader(signature), reader, w); err != nil {
				return badRequest(err)
			}
			return nil
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(e.Size, 10))
		w.Header().Set("ETag", strconv.Quote(e.SHA256))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(e.Path)}))
		_, err = io.Copy(w, reader)
		return err
	})
}
func (s *Server) signature(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		e, reader, err := p.File(r.Context(), r.PathValue("version"), r.URL.Query().Get("path"))
		if err != nil {
			return err
		}
		defer reader.Close()
		if e.Size > transfer.MaxBasisSize {
			return badRequest(errors.New("file exceeds signature size limit"))
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		return transfer.Signature(r.Context(), reader, e.Size, w)
	})
}

func (s *Server) files(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		var files []model.Entry
		if err := decode(r.Body, remote.MaxMetadata, &files); err != nil {
			return err
		}
		if err := remote.ValidateDownloadBatch(files); err != nil {
			return badRequest(err)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		return p.ReadFiles(r.Context(), r.PathValue("version"), files, func(e model.Entry, in io.Reader) error {
			n, err := io.Copy(w, io.LimitReader(in, e.Size+1))
			if err != nil {
				return err
			}
			if n != e.Size {
				return content.ErrIdentity
			}
			return nil
		})
	})
}

func (s *Server) save(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.withProject(r, actor, instance.Writer, func(p *project.Store) error {
		body, err := r.MultipartReader()
		if err != nil {
			return badRequest(err)
		}
		part, err := body.NextPart()
		if err != nil {
			return badRequest(err)
		}
		if part.FormName() != "request" {
			return badRequest(errors.New("first part must be the save request"))
		}
		var request model.Save
		if err := decode(part, remote.MaxMetadata, &request); err != nil {
			return err
		}
		if err := request.Validate(); err != nil {
			return badRequest(err)
		}
		source := project.Source{
			Write: func(e model.Entry, out io.Writer, basis *project.Basis) error {
				part, err := body.NextPart()
				if err != nil {
					return badRequest(err)
				}
				_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
				if err != nil {
					return badRequest(err)
				}
				if part.FormName() != "file" || params["filename"] != e.Path {
					return badRequest(fmt.Errorf("expected file %q", e.Path))
				}
				switch part.Header.Get("Content-Type") {
				case "application/octet-stream":
					_, err = io.Copy(out, part)
				case remote.DeltaType:
					if basis == nil {
						return badRequest(fmt.Errorf("%q has no base file", e.Path))
					}
					err = transfer.Patch(r.Context(), basis.Reader, basis.Entry.Size, part, e.Size, out)
				default:
					return badRequest(errors.New("unknown file encoding"))
				}
				if err != nil {
					if errors.Is(err, content.ErrStorage) {
						return err
					}
					return badRequest(err)
				}
				return nil
			},
			Complete: func() error {
				_, err := body.NextPart()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return badRequest(err)
				}
				return badRequest(errors.New("unexpected extra file"))
			},
		}
		version, err := p.Save(r.Context(), actor.User.ID, request, source)
		if err != nil {
			return err
		}
		return jsonReply(w, version)
	})
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	// Resolving one's own outcome cannot publish. Readers may recover an
	// accepted response after losing write permission.
	if r.Header.Get("Lode-Instance") == "" || r.Header.Get("Lode-Project") == "" {
		return badRequest(errors.New("save resolution requires instance and project identities"))
	}
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		var request model.Save
		if err := decode(r.Body, remote.MaxMetadata, &request); err != nil {
			return err
		}
		v, err := p.Resolve(r.Context(), actor.User.ID, request)
		if errors.Is(err, project.ErrUnaccepted) {
			// An unaccepted result invites the client to open upload sources.
			// Check write authority before giving that preflight result.
			if _, accessErr := s.instance.Project(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), instance.Writer); accessErr != nil {
				return accessErr
			}
		}
		if err != nil {
			return err
		}
		return jsonReply(w, v)
	})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	if r.Header.Get("Lode-Instance") == "" || r.Header.Get("Lode-Project") == "" {
		return badRequest(errors.New("cancellation requires instance and project identities"))
	}
	// A demoted writer can still settle their own pending save. This cannot
	// publish contents or cancel another account's request.
	return s.withProject(r, actor, instance.Reader, func(p *project.Store) error {
		var request model.Save
		if err := decode(r.Body, remote.MaxMetadata, &request); err != nil {
			return err
		}
		v, err := p.Cancel(r.Context(), actor.User.ID, request)
		if err != nil {
			return err
		}
		return jsonReply(w, v)
	})
}
