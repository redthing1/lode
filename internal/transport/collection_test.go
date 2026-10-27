package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestClientCollectionAndCapacityPolicyKeepProtectedPayload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	body := "protected collection payload"
	commit, err := f.client.Apply(ctx, project.ChangeRequest{
		OperationID: "collection-protected", Branch: "main", Author: "test", Message: "keep this payload",
		Changes: []project.Change{change("protected.bin", body)},
	}, opener(map[string]string{"protected.bin": body}, nil))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := f.client.CapacityPolicy(ctx)
	if err != nil || policy != project.DefaultCapacityPolicy() {
		t.Fatalf("CapacityPolicy() = %+v, %v", policy, err)
	}
	wantPolicy := project.CapacityPolicy{ReserveBytes: 2 << 30}
	if err := f.client.SetCapacityPolicy(ctx, wantPolicy); err != nil {
		t.Fatal(err)
	}
	if policy, err = f.client.CapacityPolicy(ctx); err != nil || policy != wantPolicy {
		t.Fatalf("CapacityPolicy() after update = %+v, %v", policy, err)
	}
	result, err := f.client.Collect(ctx, project.CollectRequest{MaxRewriteBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Collect() = %+v, %v", result, err)
	}
	if result.Before.BlobCount == 0 || result.After.BlobCount == 0 {
		t.Fatalf("collection omitted blob measurements: %+v", result)
	}
	reader, node, err := f.client.OpenFile(ctx, commit.ID, "protected.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(got) != body || node.Kind != model.File {
		t.Fatalf("protected file after collection = %q, read=%v close=%v node=%+v", got, readErr, closeErr, node)
	}
}

func TestCollectionRoutesRejectUnauthorizedAndInvalidRequests(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		method, path, body, token string
		wantStatus                int
	}{
		{method: http.MethodGet, path: "/v1/storage/capacity", wantStatus: http.StatusUnauthorized},
		{method: http.MethodGet, path: "/v1/storage/capacity?extra=1", token: f.local.Token, wantStatus: http.StatusBadRequest},
		{method: http.MethodPost, path: "/v1/storage/capacity", body: `{"reserveBytes":0}`, token: f.local.Token, wantStatus: http.StatusBadRequest},
		{method: http.MethodPost, path: "/v1/storage/capacity", body: `{"reserveBytes":1,"extra":true}`, token: f.local.Token, wantStatus: http.StatusBadRequest},
		{method: http.MethodPost, path: "/v1/storage/collect", body: `{"maxRewriteBytes":1073741825}`, token: f.local.Token, wantStatus: http.StatusBadRequest},
		{method: http.MethodPost, path: "/v1/storage/collect", body: `{"maxRewriteBytes":1,"extra":true}`, token: f.local.Token, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		request, err := http.NewRequest(test.method, f.server.URL+test.path, strings.NewReader(test.body))
		if err != nil {
			t.Fatal(err)
		}
		if test.token != "" {
			request.Header.Set("Authorization", "Bearer "+test.token)
		}
		if test.body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != test.wantStatus {
			t.Errorf("%s %s status=%d, want %d", test.method, test.path, response.StatusCode, test.wantStatus)
		}
	}
}

func TestCapacityWireErrorsRetainCategories(t *testing.T) {
	for _, test := range []struct {
		err        error
		status     int
		code       string
		wantClient error
	}{
		{project.ErrInsufficientCapacity, http.StatusInsufficientStorage, "insufficient_capacity", project.ErrInsufficientCapacity},
		{project.ErrCapacityUnknown, http.StatusServiceUnavailable, "capacity_unknown", project.ErrCapacityUnknown},
	} {
		status, wire := mapError(test.err)
		if status != test.status || wire.Code != test.code {
			t.Fatalf("mapError(%v) = %d %+v", test.err, status, wire)
		}
		if !errors.Is(errorSentinel(wire.Code), test.wantClient) {
			t.Fatalf("wire code %q did not map back to %v", wire.Code, test.wantClient)
		}
	}
}
