package project

import (
	"context"
	"errors"
	"io"
	"sync"
)

// trackedReader holds the native reader gate until Close or request
// cancellation. Closing never needs the service mutation lock, so collection
// can wait for readers while preventing new reads and publications.
type trackedReader struct {
	inner *activeReader
	stop  func() bool
}

type activeReader struct {
	ctx     context.Context
	reader  io.ReadCloser
	mu      sync.Mutex
	closed  bool
	err     error
	release func()
}

func (s *Service) trackReader(ctx context.Context, id string, reader io.ReadCloser) io.ReadCloser {
	active := &activeReader{ctx: ctx, reader: reader, release: func() {
		s.readMu.Lock()
		s.readers[id]--
		if s.readers[id] == 0 {
			delete(s.readers, id)
		}
		s.readMu.Unlock()
		s.contentMu.RUnlock()
	}}
	stop := context.AfterFunc(ctx, func() { _ = active.Close() })
	return &trackedReader{inner: active, stop: stop}
}

func (r *trackedReader) Read(p []byte) (int, error) { return r.inner.Read(p) }
func (r *trackedReader) Close() error               { r.stop(); return r.inner.Close() }
func (r *activeReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.closed {
		return 0, errors.New("file stream is closed")
	}
	return r.reader.Read(p)
}
func (r *activeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.err = r.reader.Close()
		r.release()
	}
	return r.err
}

// StorageStats reports physical blob lengths and native index measurements.
// Volume free bytes may be consumed by unrelated users of the same filesystem.
func (s *Service) StorageStats(ctx context.Context) (StorageStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return StorageStats{}, ErrServiceClosed
	}
	if s.maintenance == nil {
		return StorageStats{}, errors.New("native maintenance is not configured")
	}
	stats, err := s.maintenance.Inspect(ctx)
	if err != nil {
		return StorageStats{}, err
	}
	return storageStats(stats), nil
}
