//go:build linux

// Package instance owns accounts, access and the lifecycle of hosted projects.
package instance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redthing1/lode/internal/project"
	"golang.org/x/sys/unix"
)

type Config struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type Project struct {
	ID      string
	Slug    string
	Created time.Time
}

type Member struct {
	UserID, Username string
	Role             Role
	Disabled         bool
}

func (i *Instance) Members(ctx context.Context, projectID string) ([]Member, error) {
	rows, err := i.db.QueryContext(ctx, "SELECT u.id,u.username,m.role,u.disabled FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.project_id=? ORDER BY u.username", projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username, &m.Role, &m.Disabled); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// An instance owns its directory for its entire lifetime. Native stores are
// opened on demand; active requests pin them until their final byte is served.
type Instance struct {
	*Store
	Config    Config
	directory string
	owner     *os.File
	mu        sync.Mutex
	open      map[string]*openedProject
	clock     uint64
	closed    bool
}

type openedProject struct {
	p     *project.Project
	users int
	used  uint64
}

func CanonicalURL(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.Contains(raw, "#") {
		return "", errors.New("public URL must be an HTTPS origin (HTTP is allowed on loopback)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid public URL")
	}
	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	if strings.HasSuffix(hostname, ".") {
		hostname = strings.TrimSuffix(hostname, ".")
	}
	if u.Host == "" || hostname == "" || strings.Contains(hostname, "%") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("public URL must be an HTTPS origin (HTTP is allowed on loopback)")
	}
	ip := net.ParseIP(hostname)
	if ip != nil {
		hostname = ip.String()
	} else if !validPublicHostname(hostname) {
		return "", errors.New("public URL must have a valid DNS name or IP address")
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("public URL has an invalid port")
		}
		if scheme == "https" && n == 443 || scheme == "http" && n == 80 {
			port = ""
		}
	}
	local := hostname == "localhost" || ip != nil && ip.IsLoopback()
	if scheme != "https" && !(scheme == "http" && local) {
		return "", errors.New("public URL must be an HTTPS origin (HTTP is allowed on loopback)")
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return scheme + "://" + host, nil
}

func validPublicHostname(hostname string) bool {
	if hostname == "" || len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func Create(ctx context.Context, directory, publicURL string) (instance *Instance, setupCode string, retErr error) {
	canonical, err := CanonicalURL(publicURL)
	if err != nil {
		return nil, "", err
	}
	destination, parent, name, err := targetPaths(directory)
	if err != nil {
		return nil, "", err
	}
	stage, err := os.MkdirTemp(parent, "."+name+".partial-")
	if err != nil {
		return nil, "", err
	}
	published := false
	defer func() {
		if !published {
			retErr = errors.Join(retErr, os.RemoveAll(stage))
		}
	}()
	if err := os.Chmod(stage, 0700); err != nil {
		return nil, "", err
	}
	if err := os.Mkdir(filepath.Join(stage, "projects"), 0700); err != nil {
		return nil, "", err
	}
	cfg := Config{ID: project.NewID(), URL: canonical}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, "", err
	}
	if err := writeSyncedFile(filepath.Join(stage, "instance.json"), data, 0600); err != nil {
		return nil, "", err
	}
	store, err := OpenStore(filepath.Join(stage, "instance.sqlite"))
	if err != nil {
		return nil, "", err
	}
	setupCode, err = store.InitSetup(ctx)
	if err == nil {
		err = store.Close()
	} else {
		err = errors.Join(err, store.Close())
	}
	if err != nil {
		return nil, "", err
	}
	if err := syncFile(filepath.Join(stage, "instance.sqlite")); err != nil {
		return nil, "", err
	}
	if err := syncDirectory(filepath.Join(stage, "projects")); err != nil {
		return nil, "", err
	}
	if err := syncDirectory(stage); err != nil {
		return nil, "", err
	}
	if err := renameNoReplace(stage, destination); err != nil {
		return nil, "", fmt.Errorf("publish instance: %w", err)
	}
	published = true
	if err := syncDirectory(parent); err != nil {
		return nil, setupCode, fmt.Errorf("instance was published at %s but parent sync failed: %w", destination, err)
	}
	instance, err = Open(ctx, destination)
	if err != nil {
		return nil, setupCode, fmt.Errorf("instance was published at %s but could not be reopened: %w", destination, err)
	}
	return instance, setupCode, nil
}

func Open(ctx context.Context, directory string) (_ *Instance, err error) {
	directory, err = existingDirectory(directory)
	if err != nil {
		return nil, err
	}
	data, err := readRegularFile(filepath.Join(directory, "instance.json"))
	if err != nil {
		return nil, fmt.Errorf("open instance: %w", err)
	}
	i := &Instance{directory: directory, open: make(map[string]*openedProject)}
	if err := json.Unmarshal(data, &i.Config); err != nil {
		return nil, err
	}
	canonical, err := CanonicalURL(i.Config.URL)
	if err != nil || canonical != i.Config.URL || !project.ValidID(i.Config.ID) {
		return nil, errors.New("invalid instance configuration")
	}
	projectsInfo, err := os.Lstat(filepath.Join(directory, "projects"))
	if err != nil {
		return nil, fmt.Errorf("open instance projects: %w", err)
	}
	if !projectsInfo.IsDir() || projectsInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid instance projects directory")
	}
	i.owner, err = os.OpenFile(filepath.Join(directory, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(i.owner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		i.owner.Close()
		return nil, errors.New("instance is in use; stop its server first")
	}
	defer func() {
		if err != nil {
			i.Close()
		}
	}()
	i.Store, err = openStore(filepath.Join(directory, "instance.sqlite"), false)
	if err != nil {
		return nil, err
	}
	if err = i.validateRegisteredProjects(ctx); err != nil {
		return nil, err
	}
	return i, nil
}

// Close is called after the HTTP server drains its requests.
func (i *Instance) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	for _, slot := range i.open {
		if slot.users != 0 {
			return errors.New("instance still has active project leases")
		}
	}
	var errs []error
	for id, slot := range i.open {
		errs = append(errs, slot.p.Close())
		delete(i.open, id)
	}
	if i.Store != nil {
		errs = append(errs, i.Store.Close())
		i.Store = nil
	}
	if i.owner != nil {
		errs = append(errs, i.owner.Close())
		i.owner = nil
	}
	i.closed = true
	return errors.Join(errs...)
}

func validSlug(s string) bool {
	if len(s) < 1 || len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func (i *Instance) CreateProject(ctx context.Context, slug string) (Project, error) {
	if !validSlug(slug) {
		return Project{}, errors.New("project name must use 1–64 lowercase letters, digits or internal hyphens")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.Store == nil {
		return Project{}, errors.New("instance is closed")
	}
	var exists int
	if err := i.db.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE slug=?", slug).Scan(&exists); err != nil {
		return Project{}, err
	}
	if exists != 0 {
		return Project{}, errors.New("project name is already in use")
	}
	stage := filepath.Join(i.directory, "projects", ".new-"+project.NewID())
	p, err := project.Create(ctx, stage)
	if err != nil {
		return Project{}, fmt.Errorf("project creation failed; staging data is preserved at %s: %w", stage, err)
	}
	info := Project{ID: p.ID(), Slug: slug, Created: time.Now().UTC()}
	if err := p.Close(); err != nil {
		return Project{}, fmt.Errorf("project %s could not close; data is preserved at %s: %w", info.ID, stage, err)
	}
	destination := filepath.Join(i.directory, "projects", info.ID)
	if err := renameNoReplace(stage, destination); err != nil {
		return Project{}, fmt.Errorf("project %s could not be published; data is preserved at %s: %w", info.ID, stage, err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return Project{}, fmt.Errorf("project %s is preserved at %s but its directory sync failed: %w", info.ID, destination, err)
	}
	_, err = i.db.ExecContext(ctx, "INSERT INTO projects(id,slug,created) VALUES(?,?,?)", info.ID, info.Slug, info.Created.Unix())
	if err != nil {
		return Project{}, fmt.Errorf("project %s is preserved at %s but is not registered: %w; stop the instance, move it outside projects, then import it to recover", info.ID, destination, err)
	}
	return info, nil
}

// ResetAccount is an explicit local-operator recovery operation. It does not
// change account roles or enabled state; it invalidates sessions and
// credentials and requires the account to choose a new password at next login.
func (i *Instance) ResetAccount(ctx context.Context, username, newPassword string) (User, error) {
	if !validUsername(username) || validatePassword(newPassword) != nil {
		return User{}, ErrInvalid
	}
	salt, hash, err := hashPassword(ctx, newPassword)
	if err != nil {
		return User{}, err
	}
	params := currentPasswordParams
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.Store == nil {
		return User{}, errors.New("instance is closed")
	}
	var result User
	err = i.tx(ctx, func(tx *sql.Tx) error {
		var userID string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE username=?", username).Scan(&userID); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_salt=?,password_hash=?,password_version=?,password_memory=?,password_iterations=?,password_parallelism=?,password_key_len=?,force_password_change=1,auth_version=auth_version+1 WHERE id=?`, salt, hash, params.version, params.memory, params.iterations, params.parallelism, params.keyLen, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE personal_tokens SET revoked=1 WHERE user_id=?", userID); err != nil {
			return err
		}
		for _, table := range []string{"security_keys", "recovery_codes", "ceremonies"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE user_id=?", userID); err != nil {
				return err
			}
		}
		return scanUserTx(ctx, tx, userID, &result)
	})
	return result, err
}

// ImportProject copies a stopped legacy project into this instance without
// changing its immutable project ID. Registration follows durable publication;
// a crash in between leaves recoverable project data rather than a false row.
func (i *Instance) ImportProject(ctx context.Context, slug, source string) (Project, error) {
	if !validSlug(slug) {
		return Project{}, errors.New("project name must use 1–64 lowercase letters, digits or internal hyphens")
	}
	source, err := existingDirectory(source)
	if err != nil {
		return Project{}, err
	}
	configData, err := readRegularFile(filepath.Join(source, "project.json"))
	if err != nil {
		return Project{}, err
	}
	var config project.Config
	if err := json.Unmarshal(configData, &config); err != nil || !project.ValidID(config.ID) {
		return Project{}, errors.New("invalid source project identity")
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.Store == nil {
		return Project{}, errors.New("instance is closed")
	}
	var count int
	if err := i.db.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE slug=?", slug).Scan(&count); err != nil {
		return Project{}, err
	}
	if count != 0 {
		return Project{}, errors.New("project name is already in use")
	}
	if err := i.db.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE id=?", config.ID).Scan(&count); err != nil {
		return Project{}, err
	}
	if count != 0 {
		return Project{}, errors.New("project ID is already registered in this instance")
	}
	stage := filepath.Join(i.directory, "projects", ".import-"+project.NewID())
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := project.Backup(ctx, source, stage); err != nil {
		return Project{}, fmt.Errorf("import source project: %w", err)
	}
	if err := validateRegisteredProject(stage, config.ID); err != nil {
		return Project{}, fmt.Errorf("verify imported project: %w", err)
	}
	destination := filepath.Join(i.directory, "projects", config.ID)
	if err := renameNoReplace(stage, destination); err != nil {
		return Project{}, fmt.Errorf("publish imported project %s: %w", config.ID, err)
	}
	published = true
	info := Project{ID: config.ID, Slug: slug, Created: time.Now().UTC()}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return Project{}, fmt.Errorf("imported project %s is preserved at %s but directory sync failed: %w", info.ID, destination, err)
	}
	if _, err := i.db.ExecContext(ctx, "INSERT INTO projects(id,slug,created) VALUES(?,?,?)", info.ID, info.Slug, info.Created.Unix()); err != nil {
		return Project{}, fmt.Errorf("imported project %s is preserved at %s but is not registered: %w", info.ID, destination, err)
	}
	return info, nil
}

func (i *Instance) Project(ctx context.Context, slug string) (Project, error) {
	var p Project
	var created int64
	err := i.db.QueryRowContext(ctx, "SELECT id,slug,created FROM projects WHERE slug=?", slug).Scan(&p.ID, &p.Slug, &created)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	p.Created = time.Unix(created, 0).UTC()
	return p, err
}

func (i *Instance) Projects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT p.id,p.slug,p.created FROM projects p JOIN users u ON u.id=?
	 WHERE NOT u.disabled AND (u.admin OR EXISTS(SELECT 1 FROM memberships m WHERE m.project_id=p.id AND m.user_id=u.id)) ORDER BY p.slug`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Project
	for rows.Next() {
		var p Project
		var created int64
		if err := rows.Scan(&p.ID, &p.Slug, &created); err != nil {
			return nil, err
		}
		p.Created = time.Unix(created, 0).UTC()
		result = append(result, p)
	}
	return result, rows.Err()
}

// Acquire bounds open native stores to eight, evicting the least recently used
// idle store. A busy instance returns a retryable error instead of growing
// native caches without a bound. Release must follow completion of the request.
func (i *Instance) Acquire(ctx context.Context, id string) (*project.Project, func(), error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.Store == nil {
		return nil, nil, errors.New("instance is closed")
	}
	slot := i.open[id]
	if slot == nil {
		if !project.ValidID(id) {
			return nil, nil, project.ErrInvalid
		}
		var present int
		if err := i.db.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE id=?", id).Scan(&present); err != nil {
			return nil, nil, err
		}
		if present != 1 {
			return nil, nil, project.ErrNotFound
		}
		if len(i.open) >= 8 {
			var oldest string
			for key, v := range i.open {
				if v.users == 0 && (oldest == "" || v.used < i.open[oldest].used) {
					oldest = key
				}
			}
			if oldest == "" {
				return nil, nil, errors.New("all project slots are busy; retry shortly")
			}
			if err := i.open[oldest].p.Close(); err != nil {
				return nil, nil, err
			}
			delete(i.open, oldest)
		}
		p, err := project.Open(ctx, filepath.Join(i.directory, "projects", id))
		if err != nil {
			return nil, nil, err
		}
		if p.ID() != id {
			p.Close()
			return nil, nil, errors.New("stored project identity mismatch")
		}
		slot = &openedProject{p: p}
		i.open[id] = slot
	}
	i.clock++
	slot.used, slot.users = i.clock, slot.users+1
	var once sync.Once
	return slot.p, func() {
		once.Do(func() {
			i.mu.Lock()
			defer i.mu.Unlock()
			if slot.users > 0 {
				slot.users--
			}
		})
	}, nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (i *Instance) validateRegisteredProjects(ctx context.Context) error {
	rows, err := i.db.QueryContext(ctx, "SELECT id FROM projects ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if !project.ValidID(id) {
			return fmt.Errorf("instance registry contains invalid project ID %q", id)
		}
		if err := validateRegisteredProject(filepath.Join(i.directory, "projects", id), id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func validateRegisteredProject(directory, id string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("registered project %s: %w", id, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("registered project %s has an invalid directory", id)
	}
	data, err := readRegularFile(filepath.Join(directory, "project.json"))
	if err != nil {
		return fmt.Errorf("registered project %s configuration: %w", id, err)
	}
	var config project.Config
	if err := json.Unmarshal(data, &config); err != nil || config.ID != id || !project.ValidID(config.ID) {
		return fmt.Errorf("registered project %s has an invalid project identity", id)
	}
	historyInfo, err := os.Lstat(filepath.Join(directory, "history.sqlite"))
	if err != nil {
		return fmt.Errorf("registered project %s history database: %w", id, err)
	}
	if !historyInfo.Mode().IsRegular() || historyInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("registered project %s history database is not a regular file", id)
	}
	contentInfo, err := os.Lstat(filepath.Join(directory, "content", "repository"))
	if err != nil {
		return fmt.Errorf("registered project %s content repository is missing or invalid: %w", id, err)
	}
	if !contentInfo.IsDir() || contentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("registered project %s content repository is not a directory", id)
	}
	return nil
}

func existingDirectory(directory string) (string, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("instance path is not a directory")
	}
	return resolved, nil
}

func targetPaths(directory string) (destination, parent, name string, err error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", "", "", err
	}
	abs = filepath.Clean(abs)
	name = filepath.Base(abs)
	if name == "." || name == string(filepath.Separator) || name == ".." {
		return "", "", "", errors.New("instance path must name a directory")
	}
	parent, err = filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", "", "", err
	}
	destination = filepath.Join(parent, name)
	return destination, parent, name, nil
}

func readRegularFile(filename string) ([]byte, error) {
	fd, err := unix.Open(filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filename)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open file descriptor")
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	data, readErr := io.ReadAll(f)
	return data, errors.Join(readErr, f.Close())
}

func writeSyncedFile(filename string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func syncFile(filename string) error {
	f, err := os.OpenFile(filename, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func renameNoReplace(source, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}
