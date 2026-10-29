package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
)

func TestPublicationWaitersSeeSettledOutcome(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture-fails=%t", fail), func(t *testing.T) {
			ctx := context.Background()
			f := openFixture(t, t.TempDir(), true)
			first := publish(t, f.store, "", []model.Entry{file("a", "old")}, map[string]string{"a": "old"})
			request := model.Save{ID: operation(t), Branch: "main", Base: first.ID, Changes: []model.Entry{file("a", "new")}}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			resume := func() { once.Do(func() { close(release) }) }
			saveDone, saveResult := make(chan struct{}), make(chan error, 1)
			t.Cleanup(func() { resume(); <-saveDone })
			captureFailure := errors.New("capture interrupted")
			go func() {
				defer close(saveDone)
				_, err := f.store.Save(ctx, "alice", request, Source{Write: func(_ model.Entry, out io.Writer, _ *Basis) error {
					close(entered)
					<-release
					if fail {
						return captureFailure
					}
					_, err := io.WriteString(out, "new")
					return err
				}})
				saveResult <- err
			}()
			<-entered
			checks := []func() error{
				func() error {
					v, err := f.store.Resolve(ctx, "alice", request)
					if fail && errors.Is(err, ErrUnaccepted) || !fail && err == nil && v.ID == request.ID {
						return nil
					}
					return fmt.Errorf("resolved outcome: %s, %v", v.ID, err)
				},
				func() error {
					p, err := f.store.PreviewCleanup(ctx)
					expected := 2
					if fail {
						expected = 1
					}
					if err != nil || p.Retained != expected {
						return fmt.Errorf("cleanup preview after capture: %+v, %v", p, err)
					}
					return nil
				},
				func() error { return f.store.Verify(ctx) },
			}
			if !fail {
				checks = append(checks, func() error {
					v, err := f.store.Cancel(ctx, "alice", request)
					if err != nil || v.ID != request.ID {
						return fmt.Errorf("cancel lost accepted outcome: %s, %v", v.ID, err)
					}
					return nil
				})
			}
			started, results := make(chan struct{}, len(checks)), make(chan error, len(checks))
			for _, check := range checks {
				go func() { started <- struct{}{}; results <- check() }()
			}
			for range checks {
				<-started
			}
			select {
			case err := <-results:
				t.Fatalf("operation observed unsettled publication: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			resume()
			if err := <-saveResult; fail && !errors.Is(err, captureFailure) || !fail && err != nil {
				t.Fatalf("save: %v", err)
			}
			for range checks {
				select {
				case err := <-results:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("waiter did not finish after publication settled")
				}
			}
		})
	}
}

func TestCleanupWaitsForFileAndBatchReaders(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			ctx := context.Background()
			f := openFixture(t, t.TempDir(), true)
			first := publish(t, f.store, "", []model.Entry{file("a", "old")}, map[string]string{"a": "old"})
			tip := publish(t, f.store, first.ID, []model.Entry{file("a", "new")}, map[string]string{"a": "new"})
			if err := f.store.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
				t.Fatal(err)
			}
			preview, err := f.store.PreviewCleanup(ctx)
			if err != nil {
				t.Fatal(err)
			}
			readDone := make(chan error, 1)
			var finish func()
			if batch {
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				finish = func() { once.Do(func() { close(release) }) }
				go func() {
					readDone <- f.store.ReadFiles(ctx, first.ID, []model.Entry{file("a", "old")}, func(_ model.Entry, in io.Reader) error {
						close(entered)
						<-release
						b, err := io.ReadAll(in)
						if err == nil && string(b) != "old" {
							err = errors.New("open batch contents changed")
						}
						return err
					})
				}()
				<-entered
			} else {
				_, in, err := f.store.File(ctx, first.ID, "a")
				if err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				finish = func() {
					once.Do(func() {
						b, err := io.ReadAll(in)
						if err == nil && string(b) != "old" {
							err = errors.New("open file contents changed")
						}
						readDone <- errors.Join(err, in.Close())
					})
				}
			}
			t.Cleanup(finish)
			started, cleaned := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				_, err := f.store.Cleanup(ctx, preview.Token)
				cleaned <- err
			}()
			<-started
			select {
			case err := <-cleaned:
				t.Fatalf("cleanup ran during a retained read: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			finish()
			if err := <-readDone; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-cleaned:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup remained blocked after read completed")
			}
			if _, _, err := f.store.File(ctx, first.ID, "a"); !errors.Is(err, ErrExpired) {
				t.Fatalf("expired version: %v", err)
			}
			if read(t, f.store, tip.ID, "a") != "new" {
				t.Fatal("cleanup changed retained tip")
			}
		})
	}
}
