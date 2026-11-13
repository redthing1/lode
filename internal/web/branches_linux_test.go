package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

func TestBranchAPIEnforcesRolesAndProjectIdentity(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	b := model.Reference{Name: "experiment"}
	if _, err := f.client.CreateBranch(ctx, "alice", "project", b); err != nil {
		t.Fatal(err)
	}
	if page, err := f.client.Branches(ctx, "alice", "project", ""); err != nil || len(page.Branches) != 2 {
		t.Fatalf("branches: %v %v", page, err)
	}
	if v, err := f.client.Reference(ctx, "alice", "project", "experiment"); err != nil || v.ID != "" {
		t.Fatalf("empty branch: %v %v", v, err)
	}
	var failure *remote.Error
	if err := f.client.DeleteBranch(ctx, "alice", "project", b); !errors.As(err, &failure) || failure.Code != "access" {
		t.Fatalf("writer deletion: %v", err)
	}
	client := func(role instance.Role) *remote.Client {
		t.Helper()
		token, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "role-test", Project: f.project.ID, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		c, err := remote.New(f.origin, token)
		if err != nil {
			t.Fatal(err)
		}
		c.InstanceID, c.ProjectID = f.store.ID, f.project.ID
		t.Cleanup(c.HTTP.CloseIdleConnections)
		return c
	}
	reader := client(instance.Reader)
	if _, err := reader.CreateBranch(ctx, "alice", "project", model.Reference{Name: "forbidden"}); !errors.As(err, &failure) || failure.Code != "access" {
		t.Fatalf("reader creation: %v", err)
	}
	maintainer := client(instance.Maintainer)
	maintainer.ProjectID = "other-project"
	if err := maintainer.DeleteBranch(ctx, "alice", "project", b); !errors.As(err, &failure) || failure.Code != "identity" {
		t.Fatalf("wrong identity deletion: %v", err)
	}
	maintainer.ProjectID = f.project.ID
	if err := maintainer.DeleteBranch(ctx, "alice", "project", b); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Head(ctx, "alice", "project", b.Name); !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("deleted branch: %v", err)
	}
}

func TestBrowserBranchFilesAndHistory(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	browser := newBrowserSession(t, f.origin)
	browser.request("POST", "/login", url.Values{"username": {"alice"}, "password": {"a sufficiently long test password"}}, 303)
	if _, err := f.client.CreateBranch(ctx, "alice", "project", model.Reference{Name: "experiment/model"}); err != nil {
		t.Fatal(err)
	}
	contents := "# Branch contents\n\n[Project file](README.md)\n"
	v, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "experiment/model", Message: "Experimental model", Changes: []model.Entry{entry("README.md", []byte(contents)), {Path: "docs", Kind: model.Directory}, entry("docs/note.txt", []byte(contents))}}, nil, func(model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(contents)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page := browser.request("GET", "/alice/project?branch=experiment%2Fmodel", nil, 200)
	if !strings.Contains(page, "Branch contents") || !strings.Contains(page, "Experimental model") || !strings.Contains(page, `class="branch-picker"`) {
		t.Fatal("branch files or picker missing")
	}
	if !strings.Contains(page, "lode clone --branch experiment/model "+f.origin+"/alice/project") {
		t.Fatal("clone command does not follow the selected branch")
	}
	if strings.Contains(page, `class="ref-kind"`) || strings.Contains(page, `class="latest-link"`) {
		t.Fatal("live branch displayed as a saved version")
	}
	projectInfo := instance.Project{Owner: "alice", Name: "project"}
	folderURL := referenceTreeURL(projectInfo, "experiment/model", "", "docs", "")
	if !strings.Contains(page, `href="`+html.EscapeString(folderURL)+`"`) {
		t.Fatal("folder navigation does not preserve the live branch")
	}
	folder := browser.request("GET", folderURL, nil, 200)
	if !strings.Contains(folder, "lode clone --branch experiment/model ") || strings.Contains(folder, `class="ref-kind"`) {
		t.Fatal("browsing a folder lost branch context or changed the clone target")
	}
	snapshotLink := referenceTreeURL(projectInfo, "experiment/model", v.ID, "README.md", "")
	if !strings.Contains(page, `href="`+html.EscapeString(snapshotLink)+`"`) {
		t.Fatal("Markdown link lost its immutable version or originating branch")
	}
	pinned := browser.request("GET", "/alice/project?branch=experiment%2Fmodel&version="+v.ID+"&path=README.md", nil, 200)
	if !strings.Contains(pinned, `class="ref-kind">Version</span>`) || !strings.Contains(pinned, `class="latest-link" href="/alice/project?branch=experiment%2Fmodel"`) {
		t.Fatal("pinned version lacks distinct context or branch return link")
	}
	if !strings.Contains(pinned, "lode clone --branch experiment/model ") || !strings.Contains(pinned, "Clones the latest experiment/model, not this saved version.") {
		t.Fatal("saved version clone targets a different branch than its return link")
	}
	snapshotFolder := referenceTreeURL(projectInfo, "experiment/model", v.ID, "docs", "")
	root := browser.request("GET", "/alice/project?branch=experiment%2Fmodel&version="+v.ID, nil, 200)
	if !strings.Contains(root, `href="`+html.EscapeString(snapshotFolder)+`"`) {
		t.Fatal("saved directory navigation loses version or branch context")
	}
	pinned = browser.request("GET", "/alice/project?version="+v.ID+"&path=README.md", nil, 200)
	if !strings.Contains(pinned, `class="latest-link" href="/alice/project?branch=main"`) {
		t.Fatal("unqualified saved version lacks an explicit main return link")
	}
	page = browser.request("GET", "/alice/project/history?branch=experiment%2Fmodel", nil, 200)
	if !strings.Contains(page, "Experimental model") || !strings.Contains(page, `>experiment/model</span>`) {
		t.Fatal("history followed the wrong branch")
	}
	page = browser.request("GET", "/alice/project/history?from="+v.ID, nil, 200)
	if !strings.Contains(page, `>`+model.ShortVersion(v.ID)+`</span>`) {
		t.Fatal("immutable history mislabeled as main")
	}
	page = browser.request("GET", "/alice/project", nil, 200)
	if !strings.Contains(page, "No saved versions") || strings.Contains(page, "Branch contents") {
		t.Fatal("main unexpectedly changed")
	}
	page = browser.request("GET", "/alice/project/branches", nil, 200)
	if !strings.Contains(page, "experiment/model") || !strings.Contains(page, "main") || !strings.Contains(page, model.ShortVersion(v.ID)) {
		t.Fatal("branch listing missing tips")
	}
	counts := &countedTransport{RoundTripper: f.client.HTTP.Transport}
	f.client.HTTP.Transport = counts
	merged, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Merge: v.ID, Message: "Integrate model"}, nil, nil)
	if err != nil || merged.MergeParent != v.ID {
		t.Fatalf("metadata-only integration: %+v %v", merged, err)
	}
	if counts.upload.Load() > 2048 || counts.signature.Load() != 0 {
		t.Fatalf("unexpected integration transfer: upload %d, signature %d", counts.upload.Load(), counts.signature.Load())
	}
	page = browser.request("GET", "/alice/project/history", nil, 200)
	if !strings.Contains(page, "Integrate model") || !strings.Contains(page, "merged <a") || !strings.Contains(page, model.ShortVersion(v.ID)) {
		t.Fatal("merge history/source link missing")
	}
}

func TestBrowserPaginationKeepsItsVersionWhenBranchAdvances(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	browser := newBrowserSession(t, f.origin)
	browser.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	var changes []model.Entry
	for i := range 102 {
		changes = append(changes, entry(fmt.Sprintf("asset-%03d.bin", i), []byte("x")))
	}
	read := func(model.Entry) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("x")), nil }
	first, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Message: "Batch baseline", Changes: changes}, nil, read)
	if err != nil {
		t.Fatal(err)
	}
	body := browser.request("GET", "/alice/project", nil, 200)
	match := regexp.MustCompile(`class="pagination"><a[^>]*href="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("missing next page")
	}
	next := html.UnescapeString(match[1])
	parsed, err := url.Parse(next)
	if err != nil || parsed.Query().Get("version") != first.ID || parsed.Query().Get("branch") != "main" {
		t.Fatalf("pagination lost its snapshot or branch context: %s", next)
	}
	_, err = f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Base: first.ID, Message: "Later version", Changes: []model.Entry{entry("asset-999.bin", []byte("x"))}}, nil, read)
	if err != nil {
		t.Fatal(err)
	}
	body = browser.request("GET", next, nil, 200)
	if !strings.Contains(body, "asset-100.bin") || !strings.Contains(body, "asset-101.bin") || !strings.Contains(body, "Batch baseline") || strings.Contains(body, "asset-999.bin") {
		t.Fatal("next page mixed branch versions")
	}
}
