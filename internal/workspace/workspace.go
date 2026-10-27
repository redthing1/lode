// Package workspace owns selected local files, their baseline, and recovery.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
)

var ErrPending = errors.New("workspace has an unfinished operation; use resume or cancel")

type State struct {
	Version   int             `json:"version"`
	URL       string          `json:"url"`
	Token     string          `json:"token"`
	Project   string          `json:"project"`
	Branch    string          `json:"branch"`
	Base      string          `json:"base"`
	Selection model.Selection `json:"selection"`
	Baseline  []model.Entry   `json:"baseline"`
}

type Workspace struct {
	root  *os.Root
	lock  *os.File
	state State
}

func Open(directory string) (*Workspace, error) {
	w, err := lock(directory)
	if err != nil {
		return nil, err
	}
	if err := w.load("state.json", &w.state); err != nil {
		// A checkout has a sealed target before it has an installed baseline.
		var plan materialization
		if !errors.Is(err, os.ErrNotExist) {
			w.Close()
			return nil, err
		}
		if err := w.load("checkout.json", &plan); err != nil {
			w.Close()
			return nil, err
		}
		w.state = plan.Target
	}
	if err := w.state.validate(); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func lock(directory string) (*Workspace, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	w := &Workspace{root: root}
	info, err := root.Lstat(".lode")
	if err == nil && !info.IsDir() {
		err = errors.New(".lode must be a directory")
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	w.lock, err = root.OpenFile(".lode/lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err == nil {
		err = syscall.Flock(int(w.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("lock workspace: %w", err)
	}
	return w, nil
}

func (w *Workspace) Close() error {
	var err error
	if w.lock != nil {
		err = w.lock.Close()
	}
	return errors.Join(err, w.root.Close())
}

func (w *Workspace) State() State { return w.state }

func (s State) validate() error {
	if s.Version != 1 || !project.ValidID(s.Project) || s.Branch == "" || (s.Base != "" && !project.ValidID(s.Base)) {
		return errors.New("invalid workspace state")
	}
	if err := s.Selection.Validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range s.Baseline {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.Kind == model.Absent || !s.Selection.Includes(e.Path) || seen[e.Path] {
			return errors.New("invalid selected baseline")
		}
		seen[e.Path] = true
	}
	_, err := remote.New(s.URL, s.Token, s.Project)
	return err
}

func connect(ctx context.Context, url, token, branch string, selection model.Selection) (*remote.Client, State, error) {
	if selection == nil {
		selection = model.Selection{{Include: true}}
	}
	if err := selection.Validate(); err != nil {
		return nil, State{}, err
	}
	client, err := remote.New(url, token, "")
	if err != nil {
		return nil, State{}, err
	}
	info, err := client.Info(ctx)
	if err != nil {
		return nil, State{}, err
	}
	client, err = remote.New(url, token, info.ID)
	if err != nil {
		return nil, State{}, err
	}
	head, err := client.Head(ctx, branch)
	if err != nil {
		return nil, State{}, err
	}
	return client, State{Version: 1, URL: url, Token: token, Project: info.ID, Branch: branch, Base: head, Selection: selection}, nil
}

func initialize(directory string) (*Workspace, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	err = root.Mkdir(".lode", 0700)
	if err == nil {
		err = syncDirectory(root, ".")
	}
	err = errors.Join(err, root.Close())
	if err != nil {
		return nil, err
	}
	return lock(directory)
}

// Attach starts tracking an existing folder in an empty project. Existing
// projects use Clone so absent paths are never mistaken for local deletions.
func Attach(ctx context.Context, url, token, directory string, selection model.Selection) (*Workspace, error) {
	_, state, err := connect(ctx, url, token, "main", selection)
	if err != nil {
		return nil, err
	}
	if state.Base != "" {
		return nil, errors.New("project already has versions; use clone")
	}
	w, err := initialize(directory)
	if err != nil {
		return nil, err
	}
	w.state = state
	if err := w.store("state.json", state); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func (w *Workspace) client() (*remote.Client, error) {
	return remote.New(w.state.URL, w.state.Token, w.state.Project)
}

func (w *Workspace) pending() error {
	for _, name := range []string{"save.json", "checkout.json"} {
		if _, err := w.root.Lstat(".lode/" + name); err == nil {
			return ErrPending
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (w *Workspace) Save(ctx context.Context, author, message string) (project.Commit, error) {
	if strings.TrimSpace(message) == "" {
		return project.Commit{}, errors.New("save requires a nonempty message")
	}
	if err := w.pending(); err != nil {
		return project.Commit{}, err
	}
	changes, err := w.Status()
	if err != nil {
		return project.Commit{}, err
	}
	if len(changes) == 0 {
		return project.Commit{}, errors.New("no selected changes")
	}
	r := project.Request{ID: project.NewID(), Branch: w.state.Branch, Base: w.state.Base, Author: author, Message: message, Changes: changes}
	if err := w.store("save.json", r); err != nil {
		return project.Commit{}, err
	}
	return w.finishSave(ctx, r)
}

func (w *Workspace) finishSave(ctx context.Context, r project.Request) (project.Commit, error) {
	client, err := w.client()
	if err != nil {
		return project.Commit{}, err
	}
	commit, err := client.Save(ctx, r, project.Source{Open: func(name string) (io.ReadCloser, error) { return w.openFile(name) }})
	if err != nil {
		return project.Commit{}, err
	}
	if w.state.Base != commit.ID {
		if w.state.Base != r.Base {
			return project.Commit{}, errors.New("save does not match the workspace baseline")
		}
		entries := map[string]model.Entry{}
		for _, e := range w.state.Baseline {
			entries[e.Path] = e
		}
		for _, e := range r.Changes {
			if e.Kind == model.Absent {
				delete(entries, e.Path)
			} else {
				entries[e.Path] = e
			}
		}
		next := w.state
		next.Base = commit.ID
		next.Baseline = make([]model.Entry, 0, len(entries))
		for _, e := range entries {
			next.Baseline = append(next.Baseline, e)
		}
		model.Sort(next.Baseline)
		if err := w.store("state.json", next); err != nil {
			return project.Commit{}, err
		}
		w.state = next
	}
	return commit, w.remove("save.json")
}

func (w *Workspace) Resume(ctx context.Context) error {
	var request project.Request
	if err := w.load("save.json", &request); err == nil {
		_, err = w.finishSave(ctx, request)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var plan materialization
	if err := w.load("checkout.json", &plan); err != nil {
		return err
	}
	return w.materialize(ctx, plan)
}

// Cancel resolves publication before forgetting its intent. Installed checkout
// files stay in place; resuming checkout is required to establish its baseline.
func (w *Workspace) Cancel(ctx context.Context) error {
	var request project.Request
	if err := w.load("save.json", &request); err != nil {
		return err
	}
	client, err := w.client()
	if err != nil {
		return err
	}
	if err := client.Cancel(ctx, request.ID); err != nil {
		return err
	}
	return w.remove("save.json")
}

func (w *Workspace) load(name string, v any) error {
	f, err := w.root.OpenFile(".lode/"+name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("unexpected data after workspace state")
	}
	return nil
}

// store replaces one durable fact atomically. The fixed temporary name avoids
// accumulating files after repeated interruptions; the workspace lock owns it.
func (w *Workspace) store(name string, v any) error {
	f, err := w.root.OpenFile(".lode/write.tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(v)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := w.root.Rename(".lode/write.tmp", ".lode/"+name); err != nil {
		return err
	}
	return syncDirectory(w.root, ".lode")
}

func (w *Workspace) remove(name string) error {
	if err := w.root.Remove(".lode/" + name); err != nil {
		return err
	}
	return syncDirectory(w.root, ".lode")
}

func syncDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (w *Workspace) openFile(name string) (*os.File, error) {
	if !model.ValidPath(name) {
		return nil, errors.New("invalid workspace path")
	}
	for p := path.Dir(name); p != "."; p = path.Dir(p) {
		info, err := w.root.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not an ordinary directory", p)
		}
	}
	f, err := w.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
