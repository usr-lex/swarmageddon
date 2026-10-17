// Package gateway is a small client for LiteLLM's management API: virtual
// keys per room, team budgets per workspace, and spend (docs/design.md §10).
// Shapes follow litellm 1.105.1 (GenerateKeyRequest, KeyRequest, TeamBase).
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to LiteLLM with the master key.
type Client struct {
	// BaseURL, e.g. http://gateway.swarmageddon-platform.svc:4000.
	BaseURL   string
	MasterKey string
	HTTP      *http.Client
}

// New returns a client.
func New(baseURL, masterKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), MasterKey: masterKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// APIError is a non-2xx answer.
type APIError struct {
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gateway: %s: %d %s", e.Path, e.Status, strings.TrimSpace(e.Body))
}

// IsNotFound reports a 404 (or LiteLLM's 400 "not found" for keys and teams).
func IsNotFound(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound ||
		(ae.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(ae.Body), "not found"))
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("gateway: %s: decode: %w", path, err)
		}
	}
	return nil
}

// KeyRequest is the body of /key/generate.
type KeyRequest struct {
	KeyAlias       string            `json:"key_alias,omitempty"`
	Models         []string          `json:"models,omitempty"`
	MaxBudget      *float64          `json:"max_budget,omitempty"`
	BudgetDuration string            `json:"budget_duration,omitempty"`
	TeamID         string            `json:"team_id,omitempty"`
	UserID         string            `json:"user_id,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// Key is a generated virtual key.
type Key struct {
	Key      string `json:"key"`
	KeyAlias string `json:"key_alias"`
	TokenID  string `json:"token_id"`
}

// GenerateKey creates a virtual key.
func (c *Client) GenerateKey(ctx context.Context, r KeyRequest) (*Key, error) {
	var k Key
	return &k, c.do(ctx, http.MethodPost, "/key/generate", r, &k)
}

// RoomKeyRequest is the key for one room: budget per month in USD (0 = none),
// models allowed (empty = all), and metadata room/workspace.
func RoomKeyRequest(workspace, room, alias string, monthlyUSD int32, models []string) KeyRequest {
	r := KeyRequest{
		KeyAlias: alias,
		Models:   models,
		TeamID:   TeamID(workspace),
		Metadata: map[string]string{"room": room, "workspace": workspace},
	}
	if monthlyUSD > 0 {
		b := float64(monthlyUSD)
		r.MaxBudget = &b
		r.BudgetDuration = "1mo"
	}
	return r
}

// KeyUpdate changes an existing key's limits in place (/key/update).
// LiteLLM applies only the fields sent (it reads the body with
// exclude_unset), so a nil MaxBudget is sent as null and clears the budget.
type KeyUpdate struct {
	// Models allowed; nil leaves them, empty allows all.
	Models []string
	// MaxBudget per BudgetDuration in USD; nil means none.
	MaxBudget      *float64
	BudgetDuration string
	// Metadata is merged into the key's metadata.
	Metadata map[string]string
}

// UpdateKey updates a key (by value) in place, so the room keeps its key.
func (c *Client) UpdateKey(ctx context.Context, key string, u KeyUpdate) error {
	body := map[string]any{"key": key, "max_budget": u.MaxBudget}
	if u.MaxBudget != nil {
		body["budget_duration"] = u.BudgetDuration
	} else {
		body["budget_duration"] = nil
	}
	if u.Models != nil {
		body["models"] = u.Models
	}
	if u.Metadata != nil {
		body["metadata"] = u.Metadata
	}
	return c.do(ctx, http.MethodPost, "/key/update", body, nil)
}

// RoomKeyUpdate is the update matching RoomKeyRequest.
func RoomKeyUpdate(workspace, room string, monthlyUSD int32, models []string) KeyUpdate {
	r := RoomKeyRequest(workspace, room, "", monthlyUSD, models)
	return KeyUpdate{Models: models, MaxBudget: r.MaxBudget, BudgetDuration: r.BudgetDuration, Metadata: r.Metadata}
}

// DeleteKeys deletes keys by value (or hash).
func (c *Client) DeleteKeys(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	err := c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"keys": keys}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteKeyAliases deletes keys by alias.
func (c *Client) DeleteKeyAliases(ctx context.Context, aliases ...string) error {
	if len(aliases) == 0 {
		return nil
	}
	err := c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"key_aliases": aliases}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// KeyInfo is a key's row as /key/info returns it.
type KeyInfo struct {
	KeyAlias       string         `json:"key_alias"`
	Spend          float64        `json:"spend"`
	MaxBudget      *float64       `json:"max_budget"`
	BudgetDuration string         `json:"budget_duration"`
	BudgetResetAt  string         `json:"budget_reset_at"`
	Models         []string       `json:"models"`
	Metadata       map[string]any `json:"metadata"`
	Blocked        *bool          `json:"blocked"`
	Status         string         `json:"status"`
}

// HashKey is the sha256 hex LiteLLM stores; pass it instead of the raw key in URLs.
func HashKey(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

// KeyInfo returns a key's spend and budget. The key is sent hashed.
func (c *Client) KeyInfo(ctx context.Context, key string) (*KeyInfo, error) {
	var out struct {
		Info KeyInfo `json:"info"`
	}
	if err := c.do(ctx, http.MethodGet, "/key/info?key="+url.QueryEscape(HashKey(key)), nil, &out); err != nil {
		return nil, err
	}
	return &out.Info, nil
}

// SpendLog is one row of /spend/logs.
type SpendLog struct {
	RequestID        string         `json:"request_id"`
	APIKey           string         `json:"api_key"`
	Model            string         `json:"model"`
	Spend            float64        `json:"spend"`
	TotalTokens      int64          `json:"total_tokens"`
	PromptTokens     int64          `json:"prompt_tokens"`
	CompletionTokens int64          `json:"completion_tokens"`
	StartTime        string         `json:"startTime"`
	Metadata         map[string]any `json:"metadata"`
}

// SpendLogs returns a key's individual spend rows between start and end (YYYY-MM-DD).
func (c *Client) SpendLogs(ctx context.Context, key, start, end string) ([]SpendLog, error) {
	q := url.Values{"api_key": {HashKey(key)}}
	if start != "" && end != "" {
		q.Set("start_date", start)
		q.Set("end_date", end)
		q.Set("summarize", "false")
	}
	var out []SpendLog
	return out, c.do(ctx, http.MethodGet, "/spend/logs?"+q.Encode(), nil, &out)
}

// TeamID is the LiteLLM team of a workspace.
func TeamID(workspace string) string { return "ws-" + workspace }

// Team is a LiteLLM team (the fields we set).
type Team struct {
	TeamID         string            `json:"team_id"`
	TeamAlias      string            `json:"team_alias,omitempty"`
	MaxBudget      *float64          `json:"max_budget,omitempty"`
	BudgetDuration string            `json:"budget_duration,omitempty"`
	Models         []string          `json:"models,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// TeamInfo returns a team, or an error IsNotFound reports.
func (c *Client) TeamInfo(ctx context.Context, teamID string) (*Team, error) {
	var out struct {
		TeamInfo Team `json:"team_info"`
	}
	if err := c.do(ctx, http.MethodGet, "/team/info?team_id="+url.QueryEscape(teamID), nil, &out); err != nil {
		return nil, err
	}
	return &out.TeamInfo, nil
}

// EnsureTeam creates the team, or updates its budget if it exists.
func (c *Client) EnsureTeam(ctx context.Context, t Team) error {
	_, err := c.TeamInfo(ctx, t.TeamID)
	if IsNotFound(err) {
		return c.do(ctx, http.MethodPost, "/team/new", t, nil)
	}
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/team/update", t, nil)
}
