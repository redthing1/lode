package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/remote"
)

const browserPassword = "a sufficiently long test password"
const changedPassword = "a different sufficiently long password"

func accountGeneration(t *testing.T, b *browserSession, name string) string {
	t.Helper()
	body := b.request("GET", "/admin/users/"+name+"/edit", nil, 200)
	match := regexp.MustCompile(`name="generation" value="([0-9]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("account form has no generation")
	}
	return match[1]
}

func expectTokenStatus(t *testing.T, origin, token string, status int) {
	t.Helper()
	client, err := remote.New(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTP.CloseIdleConnections()
	_, err = client.Me(context.Background())
	if status == 200 && err == nil {
		return
	}
	var refusal *remote.Error
	if !errors.As(err, &refusal) || refusal.Status != status {
		t.Fatalf("token status: %v, expected %d", err, status)
	}
}

func TestBrowserPasswordChangeInvalidatesSessionsAndTokens(t *testing.T) {
	f := apiTest(t)
	alice, other := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	for _, b := range []*browserSession{alice, other} {
		b.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
		b.request("GET", "/settings/security", nil, 200)
	}
	expectTokenStatus(t, f.origin, f.token, 200)

	// Hold an authenticated request's session before the password change. A late
	// session write must not restore access, even after that session was deleted.
	sessions := New(f.store).sessions
	origin, err := url.Parse(f.origin)
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, candidate := range other.client.Jar.Cookies(origin) {
		if candidate.Name == sessions.Cookie.Name {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("session cookie missing")
	}
	late, err := sessions.Load(context.Background(), cookie.Value)
	if err != nil || sessions.GetString(late, "user") != f.user.ID {
		t.Fatalf("load old session: %v", err)
	}
	sessions.Put(late, "late-request", true)

	for _, form := range []url.Values{
		{"current": {"incorrect password"}, "password": {changedPassword}, "confirm": {changedPassword}},
		{"current": {browserPassword}, "password": {changedPassword}, "confirm": {"different confirmation"}},
		{"current": {browserPassword}, "password": {"short"}, "confirm": {"short"}},
	} {
		body := alice.request("POST", "/settings/security", form, 400)
		if strings.Contains(body, browserPassword) || strings.Contains(body, changedPassword) || strings.Contains(body, "incorrect password") {
			t.Fatal("password echoed in error form")
		}
	}
	expectTokenStatus(t, f.origin, f.token, 200)
	alice.request("POST", "/settings/security", url.Values{"current": {browserPassword}, "password": {changedPassword}, "confirm": {changedPassword}}, 303)
	if body := alice.request("GET", "/login", nil, 200); !strings.Contains(body, "Password changed. Sign in with your new password.") {
		t.Fatal("password-change notice missing")
	}
	alice.request("GET", "/alice/project", nil, 303)
	other.request("GET", "/alice/project", nil, 303)
	expectTokenStatus(t, f.origin, f.token, 401)
	if _, _, err := sessions.Commit(late); err != nil {
		t.Fatal(err)
	}
	other.client.Jar.SetCookies(origin, []*http.Cookie{cookie})
	other.request("POST", "/settings/tokens", url.Values{"name": {"late"}, "role": {"writer"}, "days": {"30"}}, 303)
	other.request("GET", "/alice/project", nil, 303)

	// Authentication may finish before reset while its response creates a new
	// session afterward. The captured account generation must still be refused.
	lateLogin, err := sessions.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sessions.Put(lateLogin, "user", f.user.ID)
	sessions.Put(lateLogin, "generation", f.user.Generation)
	staleToken, _, err := sessions.Commit(lateLogin)
	if err != nil {
		t.Fatal(err)
	}
	other.client.Jar.SetCookies(origin, []*http.Cookie{{Name: sessions.Cookie.Name, Value: staleToken, Path: "/"}})
	other.request("GET", "/admin/users", nil, 303)

	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 400)
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {changedPassword}}, 303)
	alice.request("GET", "/alice/project", nil, 200)
	expectTokenStatus(t, f.origin, browserToken(t, alice, f.project.ID, "writer"), 200)
}

func TestBrowserAdministratorAccountLifecycle(t *testing.T) {
	f := apiTest(t)
	alice, bob := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	alice.request("POST", "/admin/users/new", url.Values{"username": {"bob"}, "password": {browserPassword}}, 303)
	if body := alice.request("GET", "/admin/users", nil, 200); !strings.Contains(body, `href="/admin/users/bob/edit"`) {
		t.Fatal("account edit link missing")
	}
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {browserPassword}}, 303)
	bob.request("POST", "/projects/new", url.Values{"name": {"kept"}}, 303)
	bob.request("GET", "/admin/users/alice/edit", nil, 403)
	bob.request("POST", "/admin/users/bob/edit", url.Values{"generation": {"1"}, "admin": {"1"}}, 403)
	oldToken := browserToken(t, bob, "", "writer")

	for _, form := range []url.Values{
		{"generation": {accountGeneration(t, alice, "alice")}, "enabled": {"1"}},
		{"generation": {accountGeneration(t, alice, "alice")}, "admin": {"1"}},
	} {
		if body := alice.request("POST", "/admin/users/alice/edit", form, 400); !strings.Contains(body, "keep at least one enabled administrator") {
			t.Fatal("last-administrator feedback missing")
		}
	}
	form := url.Values{"generation": {accountGeneration(t, alice, "bob")}, "enabled": {"1"}, "password": {changedPassword}, "confirm": {changedPassword}}
	alice.request("POST", "/admin/users/bob/edit", form, 303)
	if body := alice.request("POST", "/admin/users/bob/edit", form, 409); !strings.Contains(body, "account changed") || strings.Contains(body, changedPassword) {
		t.Fatal("stale form feedback or password echo")
	}
	bob.request("GET", "/bob/kept", nil, 303)
	expectTokenStatus(t, f.origin, oldToken, 401)
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {changedPassword}}, 303)
	bob.request("GET", "/bob/kept", nil, 200)
	resetToken := browserToken(t, bob, "", "writer")
	alice.request("POST", "/admin/users/bob/edit", url.Values{"generation": {accountGeneration(t, alice, "bob")}}, 303)
	bob.request("GET", "/bob/kept", nil, 303)
	if body := alice.request("GET", "/admin/users", nil, 200); !strings.Contains(body, "Disabled") {
		t.Fatal("disabled account not identified")
	}
	alice.request("POST", "/admin/users/bob/edit", url.Values{"generation": {accountGeneration(t, alice, "bob")}, "enabled": {"1"}}, 303)
	bob.request("GET", "/bob/kept", nil, 303)
	expectTokenStatus(t, f.origin, resetToken, 401)
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {changedPassword}}, 303)
	bob.request("GET", "/bob/kept", nil, 200)

	alice.request("POST", "/admin/users/bob/edit", url.Values{"generation": {accountGeneration(t, alice, "bob")}, "enabled": {"1"}, "admin": {"1"}}, 303)
	bob.request("GET", "/admin/users", nil, 303)
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {changedPassword}}, 303)
	bob.request("GET", "/admin/users", nil, 200)
	alice.request("POST", "/admin/users/alice/edit", url.Values{"generation": {accountGeneration(t, alice, "alice")}, "enabled": {"1"}}, 303)
	if body := alice.request("GET", "/login", nil, 200); !strings.Contains(body, "Account updated. Your session has ended.") {
		t.Fatal("self-demotion notice missing")
	}
	alice.request("GET", "/admin/users", nil, 303)
	expectTokenStatus(t, f.origin, f.token, 401)
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	alice.request("GET", "/admin/users", nil, 403)
	alice.request("GET", "/alice/project", nil, 200)

	// Submitting unchanged settings must not sign out the remaining admin.
	currentToken := browserToken(t, bob, "", "reader")
	bob.request("POST", "/admin/users/bob/edit", url.Values{"generation": {accountGeneration(t, bob, "bob")}, "enabled": {"1"}, "admin": {"1"}}, 303)
	bob.request("GET", "/admin/users", nil, 200)
	expectTokenStatus(t, f.origin, currentToken, 200)
}
