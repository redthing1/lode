package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/redthing1/lode/internal/model"
)

// Status observes only the selection. Any observation error aborts the whole
// result; a failed read cannot become an inferred deletion.
func (w *Workspace) Status() ([]model.Entry, error) {
	if err := w.pending(); err != nil {
		return nil, err
	}
	current := map[string]model.Entry{}
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
		selected := w.state.Selection.Includes(name)
		if d.IsDir() && !w.state.Selection.Descends(name) {
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
		current[name] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	previous := make(map[string]model.Entry, len(w.state.Baseline))
	for _, e := range w.state.Baseline {
		previous[e.Path] = e
	}
	var changes []model.Entry
	for name, e := range previous {
		if _, exists := current[name]; !exists {
			changes = append(changes, model.Entry{Path: e.Path, Kind: model.Absent})
		}
	}
	for name, e := range current {
		if previous[name] != e {
			changes = append(changes, e)
		}
	}
	model.Sort(changes)
	return changes, nil
}

func (w *Workspace) identify(name string) (model.Entry, error) {
	f, err := w.openFile(name)
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
	return model.Entry{Path: name, Kind: model.File, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
