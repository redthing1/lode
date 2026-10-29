package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
)

func cleanupAt(t *testing.T, s *Store, when time.Time) cleanupPlan {
	t.Helper()
	s.mutation.Lock()
	defer s.mutation.Unlock()
	s.gate.Lock()
	defer s.gate.Unlock()
	p, err := s.planCleanup(context.Background(), when)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.applyCleanup(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExpiredCancellationPersistsCutoffBeforeAllowingDiscard(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	now := time.Now()
	stamp := now.Add(-9 * 24 * time.Hour).UnixMilli()
	r := model.Save{ID: fmt.Sprintf("%08x-%04x-%s", stamp>>16, stamp&0xffff, operation(t)[14:]), Branch: "main", Changes: []model.Entry{file("a", "old")}}
	// This request would become valid again after a three-day clock rollback.
	rolledBack := now.Add(-3 * 24 * time.Hour)
	if err := validOperation(r.ID, rolledBack, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec("CREATE TRIGGER reject_cutoff BEFORE UPDATE ON retention BEGIN SELECT RAISE(ABORT,'stop expiry'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, "alice", r); err == nil || errors.Is(err, ErrOperationExpired) || !strings.Contains(err.Error(), "stop expiry") {
		t.Fatalf("failed persistence authorized discard: %v", err)
	}
	if _, floor, err := s.retention(ctx); err != nil || floor != "" {
		t.Fatalf("failed cutoff write: %q %v", floor, err)
	}
	if _, err := f.db.Exec("DROP TRIGGER reject_cutoff"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, "alice", r); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("expired cancellation: %v", err)
	}
	f.close(t)
	f = openFixture(t, f.directory, false)
	if err := f.store.validOperation(ctx, r.ID, rolledBack); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("expiry revived after reopen and clock rollback: %v", err)
	}
	if err := f.store.validOperation(ctx, r.ID, now.Add(-12*24*time.Hour)); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("future-skew check hid persisted expiry: %v", err)
	}
}

func TestCleanupKeepsProtectedAcceptedOutcomeBeyondRetryWindow(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	r := model.Save{ID: operation(t), Branch: "main", Changes: []model.Entry{file("a", "protected")}}
	v, err := f.store.Save(ctx, "alice", r, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error { _, err := io.WriteString(w, "protected"); return err }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateTag(ctx, "release", v.ID); err != nil {
		t.Fatal(err)
	}
	publish(t, f.store, v.ID, []model.Entry{file("a", "latest")}, map[string]string{"a": "latest"})
	if err := f.store.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	cleanupAt(t, f.store, time.Now().Add(100*24*time.Hour))
	for _, settle := range []func(context.Context, string, model.Save) (model.Version, error){f.store.Resolve, f.store.Cancel} {
		if got, err := settle(ctx, "alice", r); err != nil || got != v {
			t.Fatalf("protected acceptance lost: %+v %v", got, err)
		}
	}
	if got, err := f.store.Save(ctx, "alice", r, Source{}); err != nil || got != v {
		t.Fatalf("accepted retry requested contents: %+v %v", got, err)
	}
}

func TestCleanupProtectsRefsAndSeparatesContentsFromHistory(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	first := publish(t, s, "", []model.Entry{file("asset", "first"), file("shared/keep", "same")}, map[string]string{"asset": "first", "shared/keep": "same"})
	second := publish(t, s, first.ID, []model.Entry{file("asset", "second")}, map[string]string{"asset": "second"})
	third := publish(t, s, second.ID, []model.Entry{file("asset", "third")}, map[string]string{"asset": "third"})
	tip := publish(t, s, third.ID, []model.Entry{file("asset", "tip")}, map[string]string{"asset": "tip"})
	if _, err := s.CreateTag(ctx, "important", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBranch(ctx, "experiment", second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	p, err := s.PreviewCleanup(ctx)
	if err != nil || p.Expire != 1 || p.Forget != 0 || p.Retained != 3 {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if _, err := s.Cleanup(ctx, p.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.File(ctx, third.ID, "asset"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired read: %v", err)
	}
	var entries []model.Entry
	if err := s.Inventory(ctx, third.ID, model.Selection{{Include: true}}, func(e model.Entry) error { entries = append(entries, e); return nil }); err != nil || len(entries) != 3 {
		t.Fatalf("expired logical metadata: %v %v", entries, err)
	}
	if _, err := s.CreateTag(ctx, "too-late", third.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("protected expired contents: %v", err)
	}
	for _, v := range []model.Version{first, second, tip} {
		if read(t, s, v.ID, "shared/keep") != "same" {
			t.Fatal("shared contents lost")
		}
	}
	// The complete metadata of a tag is kept, but its historical ancestors need
	// not survive. Unprotected old metadata and native roots are released.
	future := time.Now().Add(100 * 24 * time.Hour)
	plan := cleanupAt(t, s, future)
	if plan.preview.Forget != 1 || plan.forget[0] != third.ID || len(plan.directories) == 0 || len(plan.groups) == 0 {
		t.Fatalf("pruning: %+v", plan)
	}
	if _, err := s.Version(ctx, third.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("forgotten row: %v", err)
	}
	history, err := s.History(ctx, tip.ID, 1)
	if err != nil || len(history.Versions) != 1 || history.Next != "" || history.Boundary != third.ID {
		t.Fatalf("history boundary: %+v %v", history, err)
	}
	if _, err := s.mergeBase(ctx, tip.ID, second.ID); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("guessed missing ancestry: %v", err)
	}
	f.close(t)
	f = openFixture(t, f.directory, false)
	s = f.store
	if read(t, s, first.ID, "asset") != "first" || read(t, s, second.ID, "asset") != "second" || read(t, s, tip.ID, "asset") != "tip" {
		t.Fatal("protected contents after reopen")
	}
	if roots, err := f.content.Roots(ctx); err != nil || len(roots) != 3 {
		t.Fatalf("native protection: %v %v", roots, err)
	}
	if v, err := s.Reference(ctx, "important"); err != nil || v.ID != first.ID {
		t.Fatalf("tag lookup: %+v %v", v, err)
	}
}

func TestCleanupCutoffPreventsRetryRevivalAfterClockRollback(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	r := model.Save{ID: operation(t), Branch: "main", Message: "original", Changes: []model.Entry{file("a", "old")}}
	first, err := s.Save(ctx, "alice", r, Source{Write: func(_ model.Entry, w io.Writer, _ *Basis) error { _, err := io.WriteString(w, "old"); return err }})
	if err != nil {
		t.Fatal(err)
	}
	tip := publish(t, s, first.ID, []model.Entry{file("a", "new")}, map[string]string{"a": "new"})
	cancelled := model.Save{ID: operation(t), Branch: "main", Base: tip.ID, Changes: []model.Entry{file("a", "cancelled")}}
	if _, err := s.Cancel(ctx, "alice", cancelled); err != nil {
		t.Fatal(err)
	}
	absent := cancelled
	absent.ID = operation(t)
	if err := s.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	cleanupAt(t, s, time.Now().Add(10*24*time.Hour))
	var count int
	if err := f.db.QueryRow("SELECT count(*) FROM cancelled").Scan(&count); err != nil || count != 0 {
		t.Fatalf("receipts not pruned: %d %v", count, err)
	}
	// Current wall time is ten days behind the cleanup clock. Old valid-looking
	// IDs must stay refused, including the request with no prior outcome row.
	for _, old := range []model.Save{r, cancelled, absent} {
		if _, err := s.Resolve(ctx, "alice", old); !errors.Is(err, ErrOperationExpired) {
			t.Fatalf("resolve revival: %v", err)
		}
		if _, err := s.Cancel(ctx, "alice", old); !errors.Is(err, ErrOperationExpired) {
			t.Fatalf("cancel revival: %v", err)
		}
		if _, err := s.Save(ctx, "alice", old, Source{}); !errors.Is(err, ErrOperationExpired) {
			t.Fatalf("publication revival: %v", err)
		}
	}
	_, floor, err := s.retention(ctx)
	if err != nil || floor < absent.ID {
		t.Fatalf("floor failed to cover absent request: %q %v", floor, err)
	}
	cleanupAt(t, s, time.Now())
	_, laterFloor, _ := s.retention(ctx)
	if laterFloor != floor {
		t.Fatal("clock rollback lowered cutoff")
	}
	f.close(t)
	f = openFixture(t, f.directory, false)
	if _, err := f.store.Resolve(ctx, "alice", r); !errors.Is(err, ErrOperationExpired) {
		t.Fatalf("reopened cutoff: %v", err)
	}
	if read(t, f.store, tip.ID, "a") != "new" {
		t.Fatal("live tip lost")
	}
}

func TestCleanupTransactionAndReleaseRecovery(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	first := publish(t, s, "", []model.Entry{file("a", "old")}, map[string]string{"a": "old"})
	tip := publish(t, s, first.ID, []model.Entry{file("a", "new")}, map[string]string{"a": "new"})
	if err := s.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	p, err := s.planCleanup(ctx, time.Now().Add(10*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec("CREATE TRIGGER reject_cleanup BEFORE DELETE ON versions BEGIN SELECT RAISE(ABORT,'stop cleanup'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applyCleanup(ctx, p); err == nil {
		t.Fatal("aborted cleanup succeeded")
	}
	_, floor, _ := s.retention(ctx)
	v, _ := s.Version(ctx, first.ID)
	if floor != "" || !v.Retained || read(t, s, first.ID, "a") != "old" {
		t.Fatal("partial cleanup transaction")
	}
	if _, err := f.db.Exec("DROP TRIGGER reject_cleanup"); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the committed-expiry boundary before native release. Startup
	// must finish release without touching retained roots.
	if _, err := f.db.Exec("UPDATE versions SET retained=0,protection='' WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	if roots, err := f.content.Roots(ctx); err != nil || len(roots) != 2 {
		t.Fatalf("fixture did not retain orphan: %v %v", roots, err)
	}
	f.close(t)
	f = openFixture(t, f.directory, false)
	if roots, err := f.content.Roots(ctx); err != nil || len(roots) != 1 {
		t.Fatalf("release recovery: %v %v", roots, err)
	}
	if read(t, f.store, tip.ID, "a") != "new" {
		t.Fatal("release recovery damaged live root")
	}
}

func TestCleanupReviewBindsPolicyReferencesAndAvailability(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	first := publish(t, s, "", []model.Entry{file("a", "first")}, map[string]string{"a": "first"})
	tip := publish(t, s, first.ID, []model.Entry{file("a", "tip")}, map[string]string{"a": "tip"})
	p, err := s.PreviewCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(ctx, p.Token); !errors.Is(err, ErrCleanupChanged) {
		t.Fatalf("policy changed: %v", err)
	}
	p, err = s.PreviewCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTag(ctx, "important", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(ctx, p.Token); !errors.Is(err, ErrCleanupChanged) {
		t.Fatalf("tag changed: %v", err)
	}
	if _, err := s.CreateTag(ctx, "important", tip.ID); !errors.Is(err, ErrTagExists) {
		t.Fatalf("tag moved: %v", err)
	}
	if err := s.DeleteTag(ctx, "important", tip.ID); !errors.Is(err, ErrTagChanged) {
		t.Fatalf("stale tag deletion: %v", err)
	}
	if err := s.DeleteTag(ctx, "important", first.ID); err != nil {
		t.Fatal(err)
	}
	p, err = s.PreviewCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(ctx, p.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(ctx, p.Token); !errors.Is(err, ErrCleanupChanged) {
		t.Fatalf("reused obsolete preview: %v", err)
	}
	if _, err := s.CreateTag(ctx, "empty", ""); !errors.Is(err, ErrRequest) {
		t.Fatalf("empty tag: %v", err)
	}
	if _, err := s.CreateTag(ctx, "main", first.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired tag: %v", err)
	}
	if _, err := s.CreateTag(ctx, "tag", tip.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBranch(ctx, "tag", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reference(ctx, "tag"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("tag/branch collision: %v", err)
	}
}

func TestCleanupPreservesRecentOutcomesAndRefusesMissingMetadata(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	s := f.store
	first := publish(t, s, "", []model.Entry{file("a", "first")}, map[string]string{"a": "first"})
	tip := publish(t, s, first.ID, []model.Entry{file("a", "tip")}, map[string]string{"a": "tip"})
	if err := s.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 1}); err != nil {
		t.Fatal(err)
	}
	p := cleanupAt(t, s, time.Now().Add(2*24*time.Hour))
	if p.preview.Forget != 0 {
		t.Fatal("forgot result still inside seven-day retry window")
	}
	if v, err := s.Version(ctx, first.ID); err != nil || v.Retained {
		t.Fatalf("expired recent result: %+v %v", v, err)
	}
	stored, _ := s.version(ctx, tip.ID)
	if _, err := f.db.Exec("DELETE FROM directories WHERE id=?", stored.root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewCleanup(ctx); !errors.Is(err, ErrMetadata) {
		t.Fatalf("missing metadata did not stop cleanup: %v", err)
	}
	if v, _ := s.Version(ctx, tip.ID); !v.Retained {
		t.Fatal("preview changed availability")
	}
}
