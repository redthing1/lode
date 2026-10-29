package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/redthing1/lode/internal/instance"
)

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Security", Section: "security", User: actor.User}
	status := 200
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		if r.FormValue("password") != r.FormValue("confirm") {
			p.Error = "New passwords do not match."
		} else {
			err := s.instance.ChangePassword(r.Context(), actor, r.FormValue("current"), r.FormValue("password"))
			if errors.Is(err, instance.ErrPassword) || errors.Is(err, instance.ErrCurrentPassword) {
				p.Error = err.Error()
			} else if err != nil {
				return err
			} else {
				return s.signedOut(w, r, "Password changed. Sign in with your new password.")
			}
		}
		status = 400
	}
	var err error
	p.Keys, err = s.instance.Keys(r.Context(), actor.User)
	if err != nil {
		return err
	}
	render(w, status, "security", p)
	return nil
}

func (s *Server) changeKeys(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	if err := r.ParseForm(); err != nil {
		return err
	}
	var user instance.User
	var codes []string
	var err error
	if key := r.PathValue("key"); key != "" {
		user, err = s.instance.RemoveKey(r.Context(), actor, r.FormValue("current"), key)
	} else {
		user, codes, err = s.instance.RecoveryCodes(r.Context(), actor, r.FormValue("current"))
	}
	if errors.Is(err, instance.ErrCurrentPassword) || errors.Is(err, instance.ErrKey) || errors.Is(err, instance.ErrNoKeys) {
		keys, loadErr := s.instance.Keys(r.Context(), actor.User)
		if loadErr != nil {
			return loadErr
		}
		render(w, 400, "security", page{Title: "Security", Section: "security", User: actor.User, Keys: keys, Error: err.Error()})
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.startSession(r, user, false); err != nil {
		return err
	}
	if len(codes) != 0 {
		render(w, 200, "recovery", page{Title: "Recovery codes", User: user, Codes: codes})
	} else {
		http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
	}
	return nil
}

func (s *Server) signedOut(w http.ResponseWriter, r *http.Request, notice string) error {
	if err := s.sessions.Destroy(r.Context()); err != nil {
		return err
	}
	s.sessions.Put(r.Context(), "notice", notice)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	return nil
}

func (s *Server) userPage(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	p := page{Title: "Edit user", User: actor.User}
	var err error
	p.Subject, err = s.instance.ManagedUser(r.Context(), actor, r.PathValue("user"))
	if err != nil {
		return err
	}
	status := 200
	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		generation, err := strconv.ParseInt(r.FormValue("generation"), 10, 64)
		if err != nil || generation < 1 {
			p.Error = "Reload this account before saving changes."
		} else if r.FormValue("password") != r.FormValue("confirm") {
			p.Error = "New passwords do not match."
		} else {
			err := s.instance.UpdateUser(r.Context(), actor, p.Subject.ID, instance.AccountUpdate{Generation: generation, Admin: r.FormValue("admin") == "1", Disabled: r.FormValue("enabled") != "1", Password: r.FormValue("password")})
			switch {
			case errors.Is(err, instance.ErrAccountChanged):
				p.Error, status = err.Error(), 409
			case errors.Is(err, instance.ErrLastAdministrator), errors.Is(err, instance.ErrPassword):
				p.Error = err.Error()
			case err != nil:
				return err
			default:
				if p.Subject.ID == actor.User.ID {
					current, err := s.instance.User(r.Context(), actor.User.ID)
					if errors.Is(err, instance.ErrCredentials) || err == nil && current.Generation != actor.User.Generation {
						return s.signedOut(w, r, "Account updated. Your session has ended.")
					}
					if err != nil {
						return err
					}
				}
				http.Redirect(w, r, "/admin/users/"+p.Subject.Name+"/edit", http.StatusSeeOther)
				return nil
			}
		}
		if status == 200 {
			status = 400
		}
	}
	render(w, status, "edit-user", p)
	return nil
}
