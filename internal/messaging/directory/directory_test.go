package directory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dir.yaml")
	err := os.WriteFile(p, []byte(`apiVersion: swarmageddon.dev/v1alpha1
kind: Workspace
metadata: {name: acme}
spec:
  forge: {org: acme}
  groups: [{name: clean}]
  messaging:
    retention: {messages: 1h}
---
apiVersion: swarmageddon.dev/v1alpha1
kind: RoleTemplate
metadata: {name: reviewer, namespace: ws-acme}
spec: {title: Reviewer, summary: Reviews PRs., roleText: x}
---
apiVersion: swarmageddon.dev/v1alpha1
kind: Room
metadata: {name: athena, namespace: ws-acme}
spec: {role: reviewer, cli: claude, group: clean}
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	d, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	ws, ok := d.Workspace("acme")
	if !ok {
		t.Fatal("no workspace")
	}
	r := ws.Rooms["athena"]
	if r.Group != "clean" || r.Title != "Reviewer" || r.Summary != "Reviews PRs." {
		t.Fatalf("room %+v", r)
	}
	if ws.MessageRetention() != time.Hour || ws.AuditRetention() != DefaultAuditRetention ||
		ws.MaxBytes() != DefaultMaxBytes || !ws.HumansReachable() || !ws.WithinGroup() || !ws.HasGroup("clean") {
		t.Fatalf("defaults wrong: %+v", ws)
	}
}
