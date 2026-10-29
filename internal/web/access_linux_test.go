package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

type browserSession struct {
	t      *testing.T
	origin string
	client *http.Client
}

func newBrowserSession(t *testing.T, origin string) *browserSession {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	return &browserSession{t, origin, client}
}
func (b *browserSession) request(method, path string, form url.Values, want int) string {
	b.t.Helper()
	r, err := http.NewRequest(method, b.origin+path, strings.NewReader(form.Encode()))
	if err != nil {
		b.t.Fatal(err)
	}
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", b.origin)
	}
	response, err := b.client.Do(r)
	if err != nil {
		b.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	if response.StatusCode != want {
		b.t.Fatalf("%s %s: %d expected %d\n%s", method, path, response.StatusCode, want, data)
	}
	return string(data)
}
func browserToken(t *testing.T, b *browserSession, project, role string) string {
	t.Helper()
	body := b.request("POST", "/settings/tokens", url.Values{"name": {"workstation"}, "project": {project}, "role": {role}, "days": {"30"}}, 200)
	match := regexp.MustCompile(`<pre class="token">([^<]+)</pre>`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("token missing")
	}
	return match[1]
}

func TestBrowserSharingAndCurrentTokenAuthority(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	alice, bob := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	password := "a sufficiently long test password"
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {password}}, 303)
	alice.request("GET", "/admin/users", nil, 200)
	alice.request("POST", "/admin/users/new", url.Values{"username": {"bob"}, "password": {password}}, 303)
	body := alice.request("POST", "/admin/users/new", url.Values{"username": {"bob"}, "password": {password}}, 400)
	if !strings.Contains(body, "already in use") || strings.Contains(body, password) {
		t.Fatal("invalid account form feedback")
	}
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {password}}, 303)
	bob.request("GET", "/admin/users", nil, 403)
	bob.request("GET", "/admin/users/new", nil, 403)
	bob.request("POST", "/admin/users/new", url.Values{"username": {"eve"}, "password": {password}}, 403)
	if body := bob.request("GET", "/bob", nil, 200); !strings.Contains(body, "No projects yet.") || !strings.Contains(body, `href="/projects/new"`) {
		t.Fatal("empty owner page missing project creation")
	}
	bob.request("POST", "/projects/new", url.Values{"name": {"own"}}, 303)
	bob.request("GET", "/bob/own", nil, 200)
	if body := bob.request("GET", "/", nil, 200); strings.Contains(body, "/alice/project") {
		t.Fatal("unshared project listed")
	}
	bob.request("GET", "/alice/project", nil, 403)
	bob.request("GET", "/alice", nil, 404)
	bob.request("GET", "/unknown", nil, 404)
	members := "/alice/project/settings/members"
	alice.request("POST", members, url.Values{"username": {"bob"}, "role": {"writer"}}, 303)
	if body := bob.request("GET", "/", nil, 200); !strings.Contains(body, "/alice/project") {
		t.Fatal("shared project missing")
	}
	if body := bob.request("GET", "/alice", nil, 200); !strings.Contains(body, `href="/alice/project"`) || strings.Contains(body, `href="/bob/own"`) || strings.Contains(body, `href="/projects/new"`) {
		t.Fatal("owner page did not restrict its projects and creation action")
	}
	if body := bob.request("GET", "/alice/project", nil, 200); strings.Contains(body, `href="`+members+`"`) {
		t.Fatal("writer offered settings")
	}
	if body := bob.request("GET", "/settings/tokens", nil, 200); !strings.Contains(body, ">alice/project</label>") {
		t.Fatal("shared project scope missing")
	}
	token := browserToken(t, bob, f.project.ID, "writer")
	c, err := remote.New(f.origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	c.InstanceID = f.store.ID
	c.ProjectID = f.project.ID
	r := model.Save{ID: operation(t), Branch: "main", Message: "Bob's first save", Changes: []model.Entry{entry("a", []byte("Bob's contents"))}}
	v, err := c.Save(ctx, "alice", "project", r, nil, func(model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("Bob's contents")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bobUser, err := f.store.Password(ctx, "bob", password)
	if err != nil || v.Actor != bobUser.ID {
		t.Fatalf("actor binding: %v %v", v, err)
	}
	if body := alice.request("GET", "/alice/project/history", nil, 200); !strings.Contains(body, "bob · ") {
		t.Fatal("history author missing")
	}
	bob.request("GET", members, nil, 403)
	bob.request("POST", members, url.Values{"username": {"bob"}, "role": {"maintainer"}}, 403)
	alice.request("POST", members, url.Values{"username": {"bob"}, "role": {"reader"}}, 303)
	if recovered, err := c.Resolve(ctx, "alice", "project", r); err != nil || recovered != v {
		t.Fatalf("demoted writer's accepted outcome: %+v %v", recovered, err)
	}
	bob.request("GET", "/alice/project", nil, 200)
	bob.request("GET", "/alice/project/raw/"+v.ID+"/a", nil, 200)
	r.ID = operation(t)
	r.Base = v.ID
	r.Changes = []model.Entry{entry("a", []byte("forbidden"))}
	opened := false
	_, err = c.Save(ctx, "alice", "project", r, nil, func(model.Entry) (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(strings.NewReader("forbidden")), nil
	})
	var denial *remote.Error
	if !errors.As(err, &denial) || denial.Status != 403 || opened {
		t.Fatalf("demoted token: %v source opened %v", err, opened)
	}
	alice.request("POST", members, url.Values{"username": {"bob"}, "role": {"maintainer"}}, 303)
	bob.request("GET", members, nil, 200)
	bob.request("POST", members, url.Values{"username": {"alice"}, "role": {"reader"}}, 400)
	bob.request("POST", members+"/alice/remove", nil, 400)
	alice.request("POST", members+"/bob/remove", nil, 303)
	bob.request("GET", "/alice/project", nil, 403)
	bob.request("GET", "/alice", nil, 404)
	bob.request("GET", "/alice/project/history", nil, 403)
	bob.request("GET", "/alice/project/raw/"+v.ID+"/a", nil, 403)
	if _, err := c.Head(ctx, "alice", "project", "main"); !errors.As(err, &denial) || denial.Status != 403 {
		t.Fatalf("removed member API read: %v", err)
	}
	bob.request("GET", "/bob/own", nil, 200)
	if body := bob.request("GET", "/", nil, 200); strings.Contains(body, "/alice/project") {
		t.Fatal("removed project listed")
	}
}
