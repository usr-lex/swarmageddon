package gateway

import (
	"context"
	"reflect"
	"testing"
)

func TestUpdateKey(t *testing.T) {
	calls, c := fake(t, func(string, string) (int, any) { return 200, map[string]any{} })
	ctx := context.Background()
	if err := c.UpdateKey(ctx, "sk-room", RoomKeyUpdate("acme", "athena", 300, []string{"m"})); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateKey(ctx, "sk-room", RoomKeyUpdate("acme", "athena", 0, nil)); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"key": "sk-room", "max_budget": float64(300), "budget_duration": "1mo", "models": []any{"m"},
			"metadata": map[string]any{"room": "athena", "workspace": "acme"}},
		// No budget: explicit nulls clear it; models are left alone.
		{"key": "sk-room", "max_budget": nil, "budget_duration": nil,
			"metadata": map[string]any{"room": "athena", "workspace": "acme"}},
	}
	for i, w := range want {
		got := (*calls)[i]
		if got.uri != "/key/update" || !reflect.DeepEqual(got.body, w) {
			t.Errorf("call %d = %s %v", i, got.uri, got.body)
		}
	}
}
