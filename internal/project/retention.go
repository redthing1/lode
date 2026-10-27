package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/redthing1/lode/internal/content"
	"github.com/redthing1/lode/internal/model"
)

type Tag struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

func (p *Project) Tags(ctx context.Context) ([]Tag, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tags(ctx)
}

func (p *Project) tags(ctx context.Context) ([]Tag, error) {
	rows, err := p.db.QueryContext(ctx, "SELECT name,commit_id FROM tags ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Tag{}
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&tag.Name, &tag.Commit); err != nil {
			return nil, err
		}
		result = append(result, tag)
	}
	return result, rows.Err()
}

func (p *Project) CreateTag(ctx context.Context, name, id string) error {
	if !model.ValidName(name) || !ValidID(id) {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.requireRetained(ctx, id); err != nil {
		return err
	}
	_, err := p.db.ExecContext(ctx, "INSERT INTO tags(name,commit_id) VALUES(?,?) ON CONFLICT(name) DO NOTHING", name, id)
	if err != nil {
		return err
	}
	var current string
	if err := p.db.QueryRowContext(ctx, "SELECT commit_id FROM tags WHERE name=?", name).Scan(&current); err != nil {
		return err
	}
	if current != id {
		return ErrExists
	}
	return nil
}

func (p *Project) DeleteTag(ctx context.Context, name, expected string) error {
	if !model.ValidName(name) || !ValidID(expected) {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result, err := p.db.ExecContext(ctx, "DELETE FROM tags WHERE name=? AND commit_id=?", name, expected)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 0 {
		return err
	}
	var current string
	err = p.db.QueryRowContext(ctx, "SELECT commit_id FROM tags WHERE name=?", name).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrReferenceChanged
}

func (p *Project) requireRetained(ctx context.Context, id string) error {
	if id == "" {
		return nil // The empty tree has no payload to retain.
	}
	var retained bool
	if err := p.db.QueryRowContext(ctx, "SELECT root<>'' FROM commits WHERE id=?", id).Scan(&retained); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if !retained {
		return fmt.Errorf("%s: %w", id, ErrExpired)
	}
	return nil
}

type RetentionVersion struct {
	ID       string    `json:"id"`
	Created  time.Time `json:"created"`
	Retained bool      `json:"retained"`
	Branches int       `json:"branches"`
	Tags     int       `json:"tags"`
	Recent   int       `json:"recent"`
}

func (v RetentionVersion) Protected() bool {
	return v.Branches > 0 || v.Tags > 0 || v.Recent > 0
}

type CleanupPlan struct {
	Keep     int                `json:"keep"`
	Token    string             `json:"token"`
	Versions []RetentionVersion `json:"versions"`
}

func (p *Project) PreviewCleanup(ctx context.Context, keep int) (CleanupPlan, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cleanupPlan(ctx, keep)
}

func (p *Project) cleanupPlan(ctx context.Context, keep int) (CleanupPlan, error) {
	plan := CleanupPlan{Keep: keep, Versions: []RetentionVersion{}}
	if keep < 0 {
		return plan, ErrInvalid
	}
	rows, err := p.db.QueryContext(ctx, "SELECT id,parent,created,root<>'' FROM commits ORDER BY id")
	if err != nil {
		return plan, err
	}
	byID := map[string]int{}
	var parents []string
	for rows.Next() {
		var v RetentionVersion
		var parent sql.NullString
		var created string
		if err := rows.Scan(&v.ID, &parent, &created, &v.Retained); err != nil {
			rows.Close()
			return plan, err
		}
		v.Created, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			rows.Close()
			return plan, err
		}
		byID[v.ID] = len(plan.Versions)
		parents = append(parents, parent.String)
		plan.Versions = append(plan.Versions, v)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return plan, err
	}
	branches, err := p.branches(ctx)
	if err != nil {
		return plan, err
	}
	tags, err := p.tags(ctx)
	if err != nil {
		return plan, err
	}
	for _, branch := range branches {
		id := branch.Tip
		for n := 0; id != "" && (n == 0 || n < keep); n++ {
			if err := ctx.Err(); err != nil {
				return plan, err
			}
			i, ok := byID[id]
			if !ok {
				return plan, fmt.Errorf("branch history references missing version %s", id)
			}
			if n == 0 {
				plan.Versions[i].Branches++
			} else {
				plan.Versions[i].Recent++
			}
			id = parents[i]
		}
	}
	for _, tag := range tags {
		i, ok := byID[tag.Commit]
		if !ok {
			return plan, fmt.Errorf("tag references missing version %s", tag.Commit)
		}
		plan.Versions[i].Tags++
	}
	// Availability is intentionally excluded: applying this same decision again
	// can only find fewer retained candidates. New history or protection changes it.
	// Encode one fact at a time instead of allocating a second history-sized
	// JSON buffer. Input order is canonical; counts delimit the three lists.
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	_ = encoder.Encode([]int{keep, len(plan.Versions), len(branches), len(tags)})
	for _, v := range plan.Versions {
		_ = encoder.Encode(v.ID)
	}
	for _, branch := range branches {
		_ = encoder.Encode(branch)
	}
	for _, tag := range tags {
		_ = encoder.Encode(tag)
	}
	plan.Token = hex.EncodeToString(hash.Sum(nil))
	sort.Slice(plan.Versions, func(i, j int) bool {
		a, b := plan.Versions[i], plan.Versions[j]
		if a.Created.Equal(b.Created) {
			return a.ID < b.ID
		}
		return a.Created.After(b.Created)
	})
	return plan, nil
}

// ApplyCleanup durably expires only the previewed contents. An empty native
// root is the availability fact; logical history remains intact. Native release
// is reconciled by Collect or startup, so a crash can delay reclamation only.
func (p *Project) ApplyCleanup(ctx context.Context, keep int, token string) (int, error) {
	if decoded, err := hex.DecodeString(token); err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != token {
		return 0, ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	plan, err := p.cleanupPlan(ctx, keep)
	if err != nil {
		return 0, err
	}
	if token != plan.Token {
		return 0, ErrCleanupChanged
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	for _, version := range plan.Versions {
		if !version.Retained || version.Protected() {
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE commits SET root='' WHERE id=?", version.ID); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (p *Project) Collect(ctx context.Context) (content.Collection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.reconcileRoots(ctx); err != nil {
		return content.Collection{}, err
	}
	return p.contents.Collect(ctx)
}
