package supervisor

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/usr-lex/swarmageddon/internal/supervisor/claude"
	"github.com/usr-lex/swarmageddon/internal/supervisor/ore"
	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// Direct model access (OpenShift report, feature 8): the CLI gets neither
// the gateway key nor the gateway's base URL, and ore drops the gateway
// provider; gateway rooms keep both.
func TestNewAgentModelAccess(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	u := &proc.User{Name: "athena", Home: "/home/athena"}
	has := func(env []string, prefix string) bool {
		return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, prefix) })
	}
	for _, access := range []string{"", "direct"} {
		room := &roomcfg.Room{Room: "athena", CLI: roomcfg.CLIClaude, GatewayURL: "http://gw:4000", ModelAccess: access}
		c := newAgent(room, u, "/w", "", []string{"A=1"}, "sk-key", nil, log).(*claude.Driver)
		viaGateway := has(c.Env, "ANTHROPIC_AUTH_TOKEN=sk-key") && has(c.Env, "ANTHROPIC_BASE_URL=")
		if viaGateway == (access == "direct") || !has(c.Env, "A=1") {
			t.Errorf("claude %q env %v", access, c.Env)
		}
		room.CLI = roomcfg.CLIOre
		o := newAgent(room, u, "/w", "", []string{"A=1"}, "sk-key", nil, log).(*ore.Driver)
		viaGateway = has(o.Env, ore.GatewayKeyEnv+"=") && o.Settings.Provider != nil
		if viaGateway == (access == "direct") || o.Settings.Direct != (access == "direct") {
			t.Errorf("ore %q env %v settings %+v", access, o.Env, o.Settings)
		}
	}
}
