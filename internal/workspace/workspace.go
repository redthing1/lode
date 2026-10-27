// Package workspace owns a selected working tree and its recoverable local state.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transport"
)

var (
	ErrBusy           = errors.New("workspace is in use")
	ErrPending        = errors.New("workspace has an incomplete operation; use resume or abort")
	ErrConflict       = errors.New("workspace content conflicts with the operation")
	ErrNothingChanged = errors.New("no selected changes")
)

type Options struct {
	Directory string
	Remote    string
	Token     string
	Branch    string
	Selection []project.SelectionRule
	// Adopt attaches an existing folder to an empty remote branch. Its files
	// remain local additions until explicitly committed.
	Adopt bool
}

type state struct {
	Version   int
	ProjectID string
	Remote    string
	Token     string
	Branch    string
	Base      string
	Selection []project.SelectionRule
	Baseline  []project.Node
	Pending   *pending
}

type pending struct {
	Kind       string
	Request    *project.ChangeRequest `json:",omitempty"`
	Merge      *project.MergeRequest  `json:",omitempty"`
	Target     string                 `json:",omitempty"`
	Entries    []project.Node         `json:",omitempty"`
	ReadID     string                 `json:",omitempty"`
	Transition *transition            `json:",omitempty"`
}

// Workspace holds an exclusive local lock. Callers serialize operations and
// close it before opening another handle on the same working folder.
type Workspace struct {
	root         *os.Root
	private      *os.Root
	lock         *os.File
	client       *transport.Client
	state        state
	lastRecovery string
	progressID   string
	progressHash string
	// Narrow mutation boundaries permit failures around real filesystem actions.
	moveEntry       func(string, string) error
	linkEntry       func(string, string) error
	replaceMetadata func(string, string) error
}

// Create attaches to an empty branch or materializes a new selected working
// folder. A failed materialization leaves a journal that Open and Resume use.
func Create(ctx context.Context, options Options) (_ *Workspace, err error) {
	if options.Branch == "" {
		options.Branch = "main"
	}
	if options.Selection == nil {
		options.Selection = []project.SelectionRule{{Included: true}}
	}
	selection, err := project.Selection(options.Selection)
	if err != nil {
		return nil, err
	}
	options.Selection = portableSelection(selection)
	client, err := transport.NewClient(options.Remote, options.Token)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			client.Close()
		}
	}()
	info, err := client.Info(ctx)
	if err != nil {
		return nil, err
	}
	head, err := client.Head(ctx, options.Branch)
	if err != nil {
		return nil, err
	}
	if options.Adopt && head != "" {
		return nil, errors.New("adopt requires an empty remote branch")
	}
	if head == "" {
		branches, _, err := client.Branches(ctx, "", 1)
		if err != nil {
			return nil, err
		}
		if len(branches) != 0 {
			return nil, fmt.Errorf("%w: %q", project.ErrBranchNotFound, options.Branch)
		}
	}
	entries, err := fetchEntries(ctx, client, head, options.Selection)
	if err != nil {
		return nil, err
	}
	if options.Directory == "" {
		return nil, errors.New("workspace directory is required")
	}
	if !options.Adopt {
		if err := os.Mkdir(options.Directory, 0755); err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
		if err := syncDirectory(filepath.Dir(options.Directory)); err != nil {
			return nil, err
		}
	}
	infoFS, err := os.Lstat(options.Directory)
	if err != nil {
		return nil, err
	}
	if !infoFS.IsDir() {
		return nil, errors.New("workspace must be a directory, not a symlink")
	}
	root, err := os.OpenRoot(options.Directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = root.Close()
		}
	}()
	if err := root.Mkdir(".lode", 0700); err != nil {
		return nil, fmt.Errorf("create workspace state: %w", err)
	}
	private, err := root.OpenRoot(".lode")
	if err != nil {
		return nil, err
	}
	w := &Workspace{root: root, private: private, client: client, state: state{
		Version: 1, ProjectID: info.ID, Remote: options.Remote, Token: options.Token,
		Branch: options.Branch, Selection: options.Selection,
	}}
	defer func() {
		if err != nil {
			_ = w.Close()
		}
	}()
	if err = w.takeLock(); err != nil {
		return nil, err
	}
	if err = syncRoot(root, "."); err != nil {
		return nil, err
	}
	if !options.Adopt {
		w.state.Pending = &pending{Kind: "checkout", Target: head, Entries: entries, ReadID: randomID()}
	}
	if err = w.save(w.state); err != nil {
		return nil, err
	}
	if !options.Adopt {
		if _, err = w.Resume(ctx); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func Open(directory string) (_ *Workspace, err error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	w := &Workspace{root: root}
	defer func() {
		if err != nil {
			_ = w.Close()
		}
	}()
	info, err := root.Lstat(".lode")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New(".lode must be a private directory")
	}
	w.private, err = root.OpenRoot(".lode")
	if err != nil {
		return nil, err
	}
	if err = w.takeLock(); err != nil {
		return nil, err
	}
	f, err := w.private.OpenFile("state.json", readFlags(), 0)
	if err != nil {
		return nil, err
	}
	fi, statErr := f.Stat()
	if statErr != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return nil, errors.New("workspace state must be a private regular file")
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&w.state)
	if err == nil && decoder.Decode(new(any)) != io.EOF {
		err = errors.New("trailing workspace state data")
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if err = validateState(w.state); err != nil {
		return nil, err
	}
	if err = w.loadProgress(); err != nil {
		return nil, err
	}
	if err = w.clearTemps(); err != nil {
		return nil, err
	}
	w.client, err = transport.NewClient(w.state.Remote, w.state.Token)
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Workspace) Close() error {
	var errs []error
	if w.client != nil {
		w.client.Close()
		w.client = nil
	}
	if w.lock != nil {
		errs = append(errs, w.lock.Close())
		w.lock = nil
	}
	if w.private != nil {
		errs = append(errs, w.private.Close())
		w.private = nil
	}
	if w.root != nil {
		errs = append(errs, w.root.Close())
		w.root = nil
	}
	return errors.Join(errs...)
}

func (w *Workspace) Base() string   { return w.state.Base }
func (w *Workspace) Branch() string { return w.state.Branch }
func (w *Workspace) Pending() string {
	if w.state.Pending == nil {
		return ""
	}
	return w.state.Pending.Kind
}

func (w *Workspace) takeLock() error {
	f, err := w.private.OpenFile("lock", os.O_RDWR|os.O_CREATE|noFollow(), 0600)
	if err != nil {
		return err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return err
	}
	w.lock = f
	return nil
}

func (w *Workspace) verifyRemote(ctx context.Context) error {
	info, err := w.client.Info(ctx)
	if err != nil {
		return err
	}
	if info.ID != w.state.ProjectID {
		return errors.New("remote project identity changed")
	}
	return nil
}

// save replaces the complete local state atomically. Even if the final directory
// sync fails, the caller stops and reopening determines which state persisted.
func (w *Workspace) save(next state) error {
	oldID := transitionID(w.state.Pending)
	var err error
	next, err = w.prepareProgress(next)
	if err != nil {
		return err
	}
	next.Version = 2
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	replaced, err := w.replacePrivate("state.json", "state-", append(encoded, '\n'))
	if replaced {
		w.state = next
	}
	if err != nil {
		return err
	}
	if oldID != "" && oldID != transitionID(next.Pending) {
		return w.removeProgress(oldID)
	}
	return nil
}

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}

func portableSelection(selection model.Selection) []project.SelectionRule {
	result := make([]project.SelectionRule, 0)
	for _, rule := range selection.Rules() {
		result = append(result, project.SelectionRule{Path: rule.Path.String(), Included: rule.Included})
	}
	return result
}

func fetchEntries(ctx context.Context, client *transport.Client, commit string, selection []project.SelectionRule) ([]project.Node, error) {
	var nodes []project.Node
	after := ""
	for {
		page, next, err := client.Entries(ctx, commit, selection, after, 256)
		if err != nil {
			return nil, err
		}
		for _, node := range page {
			if node.Path <= after {
				return nil, errors.New("remote metadata is not in canonical order")
			}
			if err := validateNode(node); err != nil {
				return nil, err
			}
			after = node.Path
			nodes = append(nodes, node)
		}
		if next == "" {
			break
		}
		if len(page) == 0 || next != after {
			return nil, errors.New("invalid remote metadata cursor")
		}
	}
	if err := validateNodes(nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// Only names generated for this workspace's interrupted writes are disposable.
// Working files and journals are never treated as cache entries.
func (w *Workspace) clearTemps() error {
	f, err := w.private.Open(".")
	if err != nil {
		return err
	}
	names, readErr := f.Readdirnames(-1)
	if err := errors.Join(readErr, f.Close()); err != nil {
		return err
	}
	for _, name := range names {
		stem := strings.TrimSuffix(name, ".tmp")
		if stem == name {
			continue
		}
		if strings.HasPrefix(stem, "download-") {
			stem = strings.TrimPrefix(stem, "download-")
		} else if strings.HasPrefix(stem, "progress-") {
			stem = strings.TrimPrefix(stem, "progress-")
		} else if strings.HasPrefix(stem, "state-") {
			stem = strings.TrimPrefix(stem, "state-")
		} else {
			continue
		}
		id, err := hex.DecodeString(stem)
		if err != nil || len(id) != 16 {
			continue
		}
		if err := w.private.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

func validateNode(node project.Node) error {
	p, err := model.ParsePath(node.Path)
	if err != nil || p.IsRoot() || node.Path == ".lode" || strings.HasPrefix(node.Path, ".lode/") {
		return errors.New("invalid workspace metadata path")
	}
	switch node.Kind {
	case model.File:
		digest, err := hex.DecodeString(node.SHA256)
		if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != node.SHA256 || node.Size < 0 || node.Size > project.MaxFileSize {
			return errors.New("invalid workspace file facts")
		}
	case model.Directory:
		if node.SHA256 != "" || node.Size != 0 {
			return errors.New("invalid workspace directory facts")
		}
	default:
		return errors.New("invalid workspace entry kind")
	}
	return nil
}

func validateState(s state) error {
	if (s.Version != 1 && s.Version != 2) || s.ProjectID == "" || s.Branch == "" {
		return errors.New("unsupported or incomplete workspace state")
	}
	if _, err := project.Selection(s.Selection); err != nil {
		return err
	}
	if err := validateNodes(s.Baseline); err != nil {
		return err
	}
	if s.Pending != nil {
		switch s.Pending.Kind {
		case "checkout":
			if s.Pending.Request != nil || s.Pending.Merge != nil || s.Pending.Transition != nil || s.Base != "" || len(s.Baseline) != 0 || s.Pending.ReadID != "" && !validReadID(s.Pending.ReadID) {
				return errors.New("invalid checkout journal")
			}
			return validateNodes(s.Pending.Entries)
		case "commit":
			if s.Pending.Request == nil || s.Pending.Merge != nil || s.Pending.Transition != nil || s.Pending.ReadID != "" || s.Pending.Request.Base != s.Base || s.Pending.Request.Branch != s.Branch || s.Pending.Target != "" || len(s.Pending.Entries) != 0 {
				return errors.New("invalid commit journal")
			}
			return project.ValidateChanges(*s.Pending.Request)
		case "merge":
			if s.Pending.Merge == nil || s.Pending.Request != nil || s.Pending.Transition != nil || s.Pending.ReadID != "" || s.Pending.Merge.Base != s.Base || s.Pending.Merge.Branch != s.Branch || s.Pending.Target != "" || len(s.Pending.Entries) != 0 {
				return errors.New("invalid merge journal")
			}
			return project.ValidateMergeRequest(*s.Pending.Merge)
		case "transition":
			if s.Pending.Request != nil || s.Pending.Merge != nil || s.Pending.ReadID != "" || s.Pending.Target != "" || len(s.Pending.Entries) != 0 {
				return errors.New("invalid transition journal")
			}
			return validateTransition(s.Pending.Transition)
		case "abandon":
			if s.Pending.Request != nil || s.Pending.Merge != nil {
				return errors.New("invalid abandonment journal")
			}
			if s.Pending.Transition != nil {
				if s.Pending.Target != "" || len(s.Pending.Entries) != 0 || s.Pending.ReadID != "" {
					return errors.New("invalid transition abandonment journal")
				}
				return validateTransition(s.Pending.Transition)
			}
			if s.Base != "" || len(s.Baseline) != 0 || s.Pending.ReadID != "" && !validReadID(s.Pending.ReadID) {
				return errors.New("invalid checkout abandonment journal")
			}
			return validateNodes(s.Pending.Entries)
		default:
			return errors.New("unknown workspace operation")
		}
	}
	return nil
}

func validateNodes(nodes []project.Node) error {
	entries := map[model.Path]model.Entry{{}: {Kind: model.Directory}}
	previous := ""
	for _, node := range nodes {
		if err := validateNode(node); err != nil {
			return err
		}
		if node.Path <= previous {
			return errors.New("workspace entries must be unique and sorted")
		}
		previous = node.Path
		path, _ := model.ParsePath(node.Path)
		entries[path] = entry(node)
	}
	_, err := model.ApplyChanges(entries, nil)
	return err
}

func entry(node project.Node) model.Entry {
	if node.Kind == 0 {
		return model.Entry{Kind: model.Absent}
	}
	result := model.Entry{Kind: node.Kind}
	if node.Kind == model.File {
		result.ContentID = "sha256:" + node.SHA256
	}
	return result
}

func sortedNodes(nodes map[string]project.Node) []project.Node {
	result := make([]project.Node, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func sameNode(a, b project.Node) bool {
	return a.Kind == b.Kind && a.SHA256 == b.SHA256 && a.Size == b.Size
}
