package instance

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func ordinaryUser(t *testing.T, s *Store, admin User, name string) User {
	t.Helper()
	user, err := s.CreateUser(context.Background(), Principal{User: admin}, name, "a sufficiently long test password")
	if err != nil {
		t.Fatal(err)
	}
	return user
}
func tokenPrincipal(t *testing.T, s *Store, user User, project string, role Role) Principal {
	t.Helper()
	token, err := s.CreateToken(context.Background(), user, TokenOptions{Name: "test", Project: project, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Token(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return actor
}

func TestOrdinaryAccountsAndAdministratorBoundary(t *testing.T) {
	ctx := context.Background()
	s, alice := instanceFixture(t)
	admin := Principal{User: alice}
	bob := ordinaryUser(t, s, alice, "bob")
	logged, err := s.Password(ctx, "bob", "a sufficiently long test password")
	if err != nil || logged != bob || logged.Admin {
		t.Fatalf("ordinary login: %v %v", logged, err)
	}
	if _, err := s.CreateUser(ctx, Principal{User: bob}, "eve", "a sufficiently long test password"); !errors.Is(err, ErrAccess) {
		t.Fatalf("ordinary provisioning: %v", err)
	}
	token := tokenPrincipal(t, s, alice, "", Maintainer)
	if _, err := s.CreateUser(ctx, token, "eve", "a sufficiently long test password"); !errors.Is(err, ErrAccess) {
		t.Fatalf("token provisioning: %v", err)
	}
	if _, err := s.CreateUser(ctx, admin, "admin", "a sufficiently long test password"); !errors.Is(err, ErrUsername) {
		t.Fatalf("reserved name: %v", err)
	}
	if _, err := s.CreateUser(ctx, admin, "eve", "short"); !errors.Is(err, ErrPassword) {
		t.Fatalf("short password: %v", err)
	}
	if _, err := s.CreateUser(ctx, admin, "bob", "a sufficiently long test password"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate account: %v", err)
	}
	p, err := s.CreateProject(ctx, Principal{User: bob}, "own")
	if err != nil || p.Owner != "bob" || p.Role != Maintainer {
		t.Fatalf("ordinary project: %v %v", p, err)
	}
	if _, err := s.Project(ctx, admin, "bob", "own", Maintainer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project(ctx, token, "bob", "own", Reader); !errors.Is(err, ErrAccess) {
		t.Fatalf("admin token inherited site authority: %v", err)
	}
	if _, err := s.CreateToken(ctx, alice, TokenOptions{Name: "unrelated", Project: p.ID, Role: Reader}); !errors.Is(err, ErrAccess) {
		t.Fatalf("issued unrelated scoped token: %v", err)
	}
	forged := Principal{User: bob}
	forged.User.Admin = true
	if _, err := s.CreateUser(ctx, forged, "eve", "a sufficiently long test password"); !errors.Is(err, ErrAccess) {
		t.Fatalf("trusted stale privilege: %v", err)
	}
	if _, _, err := s.Users(ctx, Principal{User: bob}, ""); !errors.Is(err, ErrAccess) {
		t.Fatalf("ordinary user list: %v", err)
	}
	if _, _, err := s.Users(ctx, token, ""); !errors.Is(err, ErrAccess) {
		t.Fatalf("token user list: %v", err)
	}
	// Page a realistic account listing without spending time hashing fixture-only
	// passwords for accounts that never authenticate in this test.
	for i := 0; i < 102; i++ {
		if _, err := s.db.Exec("INSERT INTO users(id,name,password,admin) SELECT ?,?,password,0 FROM users WHERE id=?", fmt.Sprintf("fixture-%d", i), fmt.Sprintf("user-%03d", i), alice.ID); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	after := ""
	for {
		users, next, err := s.Users(ctx, admin, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) > 100 {
			t.Fatal("unbounded user page")
		}
		for _, u := range users {
			if seen[u.ID] {
				t.Fatal("duplicate paged user")
			}
			seen[u.ID] = true
		}
		if next == "" {
			break
		}
		if next <= after {
			t.Fatal("cursor did not advance")
		}
		after = next
	}
	if len(seen) != 104 {
		t.Fatalf("listed %d users", len(seen))
	}
	if _, err := s.db.Exec("UPDATE users SET disabled=1 WHERE id=?", bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project(ctx, Principal{User: bob}, "bob", "own", Reader); !errors.Is(err, ErrCredentials) {
		t.Fatalf("disabled principal: %v", err)
	}
}

func TestMembershipAndTokenPermissionsStayBounded(t *testing.T) {
	ctx := context.Background()
	s, alice := instanceFixture(t)
	owner := Principal{User: alice}
	bob := ordinaryUser(t, s, alice, "bob")
	charlie := ordinaryUser(t, s, alice, "charlie")
	first, err := s.CreateProject(ctx, owner, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateProject(ctx, owner, "second")
	if err != nil {
		t.Fatal(err)
	}
	all := tokenPrincipal(t, s, bob, "", Maintainer)
	if _, err := s.Project(ctx, all, "alice", "first", Reader); !errors.Is(err, ErrAccess) {
		t.Fatalf("token granted membership: %v", err)
	}
	if ps, err := s.Projects(ctx, all); err != nil || len(ps) != 0 {
		t.Fatalf("unrelated listing: %v %v", ps, err)
	}
	if err := s.SetMember(ctx, owner, "alice", "first", "bob", Reader); err != nil {
		t.Fatal(err)
	}
	scoped := tokenPrincipal(t, s, bob, first.ID, Writer)
	if p, err := s.Project(ctx, scoped, "alice", "first", Reader); err != nil || p.Role != Reader {
		t.Fatalf("token cap widened reader: %v %v", p, err)
	}
	if _, err := s.Project(ctx, scoped, "alice", "first", Writer); !errors.Is(err, ErrAccess) {
		t.Fatalf("reader wrote: %v", err)
	}
	if err := s.SetMember(ctx, owner, "alice", "second", "bob", Maintainer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project(ctx, scoped, "alice", "second", Reader); !errors.Is(err, ErrAccess) {
		t.Fatalf("scope widened: %v", err)
	}
	if ps, err := s.Projects(ctx, scoped); err != nil || len(ps) != 1 || ps[0].ID != first.ID {
		t.Fatalf("scoped listing: %v %v", ps, err)
	}
	if err := s.SetMember(ctx, owner, "alice", "first", "bob", Writer); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Project(ctx, scoped, "alice", "first", Writer); err != nil || p.Role != Writer {
		t.Fatalf("promotion: %v %v", p, err)
	}
	if err := s.SetMember(ctx, all, "alice", "first", "charlie", Reader); !errors.Is(err, ErrAccess) {
		t.Fatalf("writer maintained: %v", err)
	}
	if err := s.SetMember(ctx, owner, "alice", "first", "bob", Maintainer); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember(ctx, scoped, "alice", "first", "charlie", Reader); !errors.Is(err, ErrAccess) {
		t.Fatalf("writer token maintained: %v", err)
	}
	if err := s.SetMember(ctx, all, "alice", "first", "charlie", Reader); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project(ctx, tokenPrincipal(t, s, charlie, first.ID, Maintainer), "alice", "first", Writer); !errors.Is(err, ErrAccess) {
		t.Fatalf("reader's maintainer token escalated: %v", err)
	}
	members, err := s.Members(ctx, all, "alice", "first")
	if err != nil || len(members) != 3 || !members[0].Owner || members[0].User.ID != alice.ID {
		t.Fatalf("members: %v %v", members, err)
	}
	if err := s.SetMember(ctx, all, "alice", "first", "alice", Reader); !errors.Is(err, ErrOwner) {
		t.Fatalf("owner demotion: %v", err)
	}
	if err := s.SetMember(ctx, owner, "alice", "first", "alice", ""); !errors.Is(err, ErrOwner) {
		t.Fatalf("owner removal: %v", err)
	}
	if err := s.SetMember(ctx, owner, "alice", "first", "bob", "administrator"); !errors.Is(err, ErrRole) {
		t.Fatalf("unknown role: %v", err)
	}
	if err := s.SetMember(ctx, all, "alice", "first", "bob", Reader); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember(ctx, all, "alice", "first", "charlie", Writer); !errors.Is(err, ErrAccess) {
		t.Fatalf("stale maintainer: %v", err)
	}
	if _, err := s.Project(ctx, scoped, "alice", "first", Writer); !errors.Is(err, ErrAccess) {
		t.Fatalf("existing token ignored demotion: %v", err)
	}
	if err := s.SetMember(ctx, owner, "alice", "first", "bob", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project(ctx, scoped, "alice", "first", Reader); !errors.Is(err, ErrAccess) {
		t.Fatalf("removed member read: %v", err)
	}
	if ps, err := s.Projects(ctx, all); err != nil || len(ps) != 1 || ps[0].ID != second.ID {
		t.Fatalf("removed membership listing: %v %v", ps, err)
	}
}
