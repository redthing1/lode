package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestSignInSourceTrust(t *testing.T) {
	l := signInLimit{trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/24")}}
	for _, tc := range []struct{ peer, forwarded, want string }{
		{"192.0.2.1:1234", "192.0.2.99", "192.0.2.1"},
		{"127.0.0.1:1234", "192.0.2.1", "192.0.2.1"},
		{"127.0.0.1:1234", "192.0.2.99, 192.0.2.1, 10.0.0.2", "192.0.2.1"},
		{"127.0.0.1:1234", "192.0.2.99, invalid", "127.0.0.1"},
		{"127.0.0.1:1234", "", "127.0.0.1"},
		{"[::ffff:192.0.2.1]:1234", "192.0.2.99", "192.0.2.1"},
		{"[2001:db8:1:2::99]:1234", "", "2001:db8:1:2::"},
	} {
		r := httptest.NewRequest("POST", "/login", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.forwarded)
		if got := l.source(r).String(); got != tc.want {
			t.Fatalf("peer %s, forwarded %q: %s, want %s", tc.peer, tc.forwarded, got, tc.want)
		}
	}
}

func TestSignInLimitBounds(t *testing.T) {
	l := signInLimit{}
	r := httptest.NewRequest("POST", "/login", nil)
	for i := range 4096 {
		r.RemoteAddr = fmt.Sprintf("192.0.%d.%d:1234", i/256, i%256)
		if !l.allow(r) {
			t.Fatal("new source refused before capacity")
		}
	}
	r.RemoteAddr = "198.51.100.1:1234"
	if l.allow(r) || len(l.sources) != 4096 {
		t.Fatal("active limit was evicted or capacity exceeded")
	}
	// A fully replenished limit can be forgotten without granting extra attempts.
	ip := netip.MustParseAddr("192.0.0.0")
	l.sources[ip] = rate.NewLimiter(2, 10)
	l.sources[ip].AllowN(time.Now().Add(-time.Minute), 10)
	if !l.allow(r) || len(l.sources) != 4096 {
		t.Fatal("replenished limit did not make room")
	}
}

func TestAuthenticationThrottleIsolation(t *testing.T) {
	f := apiTestHandler(t, func(h http.Handler) http.Handler {
		s := h.(*Server)
		s.attempts.trusted = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
		return h
	})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	request := func(path, source string, want int) {
		t.Helper()
		form := url.Values{"username": {"alice"}, "password": {browserPassword}}
		r, err := http.NewRequest("POST", f.origin+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Forwarded-For", source)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s from %s: %d, want %d", path, source, resp.StatusCode, want)
		}
	}
	for range 10 {
		request("/login/key/finish", "192.0.2.1", 400)
	}
	for _, path := range []string{"/login", "/login/key", "/login/key/finish", "/verify/recovery", "/verify/key", "/verify/key/finish"} {
		request(path, "192.0.2.1", 429)
	}
	request("/login", "192.0.2.2", 303)
}
