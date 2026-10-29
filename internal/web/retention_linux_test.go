package web

import (
	"context"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

func retentionVersions(t *testing.T, f apiFixture) []model.Version {
	t.Helper()
	var versions []model.Version
	base := ""
	for _, text := range []string{"protected", "obsolete <script>", "current"} {
		v, err := f.client.Save(context.Background(), "alice", "project", model.Save{ID: operation(t), Branch: "main", Base: base, Message: text, Changes: []model.Entry{entry("a", []byte(text))}}, nil, func(model.Entry) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(text)), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
		base = v.ID
	}
	return versions
}

func TestRetentionAPIAuthorityAndExpiredReads(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	versions := retentionVersions(t, f)
	tag := model.Reference{Name: "release/one", Version: versions[0].ID}
	if _, err := f.client.CreateTag(ctx, "alice", "project", tag); err != nil {
		t.Fatal(err)
	}
	client := func(role instance.Role) *remote.Client {
		t.Helper()
		token, err := f.store.CreateToken(ctx, f.user, instance.TokenOptions{Name: "retention-test", Project: f.project.ID, Role: role})
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
	reader, maintainer := client(instance.Reader), client(instance.Maintainer)
	wantError := func(err error, code string) {
		t.Helper()
		var failure *remote.Error
		if !errors.As(err, &failure) || failure.Code != code {
			t.Fatalf("wanted %s: %v", code, err)
		}
	}
	if page, err := reader.Tags(ctx, "alice", "project", ""); err != nil || len(page.Tags) != 1 || page.Tags[0] != tag {
		t.Fatalf("reader tags: %+v %v", page, err)
	}
	_, err := reader.CreateTag(ctx, "alice", "project", model.Reference{Name: "forbidden", Version: versions[1].ID})
	wantError(err, "access")
	policy := model.Retention{Versions: 1, HistoryDays: 1}
	for _, c := range []*remote.Client{reader, f.client} {
		wantError(c.SetRetention(ctx, "alice", "project", policy), "access")
		wantError(c.DeleteTag(ctx, "alice", "project", tag), "access")
		_, err := c.Cleanup(ctx, "alice", "project", "")
		wantError(err, "access")
		_, err = c.Collect(ctx, "alice", "project")
		wantError(err, "access")
	}
	maintainer.ProjectID = "different-project"
	wantError(maintainer.SetRetention(ctx, "alice", "project", policy), "identity")
	maintainer.ProjectID = f.project.ID
	if err := maintainer.SetRetention(ctx, "alice", "project", policy); err != nil {
		t.Fatal(err)
	}
	p, err := maintainer.Cleanup(ctx, "alice", "project", "")
	if err != nil || p.Retained != 2 || p.Expire != 1 || p.Forget != 0 {
		t.Fatalf("preview: %+v %v", p, err)
	}
	temporary := model.Reference{Name: "temporary", Version: versions[1].ID}
	if _, err := f.client.CreateTag(ctx, "alice", "project", temporary); err != nil {
		t.Fatal(err)
	}
	_, err = maintainer.Cleanup(ctx, "alice", "project", p.Token)
	wantError(err, "cleanup")
	if err := maintainer.DeleteTag(ctx, "alice", "project", temporary); err != nil {
		t.Fatal(err)
	}
	p, err = maintainer.Cleanup(ctx, "alice", "project", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maintainer.Cleanup(ctx, "alice", "project", p.Token); err != nil {
		t.Fatal(err)
	}
	if result, err := maintainer.Collect(ctx, "alice", "project"); err != nil || result.Before.Bytes == 0 || result.After.Bytes == 0 {
		t.Fatalf("collection: %+v %v", result, err)
	}
	wantError(reader.Download(ctx, "alice", "project", versions[1].ID, entry("a", []byte("obsolete <script>")), nil, 0, io.Discard), "expired")
	for _, v := range []model.Version{versions[0], versions[2]} {
		if err := reader.Download(ctx, "alice", "project", v.ID, entry("a", []byte(v.Message)), nil, 0, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	history, err := reader.History(ctx, "alice", "project", "main", 100)
	if err != nil || len(history.Versions) != 3 || history.Versions[1].Retained {
		t.Fatalf("expired history: %+v %v", history, err)
	}
}

func TestBrowserTagAndCleanupReview(t *testing.T) {
	f := apiTest(t)
	versions := retentionVersions(t, f)
	alice, bob := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	password := "a sufficiently long test password"
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {password}}, 303)
	alice.request("POST", "/admin/users/new", url.Values{"username": {"bob"}, "password": {password}}, 303)
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {password}}, 303)
	members, tags, storage := "/alice/project/settings/members", "/alice/project/tags", "/alice/project/settings/storage"
	alice.request("POST", members, url.Values{"username": {"bob"}, "role": {"writer"}}, 303)
	bob.request("POST", tags, url.Values{"name": {"release/one"}, "from": {versions[0].ID}}, 303)
	bob.request("GET", storage, nil, 403)
	bob.request("POST", storage, url.Values{"action": {"preview"}}, 403)
	bob.request("POST", tags, url.Values{"action": {"delete"}, "name": {"release/one"}, "version": {versions[0].ID}}, 403)
	alice.request("POST", members, url.Values{"username": {"bob"}, "role": {"reader"}}, 303)
	bob.request("POST", tags, url.Values{"name": {"forbidden"}, "from": {"main"}}, 403)
	if body := bob.request("GET", tags, nil, 200); !strings.Contains(body, "release/one") || strings.Contains(body, "Create tag") || strings.Contains(body, ">Remove</button>") {
		t.Fatal("reader tag controls")
	}
	if body := alice.request("POST", tags, url.Values{"name": {"missing"}, "from": {"missing"}}, 400); !strings.Contains(body, "Version not found.") {
		t.Fatal("missing-version guidance")
	}
	alice.request("POST", storage, url.Values{"action": {"policy"}, "versions": {"1"}, "history_days": {"90"}}, 200)
	preview := func() string {
		t.Helper()
		body := alice.request("POST", storage, url.Values{"action": {"preview"}}, 200)
		if !strings.Contains(body, "obsolete &lt;script&gt;") || strings.Contains(body, "obsolete <script>") || !strings.Contains(body, "Apply cleanup") {
			t.Fatal("cleanup consequences missing or unescaped")
		}
		match := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(body)
		if len(match) != 2 {
			t.Fatal("cleanup review missing")
		}
		return match[1]
	}
	stale := preview()
	alice.request("POST", storage, url.Values{"action": {"policy"}, "versions": {"1"}, "history_days": {"80"}}, 200)
	alice.request("POST", storage, url.Values{"action": {"apply"}, "token": {stale}}, 409)
	token := preview()
	if body := alice.request("POST", storage, url.Values{"action": {"apply"}, "token": {token}}, 200); !strings.Contains(body, "Cleanup applied") {
		t.Fatal("cleanup outcome missing")
	}
	bob.request("POST", storage, url.Values{"action": {"collect"}}, 403)
	if body := alice.request("POST", storage, url.Values{"action": {"collect"}}, 200); !strings.Contains(body, "Collection complete") || !strings.Contains(body, "Allocated space:") {
		t.Fatal("collection outcome missing")
	}
	bob.request("GET", "/alice/project/raw/"+versions[1].ID+"/a", nil, 410)
	if got := bob.request("GET", "/alice/project/raw/"+versions[0].ID+"/a", nil, 200); got != "protected" {
		t.Fatal("protected contents lost")
	}
	if body := bob.request("GET", "/alice/project/history", nil, 200); !strings.Contains(body, "Contents expired") {
		t.Fatal("expired history not identified")
	}
}
