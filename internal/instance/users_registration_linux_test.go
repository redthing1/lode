//go:build linux

package instance

import (
	"context"
	"errors"
	"testing"
)

func TestReservedUsernamesAndAdminUsername(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	reserved := []string{"api", "assets", "settings", "projects", "security", "passkey", "verify", "healthz", "login", "logout", "setup", "register"}
	for _, username := range reserved {
		if validUsername(username) {
			t.Errorf("validUsername(%q) = true", username)
		}
		if _, err := s.CreateUser(ctx, username, "a sufficiently long password", false); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateUser(%q) error = %v, want ErrInvalid", username, err)
		}
	}
	if !validUsername("admin") {
		t.Fatal("admin must remain a valid username")
	}
	admin, err := s.CreateUser(ctx, "admin", "a sufficiently long password", true)
	if err != nil || !admin.Admin {
		t.Fatalf("CreateUser(admin): user=%+v error=%v", admin, err)
	}
}

func TestRegisterRequiresCompletedSetupAndCreatesNormalAccount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.Register(ctx, "new-user", "a sufficiently long password"); !errors.Is(err, ErrSetupUnavailable) {
		t.Fatalf("Register before setup error = %v, want ErrSetupUnavailable", err)
	}

	code, err := s.InitSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Register(ctx, "new-user", "a sufficiently long password"); !errors.Is(err, ErrSetupUnavailable) {
		t.Fatalf("Register while setup is pending error = %v, want ErrSetupUnavailable", err)
	}
	admin, err := s.Setup(ctx, code, "admin", "another sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	if !admin.Admin {
		t.Fatalf("setup account is not administrator: %+v", admin)
	}

	user, err := s.Register(ctx, "new-user", "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	if user.Admin || user.ForcePasswordChange {
		t.Fatalf("registration account has elevated or temporary state: %+v", user)
	}
	principal, err := s.AuthenticatePassword(ctx, user.Username, "a sufficiently long password")
	if err != nil || principal.UserID != user.ID || principal.Admin || principal.ForcePasswordChange {
		t.Fatalf("registered account authentication: principal=%+v error=%v", principal, err)
	}
}
