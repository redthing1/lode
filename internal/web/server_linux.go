// Package web serves the browser and authenticated client API.
package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

type Server struct {
	instance *instance.Store
	mux      *http.ServeMux
	host     string
	sessions *scs.SessionManager
}

func New(store *instance.Store) *Server {
	u, _ := url.Parse(store.Config.URL)
	s := &Server{instance: store, mux: http.NewServeMux(), host: u.Host}
	s.mux.Handle("/api/", http.NotFoundHandler())
	s.mux.Handle("GET /assets/", http.FileServer(http.FS(browserFiles)))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("GET /api/me", s.api(s.me))
	s.mux.HandleFunc("GET /api/projects", s.api(s.projects))
	s.mux.HandleFunc("POST /api/projects", s.api(s.createProject))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}", s.api(s.projectInfo))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/head", s.api(s.head))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/history", s.api(s.history))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/branches", s.api(s.branches))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/branches", s.api(s.createBranch))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/branches/delete", s.api(s.deleteBranch))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/reference", s.api(s.reference))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/tags", s.api(s.tags))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/tags", s.api(s.createTag))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/tags/delete", s.api(s.deleteTag))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/retention", s.api(s.retention))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/retention", s.api(s.retention))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/cleanup", s.api(s.cleanup))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/collect", s.api(s.collect))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/versions/{version}/inventory", s.api(s.inventory))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/versions/{version}/file", s.api(s.file))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/versions/{version}/file", s.api(s.file))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/versions/{version}/files", s.api(s.files))
	s.mux.HandleFunc("GET /api/projects/{owner}/{project}/versions/{version}/signature", s.api(s.signature))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/save", s.api(s.save))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/resolve", s.api(s.resolve))
	s.mux.HandleFunc("POST /api/projects/{owner}/{project}/cancel", s.api(s.cancel))
	s.mux.Handle("/", s.initBrowser(u.Scheme == "https"))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Host, s.host) {
		http.Error(w, "incorrect host", http.StatusMisdirectedRequest)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	s.mux.ServeHTTP(w, r)
}

type apiHandler func(http.ResponseWriter, *http.Request, instance.Principal) error

func (s *Server) api(handler apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := &response{ResponseWriter: w}
		r.Body = &requestBody{ReadCloser: r.Body, controller: http.NewResponseController(w)}
		err := func() error {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || len(token) > 512 {
				return instance.ErrCredentials
			}
			actor, err := s.instance.Token(r.Context(), token)
			if err != nil {
				return err
			}
			if expected := r.Header.Get("Lode-Instance"); expected != "" && expected != s.instance.ID {
				return &remote.Error{Code: "identity", Message: "server identity changed", Status: http.StatusPreconditionFailed}
			}
			return handler(response, r, actor)
		}()
		if err == nil {
			return
		}
		if response.written {
			panic(http.ErrAbortHandler)
		}
		var apiError *remote.Error
		var conflicts *project.ConflictsError
		if !errors.As(err, &apiError) {
			apiError = &remote.Error{Code: "internal", Message: "request failed", Status: http.StatusInternalServerError}
			switch {
			case errors.As(err, &conflicts):
				apiError = &remote.Error{Code: "conflicts", Message: err.Error(), Status: 409, Conflicts: conflicts.Conflicts}
			case errors.Is(err, project.ErrAlreadyMerged):
				apiError = &remote.Error{Code: "already_merged", Message: err.Error(), Status: 409}
			case errors.Is(err, project.ErrMergeBase), errors.Is(err, project.ErrHistoryUnavailable):
				apiError = &remote.Error{Code: "ancestry", Message: err.Error(), Status: 409}
			case errors.Is(err, instance.ErrCredentials):
				apiError = &remote.Error{Code: "credentials", Message: "invalid credentials", Status: 401}
				w.Header().Set("WWW-Authenticate", "Bearer")
			case errors.Is(err, instance.ErrAccess):
				apiError = &remote.Error{Code: "access", Message: err.Error(), Status: 403}
			case errors.Is(err, sql.ErrNoRows), errors.Is(err, os.ErrNotExist), errors.Is(err, instance.ErrNotFound):
				apiError = &remote.Error{Code: "not_found", Message: "not found", Status: 404}
			case errors.Is(err, project.ErrStale):
				apiError = &remote.Error{Code: "stale", Message: err.Error(), Status: 409}
			case errors.Is(err, project.ErrBranchChanged), errors.Is(err, project.ErrBranchExists), errors.Is(err, project.ErrAmbiguous), errors.Is(err, project.ErrTagExists), errors.Is(err, project.ErrTagChanged):
				apiError = &remote.Error{Code: "reference", Message: err.Error(), Status: 409}
			case errors.Is(err, project.ErrCleanupChanged):
				apiError = &remote.Error{Code: "cleanup", Message: err.Error(), Status: 409}
			case errors.Is(err, project.ErrUnaccepted):
				apiError = &remote.Error{Code: "unaccepted", Message: err.Error(), Status: 404}
			case errors.Is(err, project.ErrCancelled):
				apiError = &remote.Error{Code: "cancelled", Message: err.Error(), Status: 409}
			case errors.Is(err, project.ErrIntent):
				apiError = &remote.Error{Code: "intent", Message: err.Error(), Status: 409}
			case errors.Is(err, project.ErrNoChanges):
				apiError = &remote.Error{Code: "unchanged", Message: err.Error(), Status: 409}
			case errors.Is(err, project.ErrExpired):
				apiError = &remote.Error{Code: "expired", Message: err.Error(), Status: 410}
			case errors.Is(err, project.ErrOperationExpired):
				apiError = &remote.Error{Code: "operation_expired", Message: err.Error(), Status: 410}
			case errors.Is(err, project.ErrRequest):
				apiError = &remote.Error{Code: "request", Message: err.Error(), Status: 400}
			case errors.Is(err, content.ErrIdentity):
				apiError = &remote.Error{Code: "contents", Message: err.Error(), Status: 400}
			case errors.Is(err, instance.ErrName):
				apiError = &remote.Error{Code: "name", Message: err.Error(), Status: 400}
			case errors.Is(err, instance.ErrExists):
				apiError = &remote.Error{Code: "exists", Message: err.Error(), Status: 409}
			default:
				log.Printf("API request failed: %v", err)
			}
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Disposition")
		w.Header().Del("ETag")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(apiError.Status)
		_ = json.NewEncoder(w).Encode(apiError)
	}
}

func badRequest(err error) error {
	return &remote.Error{Code: "request", Message: err.Error(), Status: http.StatusBadRequest}
}
func jsonReply(w http.ResponseWriter, value any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(value)
}
func decode(r io.Reader, limit int64, value any) error {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return badRequest(err)
	}
	if int64(len(b)) > limit {
		return badRequest(errors.New("metadata exceeds limit"))
	}
	if err := json.Unmarshal(b, value); err != nil {
		return badRequest(err)
	}
	return nil
}

type response struct {
	http.ResponseWriter
	written bool
}

func (w *response) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *response) WriteHeader(status int)      { w.written = true; w.ResponseWriter.WriteHeader(status) }
func (w *response) Write(b []byte) (int, error) {
	if err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
		return 0, err
	}
	w.written = true
	return w.ResponseWriter.Write(b)
}

type requestBody struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (r *requestBody) Read(b []byte) (int, error) {
	if err := r.controller.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(b)
}
