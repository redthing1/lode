package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

const schema = `
CREATE TABLE commits(id TEXT PRIMARY KEY, parent TEXT REFERENCES commits(id), second_parent TEXT REFERENCES commits(id),
 author TEXT NOT NULL, message TEXT NOT NULL, created TEXT NOT NULL, root TEXT NOT NULL, depth INTEGER NOT NULL, checkpoint INTEGER NOT NULL);
CREATE TABLE entries(commit_id TEXT NOT NULL REFERENCES commits(id), path TEXT NOT NULL, kind TEXT NOT NULL,
 size INTEGER NOT NULL, sha256 TEXT NOT NULL, object TEXT NOT NULL, PRIMARY KEY(commit_id,path)) WITHOUT ROWID;
CREATE TABLE branches(name TEXT PRIMARY KEY, tip TEXT REFERENCES commits(id));
CREATE TABLE operations(id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, outcome TEXT NOT NULL);
INSERT INTO branches VALUES('main',NULL);
PRAGMA user_version=1;
`

func (p *Project) commit(ctx context.Context, id string) (Commit, error) {
	c := Commit{ID: id}
	var parent, second sql.NullString
	var created string
	err := p.db.QueryRowContext(ctx, "SELECT parent,second_parent,author,message,created FROM commits WHERE id=?", id).Scan(&parent, &second, &c.Author, &c.Message, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Commit{}, ErrNotFound
	}
	if err != nil {
		return Commit{}, err
	}
	if parent.Valid {
		c.Parents = append(c.Parents, parent.String)
	}
	if second.Valid {
		c.Parents = append(c.Parents, second.String)
	}
	c.Created, err = time.Parse(time.RFC3339Nano, created)
	return c, err
}

// inventory reads at most 63 deltas and one checkpoint. Each selected prefix
// becomes an indexed path range; excluded paths are filtered by the pure rules.
func (p *Project) inventory(ctx context.Context, id string, selection model.Selection) ([]content.Entry, error) {
	if id == "" {
		return nil, nil
	}
	if selection == nil {
		selection = model.Selection{{Include: true}}
	}
	var clauses []string
	var scope []any
	for _, r := range selection {
		if r.Include {
			if r.Path == "" {
				clauses = []string{"1"}
				scope = nil
				break
			}
			clauses = append(clauses, "(path=? OR (path>=? AND path<?))")
			scope = append(scope, r.Path, r.Path+"/", r.Path+"0")
		}
	}
	if len(clauses) == 0 {
		if _, err := p.commit(ctx, id); err != nil {
			return nil, err
		}
		return []content.Entry{}, nil
	}
	query := "SELECT path,kind,size,sha256,object FROM entries WHERE commit_id=? AND (" + strings.Join(clauses, " OR ") + ")"
	seen := map[string]bool{}
	result := []content.Entry{}
	for count := 0; id != ""; count++ {
		if count >= 64 {
			return nil, errors.New("history checkpoint is missing")
		}
		var parent sql.NullString
		var checkpoint bool
		if err := p.db.QueryRowContext(ctx, "SELECT parent,checkpoint FROM commits WHERE id=?", id).Scan(&parent, &checkpoint); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		args := append([]any{id}, scope...)
		rows, err := p.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e content.Entry
			if err := rows.Scan(&e.Path, &e.Kind, &e.Size, &e.SHA256, &e.Object); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[e.Path] {
				seen[e.Path] = true
				if e.Kind != model.Absent && selection.Includes(e.Path) {
					result = append(result, e)
				}
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return nil, err
		}
		if checkpoint {
			sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
			return result, nil
		}
		id = parent.String
	}
	return nil, errors.New("history ended without a checkpoint")
}

func (p *Project) accept(ctx context.Context, r Request, root string, before, after []content.Entry) (Commit, error) {
	first, second := r.Base, r.SecondParent
	// The conceptual empty root is not a commit. A merge into an empty branch
	// records the source as its sole parent and deltas against that inventory.
	if first == "" && second != "" {
		first, second = second, ""
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return Commit{}, err
	}
	defer tx.Rollback()
	depth := 0
	var parent, secondParent, expected any
	if r.Base != "" {
		expected = r.Base
	}
	if second != "" {
		secondParent = second
	}
	if first != "" {
		if err := tx.QueryRowContext(ctx, "SELECT depth FROM commits WHERE id=?", first).Scan(&depth); err != nil {
			return Commit{}, err
		}
		depth++
		parent = first
	}
	checkpoint := depth%64 == 0
	c := Commit{ID: r.ID, Author: r.Author, Message: r.Message, Created: time.Now().UTC()}
	if first != "" {
		c.Parents = []string{first}
	}
	if second != "" {
		c.Parents = append(c.Parents, second)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO commits VALUES(?,?,?,?,?,?,?,?,?)", r.ID, parent, secondParent, r.Author, r.Message, c.Created.Format(time.RFC3339Nano), root, depth, checkpoint)
	if err != nil {
		return Commit{}, err
	}
	changes := map[string]content.Entry{}
	if !checkpoint {
		for _, e := range before {
			changes[e.Path] = content.Entry{Entry: model.Entry{Path: e.Path, Kind: model.Absent}}
		}
	}
	previous := map[string]content.Entry{}
	for _, e := range before {
		previous[e.Path] = e
	}
	for _, e := range after {
		if checkpoint || previous[e.Path] != e {
			changes[e.Path] = e
		} else {
			delete(changes, e.Path)
		}
	}
	statement, err := tx.PrepareContext(ctx, "INSERT INTO entries VALUES(?,?,?,?,?,?)")
	if err != nil {
		return Commit{}, err
	}
	for _, e := range changes {
		if _, err = statement.ExecContext(ctx, r.ID, e.Path, e.Kind, e.Size, e.SHA256, e.Object); err != nil {
			statement.Close()
			return Commit{}, err
		}
	}
	if err := statement.Close(); err != nil {
		return Commit{}, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE branches SET tip=? WHERE name=? AND tip IS ?", r.ID, r.Branch, expected)
	if err != nil {
		return Commit{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Commit{}, err
	}
	if count != 1 {
		return Commit{}, ErrStale
	}
	if _, err = tx.ExecContext(ctx, "UPDATE operations SET outcome='accepted' WHERE id=? AND outcome='pending'", r.ID); err != nil {
		return Commit{}, err
	}
	// If Commit fails, protection is deliberately left intact. The next request
	// or startup resolves SQLite's authoritative outcome before any release.
	if err := tx.Commit(); err != nil {
		return Commit{}, fmt.Errorf("accept version: %w", err)
	}
	return c, nil
}
