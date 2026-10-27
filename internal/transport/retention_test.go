package transport

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/project"
)

func TestClientServerTagsRetentionAndStorageStats(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "retention-first", Branch: "main", Author: "test", Message: "first",
		Changes: []project.Change{change("file.bin", "first")},
	}, opener(map[string]string{"file.bin": "first"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "retention-second", Branch: "main", Base: first.ID, Author: "test", Message: "second",
		Changes: []project.Change{change("file.bin", "second")},
	}, opener(map[string]string{"file.bin": "second"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.SetRetentionPolicy(ctx, project.RetentionPolicy{}); err != nil {
		t.Fatal(err)
	}
	if policy, err := f.client.RetentionPolicy(ctx); err != nil || policy != (project.RetentionPolicy{}) {
		t.Fatalf("RetentionPolicy() = %+v, %v", policy, err)
	}
	if err := f.client.CreateTag(ctx, "release/v1", first.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.client.Tag(ctx, "release/v1"); err != nil || got != (project.Tag{Name: "release/v1", CommitID: first.ID}) {
		t.Fatalf("Tag() = %+v, %v", got, err)
	}
	tags, next, err := f.client.Tags(ctx, "", 1)
	if err != nil || next != "" || len(tags) != 1 || tags[0].CommitID != first.ID {
		t.Fatalf("Tags() = %+v, next=%q, err=%v", tags, next, err)
	}
	availability, err := f.client.ExplainRetention(ctx, first.ID)
	if err != nil || availability.State != "retained" || !containsReason(availability.Reasons, "tag:release/v1") {
		t.Fatalf("ExplainRetention() = %+v, %v", availability, err)
	}
	preview, err := f.client.PreviewRetention(ctx, "", 10)
	if err != nil || len(preview.Candidates) != 0 {
		t.Fatalf("PreviewRetention() = %+v, %v; branch tip and tag should remain protected", preview, err)
	}
	expired, err := f.client.Expire(ctx, []string{first.ID})
	if err != nil || len(expired.Expired) != 0 || len(expired.Protected) != 1 || !containsReason(expired.Protected[0].Reasons, "tag:release/v1") {
		t.Fatalf("Expire(tagged) = %+v, %v", expired, err)
	}
	statuses, err := f.client.PayloadStatuses(ctx, []string{first.ID, second.ID})
	if err != nil || len(statuses) != 2 || statuses[0].State != "retained" || statuses[1].State != "retained" {
		t.Fatalf("PayloadStatuses() = %+v, %v", statuses, err)
	}
	stats, err := f.client.StorageStats(ctx)
	if err != nil || stats.BlobCount <= 0 || stats.PackCount <= 0 {
		t.Fatalf("StorageStats() = %+v, %v", stats, err)
	}
	if err := f.client.DeleteTag(ctx, "release/v1", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Tag(ctx, "release/v1"); err == nil {
		t.Fatal("deleted tag remained visible")
	}
}

func TestRetentionRouteStrictJSONAndQueryValidation(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		method, path, body string
		wantStatus         int
	}{
		{method: http.MethodPost, path: "/v1/tag/create", body: `{"name":"x","commitID":"id","extra":true}`, wantStatus: http.StatusBadRequest},
		{method: http.MethodGet, path: "/v1/retention/policy?extra=1", wantStatus: http.StatusBadRequest},
	} {
		request, err := http.NewRequest(test.method, f.server.URL+test.path, strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+f.local.Token)
		if test.body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != test.wantStatus {
			t.Errorf("%s %s status=%d, want %d", test.method, test.path, response.StatusCode, test.wantStatus)
		}
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
