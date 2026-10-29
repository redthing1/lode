package web

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
)

func TestUsageVisibilityAndOwnership(t *testing.T) {
	f := apiTest(t)
	anonymous := newBrowserSession(t, f.origin)
	anonymous.request("GET", "/alice/project/usage", nil, 303)
	anonymous.request("GET", "/settings/usage?refresh=1", nil, 303)
	alice, bob := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	if _, err := f.store.CreateUser(context.Background(), instance.Principal{User: f.user}, "bob", browserPassword); err != nil {
		t.Fatal(err)
	}
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {browserPassword}}, 303)
	bob.request("GET", "/alice/project/usage", nil, 403)
	alice.request("POST", "/alice/project/settings/members", url.Values{"username": {"bob"}, "role": {"reader"}}, 303)
	if body := bob.request("GET", "/alice/project", nil, 200); !strings.Contains(body, `data-storage="/alice/project/usage"`) || strings.Contains(body, `href="/alice/project/settings/storage"`) {
		t.Fatal("reader storage display missing or linked to inaccessible settings")
	}
	if body := bob.request("GET", "/alice/project/usage", nil, 200); !strings.Contains(body, "Measured ") || strings.Contains(body, "Unavailable") || strings.Contains(body, "on disk") {
		t.Fatal("storage fragment is not measured and compact")
	}
	if body := bob.request("GET", "/settings/usage?refresh=1", nil, 200); strings.Contains(body, "alice") || !strings.Contains(body, "No owned projects.") {
		t.Fatal("shared project attributed to collaborator")
	}
	if body := alice.request("GET", "/settings/usage?refresh=1", nil, 200); !strings.Contains(body, `href="/alice/project"`) || !strings.Contains(body, "Measured ") || strings.Contains(body, "Some sizes are not yet available.") {
		t.Fatal("owner storage total/list not available")
	}
	if body := alice.request("GET", "/settings/usage", nil, 200); !strings.Contains(body, `href="/settings/usage" aria-current="page"`) || !strings.Contains(body, `data-storage="/settings/usage?refresh=1"`) {
		t.Fatal("usage page does not use account settings layout")
	}
	bob.request("GET", "/alice/project/settings/storage", nil, 403)
	alice.request("POST", "/alice/project/settings/members/bob/remove", nil, 303)
	bob.request("GET", "/alice/project/usage", nil, 403)
}
