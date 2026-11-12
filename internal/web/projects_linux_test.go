package web

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
)

func TestProjectListSummariesSortingAndAccess(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	alice := newBrowserSession(t, f.origin)
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	alice.request("POST", "/projects/new", url.Values{"name": {"alpha"}}, 303)
	if _, err := f.client.CreateBranch(ctx, "alice", "project", model.Reference{Name: "experiment"}); err != nil {
		t.Fatal(err)
	}
	// Activity on any branch belongs in the overview, even while main is empty.
	save := model.Save{
		ID: operation(t), Branch: "experiment", Message: "A saved version",
		Changes: []model.Entry{entry("hello.txt", []byte("hello"))},
	}
	v, err := f.client.Save(ctx, "alice", "project", save, nil, func(model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("hello")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body := alice.request("GET", "/", nil, 200)
	for _, want := range []string{`class="project-count muted" aria-live="polite">2</span>`, "No saved versions", "data-relative-time", v.Created.UTC().Format("2006-01-02"), `data-storage="/alice/project/usage"`, "Recently updated"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing list detail %q", want)
		}
	}
	projectOrder := func(body, first, second string) {
		t.Helper()
		a, b := strings.Index(body, " / <strong>"+first+"</strong>"), strings.Index(body, " / <strong>"+second+"</strong>")
		if a < 0 || b < 0 || a >= b {
			t.Fatalf("expected %s before %s", first, second)
		}
	}
	projectOrder(body, "project", "alpha")
	projectOrder(alice.request("GET", "/?sort=name", nil, 200), "alpha", "project")
	projectOrder(alice.request("GET", "/?sort=unknown", nil, 200), "project", "alpha")
	projectOrder(alice.request("GET", "/alice?sort=name", nil, 200), "alpha", "project")
	for _, route := range []string{"/?q=ALPHA", "/alice?q=alice%2Falpha&sort=name"} {
		body := alice.request("GET", route, nil, 200)
		if !strings.Contains(body, `data-project-name="alice/alpha">`) || !strings.Contains(body, `data-project-name="alice/project" hidden>`) {
			t.Fatal("filter did not match owner/name case-insensitively")
		}
	}
	if body := alice.request("GET", "/alice?q=missing", nil, 200); !strings.Contains(body, "No matching projects.") || !strings.Contains(body, "Clear filter") {
		t.Fatal("missing empty filter state")
	}
	body = alice.request("GET", "/alice/project?branch=experiment", nil, 200)
	if !strings.Contains(body, `class="version-id"`) || !strings.Contains(body, `datetime="`+v.Created.UTC().Format("2006-01-02T15:04:05Z07:00")) || !strings.Contains(body, `title="`+v.Created.UTC().Format("2006-01-02 15:04 UTC")) {
		t.Fatal("latest version lacks glyph, machine-readable time, or exact tooltip")
	}
	if body := alice.request("GET", "/alice/project/history?branch=experiment", nil, 200); !strings.Contains(body, "data-relative-time") {
		t.Fatal("history lacks activity time")
	}
	if _, err := f.store.CreateUser(ctx, instance.Principal{User: f.user}, "bob", browserPassword); err != nil {
		t.Fatal(err)
	}
	bob := newBrowserSession(t, f.origin)
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {browserPassword}}, 303)
	if body := bob.request("GET", "/", nil, 200); strings.Contains(body, "/alice/") || !strings.Contains(body, `class="project-count muted" aria-live="polite">0</span>`) {
		t.Fatal("private summaries exposed")
	}
	alice.request("POST", "/alice/project/settings/members", url.Values{"username": {"bob"}, "role": {"reader"}}, 303)
	body = bob.request("GET", "/", nil, 200)
	if !strings.Contains(body, `class="project-count muted" aria-live="polite">1</span>`) || !strings.Contains(body, "data-relative-time") || strings.Contains(body, "/alice/alpha") {
		t.Fatal("reader list has incorrect scope")
	}
	bob.request("GET", "/alice/project/usage", nil, 200)
	alice.request("POST", "/alice/project/settings/members/bob/remove", nil, 303)
	if body := bob.request("GET", "/", nil, 200); strings.Contains(body, "/alice/project") {
		t.Fatal("revoked project summary exposed")
	}
}
