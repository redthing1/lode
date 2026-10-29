package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

func TestDifferentAccountsUpdateAndRecoverAfterDemotion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	directory := t.TempDir()
	writeFile(t, directory, "assets/a", "first a")
	writeFile(t, directory, "assets/b", "first b")
	writeFile(t, directory, "references/c", "omitted")
	alice := attach(t, f, directory)
	save(t, f, alice, "initial")
	admin := instance.Principal{User: f.user}
	user, err := f.store.CreateUser(ctx, admin, "bob", "a sufficiently long test password")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMember(ctx, admin, "alice", "project", "bob", instance.Writer); err != nil {
		t.Fatal(err)
	}
	token, err := f.store.CreateToken(ctx, user, instance.TokenOptions{Name: "Bob's workspace", Project: f.client.ProjectID, Role: instance.Writer})
	if err != nil {
		t.Fatal(err)
	}
	client, err := remote.New(f.client.Origin(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTP.CloseIdleConnections()
	bob, err := Clone(ctx, f.url, client, filepath.Join(t.TempDir(), "bob"), ConnectOptions{Selection: model.Selection{{Path: "assets", Include: true}}})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	writeFile(t, directory, "assets/a", "Alice's work")
	save(t, f, alice, "Alice")
	writeFile(t, bob.Directory(), "assets/b", "Bob's work")
	if _, err := bob.Save(ctx, token, "stale Bob"); err == nil {
		t.Fatal("stale save accepted")
	}
	if err := bob.Update(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	v, err := bob.Save(ctx, token, "Bob")
	if err != nil || v.Actor != user.ID {
		t.Fatalf("Bob's publication: %v %v", v, err)
	}
	if files := remoteFiles(t, f, v.ID); files["assets/a"] != "Alice's work" || files["assets/b"] != "Bob's work" || files["references/c"] != "omitted" {
		t.Fatal(files)
	}
	// Lose the accepted response, then change Bob's role. Read access must still
	// allow settling Bob's own publication and updating without losing later work.
	writeFile(t, bob.Directory(), "assets/b", "accepted before demotion")
	f.dropSave.Store(true)
	if _, err := bob.Save(ctx, token, "accepted response lost"); err == nil {
		t.Fatal("response was not lost")
	}
	if err := f.store.SetMember(ctx, admin, "alice", "project", "bob", instance.Reader); err != nil {
		t.Fatal(err)
	}
	writeFile(t, bob.Directory(), "assets/b", "later local edit")
	if err := bob.Update(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, bob.Directory(), "assets/b"); got != "later local edit" {
		t.Fatal(got)
	}
	if _, err := bob.Save(ctx, token, "not permitted"); err == nil {
		t.Fatal("reader published")
	}
	if err := bob.Cancel(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := bob.Update(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMember(ctx, admin, "alice", "project", "bob", ""); err != nil {
		t.Fatal(err)
	}
	var denied *remote.Error
	if err := bob.Update(ctx, token, nil); !errors.As(err, &denied) || denied.Status != 403 {
		t.Fatalf("removed member update: %v", err)
	}
	changes, err := bob.Status(ctx)
	if err != nil || len(changes) != 1 || changes[0].Path != "assets/b" {
		t.Fatalf("local work after access removal: %v %v", changes, err)
	}
}
