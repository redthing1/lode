package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redthing1/lode/internal/progress"
	"golang.org/x/term"
)

type progressDisplay struct {
	mu             sync.Mutex
	out            io.Writer
	event          progress.Event
	started        time.Time
	visible, quiet bool
	frame          int
	width          func() int
}

func startProgress(ctx context.Context, out io.Writer, command string, disabled bool) (context.Context, io.Writer, func()) {
	phases := map[string]string{
		"attach": "Connecting", "clone": "Cloning", "status": "Checking files", "save": "Saving",
		"update": "Updating", "select": "Selecting files", "switch": "Switching branch", "restore": "Restoring",
		"merge": "Merging", "resume": "Resuming", "cancel": "Cancelling", "collect": "Collecting",
		"cleanup": "Checking retention", "backup": "Backing up", "projects": "Loading projects", "log": "Loading history",
		"remote": "Checking remote",
	}
	phase := phases[command]
	if disabled || phase == "" || os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(os.Stderr.Fd())) {
		return ctx, out, func() {}
	}
	d := &progressDisplay{out: os.Stderr, event: progress.Event{Phase: phase}, started: time.Now(), width: func() int {
		width, _, err := term.GetSize(int(os.Stderr.Fd()))
		if err != nil {
			return 80
		}
		return width
	}}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				d.draw()
			case <-stop:
				return
			}
		}
	}()
	return progress.WithReporter(ctx, d.update), progressOutput{out, d}, func() {
		close(stop)
		<-done
		d.mu.Lock()
		defer d.mu.Unlock()
		d.clear()
	}
}

func (d *progressDisplay) update(e progress.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.event, d.quiet = e, false
}

func (d *progressDisplay) clear() {
	if d.visible {
		fmt.Fprint(d.out, "\r\x1b[2K")
		d.visible = false
	}
}

func (d *progressDisplay) draw() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quiet {
		return
	}
	e := d.event
	line := e.Phase
	suffix := ""
	if e.Total > 0 {
		suffix = fmt.Sprintf(" · %s / %s", byteSize(e.Bytes), byteSize(e.Total))
	}
	suffix += " · " + time.Since(d.started).Truncate(time.Second).String()
	budget := max(0, d.width()-4)
	if room := budget - len(line) - len(suffix) - len(" · "); e.Path != "" && room >= 8 {
		line += " · " + clipProgress(e.Path, room)
	}
	line = clipProgress(line+suffix, budget)
	fmt.Fprintf(d.out, "\r\x1b[2K%c %s", "|/-\\"[d.frame%4], line)
	d.frame++
	d.visible = true
}

func clipProgress(text string, budget int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, text)
	if len(text) <= budget {
		return text
	}
	if budget < 3 {
		return ""
	}
	// A byte budget is conservative for wide characters and preserves UTF-8.
	end := budget - 3
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "..."
}

func byteSize(n int64) string {
	if n < 1<<10 {
		return fmt.Sprintf("%d B", n)
	}
	value, unit := float64(n), ""
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1024
		unit = suffix
		if value < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}

// Stop redrawing before normal output, including conflicts and help.
type progressOutput struct {
	io.Writer
	display *progressDisplay
}

func (w progressOutput) Write(b []byte) (int, error) {
	w.display.mu.Lock()
	defer w.display.mu.Unlock()
	w.display.clear()
	w.display.quiet = true
	return w.Writer.Write(b)
}
