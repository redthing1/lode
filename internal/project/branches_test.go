package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestLocalBranchForksAndMovesIndependentlyWithoutNewCommit(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseData := []byte("base payload")
	baseRequest := ChangeRequest{
		OperationID: "branch-base", Branch: "main", Author: "tester", Message: "seed main",
		Changes: []Change{{Path: "base.bin", Kind: model.File, SHA256: sha256Hex(baseData), Size: int64(len(baseData))}},
	}
	base, err := local.Service.Apply(ctx, baseRequest, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(baseData)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var beforeCount int
	if err := local.metadata.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commits`).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatalf("idempotent branch creation: %v", err)
	}
	if err := local.Service.CreateBranch(ctx, "feature", "00000000000000000000000000000000"); !errors.Is(err, ErrBranchExists) {
		t.Fatalf("branch creation at another tip = %v, want ErrBranchExists", err)
	}
	var afterCount int
	if err := local.metadata.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commits`).Scan(&afterCount); err != nil {
		t.Fatal(err)
	}
	if beforeCount != afterCount {
		t.Fatalf("creating a branch added commits: %d -> %d", beforeCount, afterCount)
	}
	if tip, err := local.Service.Head(ctx, "feature"); err != nil || tip != base.ID {
		t.Fatalf("feature head = %q, %v; want base %q", tip, err, base.ID)
	}

	mainData := []byte("main-only change")
	mainCommit, err := local.Service.Apply(ctx, ChangeRequest{
		OperationID: "branch-main-change", Branch: "main", Base: base.ID, Author: "tester", Message: "move main",
		Changes: []Change{{Path: "main.bin", Kind: model.File, SHA256: sha256Hex(mainData), Size: int64(len(mainData))}},
	}, func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(mainData)), nil })
	if err != nil {
		t.Fatal(err)
	}
	featureData := []byte("feature-only change")
	featureCommit, err := local.Service.Apply(ctx, ChangeRequest{
		OperationID: "branch-feature-change", Branch: "feature", Base: base.ID, Author: "tester", Message: "move feature",
		Changes: []Change{{Path: "feature.bin", Kind: model.File, SHA256: sha256Hex(featureData), Size: int64(len(featureData))}},
	}, func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(featureData)), nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(featureCommit.Parents) != 1 || featureCommit.Parents[0] != base.ID {
		t.Fatalf("feature commit parents = %#v, want base %q", featureCommit.Parents, base.ID)
	}
	if tip, err := local.Service.Head(ctx, "main"); err != nil || tip != mainCommit.ID {
		t.Fatalf("main head = %q, %v; want %q", tip, err, mainCommit.ID)
	}
	if tip, err := local.Service.Head(ctx, "feature"); err != nil || tip != featureCommit.ID {
		t.Fatalf("feature head = %q, %v; want %q", tip, err, featureCommit.ID)
	}

	page1, next, err := local.Service.Branches(ctx, "", 1)
	if err != nil || len(page1) != 1 || page1[0] != (Branch{Name: "feature", Tip: featureCommit.ID}) || next != "feature" {
		t.Fatalf("first branch page = %#v next=%q err=%v", page1, next, err)
	}
	page2, next, err := local.Service.Branches(ctx, next, 1)
	if err != nil || len(page2) != 1 || page2[0] != (Branch{Name: "main", Tip: mainCommit.ID}) || next != "" {
		t.Fatalf("second branch page = %#v next=%q err=%v", page2, next, err)
	}
	if got, err := local.Service.Branch(ctx, "feature"); err != nil || got != page1[0] {
		t.Fatalf("Branch(feature) = %#v, %v", got, err)
	}
	if _, err := local.Service.Branch(ctx, "missing"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("Branch(missing) = %v, want ErrBranchNotFound", err)
	}
	if _, _, err := local.Service.Branches(ctx, "", 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Branches(limit=0) = %v, want ErrInvalidRequest", err)
	}
	if _, _, err := local.Service.Branches(ctx, "bad cursor", 1); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Branches(invalid cursor) = %v, want ErrInvalidRequest", err)
	}

	if err := local.Service.DeleteBranch(ctx, "feature", base.ID); !errors.Is(err, ErrStaleHead) {
		t.Fatalf("delete with stale expected tip = %v, want ErrStaleHead", err)
	}
	if got, err := local.Service.Branch(ctx, "feature"); err != nil || got.Tip != featureCommit.ID {
		t.Fatalf("stale delete moved branch: %#v, %v", got, err)
	}
	if err := local.Service.DeleteBranch(ctx, "feature", featureCommit.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.DeleteBranch(ctx, "feature", featureCommit.ID); err != nil {
		t.Fatalf("repeat delete of absent branch: %v", err)
	}
	if _, err := local.Service.Branch(ctx, "feature"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("deleted branch lookup = %v", err)
	}
	if got, err := local.Service.Commit(ctx, featureCommit.ID); err != nil || got.ID != featureCommit.ID {
		t.Fatalf("deleted branch removed commit: %#v, %v", got, err)
	}
	reader, _, err := local.Service.OpenFile(ctx, featureCommit.ID, "base.bin")
	if err != nil {
		t.Fatalf("deleted branch made retained commit data unavailable: %v", err)
	}
	gotBytes, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(gotBytes, baseData) {
		t.Fatalf("retained commit payload = %q, errors %v / %v", gotBytes, readErr, closeErr)
	}
}

func TestLocalBranchCreationRejectsInvalidMissingAndUnavailableSources(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	for _, test := range []struct {
		name string
		from string
		want error
	}{
		{name: "empty", from: "", want: ErrInvalidRequest},
		{name: "malformed", from: "not-a-commit", want: ErrInvalidRequest},
		{name: "missing", from: "00000000000000000000000000000000", want: ErrCommitNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := local.Service.CreateBranch(ctx, "candidate-"+test.name, test.from); !errors.Is(err, test.want) {
				t.Fatalf("CreateBranch error = %v, want %v", err, test.want)
			}
		})
	}
	if err := local.Service.CreateBranch(ctx, "bad branch", "00000000000000000000000000000000"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid branch name error = %v", err)
	}

	foreign := openTestLocal(t)
	foreignData := []byte("object in a different repository")
	foreignCommit, err := foreign.Service.Apply(ctx, ChangeRequest{
		OperationID: "foreign-source", Branch: "main", Author: "tester", Message: "foreign object",
		Changes: []Change{{Path: "foreign.bin", Kind: model.File, SHA256: sha256Hex(foreignData), Size: int64(len(foreignData))}},
	}, func(model.Path) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(foreignData)), nil })
	if err != nil {
		t.Fatal(err)
	}
	foreignArchive, err := foreign.Service.archive(ctx, foreignCommit.Tree)
	if err != nil {
		t.Fatal(err)
	}
	foreignTreeID, err := local.metadata.Import(ctx, local.Service.namespace, foreignArchive.Format, foreignArchive.Root, foreignArchive.Directories)
	if err != nil {
		t.Fatal(err)
	}
	unavailableCommit := Commit{ID: newID(), Tree: TreeRef{Format: foreignArchive.Format, ID: foreignTreeID}}
	encoded, err := json.Marshal(unavailableCommit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.metadata.db.ExecContext(ctx, `INSERT INTO commits (id, data) VALUES (?, ?)`, unavailableCommit.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := local.metadata.db.ExecContext(ctx, `INSERT INTO payload_retention (commit_id) VALUES (?)`, unavailableCommit.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.CreateBranch(ctx, "unavailable", unavailableCommit.ID); err == nil {
		t.Fatal("branch creation accepted a tree whose native objects are unavailable")
	}
	if _, err := local.Service.Branch(ctx, "unavailable"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("unavailable branch was inserted: %v", err)
	}
}
