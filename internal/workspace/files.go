package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

// Status observes only the selected working set. Any failed or unsupported
// observation fails the scan; it is never converted into a deletion.
func (w *Workspace) Status(ctx context.Context) ([]project.Change, error) {
	if w.state.Pending != nil {
		return nil, ErrPending
	}
	selection, err := project.Selection(w.state.Selection)
	if err != nil {
		return nil, err
	}
	local, err := w.scan(ctx, selection)
	if err != nil {
		return nil, err
	}
	base := make(map[string]project.Node, len(w.state.Baseline))
	for _, node := range w.state.Baseline {
		base[node.Path] = node
	}
	paths := make(map[string]bool, len(base)+len(local))
	for p := range base {
		paths[p] = true
	}
	for p := range local {
		paths[p] = true
	}
	var changes []project.Change
	for p := range paths {
		logical, _ := model.ParsePath(p)
		if !selection.Includes(logical) {
			continue
		}
		before, after := base[p], local[p]
		if sameNode(before, after) {
			continue
		}
		kind := after.Kind
		if kind == 0 {
			kind = model.Absent
		}
		changes = append(changes, project.Change{Path: p, Kind: kind, SHA256: after.SHA256, Size: after.Size})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

func (w *Workspace) scan(ctx context.Context, selection model.Selection) (map[string]project.Node, error) {
	result := make(map[string]project.Node)
	var walk func(string) error
	walk = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := w.root.OpenFile(directory, readFlags(), 0)
		if err != nil {
			return err
		}
		children, readErr := f.ReadDir(-1)
		if err = errors.Join(readErr, f.Close()); err != nil {
			return err
		}
		for _, child := range children {
			if directory == "." && child.Name() == ".lode" {
				continue
			}
			p := child.Name()
			if directory != "." {
				p = directory + "/" + p
			}
			logical, err := model.ParsePath(p)
			if err != nil {
				return fmt.Errorf("unsupported local name %q: %w", p, err)
			}
			if !selection.Intersects(logical) {
				continue
			}
			info, err := w.root.Lstat(p)
			if err != nil {
				return fmt.Errorf("observe %q: %w", p, err)
			}
			switch {
			case info.IsDir():
				result[p] = project.Node{Path: p, Kind: model.Directory}
				if err := walk(p); err != nil {
					return err
				}
			case info.Mode().IsRegular():
				if !selection.Includes(logical) {
					return fmt.Errorf("%w: file blocks selected descendants at %q", ErrConflict, p)
				}
				node, err := w.observeFile(ctx, p, info)
				if err != nil {
					return err
				}
				result[p] = node
			default:
				return fmt.Errorf("unsupported local entry %q: only regular files and directories are supported", p)
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	return result, nil
}

func (w *Workspace) observeFile(ctx context.Context, p string, before os.FileInfo) (project.Node, error) {
	f, err := w.openRegular(p)
	if err != nil {
		return project.Node{}, err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(hash, &contextReader{ctx: ctx, reader: f})
	after, statErr := f.Stat()
	closeErr := f.Close()
	if err := errors.Join(copyErr, statErr, closeErr); err != nil {
		return project.Node{}, err
	}
	now, err := w.root.Lstat(p)
	if err != nil {
		return project.Node{}, err
	}
	if !os.SameFile(before, after) || !os.SameFile(after, now) || before.Size() != n || after.Size() != n || now.Size() != n || !before.ModTime().Equal(after.ModTime()) || !after.ModTime().Equal(now.ModTime()) {
		return project.Node{}, fmt.Errorf("local file changed during observation: %q", p)
	}
	return project.Node{Path: p, Kind: model.File, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: n}, nil
}

// checkParents rejects symbolic links and non-directories, including links
// inside the workspace. os.Root additionally prevents traversal outside it.
func (w *Workspace) checkParents(p string) error {
	components := strings.Split(p, "/")
	for i := 1; i < len(components); i++ {
		parent := strings.Join(components[:i], "/")
		info, err := w.root.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: parent %q is not a directory", ErrConflict, parent)
		}
	}
	return nil
}

func (w *Workspace) openRegular(p string) (*os.File, error) {
	if err := w.checkParents(p); err != nil {
		return nil, err
	}
	f, err := w.root.OpenFile(p, readFlags(), 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%q is not a regular file", p)
	}
	return f, nil
}

func (w *Workspace) checkout(ctx context.Context) (project.Commit, error) {
	pending := w.state.Pending
	if pending.ReadID == "" {
		copyPending := *pending
		copyPending.ReadID = randomID()
		next := w.state
		next.Pending = &copyPending
		if err := w.save(next); err != nil {
			return project.Commit{}, err
		}
		pending = w.state.Pending
	}
	needsRead, err := w.checkoutNeedsDownload(ctx, pending)
	if err != nil {
		return project.Commit{}, err
	}
	if needsRead && pending.Target != "" {
		var result project.Commit
		err := w.withReadHold(ctx, pending.ReadID, pending.Target, func(heldCtx context.Context) error {
			var err error
			result, err = w.checkoutHeld(heldCtx)
			return err
		})
		return result, err
	}
	return w.checkoutHeld(ctx)
}

func (w *Workspace) checkoutNeedsDownload(ctx context.Context, pending *pending) (bool, error) {
	for _, node := range pending.Entries {
		if node.Kind != model.File {
			continue
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := w.checkParents(node.Path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			return false, err
		}
		info, err := w.root.Lstat(node.Path)
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("%w at %q", ErrConflict, node.Path)
		}
		observed, err := w.observeFile(ctx, node.Path, info)
		if err != nil {
			return false, err
		}
		if !sameNode(observed, node) {
			return false, fmt.Errorf("%w at %q", ErrConflict, node.Path)
		}
	}
	return false, nil
}

func (w *Workspace) checkoutHeld(ctx context.Context) (project.Commit, error) {
	pending := w.state.Pending
	if err := validateNodes(pending.Entries); err != nil {
		return project.Commit{}, err
	}
	for _, node := range pending.Entries {
		if err := ctx.Err(); err != nil {
			return project.Commit{}, err
		}
		if err := w.checkParents(node.Path); err != nil {
			return project.Commit{}, err
		}
		info, err := w.root.Lstat(node.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return project.Commit{}, err
		}
		if node.Kind == model.Directory {
			if err == nil && !info.IsDir() {
				return project.Commit{}, fmt.Errorf("%w at %q", ErrConflict, node.Path)
			}
			if errors.Is(err, os.ErrNotExist) {
				if err = w.root.Mkdir(node.Path, 0755); err != nil {
					return project.Commit{}, err
				}
			}
			if err = syncRoot(w.root, path.Dir(node.Path)); err != nil {
				return project.Commit{}, err
			}
			continue
		}
		if err == nil {
			if !info.Mode().IsRegular() {
				return project.Commit{}, fmt.Errorf("%w at %q", ErrConflict, node.Path)
			}
			observed, err := w.observeFile(ctx, node.Path, info)
			if err != nil {
				return project.Commit{}, err
			}
			if !sameNode(observed, node) {
				return project.Commit{}, fmt.Errorf("%w at %q", ErrConflict, node.Path)
			}
			// A previous process may have linked this file but stopped before
			// syncing the target directory. Re-establish that ordering.
			if err = syncRoot(w.root, path.Dir(node.Path)); err != nil {
				return project.Commit{}, err
			}
			continue
		}
		if err = w.download(ctx, pending.Target, node); err != nil {
			return project.Commit{}, err
		}
	}
	next := w.state
	next.Base = pending.Target
	next.Baseline = pending.Entries
	next.Pending = nil
	if err := w.save(next); err != nil {
		return project.Commit{}, err
	}
	if next.Base == "" {
		return project.Commit{}, nil
	}
	return w.client.Commit(ctx, next.Base)
}

func (w *Workspace) download(ctx context.Context, commit string, node project.Node) error {
	name, err := w.fetchFile(ctx, commit, node)
	if err != nil {
		return err
	}
	defer w.private.Remove(name)
	return linkNoReplace(w.root, ".lode/"+name, node.Path)
}

// fetchFile returns a verified, synced private temporary file. Its caller owns
// installation and unlinking; an incomplete download never becomes a target.
func (w *Workspace) fetchFile(ctx context.Context, commit string, node project.Node) (_ string, err error) {
	reader, actual, err := w.client.OpenFile(ctx, commit, node.Path)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if actual != node {
		return "", errors.New("remote file facts changed")
	}
	name := "download-" + randomID() + ".tmp"
	f, err := w.private.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = w.private.Remove(name)
		}
	}()
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(reader, node.Size+1))
	if copyErr != nil || n != node.Size || hex.EncodeToString(hash.Sum(nil)) != node.SHA256 {
		_ = f.Close()
		return "", fmt.Errorf("download verification failed for %q: %w", node.Path, errors.Join(copyErr, ErrConflict))
	}
	err = errors.Join(f.Chmod(0644), f.Sync(), f.Close())
	if err != nil {
		return "", err
	}
	return name, nil
}

func syncRoot(root *os.Root, name string) error {
	f, err := root.OpenFile(name, readFlags(), 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func syncDirectory(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
