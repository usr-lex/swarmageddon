package warden

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// Rule actions.
const (
	ActionAllow    = "allow"
	ActionDeny     = "deny"
	ActionEscalate = "escalate"
)

// RuleSet is layer 1 (ADR 0011). The built-in Table can be swapped for the
// shared internal/warden/rules package once it exists.
type RuleSet interface {
	// Check returns the first rule matching the call.
	Check(call agent.ToolCall) (Rule, bool)
}

// Rule matches a tool call.
type Rule struct {
	Name string
	// Tools is a set of tool names; empty matches every tool.
	Tools  map[string]bool
	Match  *regexp.Regexp
	Action string
	// Reason is fixed text shown to the agent on deny.
	Reason string
}

// Table is an ordered list of rules; the first match wins.
type Table []Rule

// Check implements RuleSet.
func (t Table) Check(call agent.ToolCall) (Rule, bool) {
	for _, r := range t {
		if len(r.Tools) > 0 && !r.Tools[call.Tool] && !(r.Tools["mcp"] && strings.HasPrefix(call.Tool, "mcp__")) {
			continue
		}
		subject := call.Command
		if subject == "" {
			subject = call.Input
		}
		if r.Match.MatchString(subject) {
			return r, true
		}
	}
	return Rule{}, false
}

// Tool sets the built-in rules use. Claude Code's and ore's names.
const (
	shellTools = "Bash|exec"
	writeTools = "Write|Edit|MultiEdit|NotebookEdit|patch"
)

func tools(s string) map[string]bool {
	if s == "" || s == "*" {
		return nil
	}
	m := map[string]bool{}
	for _, t := range strings.Split(s, "|") {
		m[strings.TrimSpace(t)] = true
	}
	return m
}

// builtin is the fixed rule table (ADR 0011, layer 1). Deny rules come
// before escalate rules for the same kind of call.
var builtin = []struct{ name, tools, match, action, reason string }{
	{"identity-files", "", `/run/swarm/identity`, ActionDeny,
		"The room's identity files are off limits."},
	{"policy-files", writeTools, `(/etc/claude-code|/etc/codex)\b`, ActionDeny,
		"The platform's managed settings are off limits."},
	{"policy-files", shellTools, `(\b(rm|mv|cp|tee|chmod|chown|truncate|ln|install)\b|\bsed\s+-i|>).*(/etc/claude-code|/etc/codex)\b`, ActionDeny,
		"The platform's managed settings are off limits."},
	{"transcripts", shellTools, `\b(rm|shred|truncate|mv)\b.*(\.claude/projects|\.ore/sessions)`, ActionDeny,
		"Session transcripts must not be removed or changed."},
	{"kill-supervisor", shellTools, `\b(pkill|killall)\b.*\bsupervisor\b|\bkill\s+(-\S+\s+)*1(\s|$)`, ActionDeny,
		"The room supervisor must keep running."},
	{"disable-hooks", "", `disableAllHooks|--dangerously-skip-permissions|--dangerously-bypass-approvals`, ActionDeny,
		"Hooks and permission settings must not be disabled."},
	{"cloud-metadata", shellTools + "|WebFetch|mcp", `169\.254\.169\.254|metadata\.google\.internal|fd00:ec2::254|100\.100\.100\.200`, ActionDeny,
		"Cloud metadata addresses are off limits."},
	{"force-push-protected", shellTools, `\bgit\s+push\b(.*\s(-f|--force|--force-with-lease)\b.*\b(main|master)\b|.*\b(main|master)\b.*\s(-f|--force|--force-with-lease)\b|.*\s\+(refs/heads/)?(main|master)\b)`, ActionDeny,
		"Force-pushing a protected branch is not allowed."},
	{"delete-protected", shellTools, `\bgit\s+push\b(.*(--delete|\s-d)\b.*\b(main|master)\b|.*\s:(refs/heads/)?(main|master)\b)`, ActionDeny,
		"Deleting a protected branch is not allowed."},
	{"force-push", shellTools, `\bgit\s+push\b.*(\s(-f|--force|--force-with-lease)\b|\s\+\S)`, ActionEscalate,
		"Force pushes need a person's approval."},
	{"pipe-to-shell", shellTools, `\b(curl|wget)\b[^|;&]*\|\s*(sudo\s+)?(ba|z|da)?sh\b`, ActionEscalate,
		"Piping a download into a shell needs a person's approval."},
	{"cluster-scan", shellTools, `\b(nmap|masscan|zmap)\b`, ActionEscalate,
		"Network scanning needs a person's approval."},
	{"upload-file", shellTools, `\bcurl\b.*(\s(-d|--data|--data-binary|--data-raw|-F|--form)\s+'?"?@|\s(-T|--upload-file)\s)`, ActionEscalate,
		"Uploading files needs a person's approval."},
	{"merge-own-pr", shellTools, `\btea\s+(pr|pulls)\s+merge\b|/pulls/\d+/merge\b`, ActionEscalate,
		"Merging a pull request needs a person's approval."},
	{"settings-files", writeTools, `\.claude/settings(\.local)?\.json|\.ore/config\.toml`, ActionEscalate,
		"Changing the CLI's settings needs a person's approval."},
}

// Compile builds the rule table: the persona's rules first, then the
// built-ins.
func Compile(extra []roomcfg.WardenRule) (Table, error) {
	var t Table
	for _, r := range extra {
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return nil, fmt.Errorf("warden rule %q: %w", r.Name, err)
		}
		switch r.Action {
		case ActionAllow, ActionDeny, ActionEscalate:
		default:
			return nil, fmt.Errorf("warden rule %q: action %q is not allow, deny or escalate", r.Name, r.Action)
		}
		reason := r.Reason
		if reason == "" {
			reason = "Blocked by the room's rule " + r.Name + "."
		}
		t = append(t, Rule{Name: r.Name, Tools: tools(r.Tool), Match: re, Action: r.Action, Reason: reason})
	}
	for _, b := range builtin {
		t = append(t, Rule{Name: b.name, Tools: tools(b.tools), Match: regexp.MustCompile(b.match), Action: b.action, Reason: b.reason})
	}
	return t, nil
}
