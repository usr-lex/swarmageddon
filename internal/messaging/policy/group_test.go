package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
)

func TestGroupSend(t *testing.T) {
	ws := wall()
	for _, name := range []string{"proteus", "themis", "metis", "argus"} {
		r := ws.Rooms[name]
		r.Role, r.Teams = "reviewer", []string{"reviewers"}
		ws.Rooms[name] = r
	}
	ws.Roles = map[string]bool{"reviewer": true, "coordinator": true}
	hermes, _ := Lookup(ws, people, "hermes")
	route, r := Check(ws, people, hermes, "@role:reviewer", "please review #42")
	if r != nil {
		t.Fatal(r)
	}
	var got, skipped []string
	for _, p := range route.Recipients {
		got = append(got, p.Name)
	}
	for _, s := range route.Skipped {
		skipped = append(skipped, s.Name+":"+s.Refusal.Rule)
	}
	if strings.Join(got, ",") != "proteus,themis" || strings.Join(skipped, ",") != "argus:mediated-group,metis:mediated-group" {
		t.Fatalf("recipients %v skipped %v", got, skipped)
	}
	// Team sends match the same rooms; a person reaches across the wall.
	route, r = Check(ws, people, Party{Name: "alice", Kind: Human}, "@team:reviewers", "hi")
	if r != nil || len(route.Recipients) != 4 || len(route.Skipped) != 0 {
		t.Fatalf("person team send: %v %v", route, r)
	}
	// Nobody reachable: refused, saying why.
	hecate, _ := Lookup(ws, people, "hecate")
	ws.Rooms["metis"] = directory.Room{Name: "metis", Group: "dirty"}
	ws.Rooms["argus"] = directory.Room{Name: "argus", Group: "dirty"}
	_, r = Check(ws, people, hecate, "@role:reviewer", "to proteus: hi")
	if r == nil || r.Rule != "mediated-group" || !strings.Contains(r.Reason, "can't reach any of the 2 members of role:reviewer") {
		t.Fatalf("refusal %v", r)
	}
	if _, r = Check(ws, people, hermes, "@role:coordinator", "x"); r == nil || r.Rule != "empty-group" {
		t.Fatalf("empty role: %v", r)
	}
	if _, r = Check(ws, people, hermes, "@team:nobody", "x"); r == nil || r.Code != NotFound {
		t.Fatalf("unknown team: %v", r)
	}
}

func TestLimiterAllowN(t *testing.T) {
	l := NewLimiter()
	now := time.Unix(1000, 0)
	if !l.AllowN("a", 50, 60, now) || l.AllowN("a", 20, 60, now) || !l.AllowN("a", 10, 60, now) {
		t.Fatal("fan-out budget")
	}
}
