package workspace

import (
	"slices"
	"testing"
)

// The control center reads no Secret in a workspace but room-secrets, and
// may create repository credentials it can't read back.
func TestControlCenterRoleSecrets(t *testing.T) {
	var create bool
	for _, r := range controlCenterRole("ws-acme").Rules {
		if !slices.Contains(r.Resources, "secrets") {
			continue
		}
		switch {
		case slices.Equal(r.ResourceNames, []string{RoomSecretsName}):
		case len(r.ResourceNames) == 0 && slices.Equal(r.Verbs, []string{"create"}):
			create = true
		default:
			t.Errorf("unexpected Secret rule %+v", r)
		}
	}
	if !create {
		t.Error("no rule to create repository credentials")
	}
}
