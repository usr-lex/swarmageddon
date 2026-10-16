package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type rec struct {
	method, uri string
	body        map[string]any
}

func fake(t *testing.T, answer func(method, uri string) (int, any)) (*[]rec, *Client) {
	var calls []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-master" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		c := rec{method: r.Method, uri: r.URL.RequestURI()}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &c.body)
		}
		calls = append(calls, c)
		code, v := answer(r.Method, c.uri)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(srv.Close)
	return &calls, New(srv.URL, "sk-master")
}

func TestGenerateRoomKey(t *testing.T) {
	calls, c := fake(t, func(string, string) (int, any) {
		return 200, map[string]any{"key": "sk-room", "key_alias": "room-acme-athena-1"}
	})
	k, err := c.GenerateKey(context.Background(), RoomKeyRequest("acme", "athena", "room-acme-athena-1", 200, []string{"claude-opus-5-5"}))
	if err != nil || k.Key != "sk-room" {
		t.Fatal(k, err)
	}
	got := (*calls)[0]
	want := map[string]any{
		"key_alias": "room-acme-athena-1", "models": []any{"claude-opus-5-5"}, "max_budget": float64(200),
		"budget_duration": "1mo", "team_id": "ws-acme", "metadata": map[string]any{"room": "athena", "workspace": "acme"},
	}
	if got.method != "POST" || got.uri != "/key/generate" || !reflect.DeepEqual(got.body, want) {
		t.Errorf("call = %+v", got)
	}
}

func TestUnlimitedBudgetOmitsFields(t *testing.T) {
	r := RoomKeyRequest("acme", "athena", "a", 0, nil)
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["max_budget"]; ok {
		t.Error("max_budget set for unlimited")
	}
	if _, ok := m["budget_duration"]; ok {
		t.Error("budget_duration set for unlimited")
	}
}

func TestDeleteKeys(t *testing.T) {
	calls, c := fake(t, func(string, string) (int, any) { return 200, map[string]any{"deleted_keys": []string{"sk-1"}} })
	if err := c.DeleteKeys(context.Background(), "sk-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteKeyAliases(context.Background(), "room-acme-athena-1"); err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].uri != "/key/delete" || !reflect.DeepEqual((*calls)[0].body, map[string]any{"keys": []any{"sk-1"}}) {
		t.Errorf("call = %+v", (*calls)[0])
	}
	if !reflect.DeepEqual((*calls)[1].body, map[string]any{"key_aliases": []any{"room-acme-athena-1"}}) {
		t.Errorf("call = %+v", (*calls)[1])
	}
}

func TestDeleteMissingKeyIsFine(t *testing.T) {
	_, c := fake(t, func(string, string) (int, any) { return 404, map[string]any{"error": "not found"} })
	if err := c.DeleteKeys(context.Background(), "sk-gone"); err != nil {
		t.Fatal(err)
	}
}

func TestKeyInfoAndSpend(t *testing.T) {
	h := HashKey("sk-room")
	calls, c := fake(t, func(m, uri string) (int, any) {
		switch uri {
		case "/key/info?key=" + h:
			return 200, map[string]any{"key": h, "info": map[string]any{"spend": 12.5, "max_budget": 200, "key_alias": "a"}}
		case "/spend/logs?api_key=" + h + "&end_date=2026-10-31&start_date=2026-10-01&summarize=false":
			return 200, []map[string]any{{"request_id": "r1", "spend": 0.5, "model": "m", "total_tokens": 10}}
		}
		return 404, nil
	})
	ki, err := c.KeyInfo(context.Background(), "sk-room")
	if err != nil || ki.Spend != 12.5 || *ki.MaxBudget != 200 {
		t.Fatal(ki, err)
	}
	logs, err := c.SpendLogs(context.Background(), "sk-room", "2026-10-01", "2026-10-31")
	if err != nil || len(logs) != 1 || logs[0].TotalTokens != 10 {
		t.Fatal(logs, err, *calls)
	}
}

func TestEnsureTeam(t *testing.T) {
	exists := false
	calls, c := fake(t, func(m, uri string) (int, any) {
		if uri == "/team/info?team_id=ws-acme" {
			if !exists {
				return 404, map[string]any{"detail": "Team not found"}
			}
			return 200, map[string]any{"team_id": "ws-acme", "team_info": map[string]any{"team_id": "ws-acme"}}
		}
		return 200, map[string]any{}
	})
	b := 1000.0
	team := Team{TeamID: "ws-acme", TeamAlias: "acme", MaxBudget: &b, BudgetDuration: "1mo"}
	if err := c.EnsureTeam(context.Background(), team); err != nil {
		t.Fatal(err)
	}
	exists = true
	if err := c.EnsureTeam(context.Background(), team); err != nil {
		t.Fatal(err)
	}
	if (*calls)[1].uri != "/team/new" || (*calls)[3].uri != "/team/update" {
		t.Errorf("calls = %+v", *calls)
	}
	if (*calls)[1].body["max_budget"] != 1000.0 || (*calls)[1].body["team_id"] != "ws-acme" {
		t.Errorf("body = %v", (*calls)[1].body)
	}
}
