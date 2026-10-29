// Package progress reports transient operation activity without owning output.
package progress

import (
	"context"
	"io"
)

type Event struct {
	Phase, Path  string
	Bytes, Total int64
}

type key struct{}

// WithReporter attaches an observer. The observer must permit concurrent calls.
func WithReporter(ctx context.Context, report func(Event)) context.Context {
	return context.WithValue(ctx, key{}, report)
}

func Report(ctx context.Context, event Event) {
	if report, ok := ctx.Value(key{}).(func(Event)); ok {
		report(event)
	}
}

type reader struct {
	io.Reader
	report func(Event)
	event  Event
}

func (r *reader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.event.Bytes += int64(n)
	r.report(r.event)
	return n, err
}

func Reader(ctx context.Context, phase, path string, total int64, in io.Reader) io.Reader {
	report, ok := ctx.Value(key{}).(func(Event))
	if !ok {
		return in
	}
	event := Event{Phase: phase, Path: path, Total: total}
	report(event)
	return &reader{in, report, event}
}

type writer struct {
	io.Writer
	report func(Event)
	event  Event
}

func (w *writer) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	w.event.Bytes += int64(n)
	w.report(w.event)
	return n, err
}

func Writer(ctx context.Context, phase, path string, total int64, out io.Writer) io.Writer {
	report, ok := ctx.Value(key{}).(func(Event))
	if !ok {
		return out
	}
	event := Event{Phase: phase, Path: path, Total: total}
	report(event)
	return &writer{out, report, event}
}
