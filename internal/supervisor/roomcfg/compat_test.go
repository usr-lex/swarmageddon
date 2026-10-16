package roomcfg_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/usr-lex/swarmageddon/internal/operator/persona"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// TestOperatorRoomJSON checks that what the operator renders loads here and
// that no field is lost.
func TestOperatorRoomJSON(t *testing.T) {
	in := persona.RoomJSON{
		Room: "athena", Workspace: "acme", Address: "athena@acme", DisplayName: "Athena", Email: "athena@agents.acme.invalid",
		Group: "clean", Role: "reviewer", RoleTitle: "Reviewer", CLI: "ore", Model: "claude-opus-5-5",
		ForgeURL: "http://forgejo:3000", ForgeSSHHost: "forgejo-ssh:22", ForgeOrg: "acme",
		MessagingURL: "https://messaging:8443", GatewayURL: "http://gateway:4000", WorkDir: "work/api",
		Repos: []string{"acme/api"}, FirstTurn: "go", Sudo: false, PermissionMode: "acceptEdits",
		ApprovalTimeoutSeconds: 3600, Env: map[string]string{"FOO": "bar"},
	}
	b, _ := json.Marshal(in)
	f := filepath.Join(t.TempDir(), "room.json")
	os.WriteFile(f, b, 0o644)
	r, err := roomcfg.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(r)
	var back persona.RoomJSON
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(back)
	if string(b) != string(b2) {
		t.Errorf("round trip differs:\n%s\n%s", b, b2)
	}
	if r.SudoEnabled() || r.OreWireAPI() != "anthropic" || r.OreBaseURL() != "http://gateway:4000/v1" || r.ForgeSSHHost != "forgejo-ssh:22" {
		t.Errorf("fields %+v", r)
	}
}
