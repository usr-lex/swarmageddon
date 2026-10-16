package forgehook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

func activities(t *testing.T, name string) []store.Activity {
	t.Helper()
	f := load(t, name)
	var p payload
	if err := json.Unmarshal([]byte(f.Body), &p); err != nil {
		t.Fatal(err)
	}
	return Activities(Delivery{Workspace: "acme", Event: f.Event, EventType: f.EventType, ID: f.Delivery, Body: []byte(f.Body)}, &p, time.Unix(1, 0))
}

func brief(evs []store.Activity) string {
	var parts []string
	for _, e := range evs {
		parts = append(parts, fmt.Sprintf("%s %s>%s %s#%d", e.Kind, e.Actor, e.Target, e.Repo, e.Number))
	}
	return strings.Join(parts, "; ")
}

func TestActivities(t *testing.T) {
	cases := map[string]string{
		"issue_opened":        "issue_opened hermes> acme/api#1; mentioned hermes>athena acme/api#1; mentioned hermes>metis acme/api#1",
		"issue_assigned":      "assigned hermes>athena acme/api#1",
		"issue_comment":       "comment athena>hermes acme/api#1; mentioned athena>hermes acme/api#1",
		"issue_closed":        "issue_closed hermes> acme/api#1",
		"pr_opened":           "pr_opened athena> acme/api#2",
		"pr_review_requested": "review_requested athena>hermes acme/api#2",
		"pr_review_rejected":  "review_changes hermes>athena acme/api#2; mentioned hermes>athena acme/api#2",
		"pr_review_approved":  "review_approved hermes>athena acme/api#2",
		"pr_comment":          "comment hermes>athena acme/api#2",
		"pr_merged":           "pr_merged hermes>athena acme/api#2",
		"pr_closed":           "pr_closed athena>athena acme/api#3",
		"push_branch":         "commit athena> acme/api#0",
		"push_merge":          "commit hermes> acme/api#0; commit athena> acme/api#0; commit athena> acme/api#0",
	}
	for name, want := range cases {
		if got := brief(activities(t, name)); got != want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
	}
	// The branch's commit pushed again by the merge has the same dedup key.
	b := activities(t, "push_branch")[0]
	var found bool
	for _, e := range activities(t, "push_merge") {
		if e.Dedup == b.Dedup {
			found = true
		}
	}
	if !found || !strings.HasPrefix(b.Dedup, "commit/acme/api/") || b.Summary != "Add b" {
		t.Fatalf("commit dedup %q summary %q", b.Dedup, b.Summary)
	}
}

func TestHandlerRecords(t *testing.T) {
	h, _, _ := newHandler()
	var got []store.Activity
	h.Record = func(_ context.Context, evs ...store.Activity) { got = append(got, evs...) }
	post(h, load(t, "pr_review_requested"), "/hooks/forgejo/acme")
	body := ciPayload("run_completed", "pull_request")
	post(h, fixture{Event: "workflow_run_failure", EventType: "workflow_run_failure", Delivery: "d1", Signature: sign(body), Body: body}, "/hooks/forgejo/acme")
	if brief(got) != "review_requested athena>hermes acme/api#2; ci_failed athena> acme/api#2" {
		t.Fatalf("recorded %s", brief(got))
	}
}
