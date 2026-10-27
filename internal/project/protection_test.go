package project

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTagsAreCASProtectedReasonsAndCannotRestoreExpiredContents(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	first := mergeApply(t, local.Service, "tag-first", "main", "", fileChanges(map[string][]byte{"file.bin": []byte("first bytes")}), map[string][]byte{"file.bin": []byte("first bytes")})
	second := mergeApply(t, local.Service, "tag-second", "main", first.ID, fileChanges(map[string][]byte{"file.bin": []byte("second bytes")}), map[string][]byte{"file.bin": []byte("second bytes")})
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.CreateTag(ctx, "release/one", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.CreateTag(ctx, "release/one", first.ID); err != nil {
		t.Fatalf("exact tag retry: %v", err)
	}
	if err := local.Service.CreateTag(ctx, "release/one", second.ID); !errors.Is(err, ErrTagExists) {
		t.Fatalf("retarget existing tag = %v, want ErrTagExists", err)
	}
	if got, err := local.Service.Tag(ctx, "release/one"); err != nil || got != (Tag{Name: "release/one", CommitID: first.ID}) {
		t.Fatalf("Tag() = %+v, %v", got, err)
	}
	if err := local.Service.CreateTag(ctx, "bad tag", first.ID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid tag name error = %v", err)
	}
	page, next, err := local.Service.Tags(ctx, "", 1)
	if err != nil || next != "" || len(page) != 1 || page[0].CommitID != first.ID {
		t.Fatalf("Tags() = %+v, next=%q, err=%v", page, next, err)
	}
	if err := local.Service.DeleteTag(ctx, "release/one", second.ID); !errors.Is(err, ErrStaleHead) {
		t.Fatalf("delete with wrong expected commit = %v, want ErrStaleHead", err)
	}
	late := second.CreatedAt.Add(48 * time.Hour)
	local.Service.now = func() time.Time { return late }
	protected, err := local.Service.Expire(ctx, []string{first.ID})
	if err != nil || len(protected.Expired) != 0 || len(protected.Protected) != 1 || protected.Protected[0].CommitID != first.ID {
		t.Fatalf("expire tagged version = %+v, %v", protected, err)
	}
	if err := local.Service.DeleteTag(ctx, "release/one", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.DeleteTag(ctx, "release/one", first.ID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := local.Service.Tag(ctx, "release/one"); !errors.Is(err, ErrTagNotFound) {
		t.Fatalf("Tag after deletion = %v, want ErrTagNotFound", err)
	}

	// The old version's grace period has ended, and the branch points at the
	// second version. Expiry releases its root, while comparison metadata stays.
	result, err := local.Service.Expire(ctx, []string{first.ID})
	if err != nil || len(result.Expired) != 1 || result.Expired[0] != first.ID {
		t.Fatalf("expire untagged version = %+v, %v", result, err)
	}
	if err := local.Service.CreateTag(ctx, "too-late", first.ID); !errors.Is(err, ErrPayloadExpired) {
		t.Fatalf("tag expired version = %v, want ErrPayloadExpired", err)
	}
	archive, err := local.Service.archive(ctx, first.Tree)
	if err != nil {
		t.Fatalf("comparison metadata after expiry: %v", err)
	}
	if err := local.Service.roots.Validate(ctx, archive); err != nil {
		t.Fatalf("native bytes unexpectedly unavailable immediately after root release: %v", err)
	}
}

func TestReadHoldProtectsExpiryAndRenewsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "held-project")
	if err := InitializeLocal(ctx, directory); err != nil {
		t.Fatal(err)
	}
	local := openInitializedLocal(t, directory)
	base := mergeApply(t, local.Service, "hold-base", "main", "", fileChanges(map[string][]byte{"file.bin": []byte("base")}), map[string][]byte{"file.bin": []byte("base")})
	tip := mergeApply(t, local.Service, "hold-tip", "main", base.ID, fileChanges(map[string][]byte{"file.bin": []byte("tip")}), map[string][]byte{"file.bin": []byte("tip")})
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	start := tip.CreatedAt.Add(48 * time.Hour)
	local.Service.now = func() time.Time { return start }
	lease, err := local.Service.HoldRead(ctx, "hold-base-read", base.ID)
	if err != nil || lease.CommitID != base.ID || lease.Lifetime != 15*time.Minute || !lease.ExpiresAt.Equal(start.Add(15*time.Minute)) {
		t.Fatalf("HoldRead() = %+v, %v", lease, err)
	}
	if _, err := local.Service.HoldRead(ctx, "hold-base-read", tip.ID); !errors.Is(err, ErrReadLeaseConflict) {
		t.Fatalf("read hold rebind = %v, want ErrReadLeaseConflict", err)
	}
	result, err := local.Service.Expire(ctx, []string{base.ID})
	if err != nil || len(result.Expired) != 0 || len(result.Protected) != 1 || result.Protected[0].CommitID != base.ID {
		t.Fatalf("expire with active read hold = %+v, %v", result, err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openInitializedLocal(t, directory)
	renewAt := start.Add(20 * time.Minute) // the stored lease expired while the service was closed.
	reopened.Service.now = func() time.Time { return renewAt }
	renewed, err := reopened.Service.HoldRead(ctx, "hold-base-read", base.ID)
	if err != nil || renewed.CommitID != base.ID || renewed.Lifetime != 15*time.Minute || !renewed.ExpiresAt.Equal(renewAt.Add(15*time.Minute)) {
		t.Fatalf("renew after reopen = %+v, %v", renewed, err)
	}
	if err := reopened.Service.ReleaseRead(ctx, "hold-base-read"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Service.ReleaseRead(ctx, "hold-base-read"); err != nil {
		t.Fatalf("repeat release = %v", err)
	}
	result, err = reopened.Service.Expire(ctx, []string{base.ID})
	if err != nil || len(result.Expired) != 1 || result.Expired[0] != base.ID {
		t.Fatalf("expire after read release = %+v, %v", result, err)
	}
	if _, err := reopened.Service.HoldRead(ctx, "new-hold", base.ID); !errors.Is(err, ErrPayloadExpired) {
		t.Fatalf("hold expired version = %v, want ErrPayloadExpired", err)
	}

	// Expired lease rows are pruned only after their query rows have closed;
	// an exact stored-time predicate keeps deletion safe if the row changed.
	if _, err := reopened.metadata.db.ExecContext(ctx,
		`INSERT INTO read_leases (id, commit_id, expires_at) VALUES (?, ?, ?)`,
		"abandoned-hold", tip.ID, formatMetadataTime(renewAt.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	reopened.Service.mu.Lock()
	err = reopened.Service.pruneReadLeases(ctx)
	reopened.Service.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.metadata.readLease(ctx, "abandoned-hold"); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("pruned abandoned hold lookup = %v", err)
	}
}

func TestReadHoldValidatesFirstGrantAndRejectsInvalidIdentifiers(t *testing.T) {
	local := openTestLocal(t)
	commit := mergeApply(t, local.Service, "hold-validation-commit", "main", "", fileChanges(map[string][]byte{"file.bin": []byte("payload")}), map[string][]byte{"file.bin": []byte("payload")})
	if _, err := local.Service.HoldRead(context.Background(), "bad/id", commit.ID); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid hold ID error = %v", err)
	}
	if _, err := local.Service.HoldRead(context.Background(), "valid-hold", "invalid-commit"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid commit ID error = %v", err)
	}
	lease, err := local.Service.HoldRead(context.Background(), "valid-hold", commit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Service.ReleaseRead(context.Background(), lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := local.Service.Entries(context.Background(), commit.ID, []SelectionRule{{Path: "", Included: true}}, "", 10); err != nil {
		t.Fatalf("metadata entries after hold release: %v", err)
	}
}
