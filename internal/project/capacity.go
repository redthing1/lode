package project

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/kopia/kopia/repo/blob"
	"github.com/redthing1/lode/internal/model"
)

const DefaultCapacityReserveBytes uint64 = 1 << 30

var (
	ErrCapacityUnknown          = errors.New("storage capacity is unknown")
	ErrInsufficientCapacity     = errors.New("insufficient storage capacity")
	ErrCapacityEstimateOverflow = errors.New("storage capacity estimate overflow")
)

// CapacityPolicy reserves filesystem space for metadata, native indexes, and
// ordinary project operations. The default is an initial working margin, not a
// measured peak-space bound or a project storage quota.
type CapacityPolicy struct {
	ReserveBytes uint64
}

func DefaultCapacityPolicy() CapacityPolicy {
	return CapacityPolicy{ReserveBytes: DefaultCapacityReserveBytes}
}

func (p CapacityPolicy) Validate() error {
	if p.ReserveBytes == 0 {
		return fmt.Errorf("%w: capacity reserve must be greater than zero", ErrInvalidRequest)
	}
	if p.ReserveBytes > math.MaxInt64 {
		return fmt.Errorf("%w: capacity reserve exceeds SQLite integer range", ErrInvalidRequest)
	}
	return nil
}

// CapacityPolicy returns the persisted local-admission reserve.
func (s *Service) CapacityPolicy(ctx context.Context) (CapacityPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return CapacityPolicy{}, ErrServiceClosed
	}
	policy, err := s.metadata.capacityPolicy(ctx)
	if err != nil {
		return CapacityPolicy{}, err
	}
	if err := policy.Validate(); err != nil {
		return CapacityPolicy{}, fmt.Errorf("invalid stored capacity policy: %w", err)
	}
	return policy, nil
}

// SetCapacityPolicy changes the persisted local-admission reserve. It does not
// impose a hard quota or promise that unrelated filesystem use cannot race an
// admission check.
func (s *Service) SetCapacityPolicy(ctx context.Context, policy CapacityPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrServiceClosed
	}
	return s.metadata.setCapacityPolicy(ctx, policy)
}

// CapacityError reports a failed admission check. AvailableBytes is the
// filesystem-wide value reported by the storage provider; unrelated processes
// can consume it after the check.
type CapacityError struct {
	AvailableBytes uint64
	RequiredBytes  uint64
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("%v: available %d bytes, require %d bytes", ErrInsufficientCapacity, e.AvailableBytes, e.RequiredBytes)
}

func (e *CapacityError) Unwrap() error { return ErrInsufficientCapacity }

// captureBudget estimates additional admission space for declared file inputs.
// The factor of two leaves room for native framing/index overhead and modest
// expansion; it is intentionally conservative but is not a hard upper bound.
// Metadata and pack headroom come from CapacityPolicy.ReserveBytes.
func captureBudget(changes []Change) (uint64, error) {
	var sourceBytes uint64
	for _, change := range changes {
		if change.Kind != model.File {
			continue
		}
		if change.Size < 0 {
			return 0, fmt.Errorf("%w: negative size for %q", ErrInvalidRequest, change.Path)
		}
		size := uint64(change.Size)
		if sourceBytes > math.MaxUint64-size {
			return 0, ErrCapacityEstimateOverflow
		}
		sourceBytes += size
	}
	if sourceBytes > math.MaxUint64/2 {
		return 0, ErrCapacityEstimateOverflow
	}
	return sourceBytes * 2, nil
}

// admitCapacity rejects an operation before its first payload reader is
// opened when the provider cannot establish sufficient headroom. Rechecking
// with a decreasing additional estimate before each file bounds concurrent
// use by this service; unrelated filesystem users can still race the probe.
func admitCapacity(ctx context.Context, probe func(context.Context) (blob.Capacity, error), policy CapacityPolicy, additionalBytes uint64) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if probe == nil {
		return fmt.Errorf("%w: no volume capacity provider", ErrCapacityUnknown)
	}
	capacity, err := probe(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCapacityUnknown, err)
	}
	if additionalBytes > math.MaxUint64-policy.ReserveBytes {
		return ErrCapacityEstimateOverflow
	}
	required := policy.ReserveBytes + additionalBytes
	if capacity.FreeB < required {
		return &CapacityError{AvailableBytes: capacity.FreeB, RequiredBytes: required}
	}
	return nil
}
