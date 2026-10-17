package gateway

import (
	"context"
	"reflect"
	"testing"
)

func TestViewerKeyCreatesTheUser(t *testing.T) {
	calls, c := fake(t, func(string, string) (int, any) {
		return 200, map[string]any{"key": "sk-view", "user_id": "control-center"}
	})
	k, err := c.ViewerKey(context.Background(), "control-center", "spend-1")
	if err != nil || k.Key != "sk-view" {
		t.Fatal(k, err)
	}
	want := map[string]any{"user_id": "control-center", "user_role": "proxy_admin_viewer", "key_alias": "spend-1"}
	if got := (*calls)[0]; got.uri != "/user/new" || !reflect.DeepEqual(got.body, want) {
		t.Fatalf("call = %+v", got)
	}
}

func TestViewerKeyForAnExistingUser(t *testing.T) {
	calls, c := fake(t, func(_, uri string) (int, any) {
		if uri == "/user/new" {
			return 409, map[string]any{"detail": map[string]string{"error": "User with id control-center already exists"}}
		}
		return 200, map[string]any{"key": "sk-view-2"}
	})
	k, err := c.ViewerKey(context.Background(), "control-center", "spend-2")
	if err != nil || k.Key != "sk-view-2" {
		t.Fatal(k, err)
	}
	want := map[string]any{"user_id": "control-center", "key_alias": "spend-2"}
	if got := (*calls)[1]; got.uri != "/key/generate" || !reflect.DeepEqual(got.body, want) {
		t.Fatalf("call = %+v", got)
	}
}

func TestViewerKeyError(t *testing.T) {
	_, c := fake(t, func(string, string) (int, any) { return 500, map[string]string{"error": "db"} })
	if _, err := c.ViewerKey(context.Background(), "control-center", "x"); err == nil {
		t.Fatal("no error")
	}
}
