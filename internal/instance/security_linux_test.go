package instance

import (
	"context"
	"errors"
	"testing"
)

const originalPassword = "a sufficiently long test password"
const replacementPassword = "a different sufficiently long password"

func TestPasswordChangeRevokesCredentialsAtomically(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	actor := Principal{User: user}
	token, err := s.CreateToken(ctx, user, TokenOptions{Name: "workstation", Role: Writer})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword(ctx, actor, "incorrect", replacementPassword); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := s.ChangePassword(ctx, actor, originalPassword, "short"); !errors.Is(err, ErrPassword) {
		t.Fatalf("short password: %v", err)
	}
	credential, err := s.Token(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword(ctx, credential, originalPassword, replacementPassword); !errors.Is(err, ErrAccess) {
		t.Fatalf("token changed password: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_revocation BEFORE DELETE ON tokens BEGIN SELECT RAISE(ABORT,'injected revocation failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword(ctx, actor, originalPassword, replacementPassword); err == nil {
		t.Fatal("partial credential replacement accepted")
	}
	if unchanged, err := s.Password(ctx, user.Name, originalPassword); err != nil || unchanged != user {
		t.Fatalf("password/generation changed after rollback: %+v %v", unchanged, err)
	}
	if _, err := s.Token(ctx, token); err != nil {
		t.Fatal("token lost after rollback", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER refuse_revocation"); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword(ctx, actor, originalPassword, replacementPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Password(ctx, user.Name, originalPassword); !errors.Is(err, ErrCredentials) {
		t.Fatalf("old password: %v", err)
	}
	current, err := s.Password(ctx, user.Name, replacementPassword)
	if err != nil || current.Generation != user.Generation+1 {
		t.Fatalf("replacement login: %+v %v", current, err)
	}
	if _, err := s.Token(ctx, token); !errors.Is(err, ErrCredentials) {
		t.Fatalf("revoked token: %v", err)
	}
	if _, err := s.CreateToken(ctx, user, TokenOptions{Name: "late old session", Role: Writer}); !errors.Is(err, ErrCredentials) {
		t.Fatalf("stale session issued token: %v", err)
	}
	if _, err := s.CreateUser(ctx, actor, "late-user", originalPassword); !errors.Is(err, ErrCredentials) {
		t.Fatalf("stale administrator created account: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	if got, err := reopened.Password(ctx, user.Name, replacementPassword); err != nil || got != current {
		t.Fatalf("reopened login: %+v %v", got, err)
	}
	if _, err := reopened.Token(ctx, token); !errors.Is(err, ErrCredentials) {
		t.Fatalf("reopened revoked token: %v", err)
	}
}

func TestAdministratorAccountLifecycle(t *testing.T) {
	ctx := context.Background()
	s, alice := instanceFixture(t)
	admin := Principal{User: alice}
	bob := ordinaryUser(t, s, alice, "bob")
	project, err := s.CreateProject(ctx, Principal{User: bob}, "kept")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, bob, TokenOptions{Name: "scoped", Project: project.ID, Role: Maintainer})
	if err != nil {
		t.Fatal(err)
	}
	change := AccountUpdate{Generation: bob.Generation}
	if err := s.UpdateUser(ctx, Principal{User: bob}, bob.ID, change); !errors.Is(err, ErrAccess) {
		t.Fatalf("ordinary admin edit: %v", err)
	}
	if _, err := s.ManagedUser(ctx, Principal{User: bob}, alice.Name); !errors.Is(err, ErrAccess) {
		t.Fatalf("ordinary user inspection: %v", err)
	}
	if err := s.UpdateUser(ctx, tokenPrincipal(t, s, alice, "", Maintainer), bob.ID, change); !errors.Is(err, ErrAccess) {
		t.Fatalf("token admin edit: %v", err)
	}
	if err := s.UpdateUser(ctx, admin, bob.ID, change); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, token); err != nil {
		t.Fatal("unchanged settings revoked token", err)
	}
	change.Password = replacementPassword
	if err := s.UpdateUser(ctx, admin, bob.ID, change); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, token); !errors.Is(err, ErrCredentials) {
		t.Fatalf("reset token: %v", err)
	}
	if err := s.UpdateUser(ctx, admin, bob.ID, change); !errors.Is(err, ErrAccountChanged) {
		t.Fatalf("stale admin form: %v", err)
	}
	bob, err = s.Password(ctx, "bob", replacementPassword)
	if err != nil {
		t.Fatal(err)
	}
	oldSession := bob
	token, err = s.CreateToken(ctx, bob, TokenOptions{Name: "replacement", Role: Writer})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, admin, bob.ID, AccountUpdate{Generation: bob.Generation, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Password(ctx, bob.Name, replacementPassword); !errors.Is(err, ErrCredentials) {
		t.Fatalf("disabled login: %v", err)
	}
	disabled, err := s.ManagedUser(ctx, admin, bob.Name)
	if err != nil || !disabled.Disabled {
		t.Fatalf("disabled view: %+v %v", disabled, err)
	}
	if err := s.UpdateUser(ctx, admin, bob.ID, AccountUpdate{Generation: disabled.Generation}); err != nil {
		t.Fatal(err)
	}
	bob, err = s.Password(ctx, bob.Name, replacementPassword)
	if err != nil {
		t.Fatal("enable changed password", err)
	}
	if _, err := s.Token(ctx, token); !errors.Is(err, ErrCredentials) {
		t.Fatalf("enable revived token: %v", err)
	}
	if _, err := s.CreateToken(ctx, oldSession, TokenOptions{Name: "stale", Role: Reader}); !errors.Is(err, ErrCredentials) {
		t.Fatalf("enable revived session: %v", err)
	}
	if p, err := s.Project(ctx, Principal{User: bob}, bob.Name, project.Name, Maintainer); err != nil || p.ID != project.ID {
		t.Fatalf("ownership lost: %+v %v", p, err)
	}
	if err := s.UpdateUser(ctx, admin, alice.ID, AccountUpdate{Generation: alice.Generation}); !errors.Is(err, ErrLastAdministrator) {
		t.Fatalf("last admin demotion: %v", err)
	}
	if err := s.UpdateUser(ctx, admin, alice.ID, AccountUpdate{Generation: alice.Generation, Admin: true, Disabled: true}); !errors.Is(err, ErrLastAdministrator) {
		t.Fatalf("last admin disable: %v", err)
	}
}

func TestCompetingAdministratorChangesKeepAnAdministrator(t *testing.T) {
	for _, self := range []bool{false, true} {
		name := "each other"
		if self {
			name = "themselves"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, alice := instanceFixture(t)
			bob := ordinaryUser(t, s, alice, "bob")
			if err := s.UpdateUser(ctx, Principal{User: alice}, bob.ID, AccountUpdate{Generation: bob.Generation, Admin: true}); err != nil {
				t.Fatal(err)
			}
			var err error
			bob, err = s.User(ctx, bob.ID)
			if err != nil {
				t.Fatal(err)
			}
			users := []User{alice, bob}
			start := make(chan struct{})
			done := make(chan error, 2)
			for i, actor := range users {
				target := users[1-i]
				if self {
					target = actor
				}
				go func() {
					<-start
					done <- s.UpdateUser(ctx, Principal{User: actor}, target.ID, AccountUpdate{Generation: target.Generation})
				}()
			}
			close(start)
			success := 0
			for range users {
				err := <-done
				if err == nil {
					success++
				} else if !errors.Is(err, ErrCredentials) && !errors.Is(err, ErrLastAdministrator) {
					t.Fatal(err)
				}
			}
			var active int
			if err := s.db.QueryRow("SELECT count(*) FROM users WHERE admin=1 AND disabled=0").Scan(&active); err != nil {
				t.Fatal(err)
			}
			if success != 1 || active != 1 {
				t.Fatalf("successful changes=%d, active administrators=%d", success, active)
			}
		})
	}
}
