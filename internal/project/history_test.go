package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/engine/kopia"
)

const testRepositoryNamespace = "history-test-repository"

func openBoundHistoryMetadata(t *testing.T) *Metadata {
	t.Helper()
	metadata, err := OpenMetadata(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if got, err := metadata.bindProject(context.Background(), testRepositoryNamespace, "project-test-id"); err != nil || got != "project-test-id" {
		t.Fatalf("bindProject() = %q, %v", got, err)
	}
	return metadata
}

func installHistoryRoot(t *testing.T, metadata *Metadata, name string) TreeRef {
	t.Helper()
	id, err := metadata.Import(context.Background(), testRepositoryNamespace, "format-v1", []byte("root descriptor: "+name), nil)
	if err != nil {
		t.Fatal(err)
	}
	return TreeRef{Format: "format-v1", ID: id}
}

func testPublicationRecord(id, base string, tree TreeRef, now time.Time) publicationRecord {
	return publicationRecord{
		ID: id, Fingerprint: "fingerprint:" + id, CommitID: "commit:" + id,
		Input:     publicationInput{Branch: "main", Base: base, Tree: tree, Author: "tester", Message: "message " + id},
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), State: OperationPending,
	}
}

func testCaptureRecord(id, base string, now time.Time) publicationRecord {
	sealed, _ := json.Marshal(ChangeRequest{
		OperationID: id, Branch: "main", Base: base, Author: "tester", Message: "capture " + id,
		Changes: []Change{{Path: "file.bin", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Size: 7}},
	})
	digest := sha256.Sum256(append([]byte("lode-sparse-capture-v1\x00"), sealed...))
	return publicationRecord{
		ID: id, Fingerprint: hex.EncodeToString(digest[:]), CommitID: "commit:" + id,
		Input:        publicationInput{Branch: "main", Base: base, Author: "tester", Message: "capture " + id},
		CaptureInput: sealed, CreatedAt: now, ExpiresAt: now.Add(time.Hour), State: OperationCapturing,
	}
}

func TestHistoryBindingAndPublicationRetry(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	if got, err := metadata.bindProject(ctx, testRepositoryNamespace, "project-test-id"); err != nil || got != "project-test-id" {
		t.Fatalf("idempotent bindProject() = %q, %v", got, err)
	}
	if _, err := metadata.bindProject(ctx, "another-repository", "project-test-id"); !errors.Is(err, ErrRepositoryMismatch) {
		t.Fatalf("bindProject(other namespace) error = %v, want ErrRepositoryMismatch", err)
	}
	if got, err := metadata.bindProject(ctx, testRepositoryNamespace, "new-proposal-id"); err != nil || got != "project-test-id" {
		t.Fatalf("bindProject(reopen with new proposal) = %q, %v; want persisted identity", got, err)
	}

	now := time.Date(2026, 10, 26, 12, 0, 0, 123, time.UTC)
	root := installHistoryRoot(t, metadata, "first")
	record := testPublicationRecord("operation-a", "", root, now)
	if _, err := metadata.preparePublication(ctx, record); err != nil {
		t.Fatalf("preparePublication() with archived root: %v", err)
	}
	got, err := metadata.preparePublication(ctx, record)
	if err != nil {
		t.Fatalf("preparePublication() retry: %v", err)
	}
	if !reflect.DeepEqual(got, record) {
		t.Fatalf("retry record = %#v, want %#v", got, record)
	}
	newProposal := record
	newProposal.CommitID = "new-candidate-id-must-be-ignored"
	got, err = metadata.preparePublication(ctx, newProposal)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("retry with new candidate proposal = %#v, %v; want original binding %#v", got, err, record)
	}
	if got, err := metadata.publication(ctx, record.ID); err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("publication() = %#v, %v; want %#v", got, err, record)
	}
	changed := record
	changed.Fingerprint += ":changed"
	if _, err := metadata.preparePublication(ctx, changed); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("preparePublication(changed fingerprint) error = %v, want ErrOperationConflict", err)
	}
	changedInput := record
	changedInput.Input.Message += " different"
	if _, err := metadata.preparePublication(ctx, changedInput); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("preparePublication(changed input) error = %v, want ErrOperationConflict", err)
	}
	missingRoot := record
	missingRoot.ID, missingRoot.CommitID = "operation-missing-root", "commit-missing-root"
	missingRoot.Input.Tree.ID = "missing-root"
	if _, err := metadata.preparePublication(ctx, missingRoot); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("preparePublication(missing root) error = %v, want ErrMetadataNotFound", err)
	}
	if _, err := metadata.publication(ctx, missingRoot.ID); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("publication after missing-root rejection = %v, want ErrOperationNotFound", err)
	}
}

func TestHistoryAcceptWritesCommitBranchAndOperationAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := metadata.bindProject(ctx, testRepositoryNamespace, "project-test-id"); err != nil {
		t.Fatal(err)
	}
	root := installHistoryRoot(t, metadata, "accepted")
	now := time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC)
	record := testPublicationRecord("operation-accepted", "", root, now)
	if _, err := metadata.preparePublication(ctx, record); err != nil {
		t.Fatal(err)
	}
	accepted, err := metadata.acceptPublication(ctx, record.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != OperationAccepted || accepted.CommitID != record.CommitID {
		t.Fatalf("accepted operation = %+v", accepted)
	}
	if head, err := metadata.head(ctx, "main"); err != nil || head != record.CommitID {
		t.Fatalf("head = %q, %v; want %q", head, err, record.CommitID)
	}
	wantCommit := Commit{ID: record.CommitID, Tree: root, Author: record.Input.Author, Message: record.Input.Message, CreatedAt: record.CreatedAt}
	if got, err := metadata.commit(ctx, record.CommitID); err != nil || !reflect.DeepEqual(got, wantCommit) {
		t.Fatalf("commit = %+v, %v; want %+v", got, err, wantCommit)
	}
	if got, err := metadata.publication(ctx, record.ID); err != nil || got.State != OperationAccepted {
		t.Fatalf("publication = %+v, %v; want accepted", got, err)
	}
	if got, err := metadata.acceptPublication(ctx, record.ID, now.Add(2*time.Hour)); err != nil || got.State != OperationAccepted {
		t.Fatalf("accepted retry = %+v, %v; want stable accepted result", got, err)
	}
	if _, err := metadata.stopPublication(ctx, record.ID, now, false); !errors.Is(err, ErrAlreadyAccepted) {
		t.Fatalf("stop accepted publication error = %v, want ErrAlreadyAccepted", err)
	}
	if err := metadata.markPublicationReleased(ctx, record.ID); !errors.Is(err, ErrAlreadyAccepted) {
		t.Fatalf("release accepted publication error = %v, want ErrAlreadyAccepted", err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err = OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	if head, err := metadata.head(ctx, "main"); err != nil || head != record.CommitID {
		t.Fatalf("reopened head = %q, %v", head, err)
	}
	if got, err := metadata.commit(ctx, record.CommitID); err != nil || !reflect.DeepEqual(got, wantCommit) {
		t.Fatalf("reopened commit = %+v, %v; want %+v", got, err, wantCommit)
	}
}

func TestPublicationInputOmitsEmptyMergeParent(t *testing.T) {
	got, err := json.Marshal(publicationInput{
		Branch: "main", Base: "", Tree: TreeRef{Format: "format-v1", ID: "root"},
		Author: "tester", Message: "message",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"Branch":"main","Base":"","Tree":{"Format":"format-v1","ID":"root"},"Author":"tester","Message":"message"}`
	if string(got) != want {
		t.Fatalf("legacy input JSON = %s, want %s", got, want)
	}
}

func TestTwoParentPublicationAtomicityRetryAndStaleHead(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)
	root := installHistoryRoot(t, metadata, "merge")
	base := testPublicationRecord("merge-base", "", root, now)
	if _, err := metadata.preparePublication(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, base.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	secondParent := Commit{ID: "merge-second-parent", Tree: root, Author: "tester", Message: "other line", CreatedAt: now}
	tx, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := putCommitTx(ctx, tx, secondParent); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	merge := testPublicationRecord("merge-atomic", base.CommitID, root, now.Add(2*time.Minute))
	merge.Input.MergeParent = secondParent.ID
	if _, err := metadata.preparePublication(ctx, merge); err != nil {
		t.Fatal(err)
	}
	proposal := merge
	proposal.CommitID = "ignored-merge-candidate"
	prepared, err := metadata.preparePublication(ctx, proposal)
	if err != nil || prepared.CommitID != merge.CommitID || prepared.Input.MergeParent != secondParent.ID {
		t.Fatalf("merge retry = %+v, %v; want original candidate and second parent", prepared, err)
	}
	if _, err := metadata.db.Exec(`CREATE TRIGGER fail_merge_branch BEFORE UPDATE ON branches BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, merge.ID, now.Add(3*time.Minute)); err == nil {
		t.Fatal("acceptPublication() succeeded despite branch trigger")
	}
	if _, err := metadata.commit(ctx, merge.CommitID); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("candidate after rollback = %v, want absent", err)
	}
	if head, err := metadata.head(ctx, "main"); err != nil || head != base.CommitID {
		t.Fatalf("head after rollback = %q, %v; want base %q", head, err, base.CommitID)
	}
	if got, err := metadata.publication(ctx, merge.ID); err != nil || got.State != OperationPending {
		t.Fatalf("operation after rollback = %+v, %v; want pending", got, err)
	}
	if _, err := metadata.db.Exec(`DROP TRIGGER fail_merge_branch`); err != nil {
		t.Fatal(err)
	}
	accepted, err := metadata.acceptPublication(ctx, merge.ID, now.Add(4*time.Minute))
	if err != nil || accepted.State != OperationAccepted || accepted.CommitID != merge.CommitID {
		t.Fatalf("accepted merge = %+v, %v", accepted, err)
	}
	gotCommit, err := metadata.commit(ctx, merge.CommitID)
	wantParents := []string{base.CommitID, secondParent.ID}
	if err != nil || !reflect.DeepEqual(gotCommit.Parents, wantParents) {
		t.Fatalf("merge parents = %v, %v; want %v", gotCommit.Parents, err, wantParents)
	}

	stale := testPublicationRecord("merge-stale", base.CommitID, root, now.Add(5*time.Minute))
	stale.Input.MergeParent = secondParent.ID
	if _, err := metadata.preparePublication(ctx, stale); err != nil {
		t.Fatal(err)
	}
	advance := testPublicationRecord("merge-advance", merge.CommitID, root, now.Add(6*time.Minute))
	if _, err := metadata.preparePublication(ctx, advance); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, advance.ID, now.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	staleResult, err := metadata.acceptPublication(ctx, stale.ID, now.Add(8*time.Minute))
	if err != nil || staleResult.State != OperationStale || staleResult.ObservedTip != advance.CommitID {
		t.Fatalf("stale merge result = %+v, %v", staleResult, err)
	}
	if _, err := metadata.commit(ctx, stale.CommitID); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("stale merge candidate = %v, want absent", err)
	}
}

func TestTwoParentValidationAndMissingParentDoNotWrite(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 17, 30, 0, 0, time.UTC)
	root := installHistoryRoot(t, metadata, "merge-validation")
	withoutBase := testPublicationRecord("merge-no-base", "", root, now)
	withoutBase.Input.MergeParent = "other"
	if _, err := metadata.preparePublication(ctx, withoutBase); err == nil {
		t.Fatal("preparePublication accepted a merge parent without a base")
	}
	equalParents := testPublicationRecord("merge-equal-parents", "same", root, now)
	equalParents.Input.MergeParent = "same"
	if _, err := metadata.preparePublication(ctx, equalParents); err == nil {
		t.Fatal("preparePublication accepted equal parents")
	}

	base := testPublicationRecord("merge-missing-base-seed", "", root, now)
	if _, err := metadata.preparePublication(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, base.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	missing := testPublicationRecord("merge-missing-second", base.CommitID, root, now.Add(2*time.Minute))
	missing.Input.MergeParent = "missing-second-parent"
	if _, err := metadata.preparePublication(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, missing.ID, now.Add(3*time.Minute)); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("accept with missing second parent error = %v, want ErrCommitNotFound", err)
	}
	if _, err := metadata.commit(ctx, missing.CommitID); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("missing-parent candidate = %v, want absent", err)
	}
	if head, err := metadata.head(ctx, "main"); err != nil || head != base.CommitID {
		t.Fatalf("head after missing-parent rejection = %q, %v; want %q", head, err, base.CommitID)
	}
	if got, err := metadata.publication(ctx, missing.ID); err != nil || got.State != OperationPending {
		t.Fatalf("operation after missing-parent rejection = %+v, %v; want pending", got, err)
	}
}

func TestHistoryStaleHeadAndMissingNonemptyBase(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	root := installHistoryRoot(t, metadata, "stale")
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	first := testPublicationRecord("operation-first", "", root, now)
	if _, err := metadata.preparePublication(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, first.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	stale := testPublicationRecord("operation-stale-first-head", "", root, now)
	if _, err := metadata.preparePublication(ctx, stale); err != nil {
		t.Fatal(err)
	}
	got, err := metadata.acceptPublication(ctx, stale.ID, now.Add(2*time.Minute))
	if err != nil || got.State != OperationStale || got.ObservedTip != first.CommitID {
		t.Fatalf("stale first-head result = %+v, %v", got, err)
	}
	if _, err := metadata.commit(ctx, stale.CommitID); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("stale candidate commit lookup error = %v, want ErrCommitNotFound", err)
	}

	missingBase := testPublicationRecord("operation-missing-base", "missing-parent", root, now)
	missingBase.Input.Branch = "feature"
	if _, err := metadata.preparePublication(ctx, missingBase); err != nil {
		t.Fatal(err)
	}
	got, err = metadata.acceptPublication(ctx, missingBase.ID, now.Add(2*time.Minute))
	if err != nil || got.State != OperationStale || got.ObservedTip != "" {
		t.Fatalf("missing nonempty base result = %+v, %v; want stale with absent tip", got, err)
	}
	if head, err := metadata.head(ctx, "feature"); err != nil || head != "" {
		t.Fatalf("feature head after stale publication = %q, %v; want absent", head, err)
	}
}

func TestHistoryAcceptanceRollbackAfterCommitInsertFailure(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	root := installHistoryRoot(t, metadata, "trigger-rollback")
	now := time.Date(2026, 10, 26, 14, 30, 0, 0, time.UTC)
	record := testPublicationRecord("operation-trigger-rollback", "", root, now)
	if _, err := metadata.preparePublication(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(ctx, `CREATE TRIGGER reject_branch_insert BEFORE INSERT ON branches BEGIN SELECT RAISE(ABORT, 'test trigger'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, record.ID, now.Add(time.Minute)); err == nil {
		t.Fatal("acceptPublication() succeeded despite branch trigger")
	}
	if _, err := metadata.commit(ctx, record.CommitID); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("candidate commit after rolled-back acceptance = %v, want ErrCommitNotFound", err)
	}
	if head, err := metadata.head(ctx, "main"); err != nil || head != "" {
		t.Fatalf("branch after rolled-back acceptance = %q, %v; want absent", head, err)
	}
	if operation, err := metadata.publication(ctx, record.ID); err != nil || operation.State != OperationPending {
		t.Fatalf("operation after rolled-back acceptance = %+v, %v; want pending", operation, err)
	}
	if _, err := metadata.db.ExecContext(ctx, `DROP TRIGGER reject_branch_insert`); err != nil {
		t.Fatal(err)
	}
	accepted, err := metadata.acceptPublication(ctx, record.ID, now.Add(2*time.Minute))
	if err != nil || accepted.State != OperationAccepted || accepted.CommitID != record.CommitID {
		t.Fatalf("retry with same candidate ID = %+v, %v", accepted, err)
	}
	if head, err := metadata.head(ctx, "main"); err != nil || head != record.CommitID {
		t.Fatalf("branch after successful retry = %q, %v; want %q", head, err, record.CommitID)
	}
}

func TestHistoryBranchIgnoreRollsBackUnacceptedCommit(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	root := installHistoryRoot(t, metadata, "trigger-ignore")
	now := time.Date(2026, 10, 26, 14, 35, 0, 0, time.UTC)
	record := testPublicationRecord("operation-trigger-ignore", "", root, now)
	if _, err := metadata.preparePublication(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(ctx, `CREATE TRIGGER ignore_branch_insert BEFORE INSERT ON branches BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, record.ID, now.Add(time.Minute)); err == nil {
		t.Fatal("acceptPublication() succeeded despite ignored branch insert")
	}
	if _, err := metadata.commit(ctx, record.CommitID); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("candidate commit after ignored branch insert = %v, want ErrCommitNotFound", err)
	}
	if head, err := metadata.head(ctx, "main"); err != nil || head != "" {
		t.Fatalf("branch after ignored insert = %q, %v; want absent", head, err)
	}
	if operation, err := metadata.publication(ctx, record.ID); err != nil || operation.State != OperationPending {
		t.Fatalf("operation after ignored insert = %+v, %v; want pending", operation, err)
	}
	if _, err := metadata.db.ExecContext(ctx, `DROP TRIGGER ignore_branch_insert`); err != nil {
		t.Fatal(err)
	}
	accepted, err := metadata.acceptPublication(ctx, record.ID, now.Add(2*time.Minute))
	if err != nil || accepted.State != OperationAccepted || accepted.CommitID != record.CommitID {
		t.Fatalf("retry with same candidate ID = %+v, %v", accepted, err)
	}
}

func TestHistoryConcurrentAcceptSerializesAcrossMetadataHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-history.sqlite")
	firstMetadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstMetadata.Close() })
	ctx := context.Background()
	if _, err := firstMetadata.bindProject(ctx, testRepositoryNamespace, "project-test-id"); err != nil {
		t.Fatal(err)
	}
	root := installHistoryRoot(t, firstMetadata, "concurrent")
	now := time.Date(2026, 10, 26, 14, 45, 0, 0, time.UTC)
	initial := testPublicationRecord("operation-concurrent-initial", "", root, now)
	if _, err := firstMetadata.preparePublication(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := firstMetadata.acceptPublication(ctx, initial.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	left := testPublicationRecord("operation-concurrent-left", initial.CommitID, root, now.Add(2*time.Minute))
	right := testPublicationRecord("operation-concurrent-right", initial.CommitID, root, now.Add(2*time.Minute))
	if _, err := firstMetadata.preparePublication(ctx, left); err != nil {
		t.Fatal(err)
	}
	if _, err := firstMetadata.preparePublication(ctx, right); err != nil {
		t.Fatal(err)
	}
	secondMetadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondMetadata.Close() })

	type result struct {
		record publicationRecord
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, item := range []struct {
		metadata *Metadata
		id       string
	}{
		{firstMetadata, left.ID},
		{secondMetadata, right.ID},
	} {
		workers.Add(1)
		go func(metadata *Metadata, id string) {
			defer workers.Done()
			<-start
			operation, err := metadata.acceptPublication(ctx, id, now.Add(3*time.Minute))
			results <- result{record: operation, err: err}
		}(item.metadata, item.id)
	}
	close(start)
	workers.Wait()
	close(results)

	acceptedCount, staleCount := 0, 0
	var winner publicationRecord
	var stale publicationRecord
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent accept error: %v", result.err)
		}
		switch result.record.State {
		case OperationAccepted:
			acceptedCount++
			winner = result.record
		case OperationStale:
			staleCount++
			stale = result.record
		default:
			t.Fatalf("concurrent accept returned unexpected state: %+v", result.record)
		}
	}
	if acceptedCount != 1 || staleCount != 1 {
		t.Fatalf("concurrent outcomes: %d accepted, %d stale; want one each", acceptedCount, staleCount)
	}
	if stale.ObservedTip != winner.CommitID {
		t.Fatalf("stale observed tip = %q, winner commit = %q", stale.ObservedTip, winner.CommitID)
	}
	if head, err := firstMetadata.head(ctx, "main"); err != nil || head != winner.CommitID {
		t.Fatalf("final branch head = %q, %v; want winning commit %q", head, err, winner.CommitID)
	}
	if _, err := firstMetadata.commit(ctx, stale.CommitID); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("stale candidate commit lookup error = %v, want ErrCommitNotFound", err)
	}
}

func TestHistoryExpirationStopReleaseAndRecovery(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	root := installHistoryRoot(t, metadata, "recovery")
	now := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)

	pending := testPublicationRecord("a-pending", "", root, now)
	if _, err := metadata.preparePublication(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if got, err := metadata.stopPublication(ctx, pending.ID, now, true); err != nil || got.State != OperationPending {
		t.Fatalf("expire-only stop before deadline = %+v, %v; want pending", got, err)
	}

	aborted := testPublicationRecord("b-aborted", "", root, now)
	if _, err := metadata.preparePublication(ctx, aborted); err != nil {
		t.Fatal(err)
	}
	if got, err := metadata.stopPublication(ctx, aborted.ID, now, false); err != nil || got.State != OperationAborted {
		t.Fatalf("stop pending publication = %+v, %v; want aborted", got, err)
	}
	if err := metadata.markPublicationReleased(ctx, aborted.ID); err != nil {
		t.Fatal(err)
	}
	if err := metadata.markPublicationReleased(ctx, aborted.ID); err != nil {
		t.Fatalf("repeat release completion: %v", err)
	}

	expired := testPublicationRecord("c-expired", "", root, now)
	expired.ExpiresAt = now.Add(time.Minute)
	if _, err := metadata.preparePublication(ctx, expired); err != nil {
		t.Fatal(err)
	}
	got, err := metadata.acceptPublication(ctx, expired.ID, expired.ExpiresAt)
	if err != nil || got.State != OperationExpired {
		t.Fatalf("accept at expiration deadline = %+v, %v; want expired", got, err)
	}
	if got, err := metadata.stopPublication(ctx, expired.ID, now, false); err != nil || got.State != OperationExpired {
		t.Fatalf("stop expired terminal operation = %+v, %v; want unchanged expired", got, err)
	}
	if err := metadata.markPublicationReleased(ctx, expired.ID); err != nil {
		t.Fatal(err)
	}

	recoverable, err := metadata.recoverablePublications(ctx, "", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].ID != pending.ID {
		t.Fatalf("recoverable after releases = %+v, want only pending operation", recoverable)
	}
	if got, err := metadata.recoverablePublications(ctx, "a-pending", 1); err != nil || len(got) != 0 {
		t.Fatalf("recoverable after cursor = %+v, %v; want empty", got, err)
	}
	if _, err := metadata.recoverablePublications(ctx, "", 0); err == nil {
		t.Fatal("recoverable with zero limit succeeded")
	}
}

func TestCaptureReservationRetryFinishAndAccept(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)
	record := testCaptureRecord("capture-retry", "", now)
	prepared, err := metadata.prepareCapture(ctx, record)
	if err != nil || prepared.State != OperationCapturing || prepared.CommitID != record.CommitID {
		t.Fatalf("prepareCapture() = %+v, %v", prepared, err)
	}
	proposal := record
	proposal.CommitID = "ignored-new-proposal"
	prepared, err = metadata.prepareCapture(ctx, proposal)
	if err != nil || prepared.CommitID != record.CommitID {
		t.Fatalf("prepareCapture retry = %+v, %v; want original candidate", prepared, err)
	}
	changed := record
	changed.CaptureInput = append([]byte(nil), record.CaptureInput...)
	changed.CaptureInput = append(changed.CaptureInput, ' ')
	if _, err := metadata.prepareCapture(ctx, changed); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("prepareCapture changed bytes error = %v, want conflict", err)
	}
	changed = record
	changed.Input.Message += " changed"
	if _, err := metadata.prepareCapture(ctx, changed); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("prepareCapture changed input error = %v, want conflict", err)
	}
	if got, err := metadata.acceptPublication(ctx, record.ID, now.Add(time.Minute)); err != nil || got.State != OperationCapturing {
		t.Fatalf("accept while capturing = %+v, %v; want unchanged capturing", got, err)
	}

	tree := installHistoryRoot(t, metadata, "capture-finish")
	finished, err := metadata.finishCapture(ctx, record.ID, tree)
	if err != nil || finished.State != OperationPending || finished.Input.Tree != tree || finished.CommitID != record.CommitID || !bytes.Equal(finished.CaptureInput, record.CaptureInput) {
		t.Fatalf("finishCapture() = %+v, %v", finished, err)
	}
	proposal.Input.Tree = tree // retry ignores the tree populated by finishCapture.
	proposal.CommitID = "another-ignored-candidate"
	if got, err := metadata.prepareCapture(ctx, proposal); err != nil || got.State != OperationPending || got.CommitID != record.CommitID || got.Input.Tree != tree {
		t.Fatalf("prepareCapture retry after finish = %+v, %v", got, err)
	}
	if got, err := metadata.finishCapture(ctx, record.ID, tree); err != nil || got.State != OperationPending {
		t.Fatalf("finishCapture idempotent retry = %+v, %v", got, err)
	}
	if _, err := metadata.finishCapture(ctx, record.ID, TreeRef{Format: tree.Format, ID: "other-root"}); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("finishCapture with different tree error = %v, want conflict", err)
	}
	accepted, err := metadata.acceptPublication(ctx, record.ID, now.Add(2*time.Minute))
	if err != nil || accepted.State != OperationAccepted || accepted.CommitID != record.CommitID {
		t.Fatalf("accept finished capture = %+v, %v", accepted, err)
	}
	if head, err := metadata.head(ctx, "main"); err != nil || head != record.CommitID {
		t.Fatalf("captured publication head = %q, %v", head, err)
	}
}

func TestCaptureMergeParentSurvivesFinishAndAcceptance(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	baseTree := installHistoryRoot(t, metadata, "capture-merge-base")
	base := testPublicationRecord("capture-merge-base", "", baseTree, now)
	if _, err := metadata.preparePublication(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, base.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	secondParent := Commit{ID: "capture-merge-second", Tree: baseTree, Author: "tester", Message: "other line", CreatedAt: now}
	tx, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := putCommitTx(ctx, tx, secondParent); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	record := testCaptureRecord("capture-merge", base.CommitID, now.Add(2*time.Minute))
	record.Input.MergeParent = secondParent.ID
	if _, err := metadata.prepareCapture(ctx, record); err != nil {
		t.Fatal(err)
	}
	proposal := record
	proposal.CommitID = "ignored-capture-merge-candidate"
	if got, err := metadata.prepareCapture(ctx, proposal); err != nil || got.CommitID != record.CommitID || got.Input.MergeParent != secondParent.ID {
		t.Fatalf("capture merge retry = %+v, %v", got, err)
	}
	changed := record
	changed.Input.MergeParent = "different-merge-parent"
	if _, err := metadata.prepareCapture(ctx, changed); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("capture retry with changed merge parent error = %v, want conflict", err)
	}
	tree := installHistoryRoot(t, metadata, "capture-merge-result")
	if _, err := metadata.finishCapture(ctx, record.ID, tree); err != nil {
		t.Fatal(err)
	}
	proposal.Input.Tree = tree
	if got, err := metadata.prepareCapture(ctx, proposal); err != nil || got.State != OperationPending || got.Input.MergeParent != secondParent.ID {
		t.Fatalf("capture merge retry after finish = %+v, %v", got, err)
	}
	accepted, err := metadata.acceptPublication(ctx, record.ID, now.Add(4*time.Minute))
	if err != nil || accepted.State != OperationAccepted {
		t.Fatalf("accept captured merge = %+v, %v", accepted, err)
	}
	commit, err := metadata.commit(ctx, record.CommitID)
	wantParents := []string{base.CommitID, secondParent.ID}
	if err != nil || !reflect.DeepEqual(commit.Parents, wantParents) {
		t.Fatalf("captured merge parents = %v, %v; want %v", commit.Parents, err, wantParents)
	}
}

func TestCaptureCheckStopAndRecovery(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 15, 30, 0, 0, time.UTC)
	staleBase := testPublicationRecord("capture-stale-head-seed", "", installHistoryRoot(t, metadata, "seed"), now)
	if _, err := metadata.preparePublication(ctx, staleBase); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.acceptPublication(ctx, staleBase.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stale := testCaptureRecord("capture-stale-head", "", now)
	if _, err := metadata.prepareCapture(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if got, err := metadata.checkCapture(ctx, stale.ID, now.Add(2*time.Minute)); err != nil || got.State != OperationStale || got.ObservedTip != staleBase.CommitID {
		t.Fatalf("check stale capture = %+v, %v", got, err)
	}
	if _, err := metadata.finishCapture(ctx, stale.ID, TreeRef{Format: "format-v1", ID: "not-present"}); err != nil {
		t.Fatalf("terminal stale finish should be unchanged: %v", err)
	}

	expired := testCaptureRecord("capture-expired", "", now)
	expired.ExpiresAt = now.Add(time.Minute)
	if _, err := metadata.prepareCapture(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if got, err := metadata.checkCapture(ctx, expired.ID, expired.ExpiresAt); err != nil || got.State != OperationExpired {
		t.Fatalf("check expired capture = %+v, %v", got, err)
	}
	if err := metadata.markPublicationReleased(ctx, expired.ID); err != nil {
		t.Fatal(err)
	}

	aborted := testCaptureRecord("capture-aborted", "", now)
	if _, err := metadata.prepareCapture(ctx, aborted); err != nil {
		t.Fatal(err)
	}
	if got, err := metadata.stopPublication(ctx, aborted.ID, now.Add(time.Minute), false); err != nil || got.State != OperationAborted {
		t.Fatalf("stop capturing operation = %+v, %v", got, err)
	}
	if err := metadata.markPublicationReleased(ctx, aborted.ID); err != nil {
		t.Fatal(err)
	}

	active := testCaptureRecord("capture-recoverable", "", now)
	if _, err := metadata.prepareCapture(ctx, active); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.stopPublication(ctx, active.ID, now, true); err != nil {
		t.Fatal(err)
	}
	recoverable, err := metadata.recoverablePublications(ctx, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, operation := range recoverable {
		if operation.ID == active.ID && operation.State == OperationCapturing {
			found = true
		}
	}
	if !found {
		t.Fatalf("recoverable operations omit active capture: %+v", recoverable)
	}
	if err := metadata.markPublicationReleased(ctx, active.ID); err == nil {
		t.Fatal("markPublicationReleased accepted an active capture")
	}
}

func TestFileFactsAreImmutableAndAtomic(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	first := kopia.FileFact{Ref: "object-a", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Size: 12}
	second := kopia.FileFact{Ref: "object-b", SHA256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", Size: 23}
	if err := metadata.putFileFacts(ctx, testRepositoryNamespace, []kopia.FileFact{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.putFileFacts(ctx, testRepositoryNamespace, []kopia.FileFact{first, second}); err != nil {
		t.Fatalf("idempotent file fact write: %v", err)
	}
	if got, err := metadata.fileFact(ctx, testRepositoryNamespace, first.Ref); err != nil || got != first {
		t.Fatalf("fileFact() = %+v, %v; want %+v", got, err, first)
	}
	if _, err := metadata.fileFact(ctx, "other-repository", first.Ref); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("fileFact other namespace = %v, want not found", err)
	}
	changed := first
	changed.Size++
	newFact := kopia.FileFact{Ref: "object-new", SHA256: first.SHA256, Size: 1}
	if err := metadata.putFileFacts(ctx, testRepositoryNamespace, []kopia.FileFact{newFact, changed}); !errors.Is(err, ErrMetadataConflict) {
		t.Fatalf("conflicting file fact batch error = %v, want conflict", err)
	}
	if _, err := metadata.fileFact(ctx, testRepositoryNamespace, newFact.Ref); !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("partially committed file fact = %v, want not found", err)
	}
	for _, invalid := range []kopia.FileFact{
		{Ref: "", SHA256: first.SHA256, Size: 1},
		{Ref: "bad-sha", SHA256: "ABC", Size: 1},
		{Ref: "negative-size", SHA256: first.SHA256, Size: -1},
	} {
		if err := metadata.putFileFacts(ctx, testRepositoryNamespace, []kopia.FileFact{invalid}); err == nil {
			t.Fatalf("putFileFacts accepted invalid fact %+v", invalid)
		}
	}
}

func TestHistoryVersionOneMigrationPreservesArchives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-one.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE directory_manifests (namespace TEXT NOT NULL, format TEXT NOT NULL, object_id TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY (namespace, format, object_id)) WITHOUT ROWID`,
		`CREATE TABLE archive_roots (namespace TEXT NOT NULL, format TEXT NOT NULL, archive_id TEXT NOT NULL, descriptor BLOB NOT NULL, PRIMARY KEY (namespace, format, archive_id)) WITHOUT ROWID`,
		`INSERT INTO directory_manifests VALUES ('repo', 'format-v1', 'dir-id', X'00FF')`,
		`INSERT INTO archive_roots VALUES ('repo', 'format-v1', 'root-id', X'7B7D')`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create version-1 fixture: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatalf("migrate version-1 metadata: %v", err)
	}
	defer metadata.Close()
	if got, err := metadata.Directory(context.Background(), "repo", "format-v1", "dir-id"); err != nil || !reflect.DeepEqual(got, []byte{0, 0xff}) {
		t.Fatalf("migrated directory = %v, %v", got, err)
	}
	if got, err := metadata.Root(context.Background(), "repo", "format-v1", "root-id"); err != nil || !reflect.DeepEqual(got, []byte("{}")) {
		t.Fatalf("migrated root = %q, %v", got, err)
	}
	var version int
	if err := metadata.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 5 {
		t.Fatalf("schema version after migration = %d, want 5", version)
	}
	if _, err := metadata.bindProject(context.Background(), "repo", "project-after-migration"); err != nil {
		t.Fatalf("bind after migration: %v", err)
	}
}

func TestHistoryVersionTwoMigrationPreservesLedgerAndArchives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-two.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)
	root := TreeRef{Format: "format-v1", ID: "root-v2"}
	inputBytes, err := json.Marshal(publicationInput{Branch: "main", Base: "", Tree: root, Author: "tester", Message: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, err := json.Marshal(Commit{ID: "commit-v2", Tree: root, Author: "tester", Message: "accepted", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE directory_manifests (namespace TEXT NOT NULL, format TEXT NOT NULL, object_id TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY (namespace, format, object_id)) WITHOUT ROWID`,
		`CREATE TABLE archive_roots (namespace TEXT NOT NULL, format TEXT NOT NULL, archive_id TEXT NOT NULL, descriptor BLOB NOT NULL, PRIMARY KEY (namespace, format, archive_id)) WITHOUT ROWID`,
		`CREATE TABLE project_binding (singleton INTEGER PRIMARY KEY CHECK (singleton = 1), project_id TEXT NOT NULL, namespace TEXT NOT NULL) WITHOUT ROWID`,
		`CREATE TABLE publication_ops (id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, sealed_input BLOB NOT NULL, candidate_commit_id TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL, state TEXT NOT NULL CHECK (state IN ('pending','accepted','stale','aborted','expired')), observed_tip TEXT NOT NULL, release_done INTEGER NOT NULL CHECK (release_done IN (0,1))) WITHOUT ROWID`,
		`CREATE TABLE commits (id TEXT PRIMARY KEY, data BLOB NOT NULL) WITHOUT ROWID`,
		`CREATE TABLE branches (name TEXT PRIMARY KEY, tip TEXT NOT NULL) WITHOUT ROWID`,
		`INSERT INTO directory_manifests VALUES ('repo', 'format-v1', 'dir-v2', X'00FF')`,
		`INSERT INTO archive_roots VALUES ('repo', 'format-v1', 'root-v2', X'7B7D')`,
		`INSERT INTO project_binding VALUES (1, 'project-v2', 'repo')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create version-2 fixture: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO publication_ops VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		"op-pending-v2", "fingerprint-pending", inputBytes, "candidate-pending", formatMetadataTime(now), formatMetadataTime(now.Add(time.Hour)), "pending", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO publication_ops VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		"op-accepted-v2", "fingerprint-accepted", inputBytes, "commit-v2", formatMetadataTime(now), formatMetadataTime(now.Add(time.Hour)), "accepted", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO commits VALUES ('commit-v2', ?)`, commitBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO branches VALUES ('main', 'commit-v2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatalf("migrate version-2 metadata: %v", err)
	}
	defer metadata.Close()
	if got, err := metadata.Directory(context.Background(), "repo", "format-v1", "dir-v2"); err != nil || !bytes.Equal(got, []byte{0, 0xff}) {
		t.Fatalf("migrated directory = %v, %v", got, err)
	}
	if got, err := metadata.Root(context.Background(), "repo", "format-v1", "root-v2"); err != nil || string(got) != "{}" {
		t.Fatalf("migrated root = %q, %v", got, err)
	}
	for _, state := range []OperationState{OperationPending, OperationAccepted} {
		id := "op-pending-v2"
		if state == OperationAccepted {
			id = "op-accepted-v2"
		}
		got, err := metadata.publication(context.Background(), id)
		if err != nil || got.State != state || len(got.CaptureInput) != 0 {
			t.Fatalf("migrated operation %s = %+v, %v; want %s with empty capture input", id, got, err, state)
		}
	}
	if head, err := metadata.head(context.Background(), "main"); err != nil || head != "commit-v2" {
		t.Fatalf("migrated branch head = %q, %v", head, err)
	}
	var version int
	if err := metadata.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("migrated user_version = %d, %v; want 5", version, err)
	}
}
