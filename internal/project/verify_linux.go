package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/redthing1/lode/internal/model"
)

// Verify checks every surviving logical tree and reads retained file bytes.
// Immutable objects shared by versions are checked once. Backup calls this on
// its private staged copy, after Open has checked native snapshot protection.
func (s *Store) Verify(ctx context.Context) error {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	policy, _, err := s.retention(ctx)
	if err != nil {
		return err
	}
	if err := policy.Validate(); err != nil {
		return errors.Join(ErrMetadata, err)
	}
	var invalid int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM (
SELECT tip AS id,0 AS tag FROM branches WHERE project=?
UNION ALL SELECT version,1 FROM tags WHERE project=?
) refs LEFT JOIN versions v ON v.project=? AND v.id=refs.id
WHERE (refs.id<>'' OR refs.tag=1) AND (v.id IS NULL OR v.protection='')`, s.id, s.id, s.id).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("%w: reference to unavailable version", ErrMetadata)
	}
	v := verifier{store: s, directories: map[string]uint8{}, files: map[string]model.Entry{}}
	var last string
	for {
		var id, root string
		var retained bool
		err := s.db.QueryRowContext(ctx, "SELECT id,root,protection<>'' FROM versions WHERE project=? AND id>? ORDER BY id LIMIT 1", s.id, last).Scan(&id, &root, &retained)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := v.directory(ctx, root, retained, 0); err != nil {
			return fmt.Errorf("verify version %s: %w", id, err)
		}
		last = id
	}
}

type verifier struct {
	store       *Store
	directories map[string]uint8 // 1: visiting; 2: logical checked; 3: native checked
	files       map[string]model.Entry
}

func (v *verifier) directory(ctx context.Context, id string, retained bool, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state := v.directories[id]
	if id == "" || state == 1 || depth > 256 {
		return fmt.Errorf("%w: invalid directory tree", ErrMetadata)
	}
	if state == 3 || state == 2 && !retained {
		return nil
	}
	v.directories[id] = 1
	entries, err := v.store.directory(ctx, id)
	if err != nil {
		return err
	}
	if retained {
		native, err := v.store.content.Directory(ctx, id)
		if err != nil {
			return err
		}
		if len(native) != len(entries) {
			return fmt.Errorf("%w: native directory differs", ErrMetadata)
		}
		for i, e := range entries {
			e.SHA256 = "" // Native trees do not store Lode's logical hash.
			if e != native[i] {
				return fmt.Errorf("%w: native entry %q differs", ErrMetadata, e.Path)
			}
		}
	}
	for i, e := range entries {
		child := e.Entry
		if depth > 0 {
			child.Path = "child/" + child.Path
		}
		if err := child.Validate(); err != nil {
			return errors.Join(ErrMetadata, err)
		}
		if strings.Contains(e.Path, "/") || e.Object.ID == "" || i > 0 && entries[i-1].Path == e.Path {
			return fmt.Errorf("%w: invalid child %q", ErrMetadata, e.Path)
		}
		switch e.Kind {
		case model.Directory:
			if err := v.directory(ctx, e.Object.ID, retained, depth+1); err != nil {
				return err
			}
		case model.File:
			if retained {
				if err := v.file(ctx, e.Object.ID, e.Entry); err != nil {
					return fmt.Errorf("verify file %q: %w", e.Path, err)
				}
			}
		default:
			return ErrMetadata
		}
	}
	v.directories[id] = 2
	if retained {
		v.directories[id] = 3
	}
	return nil
}

func (v *verifier) file(ctx context.Context, id string, e model.Entry) error {
	e.Path = ""
	if previous, ok := v.files[id]; ok {
		if previous != e {
			return fmt.Errorf("%w: inconsistent file identity", ErrMetadata)
		}
		return nil
	}
	r, err := v.store.content.Read(ctx, id)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, verifyReader{ctx, r})
	if err := errors.Join(err, r.Close()); err != nil {
		return err
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%w: file contents differ", ErrMetadata)
	}
	v.files[id] = e
	return nil
}

type verifyReader struct {
	ctx context.Context
	in  io.Reader
}

func (r verifyReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}
