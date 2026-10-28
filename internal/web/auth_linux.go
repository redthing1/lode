//go:build linux

package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
)

func (s *Server) startSession(r *http.Request, u instance.User, pending bool) error {
	ctx := r.Context()
	if err := s.sessions.Destroy(ctx); err != nil {
		return err
	}
	if err := s.sessions.RenewToken(ctx); err != nil {
		return err
	}
	s.sessions.Put(ctx, "user", u.ID)
	s.sessions.Put(ctx, "version", u.AuthVersion)
	s.sessions.Put(ctx, "pending", pending)
	s.sessions.Put(ctx, "binding", project.NewID())
	if !pending {
		s.sessions.Put(ctx, "authenticated", time.Now().Unix())
	}
	return nil
}

func (s *Server) refresh(r *http.Request) error {
	u, err := s.i.GetUser(r.Context(), s.sessions.GetString(r.Context(), "user"))
	if err != nil {
		return err
	}
	if u.Disabled || u.AuthVersion != s.sessions.GetInt64(r.Context(), "version")+1 {
		return instance.ErrUnauthorized
	}
	if err := s.sessions.RenewToken(r.Context()); err != nil {
		return err
	}
	s.sessions.Put(r.Context(), "version", u.AuthVersion)
	return nil
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	needed, err := s.i.SetupRequired(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !needed {
		redirect(w, r, "/login")
		return
	}
	p := page{Title: "Create administrator"}
	if r.Method == http.MethodPost {
		if !s.attempts.allow("setup") {
			s.fail(w, r, 429, "Too many setup attempts. Try again in a minute.")
			return
		}
		u, err := s.i.Setup(r.Context(), r.FormValue("code"), r.FormValue("username"), r.FormValue("password"))
		if err != nil {
			p.Error = "Check the setup code. Use a username of 3–32 lowercase letters, digits, underscores or hyphens and a password of at least 12 characters."
		} else if err := s.startSession(r, u, false); err != nil {
			s.internal(w, r, err)
			return
		} else {
			redirect(w, r, "/settings/security?welcome=1")
			return
		}
	}
	status := 200
	if p.Error != "" {
		status = 400
	}
	s.render(w, r, "setup", p, status)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	needed, err := s.i.SetupRequired(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if needed {
		redirect(w, r, "/setup")
		return
	}
	p := page{Title: "Sign in"}
	if r.Method == http.MethodPost {
		username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
		if !s.attempts.allow("login:" + username) {
			s.fail(w, r, 429, "Too many sign-in attempts. Try again in a minute.")
			return
		}
		actor, err := s.i.AuthenticatePassword(r.Context(), username, r.FormValue("password"))
		if err != nil {
			p.Error = "The username or password is incorrect."
		} else {
			u, err := s.i.GetUser(r.Context(), actor.UserID)
			if err != nil {
				s.internal(w, r, err)
				return
			}
			if u.Disabled || u.AuthVersion != actor.AuthVersion {
				s.fail(w, r, 401, "The account changed while signing in. Try again.")
				return
			}
			keys, err := s.security.Keys(r.Context(), u.ID)
			if err != nil {
				s.internal(w, r, err)
				return
			}
			if err := s.startSession(r, u, len(keys) > 0); err != nil {
				s.internal(w, r, err)
				return
			}
			if len(keys) > 0 {
				redirect(w, r, "/verify")
			} else if u.ForcePasswordChange {
				redirect(w, r, "/settings/security")
			} else {
				redirect(w, r, "/")
			}
			return
		}
	}
	status := 200
	if p.Error != "" {
		status = 401
	}
	s.render(w, r, "login", p, status)
}

// passkey handles discoverable and username-first passkey sign-in. The browser
// session binding keeps each one-use challenge tied to the client.
func (s *Server) passkey(w http.ResponseWriter, r *http.Request) {
	step := r.PathValue("step")
	if step != "begin" && step != "finish" {
		http.NotFound(w, r)
		return
	}
	binding := s.sessions.GetString(r.Context(), "binding")
	if binding == "" {
		binding = project.NewID()
		s.sessions.Put(r.Context(), "binding", binding)
	}
	if !s.attempts.allow("passkey:" + binding) {
		s.fail(w, r, 429, "Too many sign-in attempts. Try again in a minute.")
		return
	}
	if step == "begin" {
		var input struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "Passkey sign-in could not be started.", http.StatusBadRequest)
			return
		}
		id, options, err := s.security.BeginPasskey(r.Context(), binding, input.Username)
		if err != nil {
			if errors.Is(err, instance.ErrUnauthorized) {
				http.Error(w, "The account or passkey could not be verified.", http.StatusUnauthorized)
				return
			}
			http.Error(w, "Passkey sign-in is temporarily unavailable.", http.StatusServiceUnavailable)
			return
		}
		jsonReply(w, struct {
			ID      string `json:"id"`
			Options any    `json:"options"`
		}{id, options})
		return
	}
	u, err := s.security.FinishPasskey(r.Context(), binding, r.URL.Query().Get("id"), r)
	if err != nil {
		http.Error(w, "The passkey could not be verified. Try again.", http.StatusUnauthorized)
		return
	}
	if err := s.startSession(r, u, false); err != nil {
		s.internal(w, r, err)
		return
	}
	jsonReply(w, struct {
		Next string `json:"next"`
	}{"/"})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(r, true); !ok {
		redirect(w, r, "/login")
		return
	}
	if !s.sessions.GetBool(r.Context(), "pending") {
		redirect(w, r, "/")
		return
	}
	s.render(w, r, "verify", page{Title: "One more step"}, 200)
}

func (s *Server) complete(r *http.Request) error {
	if err := s.sessions.RenewToken(r.Context()); err != nil {
		return err
	}
	s.sessions.Put(r.Context(), "pending", false)
	s.sessions.Put(r.Context(), "authenticated", time.Now().Unix())
	return nil
}

func (s *Server) recover(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(r, true)
	if !ok || !s.sessions.GetBool(r.Context(), "pending") {
		redirect(w, r, "/login")
		return
	}
	if !s.attempts.allow("recovery:" + u.ID) {
		s.fail(w, r, 429, "Too many attempts. Try again in a minute.")
		return
	}
	if err := s.security.UseRecovery(r.Context(), u.ID, r.FormValue("code")); err != nil {
		s.render(w, r, "verify", page{Title: "One more step", Error: "That recovery code is invalid or has already been used."}, 401)
		return
	}
	if err := s.complete(r); err != nil {
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/settings/security?recovered=1")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.sessions.Destroy(r.Context()); err != nil {
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/login")
}

func (s *Server) ceremony(w http.ResponseWriter, r *http.Request) {
	purpose := r.PathValue("purpose")
	u, ok := s.current(r, purpose == "login")
	pending := s.sessions.GetBool(r.Context(), "pending")
	if !ok || purpose == "login" && !pending || purpose == "register" && (pending || u.ForcePasswordChange || time.Now().Unix()-s.sessions.GetInt64(r.Context(), "authenticated") > 600) {
		http.Error(w, "Sign in again to continue.", 401)
		return
	}
	if purpose != "login" && purpose != "register" {
		http.NotFound(w, r)
		return
	}
	binding := s.sessions.GetString(r.Context(), "binding")
	if r.PathValue("step") == "begin" {
		id, options, err := s.security.BeginWithMode(r.Context(), u.ID, binding, purpose, r.URL.Query().Get("mode"))
		if err != nil {
			http.Error(w, "Unable to start a security key challenge.", 400)
			return
		}
		jsonReply(w, struct {
			ID      string `json:"id"`
			Options any    `json:"options"`
		}{id, options})
		return
	}
	if r.PathValue("step") != "finish" {
		http.NotFound(w, r)
		return
	}
	var codes []string
	var err error
	if purpose == "register" {
		codes, err = s.security.FinishRegistration(r.Context(), u.ID, binding, r.URL.Query().Get("id"), r.URL.Query().Get("name"), r)
		if err == nil {
			err = s.refresh(r)
		}
	} else {
		err = s.security.FinishLogin(r.Context(), u.ID, binding, r.URL.Query().Get("id"), r)
		if err == nil {
			err = s.complete(r)
		}
	}
	if err != nil {
		http.Error(w, "The security key response could not be verified. Try again.", 400)
		return
	}
	jsonReply(w, struct {
		Codes []string `json:"codes"`
		Next  string   `json:"next"`
	}{codes, "/settings/security"})
}

type securityView struct {
	Keys                        []instance.Key
	Codes                       []string
	Welcome, Recovered, Changed bool
}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request, u instance.User, message string, codes []string) {
	keys, err := s.security.Keys(r.Context(), u.ID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.render(w, r, "security", page{Title: "Account security", Section: "security", User: u, Error: message, Data: securityView{Keys: keys, Codes: codes, Welcome: r.URL.Query().Has("welcome"), Recovered: r.URL.Query().Has("recovered"), Changed: r.URL.Query().Has("changed")}}, 200)
}

func (s *Server) accountSecurity(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	s.securityPage(w, r, u, "", nil)
}

func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.fresh(w, r) {
		return
	}
	if !s.attempts.allow("password:" + u.ID) {
		s.fail(w, r, 429, "Too many password attempts. Try again in a minute.")
		return
	}
	_, err := s.i.AuthenticatePassword(r.Context(), u.Username, r.FormValue("current"))
	if err != nil {
		s.securityPage(w, r, u, "The current password is incorrect.", nil)
		return
	}
	if r.FormValue("password") != r.FormValue("confirm") {
		s.securityPage(w, r, u, "The new passwords do not match.", nil)
		return
	}
	if _, err = s.i.SetPassword(r.Context(), u.ID, r.FormValue("password")); err != nil {
		s.securityPage(w, r, u, "Use a password between 12 and 1,024 bytes.", nil)
		return
	}
	if err := s.refresh(r); err != nil {
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/settings/security?changed=1")
}

func (s *Server) removeKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.fresh(w, r) {
		return
	}
	if err := s.security.RemoveKey(r.Context(), u.ID, r.FormValue("id")); err != nil {
		s.internal(w, r, err)
		return
	}
	if err := s.refresh(r); err != nil {
		s.internal(w, r, err)
		return
	}
	redirect(w, r, "/settings/security")
}

func (s *Server) recoveryCodes(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r)
	if !ok {
		return
	}
	if !s.fresh(w, r) {
		return
	}
	codes, err := s.security.Recovery(r.Context(), u.ID)
	if err != nil {
		if errors.Is(err, instance.ErrUnauthorized) {
			s.fail(w, r, 403, "Sign in again.")
		} else {
			s.securityPage(w, r, u, "Add a security key first.", nil)
		}
		return
	}
	if err := s.refresh(r); err != nil {
		s.internal(w, r, err)
		return
	}
	s.securityPage(w, r, u, "", codes)
}
