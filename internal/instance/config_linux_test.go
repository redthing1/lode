//go:build linux

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistrationSettingSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	i, code, directory := newTestInstance(t)
	setupTestOwner(t, i, code, "admin")
	if i.Config.RegistrationOpen {
		t.Fatal("registration defaults open")
	}
	if _, err := i.Register(ctx, "closed", "long registration password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("closed registration: %v", err)
	}
	cfg := i.Config
	cfg.RegistrationOpen = true
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "instance.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	u, err := reopened.Register(ctx, "new-user", "long registration password")
	if err != nil || u.Admin || u.ForcePasswordChange {
		t.Fatalf("configured registration: %+v %v", u, err)
	}
}
