package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofrs/flock"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/workspace"
)

func authFixture(t *testing.T) (string, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LODE_TOKEN", "")
	var offline atomic.Bool
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if offline.Load() {
			w.WriteHeader(503)
			w.Write([]byte(`{"code":"offline","message":"server offline"}`))
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		user := strings.Split(token, "-")[0]
		if (user != "alice" && user != "bob") || token == "alice-revoked" {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":"credentials","message":"invalid credentials"}`))
			return
		}
		if id := r.Header.Get("Lode-Instance"); id != "" && id != "instance" {
			w.WriteHeader(409)
			w.Write([]byte(`{"code":"instance","message":"wrong instance"}`))
			return
		}
		if r.URL.Path == "/api/me" {
			json.NewEncoder(w).Encode(remote.Identity{Instance: "instance", User: "id-" + user, Username: user})
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/api/projects/alice/")
		if (name != "one" && name != "two") || token != user+"-all" && token != user+"-"+name {
			w.WriteHeader(403)
			w.Write([]byte(`{"code":"access","message":"access denied"}`))
			return
		}
		json.NewEncoder(w).Encode(remote.Project{ID: "project-" + name, Owner: "alice", Name: name})
	}))
	t.Cleanup(server.Close)
	return server.URL, &offline, &requests
}

func authRun(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(context.Background(), args, &out); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, &out)
	}
	return out.String()
}

func authSignIn(t *testing.T, token, address string) {
	t.Helper()
	t.Setenv("LODE_TOKEN", token)
	authRun(t, "auth", "login", address)
	t.Setenv("LODE_TOKEN", "")
}

func TestAuthAccountsProjectChoicesAndLogout(t *testing.T) {
	origin, _, _ := authFixture(t)
	one, two := origin+"/alice/one", origin+"/alice/two"
	authSignIn(t, "alice-all", origin)
	authSignIn(t, "bob-all", origin)
	if got := authRun(t, "--user", "alice", "auth", "status", origin); !strings.Contains(got, "alice @") {
		t.Fatal(got)
	}
	if got := authRun(t, "auth", "status", origin); !strings.Contains(got, "bob @") {
		t.Fatal(got)
	}
	authSignIn(t, "bob-one", one)
	authSignIn(t, "alice-all", origin)
	if got := authRun(t, "auth", "status", one); !strings.Contains(got, "bob @") || !strings.Contains(got, "verified; project") {
		t.Fatal(got)
	}
	// Separate scoped tokens for the same user do not overwrite one another.
	authSignIn(t, "alice-one", one)
	authSignIn(t, "alice-two", two)
	for address, token := range map[string]string{origin: "alice-all", one: "alice-one", two: "alice-two"} {
		cred, err := credentials(context.Background(), address)
		if err != nil || cred.Token != token {
			t.Fatalf("lost scoped credential for %s: %v", address, err)
		}
	}
	authSignIn(t, "bob-one", one)
	if got := authRun(t, "--user", "alice", "auth", "status", one); !strings.Contains(got, "alice @") {
		t.Fatal(got)
	}
	if got := authRun(t, "auth", "status", one); !strings.Contains(got, "bob @") {
		t.Fatal("one-off override changed project account", got)
	}
	if got := authRun(t, "auth", "status", "--all"); !strings.Contains(got, "alice") || !strings.Contains(got, "bob") {
		t.Fatal(got)
	}
	authRun(t, "auth", "logout", one)
	if _, err := credentials(context.Background(), one); err == nil {
		t.Fatal("logout fell back to alice")
	}
	if got := authRun(t, "auth", "status", two); !strings.Contains(got, "alice @") {
		t.Fatal(got)
	}
	store, err := readCredentials(origin)
	if err != nil || len(store.Accounts) != 1 {
		t.Fatal("logout did not forget all tokens for only bob", err)
	}
}

func TestAuthEnvironmentAndFailedLoginDoNotChangeAccounts(t *testing.T) {
	origin, _, _ := authFixture(t)
	one := origin + "/alice/one"
	authSignIn(t, "alice-all", origin)
	t.Setenv("LODE_TOKEN", "bob-all")
	if got := authRun(t, "auth", "status", one); !strings.Contains(got, "bob @") || !strings.Contains(got, "LODE_TOKEN") {
		t.Fatal(got)
	}
	for _, args := range [][]string{{"auth", "logout", one}, {"--user", "alice", "auth", "status", one}, {"--user", "alice", "auth", "login", one}} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted conflicting identity: %v", args)
		}
	}
	t.Setenv("LODE_TOKEN", "bob-two")
	if err := run(context.Background(), []string{"auth", "login", one}, &bytes.Buffer{}); err == nil {
		t.Fatal("saved inaccessible project")
	}
	t.Setenv("LODE_TOKEN", "alice-revoked")
	if err := run(context.Background(), []string{"auth", "login", origin}, &bytes.Buffer{}); err == nil {
		t.Fatal("saved invalid token")
	}
	t.Setenv("LODE_TOKEN", "")
	cred, err := credentials(context.Background(), one)
	if err != nil || cred.Username != "alice" || cred.Token != "alice-all" {
		t.Fatal("failed login changed account", err)
	}
}

func TestAuthWorkspaceInferenceOfflineStatusAndInstanceIdentity(t *testing.T) {
	origin, offline, requests := authFixture(t)
	authSignIn(t, "alice-all", origin)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".lode"), 0700); err != nil {
		t.Fatal(err)
	}
	state := workspace.State{Server: origin, InstanceID: "instance", Owner: "alice", Project: "one", ProjectID: "project-one", Branch: "main", Selection: model.Selection{{Include: true}}}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(root, ".lode", "state.json")
	if err := os.WriteFile(stateFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LODE_TOKEN", "bob-one")
	authRun(t, "-C", root, "auth", "login")
	t.Setenv("LODE_TOKEN", "")
	if got := authRun(t, "-C", root, "auth", "status"); !strings.Contains(got, "bob @") || !strings.Contains(got, "Project: alice/one") {
		t.Fatal(got)
	}
	offline.Store(true)
	before := requests.Load()
	if got := authRun(t, "-C", root, "auth", "status", "--all"); !strings.Contains(got, "alice") || requests.Load() != before {
		t.Fatal("offline listing contacted server", got)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-C", root, "auth", "status"}, &out); err == nil || !strings.Contains(out.String(), "saved; verification failed") {
		t.Fatal("offline status claimed authentication", err, out.String())
	}
	state.InstanceID = "different-instance"
	if _, err := workspaceCredentials(context.Background(), state); err == nil {
		t.Fatal("accepted a different workspace instance")
	}
	// Logout needs no server and leaves the workspace bound to the signed-out user.
	authRun(t, "-C", root, "auth", "logout")
	if _, err := workspaceCredentials(context.Background(), workspace.State{Server: origin, InstanceID: "instance", Owner: "alice", Project: "one"}); err == nil {
		t.Fatal("workspace changed users after logout")
	}
	offline.Store(false)
	b, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := credentialPath(origin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LODE_TOKEN", "bob-one")
	if err := run(context.Background(), []string{"-C", root, "auth", "status"}, &bytes.Buffer{}); err == nil {
		t.Fatal("environment token bypassed workspace instance identity")
	}
}

func TestAuthPrivateWritesAndConcurrentWriter(t *testing.T) {
	origin, _, _ := authFixture(t)
	authSignIn(t, "alice-all", origin)
	authSignIn(t, "bob-all", origin)
	file, err := credentialPath(origin)
	if err != nil {
		t.Fatal(err)
	}
	if got := authRun(t, "--user", "alice", "auth", "status", origin); !strings.Contains(got, "alice @") {
		t.Fatal("discarded existing account", got)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credentials are not private", err)
	}
	lock := flock.New(file + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := updateCredentials(origin, func(s *credentialStore) error { clear(s.Accounts); return nil }); err == nil {
		t.Fatal("ignored concurrent writer")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if got := authRun(t, "auth", "status", origin); !strings.Contains(got, "bob @") {
		t.Fatal("failed write changed accounts", got)
	}
}
