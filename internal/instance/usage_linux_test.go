package instance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
)

func TestUsageCacheAndColdMeasurement(t *testing.T) {
	ctx := context.Background()
	s, alice := instanceFixture(t)
	p, err := s.CreateProject(ctx, Principal{User: alice}, "project")
	if err != nil {
		t.Fatal(err)
	}
	directory := s.directory
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	if sample := s.CachedUsage(p.ID); sample.Known() {
		t.Fatal("unmeasured storage reported as known")
	}
	first, err := s.ProjectUsage(ctx, Principal{User: alice}, alice.Name, p.Name)
	if err != nil || !first.Known() || first.Failed || first.Allocated <= 0 {
		t.Fatalf("measurement: %+v %v", first, err)
	}
	if len(s.projects) != 0 {
		t.Fatal("measuring storage opened a content repository")
	}
	// A cached hit needs no filesystem IO; failed refreshes retain the last value.
	repository := filepath.Join(directory, "projects", p.ID, "repository")
	if err := os.Rename(repository, repository+".aside"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(repository+".aside", repository)
	if got := s.measureUsage(ctx, p.ID); got != first {
		t.Fatal("fresh result was measured again")
	}
	s.usage.mu.Lock()
	entry := s.usage.entries[p.ID]
	entry.checked = time.Now().Add(-2 * time.Minute)
	s.usage.entries[p.ID] = entry
	s.usage.mu.Unlock()
	failed := s.measureUsage(ctx, p.ID)
	if !failed.Failed || failed.StorageUsage != first.StorageUsage || failed.Measured != first.Measured {
		t.Fatal("failed scan discarded the last valid measurement")
	}
	fresh := s.RecordUsage(p.ID, model.StorageUsage{Bytes: 23, Allocated: 4096})
	if got := s.measureUsage(ctx, p.ID); got != fresh || got.Failed {
		t.Fatal("collection result was not reused")
	}
	// A busy scan respects request cancellation, without inventing a zero size.
	s.usage.scan <- struct{}{}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if sample := s.measureUsage(cancelled, "missing"); sample.Known() || !sample.Failed {
		t.Fatal("cancelled measurement reported a size")
	}
	<-s.usage.scan
}

func TestAccountUsageCountsOnlyOwnedProjects(t *testing.T) {
	ctx := context.Background()
	s, alice := instanceFixture(t)
	p, err := s.CreateProject(ctx, Principal{User: alice}, "owned")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, Principal{User: alice}, "bob", originalPassword)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateProject(ctx, Principal{User: bob}, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember(ctx, Principal{User: bob}, bob.Name, other.Name, alice.Name, Reader); err != nil {
		t.Fatal(err)
	}
	s.RecordUsage(p.ID, model.StorageUsage{Allocated: 4096})
	s.RecordUsage(other.ID, model.StorageUsage{Allocated: 8192})
	usage, err := s.AccountUsage(ctx, Principal{User: alice}, false)
	if err != nil || len(usage.Projects) != 1 || !usage.Complete || usage.Total.Allocated != 4096 {
		t.Fatalf("owner attribution: %+v %v", usage, err)
	}
	if _, err := s.AccountUsage(ctx, Principal{User: alice, Token: true}, true); err != ErrAccess {
		t.Fatal("token acquired browser usage authority")
	}
	if err := s.beginDeletion(ctx, Principal{User: alice}, alice.Name, p.Name, p.ID); err != nil {
		t.Fatal(err)
	}
	usage, err = s.AccountUsage(ctx, Principal{User: alice}, true)
	if err != nil || len(usage.Projects) != 1 || usage.Projects[0].State != "deleting" || usage.Total.Allocated != 4096 {
		t.Fatalf("pending deletion disappeared: %+v %v", usage, err)
	}
	if err := s.finishDeletion(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if s.CachedUsage(p.ID).Known() {
		t.Fatal("deleted project's usage remained cached")
	}
	usage, err = s.AccountUsage(ctx, Principal{User: alice}, false)
	if err != nil || len(usage.Projects) != 0 || !usage.Complete || usage.Total.Allocated != 0 {
		t.Fatal("deleted project still attributed to owner")
	}
}
