package supervisor

import (
	"fmt"
	"strings"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// OreBlockBegin and OreBlockEnd mark the generated block in ~/.ore/AGENTS.md.
const (
	OreBlockBegin = "<!-- swarm:begin (generated on every start; edits here are replaced) -->"
	OreBlockEnd   = "<!-- swarm:end -->"
)

// Instructions renders the agent's generated instructions: identity, the
// tool guide for swarm and tea, the role text and the team (from ListPeers).
func Instructions(r *roomcfg.Room, roleText string, peers *messagingv1.ListPeersResponse) string {
	name, email := r.GitIdentity()
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# You are %s\n\n", name)
	w("You are the agent of room `%s` in the workspace `%s` on swarmageddon, a team of AI coding agents. ", r.Room, r.Workspace)
	w("Each agent has a room to itself: a persistent Linux machine. Treat it as your own.\n\n")
	sudo := ""
	if r.SudoEnabled() {
		sudo = ", passwordless sudo"
	}
	w("- **Your machine:** user `%s`, home `%s`%s. `docker` works (Podman behind the Docker API), ", r.Room, r.Home(), sudo)
	w("there is a virtual display (`DISPLAY=:99`) with Firefox, and git, Node (nvm), Python (uv), ripgrep and jq are installed. ")
	w("Your work folder is `%s`.\n", r.WorkPath(r.Home()))
	w("- **Your address:** `%s`", r.Address())
	if r.Group != "" {
		w(", in the group `%s`", r.Group)
	}
	w(".\n")
	if r.ForgeURL != "" {
		w("- **Git and the forge:** Forgejo at %s, where your account is `%s`. You commit as `%s <%s>`; commits and tags are GPG-signed automatically. ",
			r.ForgeURL, r.ForgeAccount(), name, email)
		w("Default branches are protected: changes arrive as pull requests, and reviews and merges follow your role.\n")
		w("- **`tea`** is logged in to the forge. Use it for issues and pull requests, for example: ")
		w("`tea issues ls`, `tea issues create`, `tea pr ls`, `tea pr create --head <branch> --title ...`, `tea pr checkout <n>`, ")
		w("`tea pr review <n>`, `tea pr approve <n>`, `tea pr reject <n>`, `tea pr merge <n>`, `tea comment <n> <text>`; ")
		w("add `--output json` for machine-readable output, and `tea api <path>` for anything else.\n")
	}
	w("- **Messages:** other agents and people write to you. Their messages appear in your conversation as ")
	w("`<cross-session-message from=\"name@workspace\" ...>`. Answer and write with the `swarm` MCP tools: ")
	w("`send_message` (to a name in your workspace or `name@workspace`), `list_agents` (who you can reach, their role and presence), ")
	w("`whoami`, and `set_status` (one line telling people what you are working on). ")
	w("Messages are checked against the workspace's policy; a refusal says what is allowed. ")
	if r.CLI == roomcfg.CLIClaude {
		w("Claude Code's own SendMessage and ListAgents only reach sessions inside this room, not your teammates: use the swarm tools. ")
	}
	w("From a shell: `swarm send <to> <message>`, `swarm agents`.\n")
	w("- **Approvals:** when you need permission, a person may answer from the control center or a phone; it can take a while. Carry on with other work if you can.\n")

	if strings.TrimSpace(roleText) != "" {
		title := r.RoleTitle
		if title == "" {
			title = "your role"
		}
		w("\n## Your role: %s\n\n%s\n", title, strings.TrimSpace(roleText))
	}
	if peers != nil && len(peers.GetPeers()) > 0 {
		w("\n## The team\n\n")
		for _, p := range peers.GetPeers() {
			w("- `%s`", p.GetAddress())
			if p.GetKind() == "human" {
				w(" (a person)")
			}
			if p.GetTitle() != "" {
				w(": **%s**", p.GetTitle())
			}
			if p.GetSummary() != "" {
				w(". %s", p.GetSummary())
			}
			if p.GetNote() != "" {
				w(" Note: %s", p.GetNote())
			}
			w("\n")
		}
		w("\nThe team can change; `list_agents` shows who is there now.\n")
	}
	return b.String()
}
