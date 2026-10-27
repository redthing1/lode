package project

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/engine/kopia"
)

const maxFileFactBatch = 1000

// fileFacts returns every stored fact it can find for the distinct references
// in one bounded metadata query. Missing references remain absent from the map
// so callers can report the affected logical path.
func (m *Metadata) fileFacts(ctx context.Context, namespace string, refs []string) (map[string]kopia.FileFact, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("read file facts: database is not open")
	}
	if namespace == "" {
		return nil, errors.New("read file facts: namespace is empty")
	}
	if len(refs) > maxFileFactBatch {
		return nil, fmt.Errorf("read file facts: at most %d references per batch", maxFileFactBatch)
	}
	result := make(map[string]kopia.FileFact, len(refs))
	if len(refs) == 0 {
		return result, nil
	}
	unique := make(map[string]struct{}, len(refs))
	ordered := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref == "" {
			return nil, errors.New("read file facts: empty object reference")
		}
		if _, found := unique[ref]; found {
			continue
		}
		unique[ref] = struct{}{}
		ordered = append(ordered, ref)
	}
	sort.Strings(ordered)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ordered)), ",")
	args := make([]any, 0, len(ordered)+1)
	args = append(args, namespace)
	for _, ref := range ordered {
		args = append(args, ref)
	}
	rows, err := m.db.QueryContext(ctx, `SELECT ref, sha256, size FROM file_facts WHERE namespace = ? AND ref IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("query file facts: %w", err)
	}
	for rows.Next() {
		var fact kopia.FileFact
		if err := rows.Scan(&fact.Ref, &fact.SHA256, &fact.Size); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan file fact: %w", err)
		}
		if fact.Size < 0 {
			_ = rows.Close()
			return nil, fmt.Errorf("stored file fact %q has negative size", fact.Ref)
		}
		result[fact.Ref] = fact
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate file facts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close file facts query: %w", err)
	}
	return result, nil
}
