package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"golang.org/x/sys/unix"
)

func backupSave(t *testing.T, p *project.Store, actor, base, data string) (model.Save, model.Version) {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(data))
	r := model.Save{ID: id.String(), Branch: "main", Base: base, Message: "backup fixture", Changes: []model.Entry{{Path: "assets/data.bin", Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}}}
	v, err := p.Save(context.Background(), actor, r, project.Source{Write: func(_ model.Entry, w io.Writer, _ *project.Basis) error {
		_, err := io.WriteString(w, data)
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r, v
}

func TestBackupRestoresHostedInstance(t *testing.T) {
	ctx := context.Background()
	s, alice := instanceFixture(t)
	auth := newSyntheticAuthenticator(t, "backup-key")
	alice, codes := registerKey(t, s, alice, auth, true)
	bob := ordinaryUser(t, s, alice, "bob")
	owner := Principal{User: alice}
	p, err := s.CreateProject(ctx, owner, "assets")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, Principal{User: bob}, "empty"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember(ctx, owner, alice.Name, p.Name, bob.Name, Reader); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, bob, TokenOptions{Name: "scoped", Project: p.ID, Role: Reader, Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := s.CreateToken(ctx, bob, TokenOptions{Name: "revoked", Role: Reader})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := s.Tokens(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range tokens {
		if token.Name == "revoked" {
			if err := s.RevokeToken(ctx, bob, token.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Sessions().Commit("kept-session", []byte("session payload"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Sessions().Delete("logged-out"); err != nil {
		t.Fatal(err)
	}
	var protected, expired, latest model.Version
	var accepted, cancelled model.Save
	err = s.WithProject(ctx, owner, alice.Name, p.Name, Writer, func(_ Project, store *project.Store) error {
		_, protected = backupSave(t, store, alice.ID, "", "protected\x00\xff")
		if _, err := store.CreateTag(ctx, "keep", protected.ID); err != nil {
			return err
		}
		_, expired = backupSave(t, store, alice.ID, protected.ID, "expired\x00\xff")
		accepted, latest = backupSave(t, store, alice.ID, expired.ID, "latest\x00\xff")
		if _, err := store.CreateBranch(ctx, "experiment", latest.ID); err != nil {
			return err
		}
		cancelled = model.Save{ID: uuid.Must(uuid.NewV7()).String(), Branch: "main", Base: latest.ID, Changes: []model.Entry{{Path: "unused", Kind: model.Directory}}}
		if _, err := store.Cancel(ctx, alice.ID, cancelled); err != nil {
			return err
		}
		if err := store.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 90}); err != nil {
			return err
		}
		preview, err := store.PreviewCleanup(ctx)
		if err != nil {
			return err
		}
		if _, err := store.Cleanup(ctx, preview.Token); err != nil {
			return err
		}
		_, err = store.Collect(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var cutoff string
	if err := s.db.QueryRow("SELECT cutoff FROM retention WHERE project=?", p.ID).Scan(&cutoff); err != nil || cutoff == "" {
		t.Fatal("missing operation cutoff", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before := backupFileStates(t, s.directory)
	backup := filepath.Join(t.TempDir(), "backup")
	if err := Backup(ctx, s.directory, backup); err != nil {
		t.Fatal(err)
	}
	if after := backupFileStates(t, s.directory); !reflect.DeepEqual(before, after) {
		t.Fatal("backup changed its source")
	}
	for name, state := range backupFileStates(t, backup) {
		if strings.Contains(name, "/repository/") {
			if original, ok := before[name]; !ok || original != state {
				t.Fatalf("native bytes/timestamp changed: %s", name)
			}
		}
		if strings.Contains(name, "/cache/") || strings.HasSuffix(name, ".config") {
			t.Fatal("copied derived state", name)
		}
	}
	// Restore follows exactly the documented operation, using the backup as source.
	restored := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(filepath.Join(restored, "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	mounted, err := os.Stat(filepath.Join(restored, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(filepath.Join(restored, "projects")); err != nil || !os.SameFile(mounted, after) {
		t.Fatal("replaced destination mount point", err)
	}
	r, err := Open(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if r.ID != s.ID || !bytes.Equal(r.key, s.key) || !reflect.DeepEqual(r.Config, s.Config) {
		t.Fatal("instance identity/config changed")
	}
	if got, err := r.Password(ctx, alice.Name, originalPassword); err != nil || got != alice {
		t.Fatal("account changed", err)
	}
	c, err := r.BeginKeyLogin(ctx, "restored-browser", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.FinishKeyLogin(ctx, "restored-browser", c.ID, webauthnRequest(passkeyAssertionResponse(t, auth, alice.ID, optionChallenge(t, c.Options, false), testOrigin, 1))); err != nil || got != alice {
		t.Fatal("restored passkey failed", err)
	}
	var codeCount int
	if err := r.db.QueryRow("SELECT count(*) FROM recovery_codes WHERE user_id=?", alice.ID).Scan(&codeCount); err != nil || codeCount != len(codes) {
		t.Fatal("recovery codes changed", err)
	}
	if _, err := r.UseRecovery(ctx, alice, codes[0]); err != nil {
		t.Fatal("restored recovery code failed", err)
	}
	if _, err := r.UseRecovery(ctx, alice, codes[0]); !errors.Is(err, ErrRecovery) {
		t.Fatal("restored recovery code reused", err)
	}
	reader, err := r.Token(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Project(ctx, reader, "bob", "empty", Reader); !errors.Is(err, ErrAccess) {
		t.Fatal("token scope widened", err)
	}
	if _, err := r.Project(ctx, reader, alice.Name, p.Name, Writer); !errors.Is(err, ErrAccess) {
		t.Fatal("reader permission widened", err)
	}
	if _, err := r.Token(ctx, revoked); !errors.Is(err, ErrCredentials) {
		t.Fatal("revoked token revived", err)
	}
	if data, found, err := r.Sessions().Find("kept-session"); err != nil || !found || string(data) != "session payload" {
		t.Fatal("session lost", err)
	}
	if err := r.Sessions().Commit("logged-out", []byte("late write"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := r.Sessions().Find("logged-out"); err != nil || found {
		t.Fatal("logout marker lost", err)
	}
	var restoredCutoff string
	if err := r.db.QueryRow("SELECT cutoff FROM retention WHERE project=?", p.ID).Scan(&restoredCutoff); err != nil || restoredCutoff != cutoff {
		t.Fatal("operation cutoff changed", err)
	}
	err = r.WithProject(ctx, reader, alice.Name, p.Name, Reader, func(_ Project, store *project.Store) error {
		for id, want := range map[string]string{protected.ID: "protected\x00\xff", latest.ID: "latest\x00\xff"} {
			_, file, err := store.File(ctx, id, "assets/data.bin")
			if err != nil {
				return err
			}
			data, err := io.ReadAll(file)
			if err := errors.Join(err, file.Close()); err != nil {
				return err
			}
			if string(data) != want {
				t.Fatal("restored bytes differ")
			}
		}
		if _, _, err := store.File(ctx, expired.ID, "assets/data.bin"); !errors.Is(err, project.ErrExpired) {
			t.Fatal("expired contents revived", err)
		}
		for ref, id := range map[string]string{"keep": protected.ID, "main": latest.ID, "experiment": latest.ID} {
			v, err := store.Reference(ctx, ref)
			if err != nil || v.ID != id {
				t.Fatal("reference changed", ref, err)
			}
		}
		if v, err := store.Save(ctx, alice.ID, accepted, project.Source{}); err != nil || v.ID != latest.ID {
			t.Fatal("accepted retry lost", err)
		}
		if _, err := store.Save(ctx, alice.ID, cancelled, project.Source{}); !errors.Is(err, project.ErrCancelled) {
			t.Fatal("cancelled save revived", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBackupRecoversSQLiteOnlyInStage(t *testing.T) {
	for _, mode := range []string{"journal", "wal"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, _ := instanceFixture(t)
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestBackupSQLiteCrashFixture$")
			child.Env = append(os.Environ(), "LODE_BACKUP_CRASH_FIXTURE="+s.directory, "LODE_BACKUP_CRASH_MODE="+mode)
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("crash fixture: %v\n%s", err, output)
			}
			info, err := os.Stat(filepath.Join(s.directory, "state", "instance.sqlite-"+mode))
			if err != nil || info.Size() <= 512 {
				t.Fatal("missing recovery sidecar", err)
			}
			before := backupFileStates(t, s.directory)
			dest := filepath.Join(t.TempDir(), "copy")
			if err := Backup(ctx, s.directory, dest); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, backupFileStates(t, s.directory)) {
				t.Fatal("source was recovered or modified")
			}
			r, err := Open(ctx, dest)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close(ctx)
			name := "alice"
			if mode == "wal" {
				name = "committed"
			}
			if _, err := r.Password(ctx, name, originalPassword); err != nil {
				t.Fatal("SQLite recovery lost committed data or kept uncommitted data", err)
			}
		})
	}
}

func TestBackupSQLiteCrashFixture(t *testing.T) {
	dir := os.Getenv("LODE_BACKUP_CRASH_FIXTURE")
	if dir == "" {
		return
	}
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LODE_BACKUP_CRASH_MODE") == "wal" {
		_, err = s.db.Exec("PRAGMA journal_mode=WAL; UPDATE users SET name='committed'")
	} else {
		_, err = s.db.Exec("PRAGMA cache_size=5; PRAGMA cache_spill=ON; BEGIN IMMEDIATE; UPDATE users SET name='uncommitted',password=zeroblob(1048576)")
	}
	if err != nil {
		t.Fatal(err)
	}
	// Exit without closing SQLite, leaving committed WAL or a hot rollback journal.
	os.Exit(0)
}

type backupFileState struct {
	Hash     [32]byte
	Modified int64
}

func backupFileStates(t *testing.T, root string) map[string]backupFileState {
	t.Helper()
	result := map[string]backupFileState{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		result[relative] = backupFileState{sha256.Sum256(data), info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestBackupRefusesUnsafeDestinationsAndLiveSource(t *testing.T) {
	ctx := context.Background()
	s, _ := instanceFixture(t)
	root := t.TempDir()
	dest := filepath.Join(root, "backup")
	if err := Backup(ctx, s.directory, dest); err == nil || !strings.Contains(err.Error(), "stop the server") {
		t.Fatal("live source accepted", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(s.directory, alias); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{s.directory, filepath.Join(s.directory, "nested"), filepath.Join(alias, "nested"), root} {
		if err := Backup(ctx, s.directory, target); err == nil {
			t.Fatal("unsafe destination accepted", target)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Backup(cancelled, s.directory, dest); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled copy accepted", err)
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed copy published a destination", err)
	}
	lock, err := prepareDirectory(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if err := Backup(ctx, s.directory, dest); err == nil {
		t.Fatal("backup ignored destination ownership")
	}
	if _, err := Initialize(ctx, dest, s.Config); err == nil {
		t.Fatal("initialization ignored destination ownership")
	}
}

func TestBackupCancellationDuringCopyRemovesStage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, user := instanceFixture(t)
	p, err := s.CreateProject(ctx, Principal{User: user}, "data")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// A sparse source makes the copy observably long without allocating a large
	// fixture. Cancel only after a nonempty staged copy proves IO has begun.
	name := filepath.Join("projects", p.ID, "repository", "cancel-probe")
	f, err := os.Create(filepath.Join(s.directory, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(f.Truncate(1<<30), f.Close()); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- Backup(ctx, s.directory, filepath.Join(parent, "backup")) }()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("copy ended before interruption: %v", err)
		case <-tick.C:
			paths, err := filepath.Glob(filepath.Join(parent, "backup", name))
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) == 0 {
				continue
			}
			info, err := os.Stat(paths[0])
			if err != nil || info.Size() == 0 {
				continue
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal("interrupted copy result", err)
			}
			destination := filepath.Join(parent, "backup")
			if _, err := os.Stat(filepath.Join(destination, "state")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("cancelled copy published state", err)
			}
			if stages, err := filepath.Glob(filepath.Join(destination, ".lode-backup-*")); err != nil || len(stages) != 0 {
				t.Fatal("cancelled copy left metadata staging", err)
			}
			if _, err := Initialize(context.Background(), destination, s.Config); err == nil {
				t.Fatal("adopted partial backup contents")
			}
			if err := Backup(context.Background(), s.directory, destination); err == nil {
				t.Fatal("overwrote partial backup contents")
			}
			return
		}
	}
}

func TestBackupRejectsIncompleteOrDamagedSources(t *testing.T) {
	for _, damage := range []string{"creating", "deleting", "unregistered", "symlink", "fifo", "native pack", "missing pack", "metadata", "foreign key", "storage identity"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			s, user := instanceFixture(t)
			p, err := s.CreateProject(ctx, Principal{User: user}, "data")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.WithProject(ctx, Principal{User: user}, user.Name, p.Name, Writer, func(_ Project, p *project.Store) error {
				backupSave(t, p, user.ID, "", strings.Repeat("opaque\x00\xff", 100000))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "creating", "deleting":
				_, err = s.db.Exec("UPDATE projects SET state=?", damage)
			case "metadata":
				_, err = s.db.Exec("DELETE FROM directories")
			case "foreign key":
				_, err = s.db.Exec("PRAGMA foreign_keys=OFF; UPDATE projects SET owner='missing'; PRAGMA foreign_keys=ON;")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			repository := filepath.Join(s.directory, "projects", p.ID, "repository")
			switch damage {
			case "storage identity":
				err = os.WriteFile(filepath.Join(s.directory, "projects", ".instance"), []byte(uuid.NewString()+"\n"), 0600)
			case "unregistered":
				err = os.Mkdir(filepath.Join(s.directory, "projects", "orphan"), 0700)
			case "symlink":
				err = os.Symlink(filepath.Join(s.directory, "state", "config.json"), filepath.Join(repository, "link"))
			case "fifo":
				err = unix.Mkfifo(filepath.Join(repository, "pipe"), 0600)
			case "native pack", "missing pack":
				changed := false
				err = filepath.WalkDir(repository, func(name string, entry fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					relative, err := filepath.Rel(repository, name)
					if err != nil {
						return err
					}
					if !entry.IsDir() && strings.HasPrefix(relative, "p") {
						changed = true
						if damage == "missing pack" {
							return os.Remove(name)
						}
						return os.WriteFile(name, []byte("corrupt native pack"), 0600)
					}
					return nil
				})
				if !changed {
					t.Fatal("fixture has no regular content pack")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			dest := filepath.Join(parent, "backup")
			if err := Backup(ctx, s.directory, dest); err == nil {
				t.Fatal("damaged/incomplete source accepted")
			}
			if _, err := os.Stat(filepath.Join(dest, "state")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed copy published state", err)
			}
			if stages, err := filepath.Glob(filepath.Join(dest, ".lode-backup-*")); err != nil || len(stages) != 0 {
				t.Fatal("failed copy left metadata staging", err)
			}
		})
	}
}
