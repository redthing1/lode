package remote

import (
	"context"
	"net/url"

	"github.com/redthing1/lode/internal/model"
)

type BranchPage struct {
	Branches []model.Reference `json:"branches"`
	Next     string            `json:"next,omitempty"`
}

func (c *Client) Branches(ctx context.Context, owner, project, after string) (BranchPage, error) {
	var page BranchPage
	err := c.json(ctx, "GET", projectPath(owner, project)+"/branches?after="+url.QueryEscape(after), nil, &page)
	return page, err
}

func (c *Client) CreateBranch(ctx context.Context, owner, project string, branch model.Reference) (model.Reference, error) {
	var result model.Reference
	err := c.json(ctx, "POST", projectPath(owner, project)+"/branches", branch, &result)
	return result, err
}

func (c *Client) DeleteBranch(ctx context.Context, owner, project string, branch model.Reference) error {
	var result struct{}
	return c.json(ctx, "POST", projectPath(owner, project)+"/branches/delete", branch, &result)
}

func (c *Client) Reference(ctx context.Context, owner, project, ref string) (model.Version, error) {
	var version model.Version
	err := c.json(ctx, "GET", projectPath(owner, project)+"/reference?ref="+url.QueryEscape(ref), nil, &version)
	return version, err
}
