// Package spend reads model spend from the LiteLLM gateway (docs/design.md
// §10.2) and raises the budget notification at 80% of a room's budget.
//
// Each room has a gateway virtual key whose metadata names the room and the
// workspace. The poller lists keys with GET /key/list?return_full_object=true
// and reads each key's spend, max_budget and model_spend. These field names
// follow LiteLLM's key objects; they were not checked against a running
// gateway here.
package spend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
)

// BudgetWarnRatio is the share of a budget that raises the budget event.
const BudgetWarnRatio = 0.8

// Client talks to LiteLLM's management API.
type Client struct {
	BaseURL string
	Key     func(ctx context.Context) (string, error)
	HTTP    *http.Client
}

// KeySpend is one room's key.
type KeySpend struct {
	Workspace, Room string
	Spend           float64
	MaxBudget       float64
	ModelSpend      map[string]float64
}

type keyObject struct {
	Spend      float64            `json:"spend"`
	MaxBudget  *float64           `json:"max_budget"`
	Metadata   map[string]any     `json:"metadata"`
	ModelSpend map[string]float64 `json:"model_spend"`
}

// Keys lists every room key.
func (c *Client) Keys(ctx context.Context) ([]KeySpend, error) {
	key, err := c.Key(ctx)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	var out []KeySpend
	for page := 1; page <= 100; page++ {
		q := url.Values{"return_full_object": {"true"}, "page": {fmt.Sprint(page)}, "size": {"100"}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/key/list?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		var body struct {
			Keys       []json.RawMessage `json:"keys"`
			TotalPages int               `json:"total_pages"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("gateway /key/list: %s", resp.Status)
		}
		if err != nil {
			return nil, err
		}
		for _, raw := range body.Keys {
			var k keyObject
			if json.Unmarshal(raw, &k) != nil {
				continue // a bare key string: no metadata
			}
			room, _ := k.Metadata["room"].(string)
			ws, _ := k.Metadata["workspace"].(string)
			if room == "" || ws == "" {
				continue
			}
			ks := KeySpend{Workspace: ws, Room: room, Spend: k.Spend, ModelSpend: k.ModelSpend}
			if k.MaxBudget != nil {
				ks.MaxBudget = *k.MaxBudget
			}
			out = append(out, ks)
		}
		if page >= body.TotalPages {
			break
		}
	}
	return out, nil
}

// Snapshot is the latest spend.
type Snapshot struct {
	Rooms     []KeySpend
	UpdatedAt time.Time
	Err       error
}

// Poller refreshes spend and raises budget events.
type Poller struct {
	Client   *Client
	Interval time.Duration
	// Budget returns the workspace's per-room monthly budget (0: none), used
	// when a key carries no max_budget.
	Budget func(ctx context.Context, workspace string) float64
	Raise  func(ctx context.Context, ev notify.Event)
	Clear  func(ws, room string, kind v1alpha1.NotificationEvent)
	Now    func() time.Time

	mu   sync.Mutex
	snap Snapshot
}

// Latest returns the latest snapshot.
func (p *Poller) Latest() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snap
}

// Poll refreshes once.
func (p *Poller) Poll(ctx context.Context) {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	keys, err := p.Client.Keys(ctx)
	p.mu.Lock()
	if err != nil {
		p.snap.Err = err
		p.mu.Unlock()
		return
	}
	p.snap = Snapshot{Rooms: keys, UpdatedAt: now()}
	p.mu.Unlock()
	period := now().UTC().Format("2006-01")
	for _, k := range keys {
		budget := k.MaxBudget
		if budget <= 0 && p.Budget != nil {
			budget = p.Budget(ctx, k.Workspace)
		}
		if budget <= 0 {
			continue
		}
		ratio := k.Spend / budget
		switch {
		case ratio >= 1:
			p.raise(ctx, k, period+"/100", fmt.Sprintf("%s has used its whole budget ($%.2f); the gateway refuses further calls.", k.Room, budget))
		case ratio >= BudgetWarnRatio:
			p.raise(ctx, k, period+"/80", fmt.Sprintf("%s has used %.0f%% of its $%.2f budget.", k.Room, ratio*100, budget))
		default:
			if p.Clear != nil {
				p.Clear(k.Workspace, k.Room, v1alpha1.EventBudget)
			}
		}
	}
}

func (p *Poller) raise(ctx context.Context, k KeySpend, fp, body string) {
	if p.Raise == nil {
		return
	}
	p.Raise(ctx, notify.Event{Kind: v1alpha1.EventBudget, Workspace: k.Workspace, Room: k.Room, Fingerprint: fp,
		Title: k.Room + ": budget", Body: body, URL: "/spend"})
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	iv := p.Interval
	if iv <= 0 {
		iv = 5 * time.Minute
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		p.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
