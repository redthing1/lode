package kopia_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/snapshot"

	lodekopia "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/project"
)

type publicationClock struct{ now time.Time }

func (c *publicationClock) Now() time.Time { return c.now }

type publicationRootsFault struct {
	native          *lodekopia.Roots
	failProtect     bool
	protectThenFail bool
	failRelease     bool
	releaseThenFail bool
	failure         error
}

func (r *publicationRootsFault) Namespace() string {
	return r.native.Namespace()
}

func (r *publicationRootsFault) Validate(ctx context.Context, archive lodekopia.Archive) error {
	return r.native.Validate(ctx, archive)
}

func (r *publicationRootsFault) Protect(ctx context.Context, key string, archive lodekopia.Archive) error {
	if r.failProtect {
		if r.protectThenFail {
			if err := r.native.Protect(ctx, key, archive); err != nil {
				return err
			}
		}
		return r.failure
	}
	return r.native.Protect(ctx, key, archive)
}

func (r *publicationRootsFault) Release(ctx context.Context, key string) error {
	if r.failRelease {
		if r.releaseThenFail {
			if err := r.native.Release(ctx, key); err != nil {
				return err
			}
		}
		return r.failure
	}
	return r.native.Release(ctx, key)
}

func openPublicationService(t *testing.T, f *archiveFixture, databasePath string, roots project.ContentRoots, clock *publicationClock, ttl time.Duration) (*project.Metadata, *project.Service) {
	t.Helper()
	metadata, err := project.OpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	service, err := project.NewService(f.ctx, metadata, roots, project.Options{OperationTTL: ttl, Now: clock.Now})
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
		if err := metadata.Close(); err != nil {
			t.Error(err)
		}
	})
	return metadata, service
}

func publicationRequest(operation, branch, base, message string, archive lodekopia.Archive) project.PublishRequest {
	return project.PublishRequest{OperationID: operation, Branch: branch, Base: base, Tree: archive, Author: "integration tester", Message: message}
}

func TestPublicationIntegrationAcceptanceRetryParentsAndStaleRelease(t *testing.T) {
	f := newArchiveFixture(t)
	baseBytes := archivePayload(64<<10, 201)
	nextBytes := archivePayload(64<<10, 202)
	baseRoot, baseArchive := makeRootArchive(t, f, "base.bin", baseBytes)
	_, nextArchive := makeRootArchive(t, f, "next.bin", nextBytes)
	clock := &publicationClock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	databasePath := filepath.Join(f.dir, "publication.sqlite")
	nativeRoots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	metadata, service := openPublicationService(t, f, databasePath, nativeRoots, clock, time.Hour)
	firstRequest := publicationRequest("publish-initial", "main", "", "initial state", baseArchive)
	first, err := service.Publish(f.ctx, firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ID) != 32 || len(first.Parents) != 0 || first.Tree.ID == "" {
		t.Fatalf("initial commit = %+v", first)
	}
	if head, err := service.Head(f.ctx, "main"); err != nil || head != first.ID {
		t.Fatalf("initial branch head = %q, %v; want %q", head, err, first.ID)
	}
	storedCommit, err := service.Commit(f.ctx, first.ID)
	if err != nil || storedCommit.Message != firstRequest.Message || storedCommit.Tree != first.Tree {
		t.Fatalf("stored initial commit = %+v, %v", storedCommit, err)
	}
	if _, err := metadata.Root(f.ctx, nativeRoots.Namespace(), first.Tree.Format, first.Tree.ID); err != nil {
		t.Fatalf("accepted archive root missing from SQLite: %v", err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+firstRequest.OperationID, baseArchive); err != nil {
		t.Fatalf("accepted native root unavailable: %v", err)
	}
	if err := service.Abort(f.ctx, firstRequest.OperationID); !errors.Is(err, project.ErrAlreadyAccepted) {
		t.Fatalf("Abort accepted operation = %v, want ErrAlreadyAccepted", err)
	}

	changedMessage := firstRequest
	changedMessage.Message += " changed"
	if _, err := service.Publish(f.ctx, changedMessage); !errors.Is(err, project.ErrOperationConflict) {
		t.Fatalf("retry with changed message = %v, want operation conflict", err)
	}
	changedTree := firstRequest
	changedTree.Tree = nextArchive
	if _, err := service.Publish(f.ctx, changedTree); !errors.Is(err, project.ErrOperationConflict) {
		t.Fatalf("retry with changed tree = %v, want operation conflict", err)
	}
	changedBase := firstRequest
	changedBase.Base = "00000000000000000000000000000000"
	if _, err := service.Publish(f.ctx, changedBase); !errors.Is(err, project.ErrOperationConflict) {
		t.Fatalf("retry with changed base = %v, want operation conflict", err)
	}

	secondRequest := publicationRequest("publish-second", "main", first.ID, "second state", nextArchive)
	second, err := service.Publish(f.ctx, secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Parents) != 1 || second.Parents[0] != first.ID {
		t.Fatalf("second commit parents = %v, want [%s]", second.Parents, first.ID)
	}
	if head, err := service.Head(f.ctx, "main"); err != nil || head != second.ID {
		t.Fatalf("second branch head = %q, %v; want %q", head, err, second.ID)
	}

	staleEmpty := publicationRequest("stale-empty", "feature", first.ID, "stale empty branch", baseArchive)
	if _, err := service.Publish(f.ctx, staleEmpty); !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("nonempty base against empty branch = %v, want stale head", err)
	}
	if operation, err := service.Operation(f.ctx, staleEmpty.OperationID); err != nil || operation.State != project.OperationStale || operation.ObservedTip != "" {
		t.Fatalf("empty-tip stale operation = %+v, %v", operation, err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+staleEmpty.OperationID, baseArchive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("stale empty-tip protection was not released: %v", err)
	}

	staleNonempty := publicationRequest("stale-nonempty", "main", "", "stale nonempty branch", baseArchive)
	if _, err := service.Publish(f.ctx, staleNonempty); !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("empty base against nonempty branch = %v, want stale head", err)
	}
	if operation, err := service.Operation(f.ctx, staleNonempty.OperationID); err != nil || operation.State != project.OperationStale || operation.ObservedTip != second.ID {
		t.Fatalf("nonempty-tip stale operation = %+v, %v", operation, err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+staleNonempty.OperationID, baseArchive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("stale nonempty-tip protection was not released: %v", err)
	}
	assertProtectedFile(t, f.ctx, f.rep, baseRoot, "base.bin", baseBytes)
}

func TestPublicationIntegrationRetryAcrossLeaseAndServiceReopen(t *testing.T) {
	f := newArchiveFixture(t)
	payload := archivePayload(96<<10, 203)
	_, archive := makeRootArchive(t, f, "stable.bin", payload)
	databasePath := filepath.Join(f.dir, "retry.sqlite")
	clock := &publicationClock{now: time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC)}
	roots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	metadata, service := openPublicationService(t, f, databasePath, roots, clock, time.Minute)
	request := publicationRequest("accepted-retry", "main", "", "stable result", archive)
	first, err := service.Publish(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if second, err := service.Publish(f.ctx, request); err != nil || second.ID != first.ID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("accepted retry after lease = %+v, %v; want stable %+v", second, err, first)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err = project.OpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	reopened := f.reader("publication-retry-reopened").(repo.DirectRepository)
	defer reopened.Close(context.WithoutCancel(f.ctx))
	reopenedRoots, err := lodekopia.NewRoots(reopened)
	if err != nil {
		t.Fatal(err)
	}
	reopenedService, err := project.NewService(f.ctx, metadata, reopenedRoots, project.Options{OperationTTL: time.Minute, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen project service for accepted retry: %v", err)
	}
	defer reopenedService.Close()
	got, err := reopenedService.Publish(f.ctx, request)
	if err != nil || got.ID != first.ID {
		t.Fatalf("accepted retry after reopen = %+v, %v; want commit %s", got, err, first.ID)
	}
	if head, err := reopenedService.Head(f.ctx, "main"); err != nil || head != first.ID {
		t.Fatalf("reopened head = %q, %v", head, err)
	}
}

func TestPublicationIntegrationServiceLockAndRepositoryBinding(t *testing.T) {
	f := newArchiveFixture(t)
	clock := &publicationClock{now: time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)}
	databasePath := filepath.Join(f.dir, "lock.sqlite")
	roots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	metadata, service := openPublicationService(t, f, databasePath, roots, clock, time.Hour)
	if _, err := project.NewService(f.ctx, metadata, roots, project.Options{Now: clock.Now}); !errors.Is(err, project.ErrProjectBusy) {
		t.Fatalf("second service instance = %v, want ErrProjectBusy", err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err = project.OpenMetadata(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	other := newArchiveFixture(t)
	otherRoots, err := lodekopia.NewRoots(other.rep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := project.NewService(other.ctx, metadata, otherRoots, project.Options{Now: clock.Now}); !errors.Is(err, project.ErrRepositoryMismatch) {
		t.Fatalf("service with another Kopia repository = %v, want ErrRepositoryMismatch", err)
	}
}

func TestPublicationIntegrationInvalidArchiveDoesNotPoisonMetadata(t *testing.T) {
	f := newArchiveFixture(t)
	payload := archivePayload(16<<10, 204)
	root, archive := makeRootArchive(t, f, "valid.bin", payload)
	forged := cloneArchive(archive)
	for id, raw := range forged.Directories {
		var directory snapshot.DirManifest
		if err := json.Unmarshal(raw, &directory); err != nil {
			t.Fatal(err)
		}
		directory.Entries[0].Permissions ^= 0o111
		encoded, err := json.Marshal(&directory)
		if err != nil {
			t.Fatal(err)
		}
		forged.Directories[id] = encoded
		break
	}
	clock := &publicationClock{now: time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)}
	roots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	_, service := openPublicationService(t, f, filepath.Join(f.dir, "no-poison.sqlite"), roots, clock, time.Hour)
	request := publicationRequest("same-after-invalid", "main", "", "genuine", forged)
	if _, err := service.Publish(f.ctx, request); !errors.Is(err, lodekopia.ErrRootConflict) {
		t.Fatalf("publish forged archive = %v, want native archive conflict", err)
	}
	request.Tree = archive
	commit, err := service.Publish(f.ctx, request)
	if err != nil {
		t.Fatalf("publish genuine archive after failed validation: %v", err)
	}
	if commit.ID == "" {
		t.Fatal("genuine publication produced empty commit ID")
	}
	assertProtectedFile(t, f.ctx, f.rep, root, "valid.bin", payload)
}

func TestPublicationIntegrationPendingExpiryRecoveryAndAbort(t *testing.T) {
	f := newArchiveFixture(t)
	payload := archivePayload(32<<10, 205)
	root, archive := makeRootArchive(t, f, "pending.bin", payload)
	clock := &publicationClock{now: time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)}
	databasePath := filepath.Join(f.dir, "pending.sqlite")
	nativeRoots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	forwardThenFail := &publicationRootsFault{native: nativeRoots, failProtect: true, protectThenFail: true, failure: errors.New("lost Protect reply")}
	_, service := openPublicationService(t, f, databasePath, forwardThenFail, clock, time.Minute)
	request := publicationRequest("expire-pending", "main", "", "pending", archive)
	if _, err := service.Publish(f.ctx, request); err == nil {
		t.Fatal("publication with lost Protect reply unexpectedly succeeded")
	}
	operation, err := service.Operation(f.ctx, request.OperationID)
	if err != nil || operation.State != project.OperationPending {
		t.Fatalf("pending operation after lost reply = %+v, %v", operation, err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+request.OperationID, archive); err != nil {
		t.Fatalf("wrapper did not delegate native Protect before failure: %v", err)
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if err := service.Recover(f.ctx); err != nil {
		t.Fatalf("Recover expired pending publication: %v", err)
	}
	operation, err = service.Operation(f.ctx, request.OperationID)
	if err != nil || operation.State != project.OperationExpired {
		t.Fatalf("expired recovered operation = %+v, %v", operation, err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+request.OperationID, archive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("Recover did not release expired protection: %v", err)
	}
	if head, err := service.Head(f.ctx, "main"); err != nil || head != "" {
		t.Fatalf("head after expiration = %q, %v", head, err)
	}
	assertProtectedFile(t, f.ctx, f.rep, root, "pending.bin", payload)

	noDelegateFailure := &publicationRootsFault{native: nativeRoots, failProtect: true, failure: errors.New("protect unavailable")}
	_, abortService := openPublicationService(t, f, filepath.Join(f.dir, "abort.sqlite"), noDelegateFailure, clock, time.Hour)
	abortRequest := publicationRequest("abort-pending", "main", "", "abort me", archive)
	if _, err := abortService.Publish(f.ctx, abortRequest); err == nil {
		t.Fatal("publication with Protect failure unexpectedly succeeded")
	}
	if err := abortService.Abort(f.ctx, abortRequest.OperationID); err != nil {
		t.Fatalf("Abort pending operation: %v", err)
	}
	if err := abortService.Abort(f.ctx, abortRequest.OperationID); err != nil {
		t.Fatalf("idempotent Abort: %v", err)
	}
	operation, err = abortService.Operation(f.ctx, abortRequest.OperationID)
	if err != nil || operation.State != project.OperationAborted {
		t.Fatalf("aborted operation = %+v, %v", operation, err)
	}
}

func TestPublicationIntegrationRetriesTerminalReleaseAfterLostReply(t *testing.T) {
	f := newArchiveFixture(t)
	payload := archivePayload(48<<10, 206)
	_, archive := makeRootArchive(t, f, "release.bin", payload)
	clock := &publicationClock{now: time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC)}
	nativeRoots, err := lodekopia.NewRoots(f.rep)
	if err != nil {
		t.Fatal(err)
	}
	releaseFailure := errors.New("native release reply lost")
	rootBoundary := &publicationRootsFault{native: nativeRoots, failure: releaseFailure}
	_, service := openPublicationService(t, f, filepath.Join(f.dir, "release-recovery.sqlite"), rootBoundary, clock, time.Hour)
	acceptedRequest := publicationRequest("release-base", "main", "", "accepted base", archive)
	accepted, err := service.Publish(f.ctx, acceptedRequest)
	if err != nil {
		t.Fatal(err)
	}

	// Native deletion succeeds, but the adapter's reply is lost before SQLite
	// can mark release_done. Recovery must safely retry the idempotent delete.
	rootBoundary.failRelease, rootBoundary.releaseThenFail = true, true
	afterNativeDelete := publicationRequest("release-lost-after-delete", "main", "", "stale operation", archive)
	if _, err := service.Publish(f.ctx, afterNativeDelete); !errors.Is(err, releaseFailure) {
		t.Fatalf("stale publish after native release reply loss = %v, want injected failure", err)
	}
	operation, err := service.Operation(f.ctx, afterNativeDelete.OperationID)
	if err != nil || operation.State != project.OperationStale || operation.ObservedTip != accepted.ID {
		t.Fatalf("terminal operation after lost release reply = %+v, %v", operation, err)
	}
	if head, err := service.Head(f.ctx, "main"); err != nil || head != accepted.ID {
		t.Fatalf("head after stale publication = %q, %v", head, err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+afterNativeDelete.OperationID, archive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("native root after successful delete/lost reply = %v, want absent", err)
	}
	rootBoundary.failRelease, rootBoundary.releaseThenFail = false, false
	if err := service.Recover(f.ctx); err != nil {
		t.Fatalf("Recover terminal operation after lost release reply: %v", err)
	}
	if err := service.Recover(f.ctx); err != nil {
		t.Fatalf("repeat Recover after release completion: %v", err)
	}
	if _, err := service.Resume(f.ctx, afterNativeDelete.OperationID); !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("Resume stale operation after recovery = %v, want stable stale result", err)
	}
	if head, err := service.Head(f.ctx, "main"); err != nil || head != accepted.ID {
		t.Fatalf("head after release recovery = %q, %v", head, err)
	}

	// A failure before native deletion leaves protection in place. A later
	// Recover call performs the release and records completion.
	rootBoundary.failRelease = true
	rootBoundary.releaseThenFail = false
	beforeNativeDelete := publicationRequest("release-failed-before-delete", "feature", accepted.ID, "stale feature", archive)
	if _, err := service.Publish(f.ctx, beforeNativeDelete); !errors.Is(err, releaseFailure) {
		t.Fatalf("stale publish before native release = %v, want injected failure", err)
	}
	operation, err = service.Operation(f.ctx, beforeNativeDelete.OperationID)
	if err != nil || operation.State != project.OperationStale || operation.ObservedTip != "" {
		t.Fatalf("terminal operation after pre-delete failure = %+v, %v", operation, err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+beforeNativeDelete.OperationID, archive); err != nil {
		t.Fatalf("protection disappeared before native release retry: %v", err)
	}
	if head, err := service.Head(f.ctx, "feature"); err != nil || head != "" {
		t.Fatalf("feature head after stale publication = %q, %v", head, err)
	}
	rootBoundary.failRelease = false
	if err := service.Recover(f.ctx); err != nil {
		t.Fatalf("Recover pre-delete release failure: %v", err)
	}
	if err := service.Recover(f.ctx); err != nil {
		t.Fatalf("repeat Recover after pre-delete release: %v", err)
	}
	if err := nativeRoots.Check(f.ctx, service.ID()+"/"+beforeNativeDelete.OperationID, archive); !errors.Is(err, lodekopia.ErrRootNotFound) {
		t.Fatalf("root remains after release recovery: %v", err)
	}
	if _, err := service.Resume(f.ctx, beforeNativeDelete.OperationID); !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("Resume pre-delete stale operation = %v, want stable stale result", err)
	}
	if head, err := service.Head(f.ctx, "feature"); err != nil || head != "" {
		t.Fatalf("feature head after release recovery = %q, %v", head, err)
	}
}

func cloneArchive(archive lodekopia.Archive) lodekopia.Archive {
	result := lodekopia.Archive{Format: archive.Format, Root: append([]byte(nil), archive.Root...), Directories: make(map[string][]byte, len(archive.Directories))}
	for id, raw := range archive.Directories {
		result.Directories[id] = append([]byte(nil), raw...)
	}
	return result
}
