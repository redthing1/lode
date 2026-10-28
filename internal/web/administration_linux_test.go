//go:build linux

package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/instance"
)

func TestRegistrationDefaultsClosedAndCannotBypassSetup(t *testing.T) {
	inst, origin, code := testServer(t)
	guest := newBrowser(t, origin)
	wantStatus(t, guest.get("/register"), http.StatusNotFound)
	wantStatus(t, guest.post("/register", postFields("username", "guest", "password", "guest secure password", "confirm", "guest secure password")), http.StatusNotFound)
	if _, err := inst.Register(context.Background(), "guest", "guest secure password"); !errors.Is(err, instance.ErrUnauthorized) {
		t.Fatalf("closed registration accepted: %v", err)
	}
	inst.Config.RegistrationOpen = true
	if response := guest.get("/register"); response.status != http.StatusSeeOther || response.header.Get("Location") != "/setup" {
		t.Fatalf("signup before setup: %+v", response)
	}
	if _, err := inst.Setup(context.Background(), code, "admin", "admin secure password"); err != nil {
		t.Fatal(err)
	}
	if response := guest.get("/login"); response.status != http.StatusOK || !strings.Contains(response.body, `href="/register"`) {
		t.Fatal("open registration not available from sign in")
	}
	wantStatus(t, guest.post("/register", postFields("username", "guest", "password", "guest secure password", "confirm", "wrong")), http.StatusBadRequest)
	wantStatus(t, guest.post("/register", postFields("username", "guest", "password", "guest secure password", "confirm", "guest secure password")), http.StatusSeeOther)
	user, err := inst.LookupUser(context.Background(), "guest")
	if err != nil || user.Admin || user.Disabled || user.ForcePasswordChange {
		t.Fatalf("registered account: %+v %v", user, err)
	}
	created := guest.post("/projects/new", postFields("slug", "assets"))
	if created.status != http.StatusSeeOther || created.header.Get("Location") != "/guest/assets" {
		t.Fatalf("signup to project: %+v", created)
	}
	wantStatus(t, guest.get("/-/admin/users"), http.StatusForbidden)
	wantStatus(t, guest.get("/-/admin/projects"), http.StatusForbidden)
}

func TestAdministrationResetDisableAndProjectRemoval(t *testing.T) {
	ctx := context.Background()
	inst, origin, code := testServer(t)
	admin := newBrowser(t, origin)
	wantStatus(t, admin.post("/setup", postFields("code", code, "username", "admin", "password", "admin secure password")), http.StatusSeeOther)
	user, err := inst.Store.Register(ctx, "member", "member secure password")
	if err != nil {
		t.Fatal(err)
	}
	member := newBrowser(t, origin)
	wantStatus(t, member.post("/login", postFields("username", "member", "password", "member secure password")), http.StatusSeeOther)
	wantStatus(t, member.post("/projects/new", postFields("slug", "assets")), http.StatusSeeOther)
	info, err := inst.Project(ctx, "member", "assets")
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := inst.CreateToken(ctx, user.ID, "client", instance.RoleWriter, info.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if response := admin.get("/-/admin/projects"); response.status != http.StatusOK || !strings.Contains(response.body, "member/assets") {
		t.Fatal("admin project list missing owned project")
	}
	reset := admin.post("/-/admin/users/member", postFields("action", "reset"))
	wantStatus(t, reset, http.StatusOK)
	password := secretFromPage(t, reset.body)
	if strings.Contains(admin.get("/-/admin/users").body, password) {
		t.Fatal("reset secret persists")
	}
	wantStatus(t, member.get("/member/assets"), http.StatusSeeOther)
	if _, err := inst.AuthenticateToken(ctx, secret); !errors.Is(err, instance.ErrUnauthorized) {
		t.Fatalf("reset retained token: %v", err)
	}
	wantStatus(t, member.post("/login", postFields("username", "member", "password", password)), http.StatusSeeOther)
	if response := member.get("/member/assets"); response.header.Get("Location") != "/settings/security" {
		t.Fatal("reset did not require password change")
	}
	wantStatus(t, admin.post("/-/admin/users/member", postFields("action", "state", "disabled", "on")), http.StatusSeeOther)
	if _, err := inst.Project(ctx, "member", "assets"); err != nil {
		t.Fatalf("disable removed owned project: %v", err)
	}
	wantStatus(t, admin.get("/member/assets/delete"), http.StatusOK)
	wantStatus(t, admin.post("/member/assets/delete", postFields("confirm", "wrong")), http.StatusOK)
	if _, err := inst.Project(ctx, "member", "assets"); err != nil {
		t.Fatal("wrong confirmation deleted project")
	}
	wantStatus(t, admin.post("/member/assets/delete", postFields("confirm", "member/assets")), http.StatusSeeOther)
	if _, err := inst.Project(ctx, "member", "assets"); !errors.Is(err, instance.ErrNotFound) {
		t.Fatalf("project still registered: %v", err)
	}
}
