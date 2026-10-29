package remote

import (
	"context"
	"net/http"
	"net/url"

	"github.com/redthing1/lode/internal/model"
)

func (c *Client) Collect(ctx context.Context, owner, project string) (model.Collection, error) {
	// Collection may exceed the ordinary response-header deadline. Keep request
	// cancellation and this client's authentication/redirect policy unchanged.
	client, connection := *c, *c.HTTP
	if transport, ok := connection.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.ResponseHeaderTimeout = 0
		defer transport.CloseIdleConnections()
		connection.Transport = transport
	}
	client.HTTP = &connection
	var result model.Collection
	err := client.json(ctx, "POST", projectPath(owner, project)+"/collect", struct{}{}, &result)
	return result, err
}

type TagPage struct {
	Tags []model.Reference `json:"tags"`
	Next string            `json:"next,omitempty"`
}

func (c *Client) Tags(ctx context.Context, owner, project, after string) (TagPage, error) {
	var page TagPage
	err := c.json(ctx, "GET", projectPath(owner, project)+"/tags?after="+url.QueryEscape(after), nil, &page)
	return page, err
}
func (c *Client) CreateTag(ctx context.Context, owner, project string, tag model.Reference) (model.Reference, error) {
	var result model.Reference
	err := c.json(ctx, "POST", projectPath(owner, project)+"/tags", tag, &result)
	return result, err
}
func (c *Client) DeleteTag(ctx context.Context, owner, project string, tag model.Reference) error {
	var result struct{}
	return c.json(ctx, "POST", projectPath(owner, project)+"/tags/delete", tag, &result)
}
func (c *Client) Retention(ctx context.Context, owner, project string) (model.Retention, error) {
	var result model.Retention
	err := c.json(ctx, "GET", projectPath(owner, project)+"/retention", nil, &result)
	return result, err
}
func (c *Client) SetRetention(ctx context.Context, owner, project string, p model.Retention) error {
	var result struct{}
	return c.json(ctx, "POST", projectPath(owner, project)+"/retention", p, &result)
}
func (c *Client) Cleanup(ctx context.Context, owner, project, token string) (model.CleanupPreview, error) {
	var result model.CleanupPreview
	err := c.json(ctx, "POST", projectPath(owner, project)+"/cleanup", struct {
		Token string `json:"token"`
	}{token}, &result)
	return result, err
}
