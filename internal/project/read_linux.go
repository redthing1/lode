package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

func (s *Store) Version(ctx context.Context, id string) (model.Version, error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	v, err := s.version(ctx, id)
	return v.Version, err
}

func (s *Store) Entry(ctx context.Context, version, name string) (model.Entry, error) {
	if !model.ValidPath(name) {
		return model.Entry{}, ErrRequest
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	v, err := s.version(ctx, version)
	if err != nil {
		return model.Entry{}, err
	}
	e, err := s.lookup(ctx, v.root, name)
	return e.Entry, err
}

// Directory lists only direct children, with a cursor over immutable metadata.
func (s *Store) Directory(ctx context.Context, version, name, after string, limit int) ([]model.Entry, string, error) {
	if (name != "" && !model.ValidPath(name)) || limit < 1 || limit > 100 {
		return nil, "", ErrRequest
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	v, err := s.version(ctx, version)
	if err != nil {
		return nil, "", err
	}
	root := v.root
	if name != "" {
		e, err := s.lookup(ctx, root, name)
		if err != nil {
			return nil, "", err
		}
		if e.Kind != model.Directory {
			return nil, "", ErrRequest
		}
		root = e.Object.ID
	}
	entries, err := s.directory(ctx, root)
	if err != nil {
		return nil, "", err
	}
	key := func(e model.Entry) string { return string(e.Kind) + ":" + path.Base(e.Path) }
	sort.Slice(entries, func(i, j int) bool { return key(entries[i].Entry) < key(entries[j].Entry) })
	var result []model.Entry
	for _, e := range entries {
		if key(e.Entry) <= after {
			continue
		}
		if len(result) == limit {
			return result, key(result[len(result)-1]), nil
		}
		entry := e.Entry
		entry.Path = path.Join(name, entry.Path)
		result = append(result, entry)
	}
	return result, "", nil
}

func (s *Store) lookup(ctx context.Context, root, name string) (content.Entry, error) {
	var entry content.Entry
	parts := strings.Split(name, "/")
	for i, part := range parts {
		var err error
		entry, err = s.child(ctx, root, part)
		if err != nil {
			return content.Entry{}, err
		}
		root = entry.Object.ID
		if i < len(parts)-1 && entry.Kind != model.Directory {
			return content.Entry{}, os.ErrNotExist
		}
	}
	entry.Path = name
	return entry, nil
}

// File holds the project's read gate until Close, preventing expiry during a read.
func (s *Store) File(ctx context.Context, version, name string) (model.Entry, io.ReadSeekCloser, error) {
	if !model.ValidPath(name) {
		return model.Entry{}, nil, fmt.Errorf("%w: invalid file path", ErrRequest)
	}
	s.gate.RLock()
	release := true
	defer func() {
		if release {
			s.gate.RUnlock()
		}
	}()
	v, err := s.version(ctx, version)
	if err != nil {
		return model.Entry{}, nil, err
	}
	if !v.Retained {
		return model.Entry{}, nil, ErrExpired
	}
	e, err := s.lookup(ctx, v.root, name)
	if err != nil {
		return model.Entry{}, nil, err
	}
	if e.Kind != model.File {
		return model.Entry{}, nil, fmt.Errorf("%w: %q is a directory", ErrRequest, name)
	}
	r, err := s.content.Read(ctx, e.Object.ID)
	if err != nil {
		return model.Entry{}, nil, err
	}
	release = false
	return e.Entry, &fileReader{ReadSeekCloser: r, release: s.gate.RUnlock}, nil
}

type fileReader struct {
	io.ReadSeekCloser
	once    sync.Once
	err     error
	release func()
}

// ReadFiles verifies requested identities before streaming a bounded caller's
// batch. One read gate keeps all of its contents available through completion.
func (s *Store) ReadFiles(ctx context.Context, version string, files []model.Entry, write func(model.Entry, io.Reader) error) error {
	if err := model.ValidateChanges(files); err != nil {
		return fmt.Errorf("%w: %v", ErrRequest, err)
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	v, err := s.version(ctx, version)
	if err != nil {
		return err
	}
	if !v.Retained {
		return ErrExpired
	}
	objects := make([]string, len(files))
	for i, e := range files {
		actual, err := s.lookup(ctx, v.root, e.Path)
		if err != nil {
			return err
		}
		if e.Kind != model.File || actual.Entry != e {
			return fmt.Errorf("%w: requested file identity differs", ErrRequest)
		}
		objects[i] = actual.Object.ID
	}
	for i, ref := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := s.content.Read(ctx, ref)
		if err != nil {
			return err
		}
		if err := errors.Join(write(files[i], r), r.Close()); err != nil {
			return err
		}
	}
	return nil
}

func (r *fileReader) Close() error {
	r.once.Do(func() { r.err = r.ReadSeekCloser.Close(); r.release() })
	return r.err
}

// Inventory visits selected metadata only. Narrow selections use exact child
// lookups; they do not decode every sibling merely to reach the requested path.
func (s *Store) Inventory(ctx context.Context, version string, selection model.Selection, visit func(model.Entry) error) error {
	if err := selection.Validate(); err != nil {
		return err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if version == "" {
		return nil
	}
	v, err := s.version(ctx, version)
	if err != nil {
		return err
	}
	return s.walk(ctx, v.root, "", selection, visit)
}

func (s *Store) walk(ctx context.Context, root, prefix string, selection model.Selection, visit func(model.Entry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var entries []content.Entry
	if selection.Includes(prefix) {
		var err error
		entries, err = s.directory(ctx, root)
		if err != nil {
			return err
		}
	} else {
		names := map[string]bool{}
		for _, rule := range selection {
			if !rule.Include {
				continue
			}
			name := rule.Path
			if prefix != "" {
				var ok bool
				name, ok = strings.CutPrefix(name, prefix+"/")
				if !ok {
					continue
				}
			}
			first, _, _ := strings.Cut(name, "/")
			if first != "" {
				names[first] = true
			}
		}
		for name := range names {
			e, err := s.child(ctx, root, name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			entries = append(entries, e)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	}
	for _, e := range entries {
		full := e.Path
		if prefix != "" {
			full = prefix + "/" + full
		}
		if selection.Includes(full) {
			entry := e.Entry
			entry.Path = full
			if err := visit(entry); err != nil {
				return err
			}
		}
		if e.Kind == model.Directory && selection.Descends(full) {
			if err := s.walk(ctx, e.Object.ID, full, selection, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// History follows the first-parent line from an immutable version ID.
func (s *Store) History(ctx context.Context, start string, limit int) (model.History, error) {
	page := model.History{Versions: []model.Version{}}
	if limit < 1 || limit > 100 {
		return page, errors.New("history limit must be 1–100")
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	for start != "" {
		v, err := s.version(ctx, start)
		if errors.Is(err, sql.ErrNoRows) && len(page.Versions) > 0 {
			page.Boundary = start
			break
		}
		if err != nil {
			return page, err
		}
		if len(page.Versions) == limit {
			page.Next = start
			break
		}
		page.Versions = append(page.Versions, v.Version)
		start = v.Parent
	}
	return page, nil
}
