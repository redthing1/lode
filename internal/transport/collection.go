package transport

import (
	"context"
	"net/http"

	"github.com/redthing1/lode/internal/project"
)

// Collect performs one bounded physical collection pass.
func (c *Client) Collect(ctx context.Context, request project.CollectRequest) (project.CollectionResult, error) {
	var result project.CollectionResult
	err := c.postJSON(ctx, "/v1/storage/collect", request, &result)
	return result, err
}

func (c *Client) CapacityPolicy(ctx context.Context) (project.CapacityPolicy, error) {
	var result project.CapacityPolicy
	err := c.getJSON(ctx, "/v1/storage/capacity", nil, &result)
	return result, err
}

func (c *Client) SetCapacityPolicy(ctx context.Context, policy project.CapacityPolicy) error {
	return c.postJSON(ctx, "/v1/storage/capacity", policy, nil)
}

// collectionRoutes handles explicit storage collection and capacity controls.
// The main handler calls it only after authentication and API-prefix checks.
func (s *server) collectionRoutes(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/storage/collect":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return true
		}
		var request project.CollectRequest
		if !decodeJSONBody(w, r, &request) {
			return true
		}
		result, err := s.service.Collect(r.Context(), request)
		if err != nil {
			writeServiceError(w, err)
			return true
		}
		writeJSON(w, result)
		return true
	case "/v1/storage/capacity":
		switch r.Method {
		case http.MethodGet:
			if len(r.URL.Query()) != 0 {
				invalidRequest(w)
				return true
			}
			policy, err := s.service.CapacityPolicy(r.Context())
			if err != nil {
				writeServiceError(w, err)
				return true
			}
			writeJSON(w, policy)
		case http.MethodPost:
			var policy project.CapacityPolicy
			if !decodeJSONBody(w, r, &policy) {
				return true
			}
			if err := s.service.SetCapacityPolicy(r.Context(), policy); err != nil {
				writeServiceError(w, err)
				return true
			}
			writeJSON(w, policy)
		default:
			methodNotAllowed(w)
		}
		return true
	default:
		return false
	}
}
