package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/redthing1/lode/internal/model"
)

type Branch struct {
	Name string `json:"name"`
	Tip  string `json:"tip"`
}

func (p *Project) Branches(ctx context.Context) ([]Branch, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rows, err := p.db.QueryContext(ctx, "SELECT name,coalesce(tip,'') FROM branches ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Branch{}
	for rows.Next() {
		var b Branch
		if err := rows.Scan(&b.Name, &b.Tip); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

func (p *Project) CreateBranch(ctx context.Context, name, tip string) error {
	if !model.ValidName(name) || tip != "" && !ValidID(tip) {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tip != "" {
		var exists int
		if err := p.db.QueryRowContext(ctx, "SELECT 1 FROM commits WHERE id=?", tip).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
	}
	_, err := p.db.ExecContext(ctx, "INSERT INTO branches(name,tip) VALUES(?,?)", name, nullableTip(tip))
	if err == nil {
		return nil
	}
	var existing string
	lookupErr := p.db.QueryRowContext(ctx, "SELECT coalesce(tip,'') FROM branches WHERE name=?", name).Scan(&existing)
	if lookupErr == nil {
		if existing == tip {
			return nil
		}
		return ErrExists
	}
	return errors.Join(err, lookupErr)
}

func nullableTip(tip string) any {
	if tip == "" {
		return nil
	}
	return tip
}

func (p *Project) DeleteBranch(ctx context.Context, name, expected string) error {
	if !model.ValidName(name) || expected != "" && !ValidID(expected) {
		return ErrInvalid
	}
	if name == "main" {
		return fmt.Errorf("main branch cannot be deleted: %w", ErrInvalid)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result, err := p.db.ExecContext(ctx, "DELETE FROM branches WHERE name=? AND tip IS ?", name, nullableTip(expected))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 0 {
		return err
	}
	var found string
	err = p.db.QueryRowContext(ctx, "SELECT coalesce(tip,'') FROM branches WHERE name=?", name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrStale
}
