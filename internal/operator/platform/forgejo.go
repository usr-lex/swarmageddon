package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/usr-lex/swarmageddon/internal/forgejo"
)

// Token scopes. Verified against Forgejo 17.0.0 (2026-10-16): basic auth on
// POST /api/v1/users/{admin}/tokens issues them.
var (
	operatorTokenScopes = []string{"all"}
	readerTokenScopes   = forgejo.ReaderTokenScopes
)

// createForgejoToken creates an access token for user, authenticating with
// the user's password (the endpoint requires basic auth). Token names must
// be unique per user, so the name gets a timestamp.
func createForgejoToken(ctx context.Context, hc *http.Client, baseURL, user, password, name string, scopes []string, now time.Time) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"name":   fmt.Sprintf("%s-%d", name, now.Unix()),
		"scopes": scopes,
	})
	u := fmt.Sprintf("%s/api/v1/users/%s/tokens", baseURL, url.PathEscape(user))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("forgejo: create token: %s: %s", resp.Status, bytes.TrimSpace(data))
	}
	var tok struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.SHA1 == "" {
		return "", fmt.Errorf("forgejo: create token: unexpected response")
	}
	return tok.SHA1, nil
}
