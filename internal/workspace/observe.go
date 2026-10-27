package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"

	"github.com/redthing1/lode/internal/model"
)

// Status observes only the selection. Any observation error aborts the whole
// result; a failed read cannot become an inferred deletion.
func (w *Workspace) Status() ([]model.Entry, error) {
	if err := w.pending(); err != nil {
		return nil, err
	}
	current, err := w.observe(w.state.Selection)
	if err != nil {
		return nil, err
	}
	var changes []model.Entry
	for _, change := range model.Diff(w.state.Baseline, current) {
		changes = append(changes, change.After)
	}
	return changes, nil
}

func (w *Workspace) observe(selections ...model.Selection) ([]model.Entry, error) {
	var current []model.Entry
	err := fs.WalkDir(w.root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if name == ".lode" {
			return fs.SkipDir
		}
		selected, descends := false, false
		for _, s := range selections {
			selected = selected || s.Includes(name)
			descends = descends || s.Descends(name)
		}
		if d.IsDir() && !descends {
			return fs.SkipDir
		}
		if !selected {
			return nil
		}
		if !model.ValidPath(name) {
			return fmt.Errorf("unsupported project path %q", name)
		}
		e := model.Entry{Path: name, Kind: model.Directory}
		if !d.IsDir() {
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("symlink is not supported: %s", name)
			}
			var err error
			e, err = w.identify(name)
			if err != nil {
				return err
			}
		}
		current = append(current, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	model.Sort(current)
	return current, nil
}

func (w *Workspace) identify(name string) (model.Entry, error) {
	e, err := w.identity(name, name)
	if err == nil && e.Kind != model.File {
		err = fmt.Errorf("%s changed during observation; retry", name)
	}
	return e, err
}

// identity can also inspect a private staged/backup file under its logical name.
func (w *Workspace) identity(name, logical string) (model.Entry, error) {
	info, err := w.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return model.Entry{Path: logical, Kind: model.Absent}, nil
	}
	if err != nil {
		return model.Entry{}, err
	}
	if info.IsDir() {
		return model.Entry{Path: logical, Kind: model.Directory}, nil
	}
	f, err := w.openRegular(name)
	if err != nil {
		return model.Entry{}, err
	}
	before, err := f.Stat()
	if err != nil {
		f.Close()
		return model.Entry{}, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	after, statErr := f.Stat()
	err = errors.Join(err, statErr, f.Close())
	if err != nil {
		return model.Entry{}, err
	}
	if n != before.Size() || n != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return model.Entry{}, fmt.Errorf("%s changed during observation; retry", name)
	}
	return model.Entry{Path: logical, Kind: model.File, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (w *Workspace) openRegular(name string) (*os.File, error) {
	parent, base, err := openRenameParent(w.root, name)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
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
