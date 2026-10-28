//go:build linux

package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectDeletionAuthorityLeasesAndTokenScope(t *testing.T) {
	ctx := context.Background()
	i, code, directory := newTestInstance(t)
	admin := setupTestOwner(t, i, code, "admin")
	owner, err := i.Store.Register(ctx, "owner", "owner secure password")
	if err != nil {
		t.Fatal(err)
	}
	member, err := i.Store.Register(ctx, "member", "member secure password")
	if err != nil {
		t.Fatal(err)
	}
	p, err := i.CreateProject(ctx, owner.ID, "assets")
	if err != nil {
		t.Fatal(err)
	}
	if err := i.PutMembership(ctx, member.ID, p.ID, RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	ownerActor := Principal{UserID: owner.ID, AuthVersion: owner.AuthVersion}
	memberActor := Principal{UserID: member.ID, AuthVersion: member.AuthVersion}
	if err := i.DeleteProject(ctx, memberActor, p.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("maintainer deletion: %v", err)
	}
	scopedSecret, _, err := i.CreateToken(ctx, owner.ID, "scoped", RoleMaintainer, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	tokenActor, err := i.AuthenticateToken(ctx, scopedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.DeleteProject(ctx, tokenActor, p.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("token deletion: %v", err)
	}
	globalSecret, _, err := i.CreateToken(ctx, owner.ID, "all projects", RoleReader, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	_, release, err := i.Acquire(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.DeleteProject(ctx, ownerActor, p.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("active deletion: %v", err)
	}
	current, err := i.Project(ctx, owner.Username, p.Slug)
	if err != nil || current.Deleting {
		t.Fatalf("refused deletion changed state: %+v %v", current, err)
	}
	release()
	if err := i.DeleteProject(ctx, ownerActor, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Project(ctx, owner.Username, p.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted registry: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "projects", p.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted files: %v", err)
	}
	if _, err := i.AuthenticateToken(ctx, scopedSecret); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("scoped token survived: %v", err)
	}
	if _, err := i.AuthenticateToken(ctx, globalSecret); err != nil {
		t.Fatalf("unscoped token removed: %v", err)
	}
	var count int
	if err := i.db.QueryRowContext(ctx, "SELECT count(*) FROM memberships WHERE project_id=?", p.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("memberships: %d %v", count, err)
	}

	second, err := i.CreateProject(ctx, owner.ID, "assets")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.SetUserState(ctx, owner.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if err := i.DeleteProject(ctx, ownerActor, second.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled owner deletion: %v", err)
	}
	if err := i.DeleteProject(ctx, Principal{UserID: admin.ID, AuthVersion: admin.AuthVersion}, second.ID); err != nil {
		t.Fatalf("admin deletion of disabled owner's project: %v", err)
	}
}

func TestProjectDeletionResumesBeforeOpeningStores(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "files-removed"}[missing], func(t *testing.T) {
			ctx := context.Background()
			i, code, directory := newTestInstance(t)
			owner := setupTestOwner(t, i, code, "owner")
			p, err := i.CreateProject(ctx, owner.ID, "assets")
			if err != nil {
				t.Fatal(err)
			}
			secret, _, err := i.CreateToken(ctx, owner.ID, "scoped", RoleWriter, p.ID, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := i.db.ExecContext(ctx, "UPDATE projects SET deleting=1 WHERE id=?", p.ID); err != nil {
				t.Fatal(err)
			}
			actor := Principal{UserID: owner.ID, AuthVersion: owner.AuthVersion}
			if err := i.Authorize(ctx, actor, p.ID, RoleReader); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("pending deletion readable: %v", err)
			}
			if _, _, err := i.Acquire(ctx, p.ID); err == nil {
				t.Fatal("opened pending deletion")
			}
			if err := i.Close(); err != nil {
				t.Fatal(err)
			}
			remove := filepath.Join(directory, "projects", p.ID)
			if !missing {
				remove = filepath.Join(remove, "history.sqlite")
			}
			if err := os.RemoveAll(remove); err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(t.TempDir(), "backup")
			if err := Backup(ctx, directory, backup); err == nil || !strings.Contains(err.Error(), "deletion is unfinished") {
				t.Fatalf("backup accepted unfinished deletion: %v", err)
			}
			reopened, err := Open(ctx, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.Project(ctx, owner.Username, p.Slug); !errors.Is(err, ErrNotFound) {
				t.Fatalf("recovered registry: %v", err)
			}
			if _, err := reopened.AuthenticateToken(ctx, secret); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("recovered token: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(directory, "projects", p.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovered files: %v", err)
			}
		})
	}
}
