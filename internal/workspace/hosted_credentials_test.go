//go:build linux

package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

func TestHostedWorkspaceDoesNotPersistTokenAndStatusIsOffline(t *testing.T) {
	p, err := project.Create(context.Background(), filepath.Join(t.TempDir(), "p"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const slug = "art"
	h := remote.Handler(p)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/p/"+slug+"/v1/") {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		u := *r.URL
		u.Path = strings.TrimPrefix(u.Path, "/p/"+slug)
		r2.URL = &u
		h.ServeHTTP(w, r2)
	}))
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	w, err := Attach(context.Background(), srv.URL+"/p/"+slug, p.Token(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.State().Token != "" {
		t.Fatal("hosted workspace retained a credential")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".lode", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), p.Token()) {
		t.Fatal("state file contains project token")
	}
	w, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Status(context.Background()); err != nil {
		t.Fatalf("offline status: %v", err)
	}
	w.UseToken(p.Token())
	if _, err := w.RemoteClient(); err != nil {
		t.Fatalf("credential override: %v", err)
	}
}
