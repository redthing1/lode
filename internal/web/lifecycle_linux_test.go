package web

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
)

func TestBrowserProjectAdministrationAndDeletion(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	alice, bob := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	password := "a sufficiently long test password"
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {password}}, 303)
	alice.request("POST", "/admin/users/new", url.Values{"username": {"bob"}, "password": {password}}, 303)
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {password}}, 303)
	bob.request("POST", "/projects/new", url.Values{"name": {"own"}}, 303)
	bob.request("GET", "/admin/projects", nil, 403)
	alice.request("POST", "/alice/project/settings/members", url.Values{"username": {"bob"}, "role": {"maintainer"}}, 303)
	if body := bob.request("GET", "/alice/project/settings/members", nil, 200); strings.Contains(body, "/settings/delete") {
		t.Fatal("collaborator offered project deletion")
	}
	bob.request("GET", "/alice/project/settings/delete", nil, 403)
	bob.request("POST", "/alice/project/settings/delete", url.Values{"id": {f.project.ID}, "confirm": {"alice/project"}}, 403)
	if body := alice.request("GET", "/admin/users", nil, 200); !strings.Contains(body, `href="/admin/projects"`) {
		t.Fatal("admin projects navigation missing")
	}
	if body := alice.request("GET", "/admin/projects", nil, 200); !strings.Contains(body, `href="/bob/own"`) {
		t.Fatal("owned project missing from site administration")
	}
	p, err := f.store.Project(ctx, instance.Principal{User: f.user}, "bob", "own", instance.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token := browserToken(t, bob, p.ID, "writer")
	path := "/bob/own/settings/delete"
	if body := bob.request("GET", path, nil, 200); !strings.Contains(body, `value="`+p.ID+`"`) || !strings.Contains(body, "Type bob/own") {
		t.Fatal("deletion confirmation missing identity or name")
	}
	bob.request("POST", path, url.Values{"id": {p.ID}, "confirm": {"own"}}, 400)
	bob.request("POST", path, url.Values{"id": {"stale"}, "confirm": {"bob/own"}}, 409)
	// Exercise the HTTP refusal while a real project callback holds a reference.
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	holdContext, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		done <- f.store.WithProject(ctx, instance.Principal{User: f.user}, "bob", "own", instance.Reader, func(_ instance.Project, _ *project.Store) error {
			close(held)
			select {
			case <-release:
			case <-holdContext.Done():
			}
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatal("hold project", err)
	}
	bob.request("POST", path, url.Values{"id": {p.ID}, "confirm": {"bob/own"}}, 409)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	bob.request("POST", path, url.Values{"id": {p.ID}, "confirm": {"bob/own"}}, 303)
	bob.request("GET", "/bob/own", nil, 404)
	if _, err := f.store.Token(ctx, token); !errors.Is(err, instance.ErrCredentials) {
		t.Fatalf("deleted scope token remains: %v", err)
	}
	bob.request("POST", "/projects/new", url.Values{"name": {"own"}}, 303)
	bob.request("POST", path, url.Values{"id": {p.ID}, "confirm": {"bob/own"}}, 409)
	replacement, err := f.store.Project(ctx, instance.Principal{User: f.user}, "bob", "own", instance.Reader)
	if err != nil {
		t.Fatal(err)
	}
	alice.request("POST", path, url.Values{"id": {replacement.ID}, "confirm": {"bob/own"}}, 303)
	if body := alice.request("GET", "/admin/projects", nil, 200); strings.Contains(body, `href="/bob/own"`) || !strings.Contains(body, `href="/alice/project"`) {
		t.Fatal("admin list did not reflect deletion")
	}
	alice.request("GET", "/alice/project", nil, 200)
}
