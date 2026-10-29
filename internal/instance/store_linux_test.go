package instance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func instanceFixture(t *testing.T) (*Store, User) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "instance")
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	code, err := Initialize(ctx, dir, Config{URL: "http://localhost:8474", Listen: "127.0.0.1:8474"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.SetupCode(ctx); err != nil || recovered != code {
		t.Fatalf("recover setup code: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	u, err := s.Setup(ctx, code, "alice", "a sufficiently long test password")
	if err != nil {
		t.Fatal(err)
	}
	return s, u
}

func TestSetupAndInstanceOwnership(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	if _, err := Open(ctx, s.directory); err == nil {
		t.Fatal("second service accepted")
	}
	if _, err := s.Setup(ctx, "invalid", "another", "another sufficiently long password"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("second setup: %v", err)
	}
	if _, err := s.Password(ctx, "alice", "wrong password"); !errors.Is(err, ErrCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	got, err := s.Password(ctx, "alice", "a sufficiently long test password")
	if err != nil || got != user {
		t.Fatalf("login: %v %v", got, err)
	}
	if needed, err := s.NeedsSetup(ctx); err != nil || needed {
		t.Fatalf("setup after acceptance: %v %v", needed, err)
	}
	if _, err := s.SetupCode(ctx); !errors.Is(err, ErrAccess) {
		t.Fatalf("completed setup code available: %v", err)
	}
}

func TestInitializeDoesNotReplaceExistingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Initialize(context.Background(), root, Config{URL: "http://localhost:8474", Listen: "127.0.0.1:8474"}); err == nil {
		t.Fatal("replaced existing directory")
	}
	b, err := os.ReadFile(filepath.Join(root, "keep"))
	if err != nil || string(b) != "original" {
		t.Fatalf("original changed: %q %v", b, err)
	}
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging leaked: %v %v", entries, err)
	}
}

func TestTokenScopeAndPublishedContents(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	web := Principal{User: user}
	first, err := s.CreateProject(ctx, web, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, web, "second"); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, user, TokenOptions{Name: "first only", Project: first.ID, Role: Writer})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Token(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Project(ctx, actor, "alice", "second", Reader); !errors.Is(err, ErrAccess) {
		t.Fatalf("scope widened: %v", err)
	}
	if _, err := s.CreateProject(ctx, actor, "third"); !errors.Is(err, ErrAccess) {
		t.Fatalf("scoped creation allowed: %v", err)
	}
	listed, err := s.Projects(ctx, actor)
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("scope listing: %v %v", listed, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	data := "binary\x00\xff"
	hash := sha256.Sum256([]byte(data))
	err = s.WithProject(ctx, actor, "alice", "first", Writer, func(info Project, p *project.Store) error {
		if info.ID != first.ID || info.ID != p.ID() || info.Role != Writer {
			t.Fatalf("authorized project differs: %+v", info)
		}
		_, err := p.Save(ctx, actor.User.ID, model.Save{ID: id.String(), Branch: "main", Message: "initial", Changes: []model.Entry{{Path: "capture.bin", Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}}}, project.Source{Write: func(_ model.Entry, w io.Writer, _ *project.Basis) error {
			_, err := io.WriteString(w, data)
			return err
		}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	readToken, err := s.CreateToken(ctx, user, TokenOptions{Name: "read", Project: first.ID, Role: Reader})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.Token(ctx, readToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WithProject(ctx, reader, "alice", "first", Writer, func(_ Project, _ *project.Store) error { t.Fatal("read token wrote"); return nil }); !errors.Is(err, ErrAccess) {
		t.Fatalf("write limit: %v", err)
	}
	err = s.WithProject(ctx, reader, "alice", "first", Reader, func(info Project, p *project.Store) error {
		if info.ID != first.ID || info.ID != p.ID() || info.Role != Reader {
			t.Fatalf("reader metadata exceeds access: %+v", info)
		}
		_, r, err := p.File(ctx, id.String(), "capture.bin")
		if err != nil {
			return err
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		if string(b) != data {
			t.Fatal("download differs")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tokens SET expires=? WHERE hash=?", time.Now().Add(-time.Hour).Unix(), digest(token)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, token); !errors.Is(err, ErrCredentials) {
		t.Fatalf("expired token: %v", err)
	}
	if _, err := s.db.Exec("UPDATE users SET disabled=1 WHERE id=?", user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(ctx, readToken); !errors.Is(err, ErrCredentials) {
		t.Fatalf("disabled account token: %v", err)
	}
}

func TestInterruptedProjectCreationResumes(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO projects(id,owner,name,created) VALUES(?,?,?,?)", id, user.ID, "pending", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := project.Create(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	err = s.WithProject(ctx, Principal{User: user}, "alice", "pending", Reader, func(_ Project, p *project.Store) error {
		head, err := p.Head(ctx, "main")
		if head != "" {
			t.Fatal("pending project acquired history")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow("SELECT state FROM projects WHERE id=?", id).Scan(&state); err != nil || state != "ready" {
		t.Fatalf("state=%v: %v", state, err)
	}
}

func TestCanonicalOriginValidatedBeforeInitialization(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:8473", "https://192.0.2.1", "https://[::1]", "http://lode.example", "https://lode.example/project", "https://user@lode.example", "https://lode.example?", "https://lode.example:", "https://lode.example:65536", "https://lode.example:0"} {
		t.Run(origin, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "instance")
			if _, err := Initialize(context.Background(), dir, Config{URL: origin, Listen: "127.0.0.1:8473"}); err == nil {
				t.Fatal("invalid origin initialized")
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid config left an instance")
			}
		})
	}
	for _, origin := range []string{"http://localhost:8473", "https://lode.example"} {
		dir := filepath.Join(t.TempDir(), "instance")
		if _, err := Initialize(context.Background(), dir, Config{URL: origin, Listen: "127.0.0.1:8473"}); err != nil {
			t.Fatal(err)
		}
		s, err := Open(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigurationRejectsUnknownFieldsAndInvalidListen(t *testing.T) {
	for _, tc := range []struct {
		origin string
		valid  bool
	}{
		{"http://server.tailnet.ts.net:8473", true},
		{"https://private.example", true},
		{"http://100.64.0.1:8473", true},
		{"http://[fd7a:115c:a1e0::1]:8473", true},
		{"http://lode.example", false},
		{"https://LODE.example", false},
		{"http://user@private.example", false},
		{"http://private.example/api", false},
		{"http://private.example?", false},
		{"http://private.example:0", false},
		{"ftp://private.example", false},
	} {
		err := (Config{URL: "https://lode.example", PrivateURL: tc.origin, Listen: "127.0.0.1:8473"}).Validate()
		if (err == nil) != tc.valid {
			t.Fatalf("private origin %q: %v", tc.origin, err)
		}
	}
	for _, listen := range []string{"", "localhost", ":8473", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:http"} {
		if err := (Config{URL: "https://lode.example", Listen: listen}).Validate(); err == nil {
			t.Fatal("invalid listen accepted", listen)
		}
	}
	s, _ := instanceFixture(t)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{
		`{"url":"http://localhost:8474","listen":"127.0.0.1:8474","registration":true}`,
		`{"url":"http://localhost:8474","listen":"127.0.0.1:8474","trusted_proxies":["invalid"]}`,
		`{"url":"http://localhost:8474","listen":"127.0.0.1:8474"} {}`,
	} {
		if err := os.WriteFile(filepath.Join(s.directory, "state", "config.json"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if unexpected, err := Open(context.Background(), s.directory); err == nil {
			unexpected.Close(context.Background())
			t.Fatal("invalid config opened")
		}
	}
}

func TestInstanceRejectsMissingOrWrongStorage(t *testing.T) {
	ctx := context.Background()
	s, _ := instanceFixture(t)
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(s.directory, "projects")
	saved := filepath.Join(filepath.Dir(s.directory), "saved-projects")
	if err := os.Rename(projects, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(projects, 0700); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{"", uuid.NewString() + "\n"} {
		if identity != "" {
			if err := os.WriteFile(filepath.Join(projects, ".instance"), []byte(identity), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if unexpected, err := Open(ctx, s.directory); err == nil {
			unexpected.Close(ctx)
			t.Fatal("opened missing or mismatched storage")
		}
	}
	if err := os.RemoveAll(projects); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, projects); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	if restored.ID != s.ID {
		t.Fatal("storage remount changed instance identity")
	}
}
