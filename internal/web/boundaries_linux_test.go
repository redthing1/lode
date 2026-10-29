package web

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Exercise the real connection deadlines without making the test wait 30 seconds.
type shortDeadlineWriter struct{ http.ResponseWriter }

func (w shortDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w shortDeadlineWriter) SetReadDeadline(t time.Time) error {
	if !t.IsZero() {
		t = time.Now().Add(100 * time.Millisecond)
	}
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(t)
}

func TestBrowserRequestBoundaries(t *testing.T) {
	f := apiTestHandler(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(shortDeadlineWriter{w}, r) })
	})
	b := newBrowserSession(t, f.origin)
	b.request("GET", "/assets/style.css", nil, 200)
	b.request("GET", "/assets/%2e%2e/templates/auth.html", nil, 404)
	b.request("GET", "/assets/%2e%2e/%2e%2e/state/config.json", nil, 404)

	u, _ := url.Parse(f.origin)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nusername=", u.Host); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal("form did not time out cleanly:", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("unfinished form: %d", resp.StatusCode)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestSignInSessionDeadlines(t *testing.T) {
	f := apiTest(t)
	b := newBrowserSession(t, f.origin)
	beginBrowserKey(t, b, "/login/key", nil)
	sessions := New(f.store).sessions
	ctx, _ := loadedBrowserSession(t, b, sessions)
	if remaining := time.Until(sessions.Deadline(ctx)); remaining <= 0 || remaining > 5*time.Minute {
		t.Fatalf("anonymous deadline: %s", remaining)
	}
	b.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	ctx, _ = loadedBrowserSession(t, b, sessions)
	if remaining := time.Until(sessions.Deadline(ctx)); remaining < 23*time.Hour {
		t.Fatalf("authenticated deadline: %s", remaining)
	}
	enrollBrowserKey(t, b, newSyntheticAuthenticator(t, "deadline-key"), "passkey")
	b.request("POST", "/logout", nil, 303)
	b.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	ctx, _ = loadedBrowserSession(t, b, sessions)
	if remaining := time.Until(sessions.Deadline(ctx)); remaining <= 0 || remaining > 5*time.Minute {
		t.Fatalf("pending deadline: %s", remaining)
	}
}
