package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialExactProjectThenInstanceFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	origin := "https://example.test"
	projectURL := origin + "/alice/art"
	if err := saveCredential(origin, credential{Token: "instance-token"}); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := findCredential(projectURL); err != nil || !ok || c.Token != "instance-token" {
		t.Fatalf("origin fallback: %#v %v %v", c, ok, err)
	}
	if err := saveCredential(projectURL, credential{Token: "project-token"}); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := findCredential(projectURL); err != nil || !ok || c.Token != "project-token" {
		t.Fatalf("project preference: %#v %v %v", c, ok, err)
	}
	path, _, err := credentialPaths()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if err := logout(projectURL, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := findCredential(projectURL); err != nil || !ok || c.Token != "instance-token" {
		t.Fatalf("fallback after project logout: %#v %v %v", c, ok, err)
	}
}

func TestMissingCredentialLookupDoesNotCreateConfig(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	if _, ok, err := findCredential("https://example.test"); err != nil || ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(base, "lode")); !os.IsNotExist(err) {
		t.Fatalf("lookup created config directory: %v", err)
	}
}

func TestTokenEnvironmentOverridesPrompt(t *testing.T) {
	t.Setenv("LODE_TOKEN", "from-environment")
	got, err := inputToken(strings.NewReader("unused\n"), io.Discard)
	if err != nil || got != "from-environment" {
		t.Fatalf("token=%q err=%v", got, err)
	}
}

func TestLoginVerifiesAndStoresProjectCredential(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const id = "0123456789abcdef0123456789abcdef"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("auth header missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ID": "user-id", "Username": "alice", "Instance": "Lode"})
	})
	mux.HandleFunc("/alice/art/v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Lode-Project-ID", id)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var out strings.Builder
	if err := login(context.Background(), srv.URL+"/alice/art", strings.NewReader("secret\n"), &out); err != nil {
		t.Fatal(err)
	}
	c, ok, err := findCredential(srv.URL + "/alice/art")
	if err != nil || !ok || c.Token != "secret" || c.User != "user-id" {
		t.Fatalf("saved=%#v ok=%v err=%v", c, ok, err)
	}
	if checked, ok, err := verifiedCredential(context.Background(), srv.URL+"/alice/art"); err != nil || !ok || checked.User != "user-id" {
		t.Fatalf("verified=%#v ok=%v err=%v", checked, ok, err)
	}
	if !strings.Contains(out.String(), "alice") {
		t.Fatalf("login output=%q", out.String())
	}
	path, _, _ := credentialPaths()
	if filepath.Base(path) != "credentials.json" {
		t.Fatal(path)
	}
}
