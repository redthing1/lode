package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrivateEndpointIsTokenOnly(t *testing.T) {
	f := apiTest(t)
	f.store.Config.URL = "https://public.example"
	f.store.Config.PrivateURL = "http://private.example:8473"
	handler := New(f.store)
	if !handler.sessions.Cookie.Secure || !strings.HasPrefix(handler.sessions.Cookie.Name, "__Host-") {
		t.Fatal("private HTTP weakened public cookies")
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		host, path, token string
		status            int
	}{
		{"private.example:8473", "/api/me", "", 401},
		{"private.example:8473", "/api/me", "wrong", 401},
		{"private.example:8473", "/api/me", f.token, 200},
		{"private.example:8473", "/api/projects/alice/project", f.token, 200},
		{"private.example:8473", "/healthz", "", 204},
		{"other.example", "/api/me", f.token, 421},
		{"private.example", "/api/me", f.token, 421},
		{"private.example:8473", "/login", f.token, 404},
		{"private.example:8473", "/setup", f.token, 404},
		{"private.example:8473", "/alice/project", f.token, 404},
		{"private.example:8473", "/assets/style.css", f.token, 404},
		{"private.example:8473", "/api/%2e%2e/login", f.token, 404},
		{"public.example", "/login", "", 200},
		{"public.example", "/api/me", f.token, 200},
	} {
		r, err := http.NewRequest("GET", server.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = tc.host
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || tc.host == "private.example:8473" && len(resp.Cookies()) != 0 {
			t.Fatalf("%s%s: status %d, cookies %v", tc.host, tc.path, resp.StatusCode, resp.Cookies())
		}
	}
}
