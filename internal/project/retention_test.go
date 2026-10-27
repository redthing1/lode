package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestRetentionPreviewRevalidatesTagsAndProtectsBranchTipAndFirstParentTail(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	firstBytes := []byte("first version")
	first := mergeApply(t, local.Service, "retention-first", "main", "", fileChanges(map[string][]byte{"file.bin": firstBytes}), map[string][]byte{"file.bin": firstBytes})
	secondBytes := []byte("second version")
	second := mergeApply(t, local.Service, "retention-second", "main", first.ID, fileChanges(map[string][]byte{"file.bin": secondBytes}), map[string][]byte{"file.bin": secondBytes})
	thirdBytes := []byte("third version")
	third := mergeApply(t, local.Service, "retention-third", "main", second.ID, fileChanges(map[string][]byte{"file.bin": thirdBytes}), map[string][]byte{"file.bin": thirdBytes})
	tipBytes := []byte("fourth version")
	tip := mergeApply(t, local.Service, "retention-tip", "main", third.ID, fileChanges(map[string][]byte{"file.bin": tipBytes}), map[string][]byte{"file.bin": tipBytes})
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{KeepLast: 2}); err != nil {
		t.Fatal(err)
	}
	late := tip.CreatedAt.Add(48 * time.Hour)
	local.Service.now = func() time.Time { return late }

	// The preview is a point-in-time candidate list. A tag added after it must
	// win the expiry-time revalidation.
	preview, err := local.Service.PreviewRetention(ctx, "", 1)
	if err != nil || preview.TotalCandidates != 2 || len(preview.Candidates) != 1 || preview.Next == "" {
		t.Fatalf("first retention page = %+v, err=%v; want one of two candidates and a cursor", preview, err)
	}
	secondPage, err := local.Service.PreviewRetention(ctx, preview.Next, 1)
	if err != nil || secondPage.TotalCandidates != 2 || len(secondPage.Candidates) != 1 || secondPage.Next != "" {
		t.Fatalf("second retention page = %+v, err=%v", secondPage, err)
	}
	if err := local.Service.CreateTag(ctx, "late-tag", first.ID); err != nil {
		t.Fatal(err)
	}
	result, err := local.Service.Expire(ctx, []string{first.ID})
	if err != nil || len(result.Expired) != 0 || len(result.Protected) != 1 || !containsString(result.Protected[0].Reasons, "tag:late-tag") {
		t.Fatalf("expire candidate after late tag = %+v, err=%v", result, err)
	}

	result, err = local.Service.Expire(ctx, []string{tip.ID})
	if err != nil || len(result.Expired) != 0 || len(result.Protected) != 1 || !containsString(result.Protected[0].Reasons, "branch:main") {
		t.Fatalf("expire branch tip = %+v, err=%v", result, err)
	}
	result, err = local.Service.Expire(ctx, []string{third.ID})
	if err != nil || len(result.Protected) != 1 || !containsString(result.Protected[0].Reasons, "recent branch history:main") {
		t.Fatalf("expire first-parent retained commit = %+v, err=%v", result, err)
	}
	result, err = local.Service.Expire(ctx, []string{second.ID})
	if err != nil || len(result.Expired) != 1 || result.Expired[0] != second.ID {
		t.Fatalf("expire commit outside first-parent tail = %+v, err=%v", result, err)
	}

	if err := local.Service.DeleteTag(ctx, "late-tag", first.ID); err != nil {
		t.Fatal(err)
	}
	result, err = local.Service.Expire(ctx, []string{first.ID})
	if err != nil || len(result.Expired) != 1 || result.Expired[0] != first.ID {
		t.Fatalf("expire old unprotected version = %+v, err=%v", result, err)
	}
	// Metadata remains available after expiry even though the commit is no
	// longer a valid source for content operations.
	if got, err := local.Service.Commit(ctx, first.ID); err != nil || got.ID != first.ID {
		t.Fatalf("expired immutable commit lookup = %+v, %v", got, err)
	}
	if _, _, err := local.Service.Entries(ctx, first.ID, []SelectionRule{{Included: true}}, "", 20); err != nil {
		t.Fatalf("expired comparison metadata was removed: %v", err)
	}
}

func TestRetentionGraceAndRecentTimeWindowAreDistinctProtections(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	firstData := []byte("old parent")
	first := mergeApply(t, local.Service, "retention-grace-parent", "main", "", fileChanges(map[string][]byte{"file.bin": firstData}), map[string][]byte{"file.bin": firstData})
	secondData := []byte("branch tip")
	secondAt := first.CreatedAt.Add(time.Hour)
	local.Service.now = func() time.Time { return secondAt }
	second := mergeApply(t, local.Service, "retention-grace-tip", "main", first.ID, fileChanges(map[string][]byte{"file.bin": secondData}), map[string][]byte{"file.bin": secondData})
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	result, err := local.Service.Expire(ctx, []string{first.ID})
	if err != nil || len(result.Protected) != 1 || !containsString(result.Protected[0].Reasons, "publication grace") {
		t.Fatalf("accepted publication grace = %+v, err=%v", result, err)
	}

	// The explicit recent window remains after the 24-hour publication grace.
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{KeepDays: 2}); err != nil {
		t.Fatal(err)
	}
	local.Service.now = func() time.Time { return first.CreatedAt.Add(30 * time.Hour) }
	result, err = local.Service.Expire(ctx, []string{first.ID})
	if err != nil || len(result.Protected) != 1 || !containsString(result.Protected[0].Reasons, "recent time window") {
		t.Fatalf("recent time-window protection = %+v, err=%v", result, err)
	}
	local.Service.now = func() time.Time { return first.CreatedAt.Add(72 * time.Hour) }
	result, err = local.Service.Expire(ctx, []string{first.ID})
	if err != nil || len(result.Expired) != 1 || result.Expired[0] != first.ID {
		t.Fatalf("expiry after time window = %+v, err=%v", result, err)
	}
	result, err = local.Service.Expire(ctx, []string{second.ID})
	if err != nil || len(result.Protected) != 1 || !containsString(result.Protected[0].Reasons, "branch:main") {
		t.Fatalf("tip remains protected after old parent expires: %+v, err=%v", result, err)
	}
}

func TestExpiredPayloadCannotBeResurrectedByContentOperations(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	oldData := []byte("old payload bytes still on disk until native collection")
	oldRequest := makeChangeRequest("resurrection-old", "", "original", map[string][]byte{"file.bin": oldData})
	old, err := local.Service.Apply(ctx, oldRequest, func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(oldData)), nil })
	if err != nil {
		t.Fatal(err)
	}
	newData := []byte("current payload")
	current := mergeApply(t, local.Service, "resurrection-current", "main", old.ID, fileChanges(map[string][]byte{"file.bin": newData}), map[string][]byte{"file.bin": newData})
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	local.Service.now = func() time.Time { return current.CreatedAt.Add(48 * time.Hour) }
	result, err := local.Service.Expire(ctx, []string{old.ID})
	if err != nil || len(result.Expired) != 1 {
		t.Fatalf("expire old payload = %+v, %v", result, err)
	}

	// Accepted retries and comparison history are independent of payload status.
	replayed, err := local.Service.Apply(ctx, oldRequest, nil)
	if err != nil || replayed.ID != old.ID {
		t.Fatalf("accepted retry after expiry = %+v, %v", replayed, err)
	}
	if _, err := local.Service.Commit(ctx, old.ID); err != nil {
		t.Fatalf("expired history lookup: %v", err)
	}
	if _, _, err := local.Service.Entries(ctx, old.ID, []SelectionRule{{Included: true}}, "", 20); err != nil {
		t.Fatalf("expired metadata lookup: %v", err)
	}

	if err := local.Service.CreateBranch(ctx, "too-late", old.ID); !errors.Is(err, ErrPayloadExpired) {
		t.Errorf("branch from expired payload = %v, want ErrPayloadExpired", err)
	}
	if err := local.Service.CreateTag(ctx, "too-late", old.ID); !errors.Is(err, ErrPayloadExpired) {
		t.Errorf("tag expired payload = %v, want ErrPayloadExpired", err)
	}
	if _, err := local.Service.HoldRead(ctx, "too-late-read", old.ID); !errors.Is(err, ErrPayloadExpired) {
		t.Errorf("hold expired payload = %v, want ErrPayloadExpired", err)
	}
	if _, _, err := local.Service.OpenFile(ctx, old.ID, "file.bin"); !errors.Is(err, ErrPayloadExpired) {
		t.Errorf("open expired payload = %v, want ErrPayloadExpired", err)
	}
	mergeRequest := MergeRequest{OperationID: "too-late-merge", MergeSpec: MergeSpec{Branch: "main", Base: current.ID, Other: old.ID}, Author: "tester", Message: "expired source"}
	if _, err := local.Service.Merge(ctx, mergeRequest); !errors.Is(err, ErrPayloadExpired) {
		t.Errorf("merge with expired source = %v, want ErrPayloadExpired", err)
	}

	// Root release marks the project policy boundary; no automatic native
	// collection ran, so Kopia still verifies the physical source contents.
	commit, err := local.Service.Commit(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := local.Service.archive(ctx, commit.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Service.roots.Validate(ctx, archive); err != nil {
		t.Fatalf("expected uncollected source bytes to remain natively available: %v", err)
	}
}

type failingReleaseRoots struct {
	ContentRoots
	fail bool
}

func (r *failingReleaseRoots) Release(ctx context.Context, key string) error {
	if r.fail {
		return errors.New("injected native root release failure")
	}
	return r.ContentRoots.Release(ctx, key)
}

func TestExpiredReleaseFailurePersistsAndRecoverDrainsWithoutChangingAcceptedOperation(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	oldData := []byte("expired after retention")
	old := mergeApply(t, local.Service, "release-old", "main", "", fileChanges(map[string][]byte{"file.bin": oldData}), map[string][]byte{"file.bin": oldData})
	newData := []byte("kept current")
	tip := mergeApply(t, local.Service, "release-tip", "main", old.ID, fileChanges(map[string][]byte{"file.bin": newData}), map[string][]byte{"file.bin": newData})
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	late := tip.CreatedAt.Add(48 * time.Hour)
	local.Service.now = func() time.Time { return late }
	originalRoots := local.Service.roots
	nativeRoots, ok := originalRoots.(*kopia.Roots)
	if !ok {
		t.Fatalf("local roots type = %T, want *kopia.Roots", originalRoots)
	}
	operation, err := local.metadata.publication(ctx, "release-old")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := local.metadata.commit(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := local.Service.archive(ctx, commit.Tree)
	if err != nil {
		t.Fatal(err)
	}
	rootKey := local.Service.protectionKey(operation.ID)
	if err := nativeRoots.Check(ctx, rootKey, archive); err != nil {
		t.Fatalf("accepted publication root not found under operation key: %v", err)
	}
	faults := &failingReleaseRoots{ContentRoots: originalRoots, fail: true}
	local.Service.roots = faults
	if _, err := local.Service.Expire(ctx, []string{old.ID}); err == nil {
		t.Fatal("Expire succeeded despite injected native release error")
	}
	statuses, err := local.Service.PayloadStatuses(ctx, []string{old.ID})
	if err != nil || len(statuses) != 1 || statuses[0].State != "expired" || !statuses[0].ReleasePending {
		t.Fatalf("expiry status after release failure = %+v, %v", statuses, err)
	}
	operation, err = local.metadata.publication(ctx, "release-old")
	if err != nil || operation.State != OperationAccepted || operation.ReleaseDone {
		t.Fatalf("accepted operation after payload release failure = %+v, %v", operation, err)
	}
	if err := nativeRoots.Check(ctx, rootKey, archive); err != nil {
		t.Fatalf("native root should remain protected after failed release: %v", err)
	}

	faults.fail = false
	if err := local.Service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.Recover(ctx); err != nil {
		t.Fatalf("repeat recovery: %v", err)
	}
	statuses, err = local.Service.PayloadStatuses(ctx, []string{old.ID})
	if err != nil || len(statuses) != 1 || statuses[0].State != "expired" || statuses[0].ReleasePending {
		t.Fatalf("expiry status after recovery = %+v, %v", statuses, err)
	}
	operation, err = local.metadata.publication(ctx, "release-old")
	if err != nil || operation.State != OperationAccepted || operation.ReleaseDone {
		t.Fatalf("recovery changed accepted operation release marker = %+v, %v", operation, err)
	}
	if err := nativeRoots.Check(ctx, rootKey, archive); !errors.Is(err, kopia.ErrRootNotFound) {
		t.Fatalf("native root after expired payload release = %v, want ErrRootNotFound", err)
	}
}

func TestActiveCapturingMergeProtectsBothParentsUntilOperationExpiry(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseData := []byte("base")
	base := mergeApply(t, local.Service, "capture-base", "main", "", fileChanges(map[string][]byte{"file.bin": baseData}), map[string][]byte{"file.bin": baseData})
	if err := local.Service.CreateBranch(ctx, "source", base.ID); err != nil {
		t.Fatal(err)
	}
	destData := []byte("destination")
	dest := mergeApply(t, local.Service, "capture-dest", "main", base.ID, fileChanges(map[string][]byte{"file.bin": destData}), map[string][]byte{"file.bin": destData})
	sourceData := []byte("source")
	source := mergeApply(t, local.Service, "capture-source", "source", base.ID, fileChanges(map[string][]byte{"file.bin": sourceData}), map[string][]byte{"file.bin": sourceData})
	if err := local.Service.DeleteBranch(ctx, "source", source.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.SetRetentionPolicy(ctx, RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	start := source.CreatedAt.Add(48 * time.Hour)
	local.Service.now = func() time.Time { return start }
	request := MergeRequest{OperationID: "active-merge-capture", MergeSpec: MergeSpec{Branch: "main", Base: dest.ID, Other: source.ID}, Author: "tester", Message: "capturing both parents"}
	reserveMergeCapture(t, local, request)

	// Move the destination branch after the durable capture intent. The old
	// destination and unbranched source are now protected only by that intent.
	advanceData := []byte("advanced destination")
	advanced := mergeApply(t, local.Service, "capture-dest-advance", "main", dest.ID, fileChanges(map[string][]byte{"file.bin": advanceData}), map[string][]byte{"file.bin": advanceData})
	local.Service.now = func() time.Time { return start.Add(time.Hour) }
	result, err := local.Service.Expire(ctx, []string{dest.ID, source.ID})
	if err != nil || len(result.Expired) != 0 || len(result.Protected) != 2 {
		t.Fatalf("expire active merge parents = %+v, %v", result, err)
	}
	for _, item := range result.Protected {
		if item.CommitID != dest.ID && item.CommitID != source.ID {
			t.Errorf("unexpected protected commit %q", item.CommitID)
		}
		if !containsString(item.Reasons, "operation:active-merge-capture") {
			t.Errorf("parent %s missing active operation reason: %v", item.CommitID, item.Reasons)
		}
	}
	if head, err := local.Service.Head(ctx, "main"); err != nil || head != advanced.ID {
		t.Fatalf("destination branch after capture = %q, %v", head, err)
	}
	op, err := local.Service.Operation(ctx, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	local.Service.now = func() time.Time { return op.ExpiresAt.Add(-time.Nanosecond) }
	result, err = local.Service.Expire(ctx, []string{dest.ID, source.ID})
	if err != nil || len(result.Expired) != 0 || len(result.Protected) != 2 {
		t.Fatalf("merge parents just before operation expiry = %+v, %v", result, err)
	}
	local.Service.now = func() time.Time { return op.ExpiresAt }
	result, err = local.Service.Expire(ctx, []string{dest.ID, source.ID})
	if err != nil || len(result.Expired) != 2 {
		t.Fatalf("merge parents at operation expiry = %+v, %v", result, err)
	}
}
