package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/redthing1/lode/internal/content"
)

const groupCount = 64

func groupIndex(name string) int { h := sha256.Sum256([]byte(name)); return int(h[0]) % groupCount }

// Each immutable directory points to name-hash groups shared across versions.
// Native directory IDs are scoped to a project, as are group content hashes.
type directory struct {
	id     string
	slots  [groupCount]string
	groups map[string][]byte
}

func indexDirectory(id string, entries []content.Entry) (directory, error) {
	d := directory{id: id, groups: map[string][]byte{}}
	var groups [groupCount][]content.Entry
	for _, e := range entries {
		n := groupIndex(e.Path)
		groups[n] = append(groups[n], e)
	}
	for i, g := range groups {
		if len(g) == 0 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].Path < g[j].Path })
		body, err := json.Marshal(g)
		if err != nil {
			return d, err
		}
		h := sha256.Sum256(body)
		key := hex.EncodeToString(h[:])
		d.slots[i] = key
		d.groups[key] = body
	}
	return d, nil
}

func decodeGroup(key string, body []byte) ([]content.Entry, error) {
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != key {
		return nil, ErrMetadata
	}
	var entries []content.Entry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, errors.Join(ErrMetadata, err)
	}
	return entries, nil
}

func (s *Store) directory(ctx context.Context, id string) ([]content.Entry, error) {
	if id == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT slot.key,slot.value,g.body FROM directories d,json_each(d.body) slot LEFT JOIN groups g ON g.project=d.project AND g.id=slot.value WHERE d.project=? AND d.id=? AND json_type(d.body)='array'`, s.id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []content.Entry
	slots := 0
	for rows.Next() {
		var index int
		var key string
		var body []byte
		if err := rows.Scan(&index, &key, &body); err != nil {
			return nil, err
		}
		if index != slots {
			return nil, ErrMetadata
		}
		slots++
		if key == "" {
			continue
		}
		g, err := decodeGroup(key, body)
		if err != nil {
			return nil, err
		}
		for _, e := range g {
			if groupIndex(e.Path) != index {
				return nil, ErrMetadata
			}
		}
		entries = append(entries, g...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if slots != groupCount {
		return nil, ErrMetadata
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (s *Store) child(ctx context.Context, id, name string) (content.Entry, error) {
	var key string
	var body []byte
	slot := fmt.Sprintf("$[%d]", groupIndex(name))
	err := s.db.QueryRowContext(ctx, `SELECT json_extract(d.body,?),g.body FROM directories d LEFT JOIN groups g ON g.project=d.project AND g.id=json_extract(d.body,?) WHERE d.project=? AND d.id=?`, slot, slot, s.id, id).Scan(&key, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return content.Entry{}, ErrMetadata
	}
	if err != nil {
		return content.Entry{}, err
	}
	if key == "" {
		return content.Entry{}, os.ErrNotExist
	}
	entries, err := decodeGroup(key, body)
	if err != nil {
		return content.Entry{}, err
	}
	i := sort.Search(len(entries), func(i int) bool { return entries[i].Path >= name })
	if i == len(entries) || entries[i].Path != name {
		return content.Entry{}, os.ErrNotExist
	}
	return entries[i], nil
}

func (s *Store) writeDirectories(ctx context.Context, tx *sql.Tx, dirs []directory) error {
	groupStmt, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO groups VALUES(?,?,?)")
	if err != nil {
		return err
	}
	defer groupStmt.Close()
	dirStmt, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO directories VALUES(?,?,?)")
	if err != nil {
		return err
	}
	defer dirStmt.Close()
	for _, d := range dirs {
		for key, body := range d.groups {
			if _, err := groupStmt.ExecContext(ctx, s.id, key, body); err != nil {
				return err
			}
		}
		body, err := json.Marshal(d.slots)
		if err != nil {
			return err
		}
		if _, err := dirStmt.ExecContext(ctx, s.id, d.id, body); err != nil {
			return err
		}
	}
	return nil
}
