package project

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/engine/kopia"
)

func TestFileFactsBatchReturnsDistinctPresentRows(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	ctx := context.Background()
	want := []kopia.FileFact{
		{Ref: "object-a", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Size: 12},
		{Ref: "object-b", SHA256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", Size: 23},
	}
	if err := metadata.putFileFacts(ctx, testRepositoryNamespace, want); err != nil {
		t.Fatal(err)
	}
	got, err := metadata.fileFacts(ctx, testRepositoryNamespace, []string{"object-b", "missing", "object-a", "object-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["object-a"] != want[0] || got["object-b"] != want[1] {
		t.Fatalf("fileFacts batch = %+v; want both distinct rows", got)
	}
	if _, found := got["missing"]; found {
		t.Fatal("fileFacts batch fabricated a missing row")
	}
	other, err := metadata.fileFacts(ctx, "other-repository", []string{"object-a"})
	if err != nil || len(other) != 0 {
		t.Fatalf("fileFacts other namespace = %+v, %v; want empty", other, err)
	}
	empty, err := metadata.fileFacts(ctx, testRepositoryNamespace, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty fileFacts batch = %+v, %v", empty, err)
	}
}

func TestEntriesUsesBatchedFactsAndRejectsMissingOrMismatchedRows(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	files := map[string][]byte{"a.bin": []byte("first payload"), "b.bin": []byte("second payload")}
	commit := mergeApply(t, local.Service, "entries-facts", "main", "", fileChanges(files), files)
	allRules := []SelectionRule{{Included: true}}
	nodes, _, err := local.Service.Entries(ctx, commit.ID, allRules, "", 20)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("initial Entries() = %+v, %v; want two files", nodes, err)
	}
	storedCommit, err := local.metadata.commit(ctx, commit.ID)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := local.Service.archive(ctx, storedCommit.Tree)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := Selection(allRules)
	if err != nil {
		t.Fatal(err)
	}
	nativeEntries, _, err := local.Service.objects.List(archive, selection, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(nativeEntries) != 2 {
		t.Fatalf("native metadata entries = %+v; want two", nativeEntries)
	}
	missing := nativeEntries[0]
	if _, err := local.metadata.db.ExecContext(ctx, `DELETE FROM file_facts WHERE namespace = ? AND ref = ?`, local.Service.namespace, missing.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _, err := local.Service.Entries(ctx, commit.ID, allRules, "", 20); !errors.Is(err, ErrMetadataNotFound) || got != nil {
		t.Fatalf("Entries() with missing fact = %+v, %v; want metadata-not-found", got, err)
	}
	if _, err := local.metadata.db.ExecContext(ctx, `INSERT INTO file_facts (namespace, ref, sha256, size) VALUES (?, ?, ?, ?)`,
		local.Service.namespace, missing.Ref, sha256Hex(files[missing.Path.String()]), len(files[missing.Path.String()])); err != nil {
		t.Fatal(err)
	}
	if _, err := local.metadata.db.ExecContext(ctx, `UPDATE file_facts SET size = size + 1 WHERE namespace = ? AND ref = ?`, local.Service.namespace, missing.Ref); err != nil {
		t.Fatal(err)
	}
	if _, _, err := local.Service.Entries(ctx, commit.ID, allRules, "", 20); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("stored file size disagrees at %q", missing.Path)) {
		t.Fatalf("Entries() with mismatched fact error = %v", err)
	}
}

func TestFileFactsBatchHonorsContextAndInputBound(t *testing.T) {
	metadata, _ := openTestMetadata(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := metadata.fileFacts(canceled, testRepositoryNamespace, []string{"object-a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fileFacts batch = %v; want context.Canceled", err)
	}
	tooMany := make([]string, maxFileFactBatch+1)
	for i := range tooMany {
		tooMany[i] = "object"
	}
	if _, err := metadata.fileFacts(context.Background(), testRepositoryNamespace, tooMany); err == nil {
		t.Fatal("fileFacts accepted a batch above its configured limit")
	}
	if _, err := metadata.fileFacts(context.Background(), testRepositoryNamespace, []string{""}); err == nil {
		t.Fatal("fileFacts accepted an empty reference")
	}
	refs := []string{"object-b", "object-a", "object-b"}
	original := append([]string(nil), refs...)
	if _, err := metadata.fileFacts(context.Background(), testRepositoryNamespace, refs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refs, original) {
		t.Fatalf("fileFacts modified input references: got %v, want %v", refs, original)
	}
}
