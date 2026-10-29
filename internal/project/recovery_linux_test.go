package project

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

// The helper uses SQLite's real failure boundary rather than a production hook.
func TestPublicationProcess(t *testing.T) {
	root := os.Getenv("LODE_TEST_PUBLICATION_DIRECTORY")
	if root == "" {
		return
	}
	f := openFixture(t, root, false)
	data, err := os.ReadFile(filepath.Join(root, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r model.Save
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Save(context.Background(), "alice", r, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error { _, err := io.WriteString(w, "published"); return err }})
	if os.Getenv("LODE_TEST_PUBLICATION_STAGE") == "before-accept" {
		if err == nil {
			t.Fatal("SQLite failure did not fire")
		}
		os.Exit(23)
	}
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(24)
}

func TestPublicationSurvivesProcessExit(t *testing.T) {
	for _, stage := range []string{"before-accept", "after-accept"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			f := openFixture(t, root, true)
			base := publish(t, f.store, "", []model.Entry{file("keep", "keep")}, map[string]string{"keep": "keep"})
			r := model.Save{ID: operation(t), Branch: "main", Base: base.ID, Message: stage, Changes: []model.Entry{file("new", "published")}}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "request.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if stage == "before-accept" {
				if _, err := f.db.Exec("CREATE TRIGGER fail_accept BEFORE INSERT ON versions BEGIN SELECT RAISE(ABORT,'publication failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			f.close(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestPublicationProcess$")
			cmd.Env = append(os.Environ(), "LODE_TEST_PUBLICATION_DIRECTORY="+root, "LODE_TEST_PUBLICATION_STAGE="+stage)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			want := 23
			if stage == "after-accept" {
				want = 24
			}
			if !errors.As(err, &exit) || exit.ExitCode() != want {
				t.Fatalf("exit %v: %s", err, output)
			}
			f = openFixture(t, root, false)
			if _, err := f.db.Exec("DROP TRIGGER IF EXISTS fail_accept"); err != nil {
				t.Fatal(err)
			}
			readSource := false
			v, err := f.store.Save(ctx, "alice", r, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error {
				readSource = true
				_, err := io.WriteString(w, "published")
				return err
			}})
			if err != nil {
				t.Fatal(err)
			}
			if stage == "after-accept" && readSource {
				t.Fatal("accepted retry consumed source")
			}
			if read(t, f.store, v.ID, "keep") != "keep" || read(t, f.store, v.ID, "new") != "published" {
				t.Fatal("recovered contents differ")
			}
			var count int
			if err := f.db.QueryRow("SELECT count(*) FROM versions WHERE id=?", r.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("accepted %d versions", count)
			}
			roots, err := f.content.Roots(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count = 0
			for _, root := range roots {
				if root.Operation == r.ID {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("protected %d roots", count)
			}
		})
	}
}

func TestMissingProtectionRefusesOpen(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	v := publish(t, f.store, "", []model.Entry{file("a", "a")}, map[string]string{"a": "a"})
	saved, err := f.store.version(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.content.Release(ctx, []string{saved.protection}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, f.db, "project", f.content); err == nil {
		t.Fatal("missing protection accepted")
	}
}
