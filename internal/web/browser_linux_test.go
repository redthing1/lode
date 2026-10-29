package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

func TestBrowserOnboardingAndProject(t *testing.T) {
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	origin := "http://" + strings.Replace(listener.Addr().String(), "127.0.0.1", "localhost", 1)
	dir := filepath.Join(t.TempDir(), "instance")
	code, err := instance.Initialize(ctx, dir, instance.Config{URL: origin, Listen: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := instance.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewUnstartedServer(New(store))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(browser.CloseIdleConnections)
	request := func(method, path string, values url.Values, from string, want int) string {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, origin+path, strings.NewReader(values.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		if method == "POST" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", from)
		}
		response, err := browser.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		b, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: %d, expected %d\n%s", method, path, response.StatusCode, want, b)
		}
		return string(b)
	}
	request("GET", "/", nil, "", 303)
	request("GET", "/login", nil, "", 303)
	if body := request("GET", "/setup", nil, "", 200); !strings.Contains(body, "Create administrator") {
		t.Fatal("missing setup form")
	}
	request("POST", "/setup", url.Values{"code": {code}, "username": {"alice"}, "password": {"a sufficiently long test password"}}, origin, 303)
	request("GET", "/setup", nil, "", 303)
	request("POST", "/projects/new", url.Values{"name": {"blocked"}}, "https://other.example", 403)
	request("POST", "/projects/new", url.Values{"name": {"project"}}, origin, 303)
	if body := request("GET", "/alice/project", nil, "", 200); !strings.Contains(body, "lode attach "+origin+"/alice/project") {
		t.Fatal("missing initial CLI instructions")
	}
	body := request("POST", "/settings/tokens", url.Values{"name": {"workstation"}, "project": {""}, "role": {"writer"}, "days": {"30"}}, origin, 200)
	match := regexp.MustCompile(`<pre class="token">([^<]+)</pre>`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("token not displayed")
	}
	token := match[1]
	if body := request("GET", "/settings/tokens", nil, "", 200); strings.Contains(body, token) {
		t.Fatal("token displayed a second time")
	}
	client, err := remote.New(origin, token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTP.CloseIdleConnections()
	client.InstanceID = store.ID
	p, err := client.Project(ctx, "alice", "project")
	if err != nil {
		t.Fatal(err)
	}
	client.ProjectID = p.ID
	data := map[string]string{
		"README.md":   "# Useful project\n\n[Text](notes.txt)\n\n<script>alert('x')</script>\n\n![remote](https://other.example/image.png)\n",
		"notes.txt":   "exact text <script>not markup</script>",
		"odd ?#&.bin": "opaque\x00\xff",
	}
	changes := []model.Entry{}
	for name, bytes := range data {
		changes = append(changes, entry(name, []byte(bytes)))
	}
	v, err := client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Message: "First files", Changes: changes}, nil, func(e model.Entry) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data[e.Path])), nil })
	if err != nil {
		t.Fatal(err)
	}
	body = request("GET", "/alice/project", nil, "", 200)
	if !strings.Contains(body, "<h1>Useful project</h1>") || strings.Contains(body, "<script>alert") || strings.Contains(body, "other.example/image") {
		t.Fatal("unsafe or missing Markdown preview")
	}
	if !strings.Contains(body, "path=notes.txt") || !strings.Contains(body, "version="+v.ID) {
		t.Fatal("relative link is not version-bound")
	}
	body = request("GET", "/alice/project?path=notes.txt", nil, "", 200)
	if !strings.Contains(body, "&lt;script&gt;not markup&lt;/script&gt;") {
		t.Fatal("text preview was not escaped")
	}
	u := url.URL{Path: "/alice/project/raw/" + v.ID + "/odd ?#&.bin"}
	if body := request("GET", u.String(), nil, "", 200); body != data["odd ?#&.bin"] {
		t.Fatal("download bytes differ")
	}
	if body := request("GET", "/alice/project/history", nil, "", 200); !strings.Contains(body, "First files") {
		t.Fatal("missing history")
	}
	request("GET", "/settings/appearance", nil, "", 200)
	request("POST", "/logout", nil, "https://other.example", 403)
	request("GET", "/", nil, "", 200)
	request("POST", "/logout", nil, origin, 303)
	request("GET", "/alice/project", nil, "", 303)
	request("POST", "/login", url.Values{"username": {"alice"}, "password": {"a sufficiently long test password"}}, origin, 303)
	request("GET", "/alice/project", nil, "", 200)
	// A browser can use two instances on different ports of the same host.
	// Their cookies share a jar but must not replace one another's session.
	other := apiTest(t)
	response, err := browser.PostForm(other.origin+"/login", url.Values{"username": {"alice"}, "password": {"a sufficiently long test password"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("second instance login: %d", response.StatusCode)
	}
	request("GET", "/alice/project", nil, "", 200)
	user, err := store.Password(ctx, "alice", "a sufficiently long test password")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := store.Tokens(ctx, user)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens: %v %v", tokens, err)
	}
	request("POST", "/settings/tokens/"+tokens[0].ID+"/revoke", nil, origin, 303)
	if _, err := client.Me(ctx); err == nil {
		t.Fatal("revoked token accepted")
	}
}
