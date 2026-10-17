package gateway

import (
	"context"
	"errors"
	"net/http"
)

// RoleProxyAdminViewer is LiteLLM's "Admin (View Only)" user role: its keys
// may read every key and all spend, but change nothing and call no model
// (litellm 1.105.1, proxy/auth/route_checks.py,
// _check_proxy_admin_viewer_access; /key/list lists every key for it).
const RoleProxyAdminViewer = "proxy_admin_viewer"

// UserRequest is the body of /user/new, which also returns a first key.
type UserRequest struct {
	UserID   string `json:"user_id"`
	UserRole string `json:"user_role,omitempty"`
	KeyAlias string `json:"key_alias,omitempty"`
}

// ViewerKey returns a new key of the view-only user userID, creating the
// user with it if it doesn't exist (/user/new answers 409 if it does).
func (c *Client) ViewerKey(ctx context.Context, userID, alias string) (*Key, error) {
	var k Key
	err := c.do(ctx, http.MethodPost, "/user/new", UserRequest{UserID: userID, UserRole: RoleProxyAdminViewer, KeyAlias: alias}, &k)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return c.GenerateKey(ctx, KeyRequest{KeyAlias: alias, UserID: userID})
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}
