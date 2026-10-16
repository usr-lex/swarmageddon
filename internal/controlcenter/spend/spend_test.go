package spend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
)

func TestPoller(t *testing.T) {
	spend := 50.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" || r.URL.Path != "/key/list" || r.URL.Query().Get("return_full_object") != "true" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total_pages": 1,
			"keys": []any{
				map[string]any{"spend": spend, "max_budget": 100.0, "metadata": map[string]any{"room": "athena", "workspace": "acme"},
					"model_spend": map[string]float64{"claude-opus-5-5": spend}},
				map[string]any{"spend": 190.0, "max_budget": nil, "metadata": map[string]any{"room": "hermes", "workspace": "acme"}},
				map[string]any{"spend": 1.0, "metadata": map[string]any{}},
				"sk-bare",
			},
		})
	}))
	defer srv.Close()
	var raised []notify.Event
	var cleared []string
	p := &Poller{
		Client: &Client{BaseURL: srv.URL, Key: func(context.Context) (string, error) { return "sk-test", nil }},
		Budget: func(context.Context, string) float64 { return 200 },
		Raise:  func(_ context.Context, ev notify.Event) { raised = append(raised, ev) },
		Clear:  func(ws, room string, k v1alpha1.NotificationEvent) { cleared = append(cleared, room) },
	}
	ctx := context.Background()
	p.Poll(ctx)
	snap := p.Latest()
	if snap.Err != nil || len(snap.Rooms) != 2 || snap.Rooms[0].ModelSpend["claude-opus-5-5"] != 50 {
		t.Fatalf("snapshot %+v", snap)
	}
	// hermes: 190 of the workspace's 200 → 95%.
	if len(raised) != 1 || raised[0].Room != "hermes" || raised[0].Kind != v1alpha1.EventBudget {
		t.Fatalf("raised %+v", raised)
	}
	if len(cleared) != 1 || cleared[0] != "athena" {
		t.Fatalf("cleared %v", cleared)
	}
	spend = 100
	raised = nil
	p.Poll(ctx)
	if len(raised) != 2 || raised[0].Room != "athena" || raised[0].Fingerprint[len(raised[0].Fingerprint)-3:] != "100" {
		t.Fatalf("exhausted: %+v", raised)
	}
}
