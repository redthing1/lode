package transport

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/redthing1/lode/internal/project"
)

// retentionRoutes handles retention and storage endpoints. The main handler
// calls it only after authentication and API-prefix validation.
func (s *server) retentionRoutes(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/tag":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return true
		}
		q, ok := readQuery(w, r, "name")
		if !ok {
			return true
		}
		result, err := s.service.Tag(r.Context(), q.Get("name"))
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, result)
		return true
	case "/v1/tags":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return true
		}
		q, ok := readQuery(w, r, "after", "limit")
		if !ok {
			return true
		}
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			invalidRequest(w)
			return true
		}
		tags, next, err := s.service.Tags(r.Context(), q.Get("after"), limit)
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, struct {
			Tags []project.Tag `json:"tags"`
			Next string        `json:"next"`
		}{tags, next})
		return true
	case "/v1/tag/create":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return true
		}
		var request struct {
			Name     string `json:"name"`
			CommitID string `json:"commitID"`
		}
		if !decodeJSONBody(w, r, &request) {
			return true
		}
		if err := s.service.CreateTag(r.Context(), request.Name, request.CommitID); err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, struct{}{})
		return true
	case "/v1/tag/delete":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return true
		}
		var request struct {
			Name             string `json:"name"`
			ExpectedCommitID string `json:"expectedCommitID"`
		}
		if !decodeJSONBody(w, r, &request) {
			return true
		}
		if err := s.service.DeleteTag(r.Context(), request.Name, request.ExpectedCommitID); err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, struct{}{})
		return true
	case "/v1/retention/policy":
		if r.Method == http.MethodGet {
			if len(r.URL.Query()) != 0 {
				invalidRequest(w)
				return true
			}
			policy, err := s.service.RetentionPolicy(r.Context())
			if err != nil {
				writeServiceError(w, err)
				return true
			}
			writeJSON(w, policy)
			return true
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return true
		}
		var policy project.RetentionPolicy
		if !decodeJSONBody(w, r, &policy) {
			return true
		}
		if err := s.service.SetRetentionPolicy(r.Context(), policy); err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, struct{}{})
		return true
	case "/v1/retention/preview":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return true
		}
		q, ok := readQuery(w, r, "after", "limit")
		if !ok {
			return true
		}
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			invalidRequest(w)
			return true
		}
		result, err := s.service.PreviewRetention(r.Context(), q.Get("after"), limit)
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, result)
		return true
	case "/v1/retention/expire":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return true
		}
		var request struct {
			CommitIDs []string `json:"commitIDs"`
		}
		if !decodeJSONBody(w, r, &request) {
			return true
		}
		result, err := s.service.Expire(r.Context(), request.CommitIDs)
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, result)
		return true
	case "/v1/retention/explain":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return true
		}
		q, ok := readQuery(w, r, "id")
		if !ok {
			return true
		}
		result, err := s.service.ExplainRetention(r.Context(), q.Get("id"))
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, result)
		return true
	case "/v1/retention/status":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return true
		}
		var request struct {
			CommitIDs []string `json:"commitIDs"`
		}
		if !decodeJSONBody(w, r, &request) {
			return true
		}
		result, err := s.service.PayloadStatuses(r.Context(), request.CommitIDs)
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, struct {
			Versions []project.Availability `json:"versions"`
		}{result})
		return true
	case "/v1/storage/stats":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return true
		}
		if len(r.URL.Query()) != 0 {
			invalidRequest(w)
			return true
		}
		stats, err := s.service.StorageStats(r.Context())
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, stats)
		return true
	default:
		return false
	}
}

func (c *Client) CreateTag(ctx context.Context, name, commitID string) error {
	return c.postJSON(ctx, "/v1/tag/create", struct {
		Name     string `json:"name"`
		CommitID string `json:"commitID"`
	}{name, commitID}, nil)
}

func (c *Client) DeleteTag(ctx context.Context, name, expectedCommitID string) error {
	return c.postJSON(ctx, "/v1/tag/delete", struct {
		Name             string `json:"name"`
		ExpectedCommitID string `json:"expectedCommitID"`
	}{name, expectedCommitID}, nil)
}

func (c *Client) Tag(ctx context.Context, name string) (project.Tag, error) {
	var result project.Tag
	err := c.getJSON(ctx, "/v1/tag", url.Values{"name": {name}}, &result)
	return result, err
}

func (c *Client) Tags(ctx context.Context, after string, limit int) ([]project.Tag, string, error) {
	var result struct {
		Tags []project.Tag `json:"tags"`
		Next string        `json:"next"`
	}
	err := c.getJSON(ctx, "/v1/tags", url.Values{"after": {after}, "limit": {strconv.Itoa(limit)}}, &result)
	return result.Tags, result.Next, err
}

func (c *Client) RetentionPolicy(ctx context.Context) (project.RetentionPolicy, error) {
	var result project.RetentionPolicy
	err := c.getJSON(ctx, "/v1/retention/policy", nil, &result)
	return result, err
}

func (c *Client) SetRetentionPolicy(ctx context.Context, policy project.RetentionPolicy) error {
	return c.postJSON(ctx, "/v1/retention/policy", policy, nil)
}

func (c *Client) PreviewRetention(ctx context.Context, after string, limit int) (project.RetentionPreview, error) {
	var result project.RetentionPreview
	err := c.getJSON(ctx, "/v1/retention/preview", url.Values{"after": {after}, "limit": {strconv.Itoa(limit)}}, &result)
	return result, err
}

func (c *Client) Expire(ctx context.Context, ids []string) (project.ExpirationResult, error) {
	var result project.ExpirationResult
	err := c.postJSON(ctx, "/v1/retention/expire", struct {
		CommitIDs []string `json:"commitIDs"`
	}{ids}, &result)
	return result, err
}

func (c *Client) ExplainRetention(ctx context.Context, id string) (project.Availability, error) {
	var result project.Availability
	err := c.getJSON(ctx, "/v1/retention/explain", url.Values{"id": {id}}, &result)
	return result, err
}

func (c *Client) PayloadStatuses(ctx context.Context, ids []string) ([]project.Availability, error) {
	var result struct {
		Versions []project.Availability `json:"versions"`
	}
	err := c.postJSON(ctx, "/v1/retention/status", struct {
		CommitIDs []string `json:"commitIDs"`
	}{ids}, &result)
	return result.Versions, err
}

func (c *Client) StorageStats(ctx context.Context) (project.StorageStats, error) {
	var result project.StorageStats
	err := c.getJSON(ctx, "/v1/storage/stats", nil, &result)
	return result, err
}
