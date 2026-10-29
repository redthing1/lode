// Package workspace connects selected ordinary files to a shared project.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

var ErrPending = errors.New("unfinished operation; run lode resume")
var ErrNoChanges = errors.New("no selected changes")

type State struct {
	Server     string          `json:"server"`
	InstanceID string          `json:"instance"`
	Owner      string          `json:"owner"`
	Project    string          `json:"project"`
	ProjectID  string          `json:"project_id"`
	Branch     string          `json:"branch"`
	Version    string          `json:"version"`
	Selection  model.Selection `json:"selection"`
	Baseline   []model.Entry   `json:"baseline"`
}

type Workspace struct {
	directory  string
	root, meta *os.Root
	lock       *flock.Flock
	state      State
}

func (w *Workspace) State() State      { return w.state }
func (w *Workspace) Directory() string { return w.directory }

func (s State) validate() error {
	if s.Server == "" || s.InstanceID == "" || s.ProjectID == "" || !model.ValidPath(s.Owner) || !model.ValidPath(s.Project) || strings.Contains(s.Owner+s.Project, "/") || !model.ValidReference(s.Branch) {
		return errors.New("invalid workspace identity")
	}
	if err := s.Selection.Validate(); err != nil {
		return err
	}
	if err := model.ValidateInventory(s.Baseline); err != nil {
		return err
	}
	for _, e := range s.Baseline {
		if !s.Selection.Includes(e.Path) {
			return fmt.Errorf("baseline contains unselected path %q", e.Path)
		}
	}
	return nil
}

func Open(directory string) (*Workspace, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("workspace path must be a directory")
	}
	for {
		info, err := os.Lstat(filepath.Join(root, ".lode"))
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New(".lode must be a directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(root)
		if parent == root {
			return nil, errors.New("not in a Lode workspace")
		}
		root = parent
	}
	w, err := openDirectory(root)
	if err != nil {
		return nil, err
	}
	if err := w.readJSON("state.json", &w.state); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.state.validate(); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func openDirectory(directory string) (_ *Workspace, err error) {
	w := &Workspace{directory: directory}
	defer func() {
		if err != nil {
			w.Close()
		}
	}()
	w.root, err = os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	w.meta, err = w.root.OpenRoot(".lode")
	if err != nil {
		return nil, err
	}
	if info, e := w.meta.Lstat("lock"); e == nil && !info.Mode().IsRegular() {
		return nil, errors.New("invalid workspace lock")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	w.lock = flock.New(filepath.Join(directory, ".lode", "lock"))
	locked, err := w.lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("workspace is in use")
	}
	return w, nil
}

func (w *Workspace) Close() error {
	var result error
	if w.meta != nil {
		result = errors.Join(result, w.meta.Close())
		w.meta = nil
	}
	if w.root != nil {
		result = errors.Join(result, w.root.Close())
		w.root = nil
	}
	if w.lock != nil {
		result = errors.Join(result, w.lock.Close())
		w.lock = nil
	}
	return result
}

func (w *Workspace) Client(token string) (*remote.Client, error) {
	c, err := remote.New(w.state.Server, token)
	if err != nil {
		return nil, err
	}
	c.InstanceID = w.state.InstanceID
	c.ProjectID = w.state.ProjectID
	return c, nil
}

type ConnectOptions struct {
	Branch    string
	Selection model.Selection
}

func connect(ctx context.Context, projectURL string, c *remote.Client, options ConnectOptions) (State, error) {
	u, err := url.Parse(projectURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return State{}, errors.New("use a project URL ending in /owner/project")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !model.ValidPath(strings.Join(parts, "/")) {
		return State{}, errors.New("use a project URL ending in /owner/project")
	}
	u.Path = ""
	u.RawPath = ""
	if u.String() != c.Origin() {
		return State{}, errors.New("project URL does not match the authenticated server")
	}
	identity, err := c.Me(ctx)
	if err != nil {
		return State{}, err
	}
	c.InstanceID = identity.Instance
	p, err := c.Project(ctx, parts[0], parts[1])
	if err != nil {
		return State{}, err
	}
	c.ProjectID = p.ID
	if options.Selection == nil {
		options.Selection = model.Selection{{Include: true}}
	}
	if options.Branch == "" {
		options.Branch = "main"
	}
	state := State{Server: u.String(), InstanceID: identity.Instance, Owner: p.Owner, Project: p.Name, ProjectID: p.ID, Branch: options.Branch, Selection: options.Selection}
	if err := state.validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

func initialize(directory string, state State, plan *transition) (*Workspace, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(root, ".lode")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(errors.New("workspace metadata already exists"), err)
	}
	// Publish populated metadata in one rename. Another initialized workspace is
	// nonempty and cannot be replaced by this directory rename.
	staging, err := os.MkdirTemp(filepath.Dir(root), ".lode-init-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	meta, err := os.OpenRoot(staging)
	if err != nil {
		return nil, err
	}
	temporary := &Workspace{meta: meta}
	err = temporary.writeJSON("state.json", state)
	if err == nil && plan != nil {
		err = temporary.writeTransition(*plan)
	}
	err = errors.Join(err, meta.Close())
	if err != nil {
		return nil, err
	}
	if err := os.Rename(staging, filepath.Join(root, ".lode")); err != nil {
		return nil, err
	}
	parent, err := os.Open(filepath.Dir(root))
	if err != nil {
		return nil, err
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
		return nil, err
	}
	w, err := Open(root)
	if err != nil {
		return nil, err
	}
	if err := syncPath(w.root, "."); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

// Attach connects an existing folder to an empty shared branch. Clone obtains an
// existing project; attach never infers deletion of files that were not downloaded.
func Attach(ctx context.Context, projectURL string, c *remote.Client, directory string, options ConnectOptions) (*Workspace, error) {
	state, err := connect(ctx, projectURL, c, options)
	if err != nil {
		return nil, err
	}
	head, err := c.Head(ctx, state.Owner, state.Project, state.Branch)
	if err != nil {
		return nil, err
	}
	if head != "" {
		return nil, errors.New("project already has versions; clone it into a new folder")
	}
	return initialize(directory, state, nil)
}

func (w *Workspace) readJSON(name string, value any) error {
	b, err := w.meta.ReadFile(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
func (w *Workspace) writeJSON(name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := name + ".next"
	if err := w.meta.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := w.meta.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := w.meta.Rename(temporary, name); err != nil {
		return err
	}
	return syncPath(w.meta, path.Dir(name))
}
func syncPath(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func (w *Workspace) remove(name string) error {
	if err := w.meta.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncPath(w.meta, path.Dir(name))
}
func (w *Workspace) pending(name string) (bool, error) {
	_, err := w.meta.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
