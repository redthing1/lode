package web

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/redthing1/lode/internal/instance"
	"golang.org/x/time/rate"
)

var errSignInRate = errors.New("too many attempts; try again shortly")

func (s *Server) sessionUser(r *http.Request, pending bool) (instance.User, error) {
	ctx := r.Context()
	u, err := s.instance.User(ctx, s.sessions.GetString(ctx, "user"))
	if err != nil {
		return instance.User{}, err
	}
	if u.Generation != s.sessions.GetInt64(ctx, "generation") || s.sessions.GetBool(ctx, "pending") != pending || pending && s.sessions.GetInt64(ctx, "pending-until") <= time.Now().Unix() {
		return instance.User{}, instance.ErrCredentials
	}
	return u, nil
}

func (s *Server) startSession(r *http.Request, user instance.User, pending bool) error {
	if err := s.sessions.Destroy(r.Context()); err != nil {
		return err
	}
	s.sessions.Put(r.Context(), "user", user.ID)
	s.sessions.Put(r.Context(), "generation", user.Generation)
	if pending {
		s.sessions.Put(r.Context(), "pending", true)
		s.sessions.Put(r.Context(), "pending-until", time.Now().Add(5*time.Minute).Unix())
	}
	return nil
}

func (s *Server) challengeBinding(r *http.Request) string {
	binding := s.sessions.GetString(r.Context(), "challenge-binding")
	if binding == "" {
		binding = rand.Text()
		s.sessions.Put(r.Context(), "challenge-binding", binding)
	}
	return binding
}

type keyReply struct {
	Next  string   `json:"next"`
	Codes []string `json:"codes,omitempty"`
}

func (s *Server) browserJSON(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := h(r)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			status, message := 500, "The request could not be completed."
			switch {
			case errors.Is(err, errSignInRate):
				status, message = 429, err.Error()
			case errors.Is(err, instance.ErrCredentials):
				status, message = 401, "Sign in again to continue."
			case errors.Is(err, instance.ErrAccess):
				status, message = 403, "You do not have permission to perform this action."
			case errors.Is(err, instance.ErrKey), errors.Is(err, instance.ErrKeyName), errors.Is(err, instance.ErrKeyLimit), errors.Is(err, instance.ErrKeyRegistered), errors.Is(err, instance.ErrCurrentPassword), errors.Is(err, instance.ErrRecovery):
				status, message = 400, err.Error()
			default:
				log.Printf("browser authentication: %v", err)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}
}

func (s *Server) initAuthentication(mux *http.ServeMux) {
	limit := rate.NewLimiter(2, 5)
	for _, route := range []string{"/setup", "/login"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) { s.passwordSignIn(w, r, limit) })
	}
	mux.HandleFunc("POST /login/key", s.browserJSON(func(r *http.Request) (any, error) {
		if !limit.Allow() {
			return nil, errSignInRate
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		return s.instance.BeginKeyLogin(r.Context(), s.challengeBinding(r), r.FormValue("username"))
	}))
	mux.HandleFunc("POST /login/key/finish", s.browserJSON(func(r *http.Request) (any, error) {
		u, err := s.instance.FinishKeyLogin(r.Context(), s.sessions.GetString(r.Context(), "challenge-binding"), r.URL.Query().Get("id"), r)
		if err != nil {
			return nil, err
		}
		return keyReply{Next: "/"}, s.startSession(r, u, false)
	}))
	mux.HandleFunc("GET /verify", s.verifyPage)
	mux.HandleFunc("POST /verify/recovery", s.verifyPage)
	mux.HandleFunc("POST /verify/key", s.browserJSON(func(r *http.Request) (any, error) {
		if !limit.Allow() {
			return nil, errSignInRate
		}
		u, err := s.sessionUser(r, true)
		if err != nil {
			return nil, err
		}
		return s.instance.BeginVerification(r.Context(), u, s.challengeBinding(r))
	}))
	mux.HandleFunc("POST /verify/key/finish", s.browserJSON(func(r *http.Request) (any, error) {
		u, err := s.sessionUser(r, true)
		if err != nil {
			return nil, err
		}
		u, err = s.instance.FinishVerification(r.Context(), u, s.sessions.GetString(r.Context(), "challenge-binding"), r.URL.Query().Get("id"), r)
		if err != nil {
			return nil, err
		}
		return keyReply{Next: "/"}, s.startSession(r, u, false)
	}))
	mux.HandleFunc("POST /settings/security/keys", s.browserJSON(func(r *http.Request) (any, error) {
		u, err := s.sessionUser(r, false)
		if err != nil {
			return nil, err
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		mode := r.FormValue("mode")
		if mode != "passkey" && mode != "username" {
			return nil, instance.ErrKey
		}
		return s.instance.BeginRegistration(r.Context(), instance.Principal{User: u}, s.challengeBinding(r), r.FormValue("current"), r.FormValue("name"), mode == "passkey")
	}))
	mux.HandleFunc("POST /settings/security/keys/finish", s.browserJSON(func(r *http.Request) (any, error) {
		u, err := s.sessionUser(r, false)
		if err != nil {
			return nil, err
		}
		u, codes, err := s.instance.FinishRegistration(r.Context(), instance.Principal{User: u}, s.sessions.GetString(r.Context(), "challenge-binding"), r.URL.Query().Get("id"), r)
		if err != nil {
			return nil, err
		}
		return keyReply{Next: "/settings/security", Codes: codes}, s.startSession(r, u, false)
	}))
}

func (s *Server) passwordSignIn(w http.ResponseWriter, r *http.Request, limit *rate.Limiter) {
	if r.Method != "GET" && r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	setup, err := s.instance.NeedsSetup(r.Context())
	if err != nil {
		http.Error(w, "Unable to read instance", 500)
		return
	}
	if setup && r.URL.Path != "/setup" {
		http.Redirect(w, r, "/setup", 303)
		return
	}
	if !setup && r.URL.Path == "/setup" {
		http.Redirect(w, r, "/login", 303)
		return
	}
	p := page{Title: "Sign in", Setup: setup, Notice: s.sessions.PopString(r.Context(), "notice")}
	if setup {
		p.Title = "Create administrator"
	}
	if r.Method == "GET" {
		render(w, 200, "auth", p)
		return
	}
	if !limit.Allow() {
		p.Error = "Too many attempts. Try again shortly."
		render(w, 429, "auth", p)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	var user instance.User
	if setup {
		user, err = s.instance.Setup(r.Context(), r.FormValue("code"), r.FormValue("username"), r.FormValue("password"))
	} else {
		user, err = s.instance.Password(r.Context(), r.FormValue("username"), r.FormValue("password"))
	}
	if err != nil {
		p.Error = "Sign-in failed. Check your username and password."
		if setup {
			p.Error = "Setup failed. Check the setup code, username, and password (12–72 bytes)."
		}
		render(w, 400, "auth", p)
		return
	}
	keys, err := s.instance.Keys(r.Context(), user)
	if err != nil {
		p.Error = "Sign-in could not be completed. Try again."
		render(w, 400, "auth", p)
		return
	}
	pending := len(keys) != 0
	if err := s.startSession(r, user, pending); err != nil {
		http.Error(w, "Unable to start session", 500)
		return
	}
	next := "/"
	if pending {
		next = "/verify"
	}
	http.Redirect(w, r, next, 303)
}

func (s *Server) verifyPage(w http.ResponseWriter, r *http.Request) {
	if !s.sessions.GetBool(r.Context(), "pending") {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u, err := s.sessionUser(r, true)
	if err != nil {
		if err := s.signedOut(w, r, "Sign in again to continue."); err != nil {
			http.Error(w, "Unable to end session", 500)
		}
		return
	}
	p := page{Title: "Verify sign-in", Username: u.Name}
	status := 200
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", 400)
			return
		}
		verified, err := s.instance.UseRecovery(r.Context(), u, r.FormValue("code"))
		if err == nil {
			if err := s.startSession(r, verified, false); err != nil {
				http.Error(w, "Unable to start session", 500)
				return
			}
			http.Redirect(w, r, "/", 303)
			return
		}
		p.Error, status = "Recovery failed. Check the code or sign in again.", 400
	}
	render(w, status, "verify", p)
}
