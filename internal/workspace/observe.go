package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/progress"
)

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func (w *Workspace) observe(ctx context.Context) ([]model.Entry, error) {
	entries, err := w.observeSelection(ctx, w.state.Selection)
	return model.Selected(entries, w.state.Selection), err
}

func (w *Workspace) observeSelection(ctx context.Context, selection model.Selection, incoming ...model.Entry) ([]model.Entry, error) {
	ignored, err := w.ignoreNew(incoming)
	if err != nil {
		return nil, err
	}
	progress.Report(ctx, progress.Event{Phase: "Checking"})
	files := 0
	fileCtx := progress.WithReporter(ctx, func(e progress.Event) {
		e.Files = files
		progress.Report(ctx, e)
	})
	var entries []model.Entry
	err = fs.WalkDir(w.root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == ".lode" {
			return fs.SkipDir
		}
		if name == "." {
			if !selection.Descends("") {
				return fs.SkipDir
			}
			return nil
		}
		included := selection.Includes(name)
		if d.IsDir() && !selection.Descends(name) {
			return fs.SkipDir
		}
		if !included && !selection.Descends(name) {
			return nil
		}
		if ignored(name, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !model.ValidPath(name) || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("unsupported local path %q", name)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			entries = append(entries, model.Entry{Path: name, Kind: model.Directory})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%q is not a regular file or directory", name)
		}
		if !included {
			return fmt.Errorf("cannot inspect selected descendants of file %q", name)
		}
		files++
		e, err := w.observeFile(fileCtx, name, info)
		if err != nil {
			return err
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (w *Workspace) observeFile(ctx context.Context, name string, observed fs.FileInfo) (model.Entry, error) {
	f, err := w.root.Open(filepath.FromSlash(name))
	if err != nil {
		return model.Entry{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return model.Entry{}, err
	}
	if !before.Mode().IsRegular() || !os.SameFile(observed, before) {
		return model.Entry{}, fmt.Errorf("%q changed while inspecting it", name)
	}
	h := sha256.New()
	displayName := name
	if strings.HasPrefix(name, ".lode/") {
		displayName = ""
	}
	n, err := io.Copy(h, progress.Reader(ctx, "Checking", displayName, before.Size(), contextReader{ctx, f}))
	if err != nil {
		return model.Entry{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return model.Entry{}, err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return model.Entry{}, fmt.Errorf("%q changed while reading it", name)
	}
	return model.Entry{Path: name, Kind: model.File, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func (w *Workspace) Status(ctx context.Context) ([]model.Entry, error) {
	if pending, err := w.pending("transition.json"); err != nil {
		return nil, err
	} else if pending {
		return nil, ErrPending
	}
	observed, err := w.observe(ctx)
	if err != nil {
		return nil, err
	}
	return model.Changes(w.state.Baseline, observed, w.state.Selection), nil
}

func (w *Workspace) checkParents(name string) error {
	if !model.ValidPath(name) || !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("unsupported local path %q", name)
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := w.root.Lstat(filepath.FromSlash(parent))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q is not an ordinary directory", parent)
		}
	}
	return nil
}

func (w *Workspace) openSource(e model.Entry) (*os.File, error) {
	if err := w.checkParents(e.Path); err != nil {
		return nil, err
	}
	info, err := w.root.Lstat(filepath.FromSlash(e.Path))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", e.Path)
	}
	f, err := w.root.Open(filepath.FromSlash(e.Path))
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() != e.Size {
		f.Close()
		return nil, errors.Join(fmt.Errorf("%q changed since this save began", e.Path), err)
	}
	return f, nil
}
