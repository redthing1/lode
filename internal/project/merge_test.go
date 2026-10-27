package project

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
)

func TestMergePreviewChoicesAndTwoParentCommit(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseFiles := map[string][]byte{
		"a.bin":               []byte("base a"),
		"b.bin":               []byte("base b"),
		"hidden/unloaded.bin": []byte("base hidden"),
		"delete.bin":          []byte("base deletion"),
		"type.bin":            []byte("base type"),
	}
	base := mergeApply(t, local.Service, "merge-preview-base", "main", "", fileChanges(baseFiles), baseFiles)
	if err := local.Service.CreateBranch(ctx, "source", base.ID); err != nil {
		t.Fatal(err)
	}

	localFiles := map[string][]byte{
		"a.bin": []byte("local a"), "b.bin": []byte("local b"), "hidden/unloaded.bin": []byte("local hidden"),
		"type.bin/child.bin": []byte("local child"),
	}
	localChanges := fileChanges(localFiles)
	localChanges = append(localChanges,
		Change{Path: "delete.bin", Kind: model.Absent},
		Change{Path: "type.bin", Kind: model.Directory},
	)
	localTip := mergeApply(t, local.Service, "merge-preview-local", "main", base.ID, localChanges, localFiles)

	targetFiles := map[string][]byte{
		"a.bin": []byte("target a"), "b.bin": []byte("target b"), "hidden/unloaded.bin": []byte("target hidden"),
		"delete.bin": []byte("target deletion"), "type.bin": []byte("target type"),
	}
	targetTip := mergeApply(t, local.Service, "merge-preview-target", "source", base.ID, fileChanges(targetFiles), targetFiles)
	spec := MergeSpec{Branch: "main", Base: localTip.ID, Other: targetTip.ID}

	page1, err := local.Service.PreviewMerge(ctx, spec, "", 2)
	if err != nil || page1.CommonBase != base.ID || page1.TotalConflicts != 5 || !reflect.DeepEqual(page1.Conflicts, []string{"a.bin", "b.bin"}) || page1.Next != "b.bin" {
		t.Fatalf("first merge preview=%#v err=%v", page1, err)
	}
	page2, err := local.Service.PreviewMerge(ctx, spec, page1.Next, 2)
	if err != nil || page2.TotalConflicts != 5 || !reflect.DeepEqual(page2.Conflicts, []string{"delete.bin", "hidden/unloaded.bin"}) || page2.Next != "hidden/unloaded.bin" {
		t.Fatalf("second merge preview=%#v err=%v", page2, err)
	}
	page3, err := local.Service.PreviewMerge(ctx, spec, page2.Next, 2)
	if err != nil || page3.TotalConflicts != 5 || !reflect.DeepEqual(page3.Conflicts, []string{"type.bin"}) || page3.Next != "" {
		t.Fatalf("third merge preview=%#v err=%v", page3, err)
	}
	// This conflict is present in complete server metadata even though it could
	// be outside a partial client's materialized selection.
	if !containsString(page2.Conflicts, "hidden/unloaded.bin") {
		t.Fatal("preview omitted the conflicting unloaded path")
	}

	unresolved := MergeRequest{OperationID: "merge-unresolved", MergeSpec: spec, Author: "tester", Message: "unresolved"}
	if _, err := local.Service.Merge(ctx, unresolved); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("unresolved merge error=%v, want ErrMergeConflict", err)
	}
	if _, err := local.Service.Operation(ctx, unresolved.OperationID); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("unresolved merge reserved operation: %v", err)
	}

	request := MergeRequest{
		OperationID: "merge-with-choices", MergeSpec: MergeSpec{
			Branch: "main", Base: localTip.ID, Other: targetTip.ID,
			Choices: []Choice{
				{Path: "a.bin", Side: model.LocalSide},
				{Path: "b.bin", Side: model.TargetSide},
				{Path: "delete.bin", Side: model.TargetSide},
				{Path: "hidden/unloaded.bin", Side: model.TargetSide},
				{Path: "type.bin", Side: model.LocalSide},
			},
		}, Author: "tester", Message: "resolve whole entries",
	}
	merged, err := local.Service.Merge(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged.Parents, []string{localTip.ID, targetTip.ID}) {
		t.Fatalf("merge parents=%v, want destination then source", merged.Parents)
	}
	wantFiles := map[string][]byte{
		"a.bin": []byte("local a"), "b.bin": []byte("target b"),
		"delete.bin": []byte("target deletion"), "hidden/unloaded.bin": []byte("target hidden"),
		"type.bin/child.bin": []byte("local child"),
	}
	all, _, err := local.Service.Entries(ctx, merged.ID, []SelectionRule{{Included: true}}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	assertFileNodes(t, all, wantFiles)
	if nodeByPath(all)["type.bin"].Kind != model.Directory {
		t.Fatalf("chosen file/directory result has wrong type: %#v", nodeByPath(all)["type.bin"])
	}
	for path, want := range wantFiles {
		reader, _, err := local.Service.OpenFile(ctx, merged.ID, path)
		if err != nil {
			t.Fatalf("open merged file %q: %v", path, err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
			t.Errorf("merged payload %q=%q readErr=%v closeErr=%v", path, got, readErr, closeErr)
		}
	}
	replayed, err := local.Service.Merge(ctx, request)
	if err != nil || replayed.ID != merged.ID {
		t.Fatalf("exact merge replay=%#v err=%v, want %q", replayed, err, merged.ID)
	}
	changed := request
	changed.Choices = append([]Choice(nil), request.Choices...)
	changed.Choices[0].Side = model.TargetSide
	if _, err := local.Service.Merge(ctx, changed); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("changed merge choices error=%v, want ErrOperationConflict", err)
	}
}

func TestMergeRootChoiceSelectsCompleteTargetTree(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseFiles := map[string][]byte{"shared.bin": []byte("base")}
	base := mergeApply(t, local.Service, "root-choice-base", "main", "", fileChanges(baseFiles), baseFiles)
	if err := local.Service.CreateBranch(ctx, "source", base.ID); err != nil {
		t.Fatal(err)
	}
	mainFiles := map[string][]byte{"shared.bin": []byte("local"), "local-only.bin": []byte("local")}
	mainTip := mergeApply(t, local.Service, "root-choice-local", "main", base.ID, fileChanges(mainFiles), mainFiles)
	targetFiles := map[string][]byte{"shared.bin": []byte("target"), "target-only.bin": []byte("target")}
	targetTip := mergeApply(t, local.Service, "root-choice-target", "source", base.ID, fileChanges(targetFiles), targetFiles)
	spec := MergeSpec{Branch: "main", Base: mainTip.ID, Other: targetTip.ID, Choices: []Choice{{Path: "", Side: model.TargetSide}}}
	preview, err := local.Service.PreviewMerge(ctx, spec, "", 10)
	if err != nil || preview.TotalConflicts != 0 {
		t.Fatalf("root-choice preview=%#v err=%v", preview, err)
	}
	merged, err := local.Service.Merge(ctx, MergeRequest{OperationID: "root-choice-merge", MergeSpec: spec, Author: "tester", Message: "take target tree"})
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := local.Service.Entries(ctx, targetTip.ID, []SelectionRule{{Included: true}}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := local.Service.Entries(ctx, merged.ID, []SelectionRule{{Included: true}}, "", 100)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("root-choice result=%#v err=%v, want target tree %#v", got, err, want)
	}
}

func TestMergeRejectsStaleDestinationAndUnrelatedRoot(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseFiles := map[string][]byte{"base.bin": []byte("base")}
	base := mergeApply(t, local.Service, "stale-base", "main", "", fileChanges(baseFiles), baseFiles)
	if err := local.Service.CreateBranch(ctx, "source", base.ID); err != nil {
		t.Fatal(err)
	}
	sourceFiles := map[string][]byte{"source.bin": []byte("source")}
	source := mergeApply(t, local.Service, "stale-source", "source", base.ID, fileChanges(sourceFiles), sourceFiles)
	mainFiles := map[string][]byte{"main.bin": []byte("main")}
	main := mergeApply(t, local.Service, "stale-main", "main", base.ID, fileChanges(mainFiles), mainFiles)
	if _, err := local.Service.PreviewMerge(ctx, MergeSpec{Branch: "main", Base: base.ID, Other: source.ID}, "", 10); !errors.Is(err, ErrStaleHead) {
		t.Fatalf("stale destination error=%v, want ErrStaleHead", err)
	}

	unrelatedFiles := map[string][]byte{"unrelated.bin": []byte("unrelated")}
	unrelated := mergeApply(t, local.Service, "unrelated-root", "unrelated", "", fileChanges(unrelatedFiles), unrelatedFiles)
	if _, err := local.Service.PreviewMerge(ctx, MergeSpec{Branch: "main", Base: main.ID, Other: unrelated.ID}, "", 10); !errors.Is(err, ErrUnrelatedHistory) {
		t.Fatalf("unrelated histories error=%v, want ErrUnrelatedHistory", err)
	}
}

func TestMergeRejectsActualCrisscrossWithMultipleBestBases(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseFiles := map[string][]byte{"base.bin": []byte("base")}
	base := mergeApply(t, local.Service, "crisscross-base", "main", "", fileChanges(baseFiles), baseFiles)
	for _, name := range []string{"left", "right"} {
		if err := local.Service.CreateBranch(ctx, name, base.ID); err != nil {
			t.Fatal(err)
		}
	}
	leftFiles := map[string][]byte{"left.bin": []byte("left")}
	left := mergeApply(t, local.Service, "crisscross-left", "left", base.ID, fileChanges(leftFiles), leftFiles)
	rightFiles := map[string][]byte{"right.bin": []byte("right")}
	right := mergeApply(t, local.Service, "crisscross-right", "right", base.ID, fileChanges(rightFiles), rightFiles)
	if err := local.Service.CreateBranch(ctx, "merged-left", left.ID); err != nil {
		t.Fatal(err)
	}
	if err := local.Service.CreateBranch(ctx, "merged-right", right.ID); err != nil {
		t.Fatal(err)
	}
	leftMerge, err := local.Service.Merge(ctx, MergeRequest{
		OperationID: "crisscross-merge-left", MergeSpec: MergeSpec{Branch: "merged-left", Base: left.ID, Other: right.ID},
		Author: "tester", Message: "left merge",
	})
	if err != nil {
		t.Fatal(err)
	}
	rightMerge, err := local.Service.Merge(ctx, MergeRequest{
		OperationID: "crisscross-merge-right", MergeSpec: MergeSpec{Branch: "merged-right", Base: right.ID, Other: left.ID},
		Author: "tester", Message: "right merge",
	})
	if err != nil {
		t.Fatal(err)
	}
	leftDesc := map[string][]byte{"left-descendant.bin": []byte("left descendant")}
	leftTip := mergeApply(t, local.Service, "crisscross-left-desc", "merged-left", leftMerge.ID, fileChanges(leftDesc), leftDesc)
	rightDesc := map[string][]byte{"right-descendant.bin": []byte("right descendant")}
	rightTip := mergeApply(t, local.Service, "crisscross-right-desc", "merged-right", rightMerge.ID, fileChanges(rightDesc), rightDesc)
	if _, err := local.Service.PreviewMerge(ctx, MergeSpec{Branch: "merged-left", Base: leftTip.ID, Other: rightTip.ID}, "", 10); !errors.Is(err, ErrAmbiguousMergeBase) {
		t.Fatalf("crisscross merge error=%v, want ErrAmbiguousMergeBase", err)
	}
}

func TestMergeRetryKeepsCandidateAfterProtectionFailure(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseFiles := map[string][]byte{"file.bin": []byte("base")}
	base := mergeApply(t, local.Service, "retry-base", "main", "", fileChanges(baseFiles), baseFiles)
	if err := local.Service.CreateBranch(ctx, "source", base.ID); err != nil {
		t.Fatal(err)
	}
	oursFiles := map[string][]byte{"file.bin": []byte("ours")}
	ours := mergeApply(t, local.Service, "retry-ours", "main", base.ID, fileChanges(oursFiles), oursFiles)
	theirsFiles := map[string][]byte{"file.bin": []byte("theirs")}
	theirs := mergeApply(t, local.Service, "retry-theirs", "source", base.ID, fileChanges(theirsFiles), theirsFiles)
	request := MergeRequest{
		OperationID: "retry-merge", MergeSpec: MergeSpec{Branch: "main", Base: ours.ID, Other: theirs.ID, Choices: []Choice{{Path: "file.bin", Side: model.TargetSide}}},
		Author: "tester", Message: "retry after protection error",
	}
	failure := &failFirstProtect{ContentRoots: local.Service.roots}
	local.Service.roots = failure
	if _, err := local.Service.Merge(ctx, request); err == nil {
		t.Fatal("Merge succeeded despite injected protection failure")
	}
	operation, err := local.Service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != OperationPending || operation.CommitID == "" {
		t.Fatalf("operation after failed protection=%#v err=%v", operation, err)
	}
	candidate := operation.CommitID
	merged, err := local.Service.Merge(ctx, request)
	if err != nil || merged.ID != candidate {
		t.Fatalf("retry result=%#v err=%v, candidate=%q", merged, err, candidate)
	}
	assertMergePayload(t, local.Service, merged.ID, "file.bin", []byte("theirs"))
}

func TestRecoverRecomposesSealedMergeChoicesAndFailsMissingSourceCleanly(t *testing.T) {
	ctx := context.Background()
	local := openTestLocal(t)
	baseFiles := map[string][]byte{"conflict.bin": []byte("base")}
	base := mergeApply(t, local.Service, "recover-base", "main", "", fileChanges(baseFiles), baseFiles)
	if err := local.Service.CreateBranch(ctx, "source", base.ID); err != nil {
		t.Fatal(err)
	}
	localFiles := map[string][]byte{"conflict.bin": []byte("local")}
	ours := mergeApply(t, local.Service, "recover-local", "main", base.ID, fileChanges(localFiles), localFiles)
	targetFiles := map[string][]byte{"conflict.bin": []byte("target")}
	theirs := mergeApply(t, local.Service, "recover-target", "source", base.ID, fileChanges(targetFiles), targetFiles)
	request := MergeRequest{
		OperationID: "recover-sealed-merge", MergeSpec: MergeSpec{
			Branch: "main", Base: ours.ID, Other: theirs.ID,
			Choices: []Choice{{Path: "conflict.bin", Side: model.TargetSide}},
		}, Author: "tester", Message: "recover sealed choices",
	}
	reserved := reserveMergeCapture(t, local, request)
	if reserved.State != OperationCapturing {
		t.Fatalf("reserved state=%q, want capturing", reserved.State)
	}
	if err := local.Service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	operation, err := local.Service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != OperationAccepted || operation.CommitID != reserved.CommitID {
		t.Fatalf("recovered operation=%#v err=%v", operation, err)
	}
	commit, err := local.Service.Commit(ctx, operation.CommitID)
	if err != nil || !reflect.DeepEqual(commit.Parents, []string{ours.ID, theirs.ID}) {
		t.Fatalf("recovered commit=%#v err=%v", commit, err)
	}
	assertMergePayload(t, local.Service, commit.ID, "conflict.bin", []byte("target"))

	missing := newID()
	missingRequest := MergeRequest{
		OperationID: "recover-missing-source", MergeSpec: MergeSpec{Branch: "main", Base: commit.ID, Other: missing},
		Author: "tester", Message: "missing source",
	}
	missingRecord := reserveMergeCapture(t, local, missingRequest)
	if err := local.Service.Recover(ctx); err == nil {
		t.Fatal("Recover succeeded with missing source commit")
	}
	after, err := local.Service.Operation(ctx, missingRequest.OperationID)
	if err != nil || after.State != OperationCapturing || after.CommitID != missingRecord.CommitID {
		t.Fatalf("missing-source operation=%#v err=%v", after, err)
	}
	if head, err := local.Service.Head(ctx, "main"); err != nil || head != commit.ID {
		t.Fatalf("missing-source recovery changed head=%q err=%v", head, err)
	}
}

func mergeApply(t *testing.T, service *Service, operation, branch, base string, changes []Change, files map[string][]byte) Commit {
	t.Helper()
	request := ChangeRequest{OperationID: operation, Branch: branch, Base: base, Author: "tester", Message: operation, Changes: changes}
	commit, err := service.Apply(context.Background(), request, func(path model.Path) (io.ReadCloser, error) {
		data, ok := files[path.String()]
		if !ok {
			return nil, errors.New("missing merge test payload")
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatalf("apply %q: %v", operation, err)
	}
	return commit
}

func fileChanges(files map[string][]byte) []Change {
	changes := make([]Change, 0, len(files))
	for path, data := range files {
		changes = append(changes, Change{Path: path, Kind: model.File, SHA256: sha256Hex(data), Size: int64(len(data))})
	}
	return changes
}

func reserveMergeCapture(t *testing.T, local *Local, request MergeRequest) publicationRecord {
	t.Helper()
	request, encoded, fingerprint, err := normalizeMerge(request)
	if err != nil {
		t.Fatalf("normalize capture request: %v", err)
	}
	now := local.Service.now().UTC()
	record := publicationRecord{
		ID: request.OperationID, Fingerprint: fingerprint, CaptureInput: encoded,
		Input: publicationInput{
			Branch: request.Branch, Base: request.Base, MergeParent: request.Other,
			Author: request.Author, Message: request.Message,
		},
		CommitID: newID(), CreatedAt: now, ExpiresAt: now.Add(local.Service.ttl), State: OperationCapturing,
	}
	reserved, err := local.metadata.prepareCapture(context.Background(), record)
	if err != nil {
		t.Fatalf("reserve merge capture: %v", err)
	}
	return reserved
}

func assertMergePayload(t *testing.T, service *Service, commitID, path string, want []byte) {
	t.Helper()
	reader, _, err := service.OpenFile(context.Background(), commitID, path)
	if err != nil {
		t.Fatalf("open %q at %s: %v", path, commitID, err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("payload %q=%q readErr=%v closeErr=%v, want %q", path, got, readErr, closeErr, want)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type failFirstProtect struct {
	ContentRoots
	failed bool
}

func (r *failFirstProtect) Protect(ctx context.Context, key string, archive kopia.Archive) error {
	if !r.failed {
		r.failed = true
		return errors.New("injected first protection failure")
	}
	return r.ContentRoots.Protect(ctx, key, archive)
}
