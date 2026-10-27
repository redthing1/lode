package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	engine "github.com/redthing1/lode/internal/engine/kopia"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

type fixture struct {
	local  *project.Local
	server *httptest.Server
	client *Client
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir() + "/project"
	if err := project.InitializeLocal(ctx, dir); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(local.Service, local.Token)
	if err != nil {
		_ = local.Close()
		t.Fatal(err)
	}
	ts := httptest.NewServer(handler)
	client, err := NewClient(ts.URL, local.Token)
	if err != nil {
		ts.Close()
		_ = local.Close()
		t.Fatal(err)
	}
	f := &fixture{local: local, server: ts, client: client}
	t.Cleanup(func() { ts.Close(); _ = local.Close() })
	return f
}

func change(path, contents string) project.Change {
	digest := sha256.Sum256([]byte(contents))
	return project.Change{Path: path, Kind: model.File, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(contents))}
}

func opener(values map[string]string, calls *int) func(model.Path) (io.ReadCloser, error) {
	return func(path model.Path) (io.ReadCloser, error) {
		if calls != nil {
			*calls++
		}
		contents, ok := values[path.String()]
		if !ok {
			return nil, errors.New("unexpected local file open")
		}
		return io.NopCloser(strings.NewReader(contents)), nil
	}
}

func TestClientServerSparseStreamingAndHistory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	info, err := f.client.Info(ctx)
	if err != nil || info.ID == "" {
		t.Fatalf("Info() = %+v, %v", info, err)
	}

	firstRequest := project.ChangeRequest{
		OperationID: "first-tree", Branch: "main", Author: "test", Message: "initial",
		Changes: []project.Change{change("z-untouched.txt", "leave me"), change("a.txt", "alpha"), change("m.txt", "middle")},
	}
	firstCalls := 0
	first, err := f.client.Apply(ctx, firstRequest, opener(map[string]string{"z-untouched.txt": "leave me", "a.txt": "alpha", "m.txt": "middle"}, &firstCalls))
	if err != nil || firstCalls != 3 {
		t.Fatalf("initial Apply() = %+v, calls=%d, err=%v", first, firstCalls, err)
	}
	if head, err := f.client.Head(ctx, "main"); err != nil || head != first.ID {
		t.Fatalf("Head() = %q, %v", head, err)
	}
	if got, err := f.client.Commit(ctx, first.ID); err != nil || got.ID != first.ID {
		t.Fatalf("Commit() = %+v, %v", got, err)
	}
	if op, err := f.client.Operation(ctx, firstRequest.OperationID); err != nil || op.State != project.OperationAccepted {
		t.Fatalf("Operation() = %+v, %v", op, err)
	}

	nodes, next, err := f.client.Entries(ctx, first.ID, []project.SelectionRule{{Path: "a.txt", Included: true}}, "", 10)
	if err != nil || next != "" || len(nodes) != 1 || nodes[0].Path != "a.txt" || nodes[0].SHA256 != change("a.txt", "alpha").SHA256 {
		t.Fatalf("selected Entries() = %+v, next=%q, err=%v", nodes, next, err)
	}
	reader, node, err := f.client.OpenFile(ctx, first.ID, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || string(data) != "alpha" || node.Path != "a.txt" || node.Size != 5 {
		t.Fatalf("OpenFile() = %q, %+v, read=%v close=%v", data, node, err, closeErr)
	}
	if _, _, err := f.client.OpenFile(ctx, first.ID, "missing.txt"); !errors.Is(err, engine.ErrPathNotFound) {
		t.Fatalf("OpenFile missing = %v, want ErrPathNotFound", err)
	}
	history, historyNext, err := f.client.History(ctx, first.ID, 1)
	if err != nil || len(history) != 1 || history[0].ID != first.ID || historyNext != "" {
		t.Fatalf("History() = %+v, %q, %v", history, historyNext, err)
	}

	secondRequest := project.ChangeRequest{
		OperationID: "change-untouched", Branch: "main", Base: first.ID, Author: "test", Message: "change one path",
		Changes: []project.Change{change("z-untouched.txt", "changed"), change("a.txt", "alpha")},
	}
	second, err := f.client.Apply(ctx, secondRequest, opener(map[string]string{"z-untouched.txt": "changed", "a.txt": "alpha"}, nil))
	if err != nil {
		t.Fatalf("Apply based on first commit: %v", err)
	}
	if second.Parents[0] != first.ID {
		t.Fatalf("second parents = %v", second.Parents)
	}

	staleRequest := project.ChangeRequest{
		OperationID: "stale-request", Branch: "main", Base: first.ID, Author: "test", Message: "stale",
		Changes: []project.Change{change("m.txt", "stale edit")},
	}
	stale, err := f.client.Apply(ctx, staleRequest, opener(map[string]string{"m.txt": "stale edit"}, nil))
	if !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("stale Apply() = %+v, %v; want stale head", stale, err)
	}
	if head, err := f.client.Head(ctx, "main"); err != nil || head != second.ID {
		t.Fatalf("head after stale request = %q, %v", head, err)
	}

	thirdRequest := project.ChangeRequest{
		OperationID: "change-m", Branch: "main", Base: second.ID, Author: "test", Message: "preserve omitted change",
		Changes: []project.Change{change("m.txt", "new middle")},
	}
	third, err := f.client.Apply(ctx, thirdRequest, opener(map[string]string{"m.txt": "new middle"}, nil))
	if err != nil {
		t.Fatalf("Apply based on updated head: %v", err)
	}
	for path, expected := range map[string]string{"a.txt": "alpha", "m.txt": "new middle", "z-untouched.txt": "changed"} {
		file, _, err := f.client.OpenFile(ctx, third.ID, path)
		if err != nil {
			t.Fatalf("OpenFile(%q): %v", path, err)
		}
		got, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || string(got) != expected {
			t.Fatalf("file %q = %q, read=%v close=%v; want %q", path, got, readErr, closeErr, expected)
		}
	}

	noOpen := func(model.Path) (io.ReadCloser, error) { t.Fatal("exact replay requested file bytes"); return nil, nil }
	replayed, err := f.client.Apply(ctx, thirdRequest, noOpen)
	if err != nil || replayed.ID != third.ID {
		t.Fatalf("exact Apply replay = %+v, %v", replayed, err)
	}
	changedRetry := thirdRequest
	changedRetry.Message = "different manifest"
	if _, err := f.client.Apply(ctx, changedRetry, noOpen); !errors.Is(err, project.ErrOperationConflict) {
		t.Fatalf("changed Apply replay = %v, want ErrOperationConflict", err)
	}
}

func applyOne(t *testing.T, f *fixture, operationID, branch, base, path, contents string) project.Commit {
	t.Helper()
	request := project.ChangeRequest{
		OperationID: operationID, Branch: branch, Base: base,
		Author: "merge-test", Message: operationID,
		Changes: []project.Change{change(path, contents)},
	}
	commit, err := f.client.Apply(context.Background(), request, opener(map[string]string{path: contents}, nil))
	if err != nil {
		t.Fatalf("Apply(%s) = %v", operationID, err)
	}
	return commit
}

func readRemoteFile(t *testing.T, f *fixture, commitID, path string) string {
	t.Helper()
	r, _, err := f.client.OpenFile(context.Background(), commitID, path)
	if err != nil {
		t.Fatalf("OpenFile(%s, %s) = %v", commitID, path, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestClientServerMergeComposesTwoTipsWithoutUploads(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	base, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "merge-base", Branch: "main", Author: "merge-test", Message: "base",
		Changes: []project.Change{change("left.txt", "left-base"), change("right.txt", "right-base")},
	}, opener(map[string]string{"left.txt": "left-base", "right.txt": "right-base"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.CreateBranch(ctx, "other", base.ID); err != nil {
		t.Fatal(err)
	}
	left := applyOne(t, f, "merge-left", "main", base.ID, "left.txt", "left-from-main")
	right := applyOne(t, f, "merge-right", "other", base.ID, "right.txt", "right-from-other")
	spec := project.MergeSpec{Branch: "main", Base: left.ID, Other: right.ID}
	preview, err := f.client.PreviewMerge(ctx, spec, "", 10)
	if err != nil || preview.CommonBase != base.ID || len(preview.Conflicts) != 0 || preview.TotalConflicts != 0 {
		t.Fatalf("PreviewMerge() = %+v, %v", preview, err)
	}
	request := project.MergeRequest{
		OperationID: "merge-compose", MergeSpec: spec,
		Author: "merge-test", Message: "merge independent changes",
	}
	merged, err := f.client.Merge(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged.Parents, []string{left.ID, right.ID}) {
		t.Fatalf("merge parents = %v, want [%s %s]", merged.Parents, left.ID, right.ID)
	}
	if got := readRemoteFile(t, f, merged.ID, "left.txt"); got != "left-from-main" {
		t.Fatalf("merged left file = %q", got)
	}
	if got := readRemoteFile(t, f, merged.ID, "right.txt"); got != "right-from-other" {
		t.Fatalf("merged right file = %q", got)
	}
}

func TestClientServerMergePreviewPagingChoicesAndRetry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	base, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "conflict-base", Branch: "main", Author: "merge-test", Message: "base",
		Changes: []project.Change{change("a.txt", "a-base"), change("z.txt", "z-base")},
	}, opener(map[string]string{"a.txt": "a-base", "z.txt": "z-base"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.CreateBranch(ctx, "other", base.ID); err != nil {
		t.Fatal(err)
	}
	left, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "conflict-left", Branch: "main", Base: base.ID, Author: "merge-test", Message: "left",
		Changes: []project.Change{change("a.txt", "a-left"), change("z.txt", "z-left")},
	}, opener(map[string]string{"a.txt": "a-left", "z.txt": "z-left"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	right, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "conflict-right", Branch: "other", Base: base.ID, Author: "merge-test", Message: "right",
		Changes: []project.Change{change("a.txt", "a-right"), change("z.txt", "z-right")},
	}, opener(map[string]string{"a.txt": "a-right", "z.txt": "z-right"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	spec := project.MergeSpec{Branch: "main", Base: left.ID, Other: right.ID}
	first, err := f.client.PreviewMerge(ctx, spec, "", 1)
	if err != nil || !reflect.DeepEqual(first.Conflicts, []string{"a.txt"}) || first.TotalConflicts != 2 || first.Next != "a.txt" {
		t.Fatalf("first conflict page = %+v, %v", first, err)
	}
	second, err := f.client.PreviewMerge(ctx, spec, first.Next, 1)
	if err != nil || !reflect.DeepEqual(second.Conflicts, []string{"z.txt"}) || second.Next != "" {
		t.Fatalf("second conflict page = %+v, %v", second, err)
	}
	request := project.MergeRequest{
		OperationID: "resolve-conflicts", MergeSpec: project.MergeSpec{
			Branch: spec.Branch, Base: spec.Base, Other: spec.Other,
			Choices: []project.Choice{{Path: "a.txt", Side: model.LocalSide}, {Path: "z.txt", Side: model.TargetSide}},
		},
		Author: "merge-test", Message: "resolve conflicts",
	}
	merged, err := f.client.Merge(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := readRemoteFile(t, f, merged.ID, "a.txt"); got != "a-left" {
		t.Fatalf("chosen local file = %q", got)
	}
	if got := readRemoteFile(t, f, merged.ID, "z.txt"); got != "z-right" {
		t.Fatalf("chosen target file = %q", got)
	}
	if retry, err := f.client.Merge(ctx, request); err != nil || retry.ID != merged.ID {
		t.Fatalf("exact merge retry = %+v, %v", retry, err)
	}
	changed := request
	changed.MergeSpec.Choices = []project.Choice{{Path: "a.txt", Side: model.TargetSide}, {Path: "z.txt", Side: model.TargetSide}}
	if _, err := f.client.Merge(ctx, changed); !errors.Is(err, project.ErrOperationConflict) {
		t.Fatalf("changed merge retry error = %v, want ErrOperationConflict", err)
	}
	stale := project.MergeRequest{
		OperationID: "stale-merge", MergeSpec: spec,
		Author: "merge-test", Message: "stale merge",
	}
	if _, err := f.client.Merge(ctx, stale); !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("merge with stale expected tip = %v, want ErrStaleHead", err)
	}
}

func TestMergeErrorWireMappings(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		want   error
	}{
		{project.ErrMergeConflict, http.StatusConflict, "merge_conflict", project.ErrMergeConflict},
		{project.ErrUnrelatedHistory, http.StatusConflict, "unrelated_history", project.ErrUnrelatedHistory},
		{project.ErrAmbiguousMergeBase, http.StatusConflict, "ambiguous_merge_base", project.ErrAmbiguousMergeBase},
		{project.ErrHistoryLimit, http.StatusRequestEntityTooLarge, "history_limit", project.ErrHistoryLimit},
		{model.ErrInvalidChoice, http.StatusBadRequest, "invalid_choice", model.ErrInvalidChoice},
	}
	for _, tc := range cases {
		status, wire := mapError(tc.err)
		if status != tc.status || wire.Code != tc.code {
			t.Errorf("mapError(%v) = (%d, %q), want (%d, %q)", tc.err, status, wire.Code, tc.status, tc.code)
		}
		if got := errorSentinel(wire.Code); !errors.Is(got, tc.want) {
			t.Errorf("errorSentinel(%q) = %v, want %v", wire.Code, got, tc.want)
		}
	}
}

func TestClientServerBranchForkListMoveAndExactDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	baseData := "branch base payload"
	base, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "branch-transport-base", Branch: "main", Author: "test", Message: "seed branch API",
		Changes: []project.Change{change("base.txt", baseData)},
	}, opener(map[string]string{"base.txt": baseData}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.client.CreateBranch(ctx, "feature", base.ID); err != nil {
		t.Fatalf("idempotent CreateBranch: %v", err)
	}
	if err := f.client.CreateBranch(ctx, "feature", strings.Repeat("0", 32)); !errors.Is(err, project.ErrBranchExists) {
		t.Fatalf("CreateBranch at another tip = %v, want ErrBranchExists", err)
	}
	if err := f.client.CreateBranch(ctx, "alpha", base.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.client.CreateBranch(ctx, "beta", base.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.client.Branch(ctx, "feature"); err != nil || got != (project.Branch{Name: "feature", Tip: base.ID}) {
		t.Fatalf("Branch(feature) = %#v, %v", got, err)
	}
	if _, err := f.client.Branch(ctx, "missing"); !errors.Is(err, project.ErrBranchNotFound) {
		t.Fatalf("Branch(missing) = %v, want ErrBranchNotFound", err)
	}
	page1, next, err := f.client.Branches(ctx, "", 2)
	if err != nil || len(page1) != 2 || page1[0].Name != "alpha" || page1[1].Name != "beta" || next != "beta" {
		t.Fatalf("first Branches page = %#v next=%q err=%v", page1, next, err)
	}
	page2, next, err := f.client.Branches(ctx, next, 2)
	if err != nil || len(page2) != 2 || page2[0].Name != "feature" || page2[1].Name != "main" || next != "" {
		t.Fatalf("second Branches page = %#v next=%q err=%v", page2, next, err)
	}

	mainData := "main moves independently"
	main, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "branch-transport-main", Branch: "main", Base: base.ID, Author: "test", Message: "move main",
		Changes: []project.Change{change("main.txt", mainData)},
	}, opener(map[string]string{"main.txt": mainData}, nil))
	if err != nil {
		t.Fatal(err)
	}
	featureData := "feature moves independently"
	feature, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "branch-transport-feature", Branch: "feature", Base: base.ID, Author: "test", Message: "move feature",
		Changes: []project.Change{change("feature.txt", featureData)},
	}, opener(map[string]string{"feature.txt": featureData}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(feature.Parents) != 1 || feature.Parents[0] != base.ID {
		t.Fatalf("feature commit parents = %#v, want %q", feature.Parents, base.ID)
	}
	if got, err := f.client.Branch(ctx, "main"); err != nil || got.Tip != main.ID {
		t.Fatalf("main branch after feature move = %#v, %v", got, err)
	}
	if got, err := f.client.Branch(ctx, "feature"); err != nil || got.Tip != feature.ID {
		t.Fatalf("feature branch after main move = %#v, %v", got, err)
	}
	if err := f.client.DeleteBranch(ctx, "main", base.ID); !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("stale branch deletion = %v, want ErrStaleHead", err)
	}
	if err := f.client.DeleteBranch(ctx, "feature", base.ID); !errors.Is(err, project.ErrStaleHead) {
		t.Fatalf("stale feature deletion = %v, want ErrStaleHead", err)
	}
	if err := f.client.DeleteBranch(ctx, "feature", feature.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.client.DeleteBranch(ctx, "feature", feature.ID); err != nil {
		t.Fatalf("repeat branch delete: %v", err)
	}
	if err := f.client.DeleteBranch(ctx, "main", main.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Branch(ctx, "main"); !errors.Is(err, project.ErrBranchNotFound) {
		t.Fatalf("Branch(main) after delete = %v", err)
	}
	if got, err := f.client.Commit(ctx, feature.ID); err != nil || got.ID != feature.ID {
		t.Fatalf("deleting branch removed retained commit: %#v, %v", got, err)
	}
	reader, _, err := f.client.OpenFile(ctx, feature.ID, "base.txt")
	if err != nil {
		t.Fatalf("deleting branch removed retained payload: %v", err)
	}
	contents, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(contents) != baseData {
		t.Fatalf("retained branch commit payload = %q, errors %v / %v", contents, readErr, closeErr)
	}
}

func TestTransportAuthenticationAndMalformedBodies(t *testing.T) {
	f := newFixture(t)
	httpClient := f.server.Client()
	request, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/info", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodPost, f.server.URL+"/v1/entries", strings.NewReader(`{"commit":"","rules":[],"after":"","limit":10,"extra":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.local.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err = httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown JSON field status = %d, want 400", response.StatusCode)
	}

	// A zero-file request with an unexpected trailing part is rejected before
	// the project operation can be reserved or accepted.
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	manifest, err := writer.CreateFormField("manifest")
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := json.Marshal(project.ChangeRequest{OperationID: "trailing-part", Branch: "main", Author: "test", Message: "bad multipart"})
	if _, err := manifest.Write(sealed); err != nil {
		t.Fatal(err)
	}
	extra, err := writer.CreateFormFile("unexpected", "data")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = extra.Write([]byte("unrequested"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err = http.NewRequest(http.MethodPost, f.server.URL+"/v1/changes", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.local.Token)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err = httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("trailing multipart status = %d, want 400", response.StatusCode)
	}
	if _, err := f.client.Operation(context.Background(), "trailing-part"); !errors.Is(err, project.ErrOperationNotFound) {
		t.Fatalf("operation after malformed upload = %v, want not found", err)
	}

	emptyDigest := sha256.Sum256(nil)
	emptyChange := project.Change{Path: "empty", Kind: model.File, SHA256: hex.EncodeToString(emptyDigest[:]), Size: 0}
	for _, testCase := range []struct {
		name        string
		includeFile bool
		trailing    bool
	}{
		{name: "missing-file"},
		{name: "trailing-file", includeFile: true, trailing: true},
	} {
		var upload bytes.Buffer
		mw := multipart.NewWriter(&upload)
		manifest, err := mw.CreateFormField("manifest")
		if err != nil {
			t.Fatal(err)
		}
		requestValue := project.ChangeRequest{OperationID: testCase.name, Branch: "main", Author: "test", Message: "bad file upload", Changes: []project.Change{emptyChange}}
		manifestBytes, _ := json.Marshal(requestValue)
		if _, err := manifest.Write(manifestBytes); err != nil {
			t.Fatal(err)
		}
		if testCase.includeFile {
			part, err := mw.CreateFormFile("file:0", "data")
			if err != nil {
				t.Fatal(err)
			}
			if testCase.trailing {
				extra, err := mw.CreateFormFile("file:1", "data")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = extra.Write([]byte("extra"))
			}
			_ = part
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		uploadRequest, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/changes", &upload)
		if err != nil {
			t.Fatal(err)
		}
		uploadRequest.Header.Set("Authorization", "Bearer "+f.local.Token)
		uploadRequest.Header.Set("Content-Type", mw.FormDataContentType())
		uploadResponse, err := httpClient.Do(uploadRequest)
		if err != nil {
			t.Fatal(err)
		}
		uploadResponse.Body.Close()
		if uploadResponse.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", testCase.name, uploadResponse.StatusCode)
		}
		operation, err := f.client.Operation(context.Background(), testCase.name)
		if err != nil || operation.State != project.OperationCapturing {
			t.Fatalf("%s operation = %+v, %v; want unaccepted capture", testCase.name, operation, err)
		}
		if _, err := f.client.Resume(context.Background(), testCase.name); !errors.Is(err, project.ErrUploadRequired) {
			t.Fatalf("%s Resume() = %v, want upload required", testCase.name, err)
		}
		if _, err := f.client.Commit(context.Background(), operation.CommitID); !errors.Is(err, project.ErrCommitNotFound) {
			t.Fatalf("%s candidate commit = %v, want not found", testCase.name, err)
		}
		if err := f.client.Abort(context.Background(), testCase.name); err != nil {
			t.Fatalf("%s Abort(): %v", testCase.name, err)
		}
		operation, err = f.client.Operation(context.Background(), testCase.name)
		if err != nil || operation.State != project.OperationAborted {
			t.Fatalf("%s operation after abort = %+v, %v", testCase.name, operation, err)
		}
	}

	if _, err := NewClient("http://example.com", "token"); err == nil {
		t.Fatal("NewClient accepted non-loopback host")
	}
	if _, err := NewClient("https://example.com", "token"); err != nil {
		t.Fatalf("NewClient rejected HTTPS origin: %v", err)
	}
	for _, base := range []string{"http://localhost/path", "http://localhost?x=1", "http://localhost#fragment", "http://user@localhost", "ftp://localhost"} {
		if _, err := NewClient(base, "token"); err == nil {
			t.Errorf("NewClient(%q) succeeded", base)
		}
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/info", http.StatusFound)
	}))
	defer redirector.Close()
	redirectClient, err := NewClient(redirector.URL, f.local.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := redirectClient.Info(context.Background()); !errors.Is(err, ErrRemote) {
		t.Fatalf("redirect response = %v, want remote HTTP error", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect target received %d requests, want 0", redirected.Load())
	}
}

type blockingUploadReader struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (r *blockingUploadReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return 0, errors.New("upload source closed")
}

func (r *blockingUploadReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

func TestClientClosesUploadSourceWhenServerStopsReading(t *testing.T) {
	readStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/operation" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"operation_not_found","message":"operation not found"}`)
			return
		}
		if r.URL.Path != "/v1/changes" {
			http.NotFound(w, r)
			return
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			http.Error(w, "full duplex unavailable", http.StatusInternalServerError)
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		parts := multipart.NewReader(r.Body, params["boundary"])
		manifest, err := parts.NextPart()
		if err != nil {
			http.Error(w, "missing manifest", http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, manifest)
		filePart, err := parts.NextPart()
		if err != nil || filePart.FormName() != "file:0" {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		select {
		case <-readStarted:
		case <-time.After(2 * time.Second):
			http.Error(w, "source did not start", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"invalid_upload","message":"invalid multipart upload"}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockingUploadReader{started: readStarted, closed: make(chan struct{})}
	digest := sha256.Sum256([]byte("0123456789"))
	request := project.ChangeRequest{
		OperationID: "blocked-upload", Branch: "main", Author: "test", Message: "stop reading",
		Changes: []project.Change{{Path: "blocked", Kind: model.File, SHA256: hex.EncodeToString(digest[:]), Size: 10}},
	}
	result := make(chan error, 1)
	go func() {
		_, err := client.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) { return blocked, nil })
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Apply unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply hung after early server response")
	}
	select {
	case <-blocked.closed:
	case <-time.After(time.Second):
		t.Fatal("client did not close the active upload source")
	}
}

func TestClientKeepsSuccessfulOutcomeWhenServerStopsReading(t *testing.T) {
	readStarted := make(chan struct{})
	accepted := project.Commit{ID: "accepted-before-upload-finished", Author: "server"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/operation" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"operation_not_found","message":"operation not found"}`)
			return
		}
		if r.URL.Path != "/v1/changes" {
			http.NotFound(w, r)
			return
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			http.Error(w, "full duplex unavailable", http.StatusInternalServerError)
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		parts := multipart.NewReader(r.Body, params["boundary"])
		manifest, err := parts.NextPart()
		if err != nil {
			http.Error(w, "missing manifest", http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, manifest)
		filePart, err := parts.NextPart()
		if err != nil || filePart.FormName() != "file:0" {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		select {
		case <-readStarted:
		case <-time.After(2 * time.Second):
			http.Error(w, "source did not start", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(accepted)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockingUploadReader{started: readStarted, closed: make(chan struct{})}
	digest := sha256.Sum256([]byte("0123456789"))
	request := project.ChangeRequest{
		OperationID: "accepted-upload-retry", Branch: "main", Author: "test", Message: "already accepted",
		Changes: []project.Change{{Path: "blocked", Kind: model.File, SHA256: hex.EncodeToString(digest[:]), Size: 10}},
	}
	result := make(chan struct {
		commit project.Commit
		err    error
	}, 1)
	go func() {
		commit, err := client.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) { return blocked, nil })
		result <- struct {
			commit project.Commit
			err    error
		}{commit, err}
	}()
	select {
	case got := <-result:
		if got.err != nil || got.commit.ID != accepted.ID {
			t.Fatalf("Apply() = %+v, %v; want authoritative accepted result", got.commit, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply hung after successful early server response")
	}
	select {
	case <-blocked.closed:
	case <-time.After(time.Second):
		t.Fatal("client did not close active source after successful response")
	}
}

type trackedReadCloser struct {
	reader *bytes.Reader
	closed bool
}

func (r *trackedReadCloser) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r *trackedReadCloser) Close() error               { r.closed = true; return nil }

func TestStreamMultipartClosesReaderReturnedAlongsideOpenError(t *testing.T) {
	data := &trackedReadCloser{reader: bytes.NewReader([]byte("data"))}
	request := project.ChangeRequest{Changes: []project.Change{{Path: "file", Kind: model.File, Size: 4}}}
	manifest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _, done, cancel := streamMultipart(context.Background(), manifest, request.Changes, func(model.Path) (io.ReadCloser, error) {
		return data, errors.New("open failed after allocating reader")
	})
	defer cancel()
	_, _ = io.Copy(io.Discard, body)
	if err := <-done; err == nil {
		t.Fatal("streamMultipart succeeded despite opener error")
	}
	if !data.closed {
		t.Fatal("reader returned with opener error was not closed")
	}
}
