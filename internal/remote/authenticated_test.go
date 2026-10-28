//go:build linux

package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redthing1/lode/internal/project"
)

func TestRequiredAccessIsClosedAndMethodSpecific(t *testing.T) {
	cases := []struct {
		path, method string
		want         Access
		ok           bool
	}{
		{"/v1/entries", "POST", Read, true}, {"/v1/entries", "GET", "", false}, {"/v1/read", "POST", Read, true}, {"/v1/save", "POST", Write, true},
		{"/v1/branches", "POST", Write, true}, {"/v1/tags", "DELETE", Write, true},
		{"/v1/cancel/abc", "POST", Write, true}, {"/v1/cleanup/preview", "POST", Manage, true}, {"/v1/collect", "POST", Manage, true},
		{"/v1/collect", "GET", "", false}, {"/v1/cleanup/apply", "DELETE", "", false}, {"/v1/future", "GET", "", false},
	}
	for _, tc := range cases {
		got, ok := requiredAccess(tc.path, tc.method)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s %s = %q,%v; want %q,%v", tc.method, tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAuthenticatedHandlerNoSharedTokenFallback(t *testing.T) {
	p, err := project.Create(context.Background(), t.TempDir()+"/p")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	calls := 0
	h := AuthenticatedHandler(p, func(r *http.Request, a Access) (project.Actor, error) {
		calls++
		if a != Read {
			t.Fatalf("access=%s", a)
		}
		if r.URL.Path != "/v1/info" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		return project.Actor{ID: "stable-user", Name: "alice"}, nil
	})
	r := httptest.NewRequest("GET", "/v1/info", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("authenticated info: status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	calls = 0
	h = AuthenticatedHandler(p, func(*http.Request, Access) (project.Actor, error) {
		calls++
		return project.Actor{}, ErrUnauthenticated
	})
	r = httptest.NewRequest("GET", "/v1/info", nil)
	r.Header.Set("Authorization", "Bearer "+p.Token())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("legacy credential fallback: status=%d calls=%d", w.Code, calls)
	}
}
