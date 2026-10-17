package kopia

import (
	"context"
	"errors"
	"sync"

	"github.com/kopia/kopia/repo"
)

var (
	ErrMaintenance  = errors.New("engine maintenance is in progress")
	ErrSessionLimit = errors.New("engine activity limit reached")
)

// Activity admits bounded engine work and excludes it from collection. Every
// engine user, including local publication, must participate in the same instance.
type Activity struct {
	mu         sync.Mutex
	limit      int
	collecting bool
	active     map[*activitySession]struct{}
	idle       chan struct{}
}

type activitySession struct{ cancel context.CancelFunc }

func NewActivity(limit int) (*Activity, error) {
	if limit <= 0 {
		return nil, errors.New("engine activity limit must be positive")
	}
	idle := make(chan struct{})
	close(idle)
	return &Activity{limit: limit, active: make(map[*activitySession]struct{}), idle: idle}, nil
}

// Enter returns a cancellable activity context and an idempotent release. Release
// must follow all engine work and writer cleanup, including on failure.
func (a *Activity) Enter(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if a.collecting {
		return nil, nil, ErrMaintenance
	}
	if len(a.active) >= a.limit {
		return nil, nil, ErrSessionLimit
	}
	ctx, cancel := context.WithCancel(ctx)
	session := &activitySession{cancel: cancel}
	if len(a.active) == 0 {
		a.idle = make(chan struct{})
	}
	a.active[session] = struct{}{}
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			a.mu.Lock()
			defer a.mu.Unlock()
			delete(a.active, session)
			if len(a.active) == 0 {
				close(a.idle)
			}
		})
	}, nil
}

// Collect stops admissions, cancels existing activity, and waits for releases
// before native maintenance. A deadline while draining prevents maintenance.
func (a *Activity) Collect(ctx context.Context, r repo.DirectRepository) error {
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}
	if a.collecting {
		a.mu.Unlock()
		return ErrMaintenance
	}
	a.collecting = true
	for session := range a.active {
		session.cancel()
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.collecting = false
		a.mu.Unlock()
	}()
	if err := a.Wait(ctx); err != nil {
		return err
	}
	return Collect(ctx, r)
}

// Wait waits for admitted work to release. It does not stop new admissions;
// shutdown must stop transports and local callers before using it.
func (a *Activity) Wait(ctx context.Context) error {
	a.mu.Lock()
	idle := a.idle
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-idle:
		return ctx.Err()
	}
}
