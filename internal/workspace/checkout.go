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
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/redthing1/lode/internal/model"
)

type materialization struct {
	Target State `json:"target"`
}

func Clone(ctx context.Context, url, token, directory string, selection model.Selection) (*Workspace, error) {
	client, state, err := connect(ctx, url, token, "main", selection)
	if err != nil {
		return nil, err
	}
	state.Baseline, err = client.Entries(ctx, state.Base, state.Selection)
	if err != nil {
		return nil, err
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, err
	}
	parent, err := os.Open(filepath.Dir(directory))
	if err != nil {
		return nil, err
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
		return nil, err
	}
	w, err := initialize(directory)
	if err != nil {
		return nil, err
	}
	w.state = state
	plan := materialization{Target: state}
	if err := w.store("checkout.json", plan); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.materialize(ctx, plan); err != nil {
		w.Close()
		return nil, fmt.Errorf("checkout incomplete; use resume in %s: %w", directory, err)
	}
	return w, nil
}

// Initial checkout only adds files. Its sealed target and the installed bytes
// are enough to infer progress; no replacement ledger is needed yet.
func (w *Workspace) materialize(ctx context.Context, plan materialization) error {
	if err := plan.Target.validate(); err != nil {
		return err
	}
	var installed State
	if err := w.load("state.json", &installed); err == nil {
		if !reflect.DeepEqual(installed, plan.Target) {
			return errors.New("checkout does not match the installed baseline")
		}
		w.state = installed
		return w.remove("checkout.json")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	client, err := w.client()
	if err != nil {
		return err
	}
	// A crash may leave this name hard-linked to an installed user file.
	// Unlink it before opening a new staging file; never truncate that inode.
	if _, err := w.root.Lstat(".lode/download"); err == nil {
		if err := w.remove("download"); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries := append([]model.Entry(nil), plan.Target.Baseline...)
	model.Sort(entries)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Kind == model.Directory {
			if err := w.directory(e.Path); err != nil {
				return err
			}
			continue
		}
		if err := w.directory(path.Dir(e.Path)); err != nil {
			return err
		}
		if _, err := w.root.Lstat(e.Path); err == nil {
			actual, err := w.identify(e.Path)
			if err != nil {
				return err
			}
			if actual != e {
				return fmt.Errorf("checkout would overwrite %s; move it aside before resuming", e.Path)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		reader, err := client.Read(ctx, plan.Target.Base, e.Path)
		if err != nil {
			return err
		}
		file, err := w.root.OpenFile(".lode/download", os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			reader.Close()
			return err
		}
		err = copyVerified(file, reader, e)
		err = errors.Join(err, reader.Close(), file.Sync(), file.Close())
		if err != nil {
			return err
		}
		// Link installs without replacing an unexpected path. The staged inode
		// remains recoverable until the new name is durable.
		if err := w.root.Link(".lode/download", e.Path); err != nil {
			return fmt.Errorf("install %s: %w", e.Path, err)
		}
		if err := syncDirectory(w.root, path.Dir(e.Path)); err != nil {
			return err
		}
		if err := w.remove("download"); err != nil {
			return err
		}
	}
	if _, err := w.root.Lstat(".lode/download"); err == nil {
		if err := w.remove("download"); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := w.store("state.json", plan.Target); err != nil {
		return err
	}
	w.state = plan.Target
	return w.remove("checkout.json")
}

func (w *Workspace) directory(name string) error {
	if name == "." {
		return nil
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		info, err := w.root.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			if err := w.root.Mkdir(p, 0755); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !info.IsDir() {
			return fmt.Errorf("checkout needs an ordinary directory at %s", p)
		}
		if err := syncDirectory(w.root, path.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}

func copyVerified(dst io.Writer, src io.Reader, e model.Entry) error {
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(dst, hash), src, e.Size); err != nil {
		return fmt.Errorf("read %s: %w", e.Path, err)
	}
	var extra [1]byte
	if n, err := io.ReadFull(src, extra[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("unexpected content length for %s", e.Path)
	}
	if hex.EncodeToString(hash.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("content hash mismatch for %s", e.Path)
	}
	return nil
}
