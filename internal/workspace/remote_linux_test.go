package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/web"
)

func TestRemoteChangePreservesWorkAndChecksIdentity(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	w := attach(t, f, t.TempDir())
	writeFile(t, w.Directory(), "file", "saved")
	save(t, f, w, "first")
	writeFile(t, w.Directory(), "file", "unsaved")
	before := w.State()
	private := httptest.NewUnstartedServer(nil)
	f.store.Config.PrivateURL = "http://" + private.Listener.Addr().String()
	private.Config.Handler = web.New(f.store)
	private.Start()
	defer private.Close()
	c, err := remote.New(private.URL, f.token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	if err := w.SetRemote(ctx, private.URL+"/alice/project", c); err != nil {
		t.Fatal(err)
	}
	want := before
	want.Server = private.URL
	if !reflect.DeepEqual(w.State(), want) || readFile(t, w.Directory(), "file") != "unsaved" {
		t.Fatal("URL change altered workspace contents or history")
	}
	w.Close()
	w, err = Open(w.Directory())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if !reflect.DeepEqual(w.State(), want) {
		t.Fatal("new URL was not persisted")
	}
	save(t, f, w, "saved via private endpoint")
	before = w.State()
	if _, err := f.store.CreateProject(ctx, instance.Principal{User: f.user}, "other"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetRemote(ctx, private.URL+"/alice/other", c); err == nil {
		t.Fatal("changed project identity")
	}
	wrong := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/me") {
			out.Write([]byte(`{"instance":"other"}`))
		} else {
			out.Write([]byte(`{"id":"other","owner":"alice","name":"project"}`))
		}
	}))
	defer wrong.Close()
	bad, err := remote.New(wrong.URL, f.token)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.HTTP.CloseIdleConnections()
	if err := w.SetRemote(ctx, wrong.URL+"/alice/project", bad); err == nil {
		t.Fatal("changed instance identity")
	}
	if !reflect.DeepEqual(w.State(), before) {
		t.Fatal("failed identity check changed state")
	}
	if err := w.writeJSON("save.json", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := w.SetRemote(ctx, f.url, f.client); !errors.Is(err, ErrPending) {
		t.Fatalf("pending URL change: %v", err)
	}
}
