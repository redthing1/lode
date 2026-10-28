//go:build linux

package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

const schema = `
CREATE TABLE commits(id TEXT PRIMARY KEY, parent TEXT REFERENCES commits(id), second_parent TEXT REFERENCES commits(id),
 author TEXT NOT NULL, message TEXT NOT NULL, created TEXT NOT NULL, root TEXT NOT NULL, depth INTEGER NOT NULL, checkpoint INTEGER NOT NULL, actor_id TEXT NOT NULL DEFAULT '');
CREATE TABLE entries(commit_id TEXT NOT NULL REFERENCES commits(id), path TEXT NOT NULL, kind TEXT NOT NULL,
 size INTEGER NOT NULL, sha256 TEXT NOT NULL, object TEXT NOT NULL, PRIMARY KEY(commit_id,path)) WITHOUT ROWID;
CREATE TABLE branches(name TEXT PRIMARY KEY, tip TEXT REFERENCES commits(id));
CREATE TABLE tags(name TEXT PRIMARY KEY, commit_id TEXT NOT NULL REFERENCES commits(id));
CREATE TABLE operations(id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, outcome TEXT NOT NULL, actor_id TEXT NOT NULL DEFAULT '');
INSERT INTO branches VALUES('main',NULL);
PRAGMA user_version=3;
`

const commitColumns = "id,parent,second_parent,author,message,created,root<>'',actor_id"

const inventoryHistory = `WITH RECURSIVE history(id,parent,checkpoint,n) AS (
 SELECT id,parent,checkpoint,0 FROM commits WHERE id=?1
 UNION ALL
 SELECT c.id,c.parent,c.checkpoint,h.n+1 FROM commits c JOIN history h ON c.id=h.parent
 WHERE NOT h.checkpoint AND h.n<63
) `

func (p *Project) prepareInventory(ctx context.Context) error {
	// Keep history outermost so both path predicates use the compound key.
	const entries = "SELECT e.path,e.kind,e.size,e.sha256,e.object,h.n FROM history h CROSS JOIN entries e ON e.commit_id=h.id"
	const prefix = inventoryHistory + "SELECT path,kind,size,sha256,object FROM ("
	var err error
	p.inventoryBounds, err = p.db.PrepareContext(ctx, inventoryHistory+"SELECT count(*),coalesce(max(checkpoint),0) FROM history")
	if err != nil {
		return err
	}
	p.inventoryAll, err = p.db.PrepareContext(ctx, prefix+entries+") ORDER BY n")
	if err != nil {
		return err
	}
	p.inventorySelected, err = p.db.PrepareContext(ctx, prefix+entries+" WHERE e.path=?2 UNION ALL "+entries+" WHERE e.path>=?3 AND e.path<?4) ORDER BY n")
	return err
}

func (p *Project) commit(ctx context.Context, id string) (Commit, error) {
	return scanCommit(p.db.QueryRowContext(ctx, "SELECT "+commitColumns+" FROM commits WHERE id=?", id))
}

func scanCommit(row interface{ Scan(...any) error }) (Commit, error) {
	var c Commit
	var parent, second sql.NullString
	var created string
	err := row.Scan(&c.ID, &parent, &second, &c.Author, &c.Message, &created, &c.Retained, &c.ActorID)
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
	var scopes [][]any
	for _, r := range selection {
		if r.Include {
			if r.Path == "" {
				scopes = [][]any{nil}
				break
			}
			scopes = append(scopes, []any{r.Path, r.Path + "/", r.Path + "0"})
		}
	}
	if len(scopes) == 0 {
		if _, err := p.commit(ctx, id); err != nil {
			return nil, err
		}
		return []content.Entry{}, nil
	}
	var count int
	var checkpoint bool
	if err := p.inventoryBounds.QueryRowContext(ctx, id).Scan(&count, &checkpoint); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrNotFound
	}
	if !checkpoint {
		return nil, errors.New("history checkpoint is missing")
	}
	seen := map[string]bool{}
	result := []content.Entry{}
	for _, scope := range scopes {
		query := p.inventoryAll
		if len(scope) != 0 {
			query = p.inventorySelected
		}
		rows, err := query.QueryContext(ctx, append([]any{id}, scope...)...)
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
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
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
	c := Commit{ID: r.ID, Author: r.Author, Message: r.Message, Created: time.Now().UTC(), Retained: true}
	if first != "" {
		c.Parents = []string{first}
	}
	if second != "" {
		c.Parents = append(c.Parents, second)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO commits(id,parent,second_parent,author,message,created,root,depth,checkpoint,actor_id) VALUES(?,?,?,?,?,?,?,?,?,?)", r.ID, parent, secondParent, r.Author, r.Message, c.Created.Format(time.RFC3339Nano), root, depth, checkpoint, r.actorID)
	if err != nil {
		return Commit{}, err
	}
	c.ActorID = r.actorID
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
