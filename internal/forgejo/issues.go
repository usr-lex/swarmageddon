package forgejo

import (
	"context"
	"fmt"
	"net/http"
)

// Issue is Forgejo's Issue (the fields we use). A pull request is an issue
// with PullRequest set.
type Issue struct {
	Number      int64     `json:"number"`
	State       string    `json:"state"`
	Assignees   []User    `json:"assignees"`
	PullRequest *struct{} `json:"pull_request"`
}

// maxIssuePages bounds ListOpenIssues (50 per page).
const maxIssuePages = 20

// ListOpenIssues lists a repository's open issues and pull requests, up to
// maxIssuePages pages. The token needs read:issue.
func (c *Client) ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error) {
	var all []Issue
	for page := 1; page <= maxIssuePages; page++ {
		var is []Issue
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues?state=open&limit=50&page=%d", esc(owner), esc(repo), page), nil, &is); err != nil {
			return nil, err
		}
		all = append(all, is...)
		if len(is) < 50 {
			break
		}
	}
	return all, nil
}
