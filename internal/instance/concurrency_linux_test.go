package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/project"
	"golang.org/x/sys/unix"
)

func TestColdProjectOpenDoesNotBlockPublishedProject(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	slow := projectWithFile(t, s, user, "slow", "cold contents")
	ready := projectWithFile(t, s, user, "ready", "published contents")
	// Evict only the idle repository, leaving the other project's reader ready.
	if err := s.projects[slow.ID].close(ctx); err != nil {
		t.Fatal(err)
	}
	delete(s.projects, slow.ID)
	format := filepath.Join(s.directory, "projects", slow.ID, "repository", "kopia.repository.f")
	data, err := os.ReadFile(format)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(format, format+".original"); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(format, 0600); err != nil {
		t.Fatal(err)
	}
	read := func(p Project, want string) error {
		return s.WithProject(ctx, Principal{User: user}, p.Owner, p.Name, Reader, func(store *project.Store) error {
			id, err := store.Head(ctx, "main")
			if err != nil {
				return err
			}
			_, r, err := store.File(ctx, id, "capture.bin")
			if err != nil {
				return err
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err == nil && string(got) != want {
				return fmt.Errorf("%s contents differ", p.Name)
			}
			return err
		})
	}
	slowDone := make(chan error, 1)
	go func() { slowDone <- read(slow, "cold contents") }()
	writer := -1
	var readyDone chan error
	// Restore the regular format file before allowing any subsequent native open.
	// Also release and join the reader when a test assertion fails.
	t.Cleanup(func() {
		if writer < 0 {
			writer, err = unix.Open(format, unix.O_RDWR|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Error(err)
				return
			}
		}
		if err := os.Rename(format+".original", format); err != nil {
			t.Error(err)
		}
		if n, err := unix.Write(writer, data); err != nil || n != len(data) {
			t.Errorf("restore format bytes: %d/%d: %v", n, len(data), err)
		}
		if err := unix.Close(writer); err != nil {
			t.Error(err)
		}
		select {
		case err := <-slowDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("cold repository did not resume after restoring its format")
		}
		if readyDone != nil {
			select {
			case err := <-readyDone:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("published reader did not resume")
			}
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		writer, err = unix.Open(format, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			break // A real native reader has opened the FIFO and must await bytes.
		}
		if !errors.Is(err, unix.ENXIO) || time.Now().After(deadline) {
			t.Fatalf("waiting for native format reader: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	readyDone = make(chan error, 1)
	go func() { readyDone <- read(ready, "published contents") }()
	select {
	case err := <-readyDone:
		readyDone = nil
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Error("published project blocked behind another repository's cold open")
	}
}

func TestConcurrentColdRequestsKeepInstanceOwned(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	p := projectWithFile(t, s, user, "shared", "retained contents")
	if err := s.projects[p.ID].close(ctx); err != nil {
		t.Fatal(err)
	}
	delete(s.projects, p.ID)
	const readers = 8
	start, finish := make(chan struct{}), make(chan struct{})
	acquired, done := make(chan error, readers), make(chan struct{}, readers)
	for range readers {
		go func() {
			<-start
			_, release, err := s.acquire(ctx, p.ID)
			acquired <- err
			if err == nil {
				<-finish
				release()
			}
			done <- struct{}{}
		}()
	}
	t.Cleanup(func() {
		close(finish)
		for range readers {
			<-done
		}
	})
	close(start)
	for range readers {
		select {
		case err := <-acquired:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent cold acquisition did not finish")
		}
	}
	if err := s.Close(ctx); err == nil {
		t.Fatal("instance closed while readers held its repository")
	}
	if other, err := Open(ctx, s.directory); err == nil {
		other.Close(ctx)
		t.Fatal("active instance released its ownership lock")
	}
	if err := s.DeleteProject(ctx, Principal{User: user}, p.Owner, p.Name, p.ID); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("active repository deletion: %v", err)
	}
}
