//go:build linux

package project

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/redthing1/lode/internal/model"
)

const browsePageLimit = 100

// Version returns logical metadata for a version, including after its contents
// have expired.
func (p *Project) Version(ctx context.Context, id string) (Commit, error) {
	if !ValidID(id) {
		return Commit{}, ErrInvalid
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.commit(ctx, id)
}

// HistoryPage returns at most 100 versions on the first-parent chain. branch
// selects the starting tip when beforeID is empty. Otherwise beforeID is an
// immutable continuation cursor: its first parent begins the next page, even
// if the cursor is not reachable from branch. nextCursor is the last returned
// version when another page exists; pass it as beforeID to continue.
func (p *Project) HistoryPage(ctx context.Context, branch, beforeID string, limit int) ([]Commit, string, error) {
	if limit < 1 || limit > browsePageLimit {
		return nil, "", ErrInvalid
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	var id string
	if beforeID == "" {
		var err error
		id, err = p.head(ctx, branch)
		if err != nil {
			return nil, "", err
		}
	} else {
		if !ValidID(beforeID) {
			return nil, "", ErrInvalid
		}
		cursor, err := p.commit(ctx, beforeID)
		if err != nil {
			return nil, "", err
		}
		if len(cursor.Parents) > 0 {
			id = cursor.Parents[0]
		}
	}

	result := make([]Commit, 0, limit+1)
	for id != "" && len(result) < limit+1 {
		commit, err := p.commit(ctx, id)
		if err != nil {
			return nil, "", err
		}
		result = append(result, commit)
		id = ""
		if len(commit.Parents) > 0 {
			id = commit.Parents[0]
		}
	}
	if len(result) <= limit {
		return result, "", nil
	}
	return result[:limit], result[limit-1].ID, nil
}

// Directory returns a keyset page of direct children at commitID. directory
// is empty for the project root. after is the last direct child previously
// returned; it need not name an existing child. The listing uses logical entry
// metadata and remains available when payload contents have expired.
func (p *Project) Directory(ctx context.Context, commitID, directory, after string, limit int) ([]model.Entry, string, error) {
	if !ValidID(commitID) || directory != "" && !model.ValidPath(directory) || limit < 1 || limit > browsePageLimit {
		return nil, "", ErrInvalid
	}
	if after != "" {
		if !model.ValidPath(after) || parentDirectory(after) != directory {
			return nil, "", ErrInvalid
		}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	layers, err := p.browseLayers(ctx, commitID)
	if err != nil {
		return nil, "", err
	}
	if directory != "" {
		kind, found, err := effectiveKind(ctx, p.db, layers, directory)
		if err != nil {
			return nil, "", err
		}
		if !found {
			return nil, "", ErrNotFound
		}
		if kind != model.Directory {
			return nil, "", ErrInvalid
		}
	}

	streams := make([]childStream, len(layers))
	for i, id := range layers {
		streams[i] = childStream{db: p.db, commitID: id, directory: directory, after: after}
		if err := streams[i].advance(ctx); err != nil {
			return nil, "", err
		}
	}

	result := make([]model.Entry, 0, limit+1)
	for len(result) < limit+1 {
		chosen := -1
		for i := range streams {
			if streams[i].entry == nil {
				continue
			}
			if chosen == -1 || streams[i].entry.Path < streams[chosen].entry.Path {
				chosen = i
			}
		}
		if chosen == -1 {
			break
		}

		name := streams[chosen].entry.Path
		var current *model.Entry
		for i := range streams {
			if streams[i].entry == nil || streams[i].entry.Path != name {
				continue
			}
			if current == nil {
				entry := *streams[i].entry
				current = &entry
			}
			if err := streams[i].advance(ctx); err != nil {
				return nil, "", err
			}
		}
		if current != nil && current.Kind != model.Absent {
			result = append(result, *current)
		}
	}
	if len(result) <= limit {
		return result, "", nil
	}
	return result[:limit], result[limit-1].Path, nil
}

// browseLayers follows only the first-parent chain and stops at the nearest
// checkpoint, bounding path reconstruction to one checkpoint plus 63 deltas.
func (p *Project) browseLayers(ctx context.Context, id string) ([]string, error) {
	layers := make([]string, 0, 64)
	for len(layers) < 64 {
		var parent sql.NullString
		var checkpoint bool
		err := p.db.QueryRowContext(ctx, "SELECT parent,checkpoint FROM commits WHERE id=?", id).Scan(&parent, &checkpoint)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		layers = append(layers, id)
		if checkpoint {
			return layers, nil
		}
		if !parent.Valid {
			return nil, errors.New("history ended without a checkpoint")
		}
		id = parent.String
	}
	return nil, errors.New("history checkpoint is missing")
}

func effectiveKind(ctx context.Context, db *sql.DB, layers []string, name string) (model.Kind, bool, error) {
	for _, id := range layers {
		var kind string
		err := db.QueryRowContext(ctx, "SELECT kind FROM entries WHERE commit_id=? AND path=?", id, name).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		return model.Kind(kind), kind != string(model.Absent), nil
	}
	return "", false, nil
}

type childStream struct {
	db        *sql.DB
	commitID  string
	directory string
	after     string
	inclusive bool
	entry     *model.Entry
}

// advance seeks through an index range. When a delta contains only a deep
// descendant and omits its unchanged parent, it jumps to the first possible
// sibling instead of scanning that descendant subtree.
func (s *childStream) advance(ctx context.Context) error {
	prefix := ""
	upper := ""
	if s.directory != "" {
		prefix = s.directory + "/"
		upper = s.directory + "0"
	}
	for {
		query := "SELECT path,kind,size,sha256 FROM entries WHERE commit_id=?"
		args := []any{s.commitID}
		if prefix != "" {
			query += " AND path>=? AND path<?"
			args = append(args, prefix, upper)
		}
		if s.after != "" {
			if s.inclusive {
				query += " AND path>=?"
			} else {
				query += " AND path>?"
			}
			args = append(args, s.after)
		}
		query += " ORDER BY path LIMIT 1"
		var entry model.Entry
		var kind string
		err := s.db.QueryRowContext(ctx, query, args...).Scan(&entry.Path, &kind, &entry.Size, &entry.SHA256)
		if errors.Is(err, sql.ErrNoRows) {
			s.entry = nil
			return nil
		}
		if err != nil {
			return err
		}
		entry.Kind = model.Kind(kind)

		rest := entry.Path
		if prefix != "" {
			rest = strings.TrimPrefix(rest, prefix)
		}
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			// '/' sorts immediately before '0'. This inclusive lower bound skips
			// the whole child's descendants while preserving a sibling named
			// child0.
			s.after = prefix + rest[:slash] + "0"
			s.inclusive = true
			continue
		}
		s.after = entry.Path
		s.inclusive = false
		s.entry = &entry
		return nil
	}
}

func parentDirectory(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return ""
}
