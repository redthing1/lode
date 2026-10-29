package instance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func projectWithFile(t *testing.T, s *Store, owner User, name, data string) Project {
	t.Helper()
	ctx := context.Background()
	actor := Principal{User: owner}
	p, err := s.CreateProject(ctx, actor, name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(data))
	err = s.WithProject(ctx, actor, owner.Name, name, Writer, func(_ Project, store *project.Store) error {
		_, err := store.Save(ctx, owner.ID, model.Save{ID: id.String(), Branch: "main", Changes: []model.Entry{{Path: "capture.bin", Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}}}, project.Source{Write: func(_ model.Entry, w io.Writer, _ *project.Basis) error {
			_, err := io.WriteString(w, data)
			return err
		}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProjectDeletionAuthorityReferencesAndIdentity(t *testing.T) {
	ctx := context.Background()
	s, admin := instanceFixture(t)
	owner := ordinaryUser(t, s, admin, "bob")
	member := ordinaryUser(t, s, admin, "casey")
	actor := Principal{User: owner}
	p := projectWithFile(t, s, owner, "remove", "remove\x00\xff")
	keep := projectWithFile(t, s, admin, "keep", "exact\x00\xffsurvivor")
	if err := s.SetMember(ctx, actor, owner.Name, p.Name, member.Name, Maintainer); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []Principal{Principal{User: member}, tokenPrincipal(t, s, owner, p.ID, Maintainer), tokenPrincipal(t, s, admin, "", Maintainer)} {
		if err := s.DeleteProject(ctx, denied, p.Owner, p.Name, p.ID); !errors.Is(err, ErrAccess) {
			t.Fatalf("non-owner deletion: %v", err)
		}
	}
	stale := actor
	stale.User.Generation--
	if err := s.DeleteProject(ctx, stale, p.Owner, p.Name, p.ID); !errors.Is(err, ErrCredentials) {
		t.Fatalf("stale actor deletion: %v", err)
	}
	_, release, err := s.acquire(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = s.DeleteProject(ctx, actor, p.Owner, p.Name, p.ID)
	release()
	if !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("active project deleted: %v", err)
	}
	if err := s.DeleteProject(ctx, actor, p.Owner, p.Name, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.acquire(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late acquisition: %v", err)
	}
	for _, table := range []string{"directories", "groups", "versions", "cancelled", "branches", "tags", "retention", "members", "tokens"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE project=?", p.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan %s: %d %v", table, count, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.directory, "projects", p.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native project remains: %v", err)
	}
	replacement, err := s.CreateProject(ctx, actor, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, actor, p.Owner, p.Name, p.ID); !errors.Is(err, ErrProjectChanged) {
		t.Fatalf("stale confirmation: %v", err)
	}
	if err := s.DeleteProject(ctx, Principal{User: admin}, p.Owner, p.Name, replacement.ID); err != nil {
		t.Fatalf("administrator deletion: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	err = reopened.WithProject(ctx, Principal{User: admin}, keep.Owner, keep.Name, Reader, func(_ Project, store *project.Store) error {
		id, err := store.Head(ctx, "main")
		if err != nil {
			return err
		}
		_, r, err := store.File(ctx, id, "capture.bin")
		if err != nil {
			return err
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if string(data) != "exact\x00\xffsurvivor" {
			t.Fatal("surviving project changed")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeletionDecisionRollback(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	p := projectWithFile(t, s, user, "rollback", "original")
	token, err := s.CreateToken(ctx, user, TokenOptions{Name: "keep", Project: p.ID, Role: Reader})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_revocation BEFORE DELETE ON tokens BEGIN SELECT RAISE(ABORT,'injected revocation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, Principal{User: user}, p.Owner, p.Name, p.ID); err == nil || errors.Is(err, ErrDeletionPending) {
		t.Fatalf("decision failure: %v", err)
	}
	if _, err := s.Token(ctx, token); err != nil {
		t.Fatalf("uncommitted deletion revoked token: %v", err)
	}
	if got, err := s.Project(ctx, Principal{User: user}, p.Owner, p.Name, Reader); err != nil || got.State != "ready" {
		t.Fatalf("uncommitted deletion changed state: %v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(s.directory, "projects", p.ID)); err != nil {
		t.Fatal("uncommitted deletion touched storage", err)
	}
}

func TestInterruptedDeletionFinishesOnOpen(t *testing.T) {
	for _, failure := range []string{"metadata", "filesystem"} {
		t.Run(failure, func(t *testing.T) {
			if failure == "filesystem" && os.Geteuid() == 0 {
				t.Skip("permission failure requires an unprivileged process")
			}
			ctx := context.Background()
			s, user := instanceFixture(t)
			actor := Principal{User: user}
			member := ordinaryUser(t, s, user, "bob")
			p := projectWithFile(t, s, user, "pending", "retained before deletion")
			token, err := s.CreateToken(ctx, user, TokenOptions{Name: "revoked", Project: p.ID, Role: Writer})
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(s.directory, "projects")
			if failure == "metadata" {
				_, err = s.db.Exec(`CREATE TRIGGER refuse_forgetting BEFORE DELETE ON projects BEGIN SELECT RAISE(ABORT,'injected metadata failure'); END`)
			} else {
				err = os.Chmod(parent, 0500)
				defer os.Chmod(parent, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteProject(ctx, actor, p.Owner, p.Name, p.ID); !errors.Is(err, ErrDeletionPending) {
				t.Fatalf("interrupted deletion: %v", err)
			}
			if _, err := s.Token(ctx, token); !errors.Is(err, ErrCredentials) {
				t.Fatalf("pending deletion token: %v", err)
			}
			if _, err := s.Project(ctx, actor, p.Owner, p.Name, Reader); !errors.Is(err, ErrNotFound) {
				t.Fatalf("pending ordinary read: %v", err)
			}
			if _, _, err := s.acquire(ctx, p.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("pending native acquisition: %v", err)
			}
			if owned, err := s.OwnedProject(ctx, actor, p.Owner, p.Name); err != nil || owned.State != "deleting" {
				t.Fatalf("owner cannot finish deletion: %+v %v", owned, err)
			}
			if _, err := s.CreateProject(ctx, actor, p.Name); !errors.Is(err, ErrExists) {
				t.Fatalf("pending name reused: %v", err)
			}
			if _, err := s.CreateToken(ctx, user, TokenOptions{Name: "late", Project: p.ID, Role: Reader}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("late scoped token: %v", err)
			}
			if err := s.SetMember(ctx, actor, p.Owner, p.Name, member.Name, Writer); !errors.Is(err, ErrNotFound) {
				t.Fatalf("late membership: %v", err)
			}
			listed, next, err := s.AdminProjects(ctx, actor, "")
			if err != nil || next != "" || len(listed) != 1 || listed[0].State != "deleting" {
				t.Fatalf("pending admin listing: %+v %q %v", listed, next, err)
			}
			var branches int
			if err := s.db.QueryRow("SELECT count(*) FROM branches WHERE project=?", p.ID).Scan(&branches); err != nil || branches != 1 {
				t.Fatalf("partial metadata transaction: %d %v", branches, err)
			}
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if unexpected, err := Open(ctx, s.directory); err == nil {
				unexpected.Close(ctx)
				t.Fatal("startup ignored incomplete deletion")
			}
			if failure == "metadata" {
				db, err := database(filepath.Join(s.directory, "state"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("DROP TRIGGER refuse_forgetting")
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(parent, 0700); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, s.directory)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(ctx)
			if _, err := reopened.OwnedProject(ctx, actor, p.Owner, p.Name); !errors.Is(err, ErrNotFound) {
				t.Fatalf("startup did not finish: %v", err)
			}
			if _, err := reopened.CreateProject(ctx, actor, p.Name); err != nil {
				t.Fatalf("completed name still reserved: %v", err)
			}
		})
	}
}

func TestDeletionDoesNotFollowRepositorySymlink(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	id := uuid.NewString()
	if _, err := s.db.Exec("INSERT INTO projects(id,owner,name,created) VALUES(?,?,?,?)", id, user.ID, "link", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.directory, "projects", id)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, Principal{User: user}, user.Name, "link", id); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(b) != "outside" {
		t.Fatalf("followed symlink: %q %v", b, err)
	}
}

func TestAdministratorProjectsPagination(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	for i := 0; i < 103; i++ {
		if _, err := s.db.Exec("INSERT INTO projects(id,owner,name,created) VALUES(?,?,?,0)", uuid.NewString(), user.ID, fmt.Sprintf("project-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AdminProjects(ctx, tokenPrincipal(t, s, user, "", Maintainer), ""); !errors.Is(err, ErrAccess) {
		t.Fatalf("token admin listing: %v", err)
	}
	seen := map[string]bool{}
	for after := ""; ; {
		projects, next, err := s.AdminProjects(ctx, Principal{User: user}, after)
		if err != nil || len(projects) > 50 {
			t.Fatalf("page: %d %v", len(projects), err)
		}
		for _, p := range projects {
			if seen[p.ID] {
				t.Fatal("duplicate project")
			}
			seen[p.ID] = true
		}
		if next == "" {
			break
		}
		if next <= after {
			t.Fatal("cursor did not advance")
		}
		after = next
	}
	if len(seen) != 103 {
		t.Fatalf("listed %d projects", len(seen))
	}
}
