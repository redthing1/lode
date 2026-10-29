package project

import (
	"context"

	"github.com/redthing1/lode/internal/model"
)

// Collect waits for active reads and writes, verifies every promised protection,
// and finishes pending releases before allowing native storage collection.
func (s *Store) Collect(ctx context.Context) (model.Collection, error) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	s.gate.Lock()
	defer s.gate.Unlock()
	if err := ctx.Err(); err != nil {
		return model.Collection{}, err
	}
	if err := s.reconcile(ctx); err != nil {
		return model.Collection{}, err
	}
	return s.content.Collect(ctx)
}
