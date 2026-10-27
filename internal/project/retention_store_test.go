package project

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func acceptedForRetention(t *testing.T, metadata *Metadata, id, base string, tree TreeRef, now time.Time, ttl time.Duration, mergeParent string) publicationRecord {
	t.Helper()
	record := testPublicationRecord(id, base, tree, now)
	record.ExpiresAt = now.Add(ttl)
	record.Input.MergeParent = mergeParent
	if _, err := metadata.preparePublication(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	acceptedAt := now.Add(time.Minute)
	if _, err := metadata.acceptPublication(context.Background(), id, acceptedAt); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRetentionSchemaVersionThreeMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retention-v3.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := metadata.bindProject(ctx, testRepositoryNamespace, "retention-project"); err != nil {
		t.Fatal(err)
	}
	root := installHistoryRoot(t, metadata, "retention-migration")
	now := time.Date(2026, 10, 26, 19, 0, 0, 0, time.UTC)
	accepted := acceptedForRetention(t, metadata, "retention-v3-accepted", "", root, now, 3*time.Hour, "")
	pending := testPublicationRecord("retention-v3-pending", accepted.CommitID, root, now.Add(4*time.Hour))
	if _, err := metadata.preparePublication(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX publication_ops_by_candidate_state`,
		`DROP TABLE payload_retention`,
		`DROP TABLE tags`,
		`DROP TABLE read_leases`,
		`DROP TABLE retention_policy`,
		`DROP TABLE compaction_state`,
		`DROP TABLE capacity_policy`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare schema-v3 database: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenMetadata(path)
	if err != nil {
		t.Fatalf("open migrated metadata: %v", err)
	}
	defer migrated.Close()
	if got, err := migrated.Directory(ctx, testRepositoryNamespace, root.Format, ""); err == nil || !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("unexpected empty object lookup: %v %v", got, err)
	}
	if got, err := migrated.Root(ctx, testRepositoryNamespace, root.Format, root.ID); err != nil || string(got) != `root descriptor: retention-migration` {
		t.Fatalf("migrated root = %q, %v", got, err)
	}
	acceptedPayload, err := migrated.payload(ctx, accepted.CommitID)
	if err != nil || !acceptedPayload.ExpiredAt.IsZero() || acceptedPayload.Released || !acceptedPayload.ProtectUntil.Equal(accepted.ExpiresAt) {
		t.Fatalf("migrated accepted payload = %+v, %v; want protected through operation expiry", acceptedPayload, err)
	}
	if got, err := migrated.publication(ctx, pending.ID); err != nil || got.State != OperationPending {
		t.Fatalf("migrated operation = %+v, %v", got, err)
	}
	if policy, err := migrated.retentionPolicy(ctx); err != nil || policy != DefaultRetentionPolicy() {
		t.Fatalf("migrated policy = %+v, %v", policy, err)
	}
	if err := migrated.expirePayloads(ctx, []string{accepted.CommitID}, accepted.ExpiresAt.Add(time.Second)); err != nil {
		t.Fatalf("expire migrated payload for mapping check: %v", err)
	}
	roots, err := migrated.expiredPayloadRoots(ctx, "", 10)
	if err != nil || !reflect.DeepEqual(roots, []expiredPayloadRoot{{CommitID: accepted.CommitID, OperationID: accepted.ID}}) {
		t.Fatalf("migrated accepted-root mapping = %+v, %v", roots, err)
	}
	var version int
	if err := migrated.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("migrated version = %d, %v; want 5", version, err)
	}
	for _, index := range []string{"publication_ops_by_candidate_state", "payload_retention_pending_release"} {
		var found int
		if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&found); err != nil || found != 1 {
			t.Fatalf("migration index %q count=%d err=%v", index, found, err)
		}
	}
}

func TestSchemaMigrationRequiresServiceOwnerLockButCurrentSchemaOpens(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	concurrent, err := OpenMetadata(local.metadata.path)
	if err != nil {
		t.Fatalf("open current schema under active service: %v", err)
	}
	if err := concurrent.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "locked-v3.sqlite")
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx = context.Background()
	if _, err := metadata.Import(ctx, "repo", "format-v1", []byte("kept root"), nil); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX publication_ops_by_candidate_state`,
		`DROP TABLE payload_retention`,
		`DROP TABLE tags`,
		`DROP TABLE read_leases`,
		`DROP TABLE retention_policy`,
		`DROP TABLE compaction_state`,
		`DROP TABLE capacity_policy`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("make v3 fixture: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	owner := flock.New(path + ".service-lock")
	locked, err := owner.TryLock()
	if err != nil || !locked {
		t.Fatalf("acquire service lock: locked=%t err=%v", locked, err)
	}
	if _, err := OpenMetadata(path); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("OpenMetadata during v3 service ownership = %v, want ErrProjectBusy", err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := check.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("schema version while migration blocked = %d, %v; want 3", version, err)
	}
	var retentionTables int
	if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='payload_retention'`).Scan(&retentionTables); err != nil || retentionTables != 0 {
		t.Fatalf("payload_retention table while migration blocked=%d err=%v", retentionTables, err)
	}
	if got, err := check.Query(`SELECT descriptor FROM archive_roots`); err != nil {
		t.Fatalf("archive rows unavailable while migration blocked: %v", err)
	} else {
		got.Close()
	}
	if err := check.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenMetadata(path)
	if err != nil {
		t.Fatalf("upgrade after lock release: %v", err)
	}
	defer migrated.Close()
	if got, err := migrated.Root(ctx, "repo", "format-v1", rootDescriptorID([]byte("kept root"))); err != nil || string(got) != "kept root" {
		t.Fatalf("archive bytes after migration = %q, %v", got, err)
	}
	if err := migrated.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("schema after lock release = %d, %v; want 5", version, err)
	}
}

func TestNewServiceRechecksSchemaAfterTakingOwnerLock(t *testing.T) {
	ctx := context.Background()
	service, metadata := openTombstoneTestService(t, "schema-owner-check")
	defer metadata.Close()
	defer service.Close()
	roots := service.roots
	var beforeID string
	if err := metadata.db.QueryRowContext(ctx, `SELECT project_id FROM project_binding WHERE singleton = 1`).Scan(&beforeID); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(ctx, `PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService(ctx, metadata, roots, Options{}); err == nil {
		t.Fatal("NewService accepted a schema changed after metadata open")
	}
	var afterID string
	if err := metadata.db.QueryRowContext(ctx, `SELECT project_id FROM project_binding WHERE singleton = 1`).Scan(&afterID); err != nil || afterID != beforeID {
		t.Fatalf("binding after rejected service = %q, %v; want unchanged %q", afterID, err, beforeID)
	}
	probe := flock.New(metadata.path + ".service-lock")
	locked, err := probe.TryLock()
	if err != nil || !locked {
		t.Fatalf("failed NewService retained owner lock: locked=%t err=%v", locked, err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewService(ctx, metadata, roots, Options{})
	if err != nil {
		t.Fatalf("NewService with current schema: %v", err)
	}
	if reopened.ID() != beforeID {
		t.Fatalf("project ID changed on valid reopen: %q != %q", reopened.ID(), beforeID)
	}
	defer reopened.Close()
}

func TestAcceptanceSetsPayloadGraceOnceForOrdinaryAndMerge(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	tree := installHistoryRoot(t, metadata, "retention-grace")
	base := acceptedForRetention(t, metadata, "grace-base", "", tree, now, 2*time.Hour, "")
	acceptedAt := now.Add(time.Minute)
	basePayload, err := metadata.payload(ctx, base.CommitID)
	wantGrace := acceptedAt.Add(2 * time.Hour)
	if err != nil || !basePayload.ProtectUntil.Equal(wantGrace) {
		t.Fatalf("ordinary commit grace = %+v, %v; want %s", basePayload, err, wantGrace)
	}
	if _, err := metadata.acceptPublication(ctx, base.ID, now.Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	basePayload, err = metadata.payload(ctx, base.CommitID)
	if err != nil || !basePayload.ProtectUntil.Equal(wantGrace) {
		t.Fatalf("accepted retry renewed grace to %+v, %v; want %s", basePayload, err, wantGrace)
	}

	otherParent := Commit{ID: "retention-merge-other", Tree: tree, Author: "tester", Message: "other", CreatedAt: now}
	tx, err := metadata.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := putCommitTx(ctx, tx, otherParent); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	merge := acceptedForRetention(t, metadata, "grace-merge", base.CommitID, tree, now.Add(9*time.Hour), time.Hour, otherParent.ID)
	mergePayload, err := metadata.payload(ctx, merge.CommitID)
	mergeAcceptedAt := now.Add(9*time.Hour + time.Minute)
	if err != nil || !mergePayload.ProtectUntil.Equal(mergeAcceptedAt.Add(time.Hour)) {
		t.Fatalf("merge commit grace = %+v, %v", mergePayload, err)
	}
}

func TestExpirePayloadsAtomicAndReleasedRootRecoveryPage(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
	tree := installHistoryRoot(t, metadata, "retention-expire")
	first := acceptedForRetention(t, metadata, "expire-first", "", tree, now, time.Hour, "")
	second := acceptedForRetention(t, metadata, "expire-second", first.CommitID, tree, now.Add(2*time.Hour), time.Hour, "")
	expireAt := now.Add(5 * time.Hour)
	if err := metadata.markPayloadReleased(ctx, first.CommitID); err == nil {
		t.Fatal("marked an unexpired payload released")
	}
	if _, err := metadata.db.Exec(`CREATE TRIGGER fail_second_expiration BEFORE UPDATE OF expired_at ON payload_retention WHEN OLD.commit_id = '` + second.CommitID + `' BEGIN SELECT RAISE(ABORT, 'test expiration rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if err := metadata.expirePayloads(ctx, []string{second.CommitID, first.CommitID}, expireAt); err == nil {
		t.Fatal("expiration batch succeeded despite trigger")
	}
	for _, id := range []string{first.CommitID, second.CommitID} {
		payload, err := metadata.payload(ctx, id)
		if err != nil || !payload.ExpiredAt.IsZero() || payload.Released {
			t.Fatalf("payload %s after rollback = %+v, %v; want retained", id, payload, err)
		}
	}
	if _, err := metadata.db.Exec(`DROP TRIGGER fail_second_expiration`); err != nil {
		t.Fatal(err)
	}
	if err := metadata.expirePayloads(ctx, []string{first.CommitID, second.CommitID}, expireAt); err != nil {
		t.Fatal(err)
	}
	if err := metadata.expirePayloads(ctx, []string{first.CommitID}, expireAt.Add(time.Hour)); err != nil {
		t.Fatalf("idempotent expiration retry: %v", err)
	}
	path := metadata.path
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err := OpenMetadata(path)
	if err != nil {
		t.Fatalf("reopen after expiration intent: %v", err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	page, err := metadata.expiredPayloadRoots(ctx, "", 1)
	if err != nil || len(page) != 1 || page[0] != (expiredPayloadRoot{CommitID: first.CommitID, OperationID: first.ID}) {
		t.Fatalf("first release page = %+v, %v", page, err)
	}
	page, err = metadata.expiredPayloadRoots(ctx, page[0].CommitID, 1)
	if err != nil || len(page) != 1 || page[0] != (expiredPayloadRoot{CommitID: second.CommitID, OperationID: second.ID}) {
		t.Fatalf("second release page = %+v, %v", page, err)
	}
	if err := metadata.markPayloadReleased(ctx, first.CommitID); err != nil {
		t.Fatal(err)
	}
	if err := metadata.markPayloadReleased(ctx, first.CommitID); err != nil {
		t.Fatalf("idempotent release marker: %v", err)
	}
	firstPayload, err := metadata.payload(ctx, first.CommitID)
	if err != nil || !firstPayload.Released || !firstPayload.ExpiredAt.Equal(expireAt) {
		t.Fatalf("released payload = %+v, %v", firstPayload, err)
	}
	page, err = metadata.expiredPayloadRoots(ctx, "", 10)
	if err != nil || !reflect.DeepEqual(page, []expiredPayloadRoot{{CommitID: second.CommitID, OperationID: second.ID}}) {
		t.Fatalf("remaining release page = %+v, %v", page, err)
	}
}

func TestExpirePayloadsRejectsAmbiguousAcceptedMappingAtomically(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 21, 30, 0, 0, time.UTC)
	tree := installHistoryRoot(t, metadata, "retention-ambiguous")
	record := acceptedForRetention(t, metadata, "ambiguous-expire", "", tree, now, time.Hour, "")
	if _, err := metadata.db.Exec(`INSERT INTO publication_ops
		(id, fingerprint, sealed_input, capture_input, candidate_commit_id, created_at, expires_at, state, observed_tip, release_done)
		SELECT 'duplicate-accepted-op', fingerprint || '-copy', sealed_input, capture_input, candidate_commit_id, created_at, expires_at, 'accepted', observed_tip, release_done
		FROM publication_ops WHERE id = ?`, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := metadata.expirePayloads(ctx, []string{record.CommitID}, now.Add(3*time.Hour)); err == nil {
		t.Fatal("expired commit with ambiguous accepted-operation mapping")
	}
	state, err := metadata.payload(ctx, record.CommitID)
	if err != nil || !state.ExpiredAt.IsZero() || state.Released {
		t.Fatalf("ambiguous mapping changed payload state = %+v, %v", state, err)
	}
}

func TestRetentionSnapshotPolicyAndMissingPayloadDistinction(t *testing.T) {
	metadata := openBoundHistoryMetadata(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	tree := installHistoryRoot(t, metadata, "retention-snapshot")
	accepted := acceptedForRetention(t, metadata, "snapshot-accepted", "", tree, now, time.Hour, "")
	pending := testPublicationRecord("snapshot-pending", accepted.CommitID, tree, now.Add(2*time.Hour))
	if _, err := metadata.preparePublication(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.Exec(`INSERT INTO tags (name, commit_id) VALUES ('keep', ?)`, accepted.CommitID); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.db.Exec(`INSERT INTO read_leases (id, commit_id, expires_at) VALUES ('read-1', ?, ?)`, accepted.CommitID, formatMetadataTime(now.Add(5*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := metadata.setRetentionPolicy(ctx, RetentionPolicy{KeepLast: 4, KeepDays: 2}); err != nil {
		t.Fatal(err)
	}
	if err := metadata.setRetentionPolicy(ctx, RetentionPolicy{KeepLast: -1, KeepDays: 0}); err == nil {
		t.Fatal("accepted invalid retention policy")
	}
	snapshot, err := metadata.retentionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Commits) != 1 || len(snapshot.Payloads) != 1 || len(snapshot.Tags) != 1 || len(snapshot.Leases) != 1 || len(snapshot.Operations) != 1 || snapshot.Operations[0].ID != pending.ID || snapshot.Policy != (RetentionPolicy{KeepLast: 4, KeepDays: 2}) {
		t.Fatalf("retention snapshot = %+v", snapshot)
	}
	if _, err := metadata.payload(ctx, "ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("payload for missing commit = %v; want ErrCommitNotFound", err)
	}
	if _, err := metadata.db.Exec(`DELETE FROM payload_retention WHERE commit_id = ?`, accepted.CommitID); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.payload(ctx, accepted.CommitID); err == nil || errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("payload row corruption error = %v; want distinct corruption error", err)
	}
}
