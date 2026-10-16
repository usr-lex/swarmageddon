// Package rules is the warden's rule table (ADR 0011, layer 1): fixed
// patterns over a tool call's command or path that say allow, deny or
// escalate. The room supervisor runs it before each side-effecting tool call;
// the gateway guard (cmd/warden) runs it over the tool calls a model request
// carries. Both use Builtin plus the rules of the room's RoleTemplate.
//
// When several rules match, the strictest action wins (deny over escalate
// over allow), then the highest severity: a persona's allow rule can mark a
// call as fine only when no built-in rule objects to it.
package rules

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Action is what a rule does with a matching call.
type Action string

const (
	Allow    Action = "allow"
	Deny     Action = "deny"
	Escalate Action = "escalate"
)

func (a Action) rank() int {
	switch a {
	case Deny:
		return 3
	case Escalate:
		return 2
	case Allow:
		return 1
	}
	return 0
}

// Severity of a verdict. The values equal swarm.messaging.v1.Severity, so
// messagingv1.Severity(s) converts.
type Severity int

const (
	SeverityUnspecified Severity = iota
	Low
	Medium
	High
	Critical
)

var severityNames = []string{"unspecified", "low", "medium", "high", "critical"}

func (s Severity) String() string {
	if s < 0 || int(s) >= len(severityNames) {
		return fmt.Sprintf("severity(%d)", int(s))
	}
	return severityNames[s]
}

// ParseSeverity parses low, medium, high or critical (any case).
func ParseSeverity(s string) (Severity, error) {
	for i, n := range severityNames[1:] {
		if strings.EqualFold(strings.TrimSpace(s), n) {
			return Severity(i + 1), nil
		}
	}
	return SeverityUnspecified, fmt.Errorf("unknown severity %q", s)
}

// Rule matches tool calls.
type Rule struct {
	Name string
	// Tool names the rule applies to, separated by "|", compared without
	// case; "*" or "" is any tool. ShellTools and WriteTools are the usual sets.
	Tool string
	// Match is tested against the call's subject (Call.Text).
	Match *regexp.Regexp
	// Unless, if set, exempts subjects it matches (RE2 has no lookahead).
	Unless   *regexp.Regexp
	Action   Action
	Severity Severity
	Reason   string
}

// Tool sets of both CLIs: Claude Code's tools and ore's (Codex) tools.
const (
	ShellTools = "Bash|BashOutput|shell|local_shell|exec|exec_command|container.exec|unified_exec|write_stdin"
	WriteTools = "Edit|MultiEdit|Write|NotebookEdit|apply_patch|patch"
)

func (r *Rule) appliesTo(tool string) bool {
	if r.Tool == "" || r.Tool == "*" {
		return true
	}
	for _, t := range strings.Split(r.Tool, "|") {
		if strings.EqualFold(strings.TrimSpace(t), tool) {
			return true
		}
	}
	return false
}

// Call is one tool call: its tool name and its subject, the text rules
// match: the command line, or the path, or the input as JSON.
type Call struct {
	Tool string
	Text string
}

// NewCall builds a Call from a tool name and its JSON input.
func NewCall(tool string, input json.RawMessage) Call {
	return Call{Tool: tool, Text: Subject(input)}
}

// subjectFields are the input fields that hold a call's subject, in order.
var subjectFields = []string{"command", "cmd", "file_path", "notebook_path", "path", "input", "patch", "url"}

// Subject is the text rules match for a tool input: the command (a string,
// or an argv joined with spaces), else the file path, else the patch or URL,
// else the whole input as compact JSON. A JSON string input (custom tools,
// apply_patch) is used as is.
func Subject(input json.RawMessage) string {
	var s string
	if json.Unmarshal(input, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return string(input)
	}
	for _, f := range subjectFields {
		v, ok := m[f]
		if !ok {
			continue
		}
		if json.Unmarshal(v, &s) == nil && s != "" {
			return s
		}
		var argv []string
		if json.Unmarshal(v, &argv) == nil && len(argv) > 0 {
			return strings.Join(argv, " ")
		}
	}
	// ore's local_shell action: {"action":{"command":[...]}}.
	if a, ok := m["action"]; ok {
		if t := Subject(a); t != string(a) {
			return t
		}
	}
	var buf strings.Builder
	if b, err := json.Marshal(m); err == nil {
		buf.Write(b)
	}
	return buf.String()
}

// Hit is a matching rule.
type Hit struct {
	Rule *Rule
	Call Call
}

// Table is an ordered set of rules.
type Table struct {
	rules []Rule
}

// New returns a table of rules.
func New(rules ...Rule) *Table {
	return &Table{rules: append([]Rule(nil), rules...)}
}

// Default is Builtin plus extra (a RoleTemplate's rules).
func Default(extra ...Rule) *Table {
	return New(append(Builtin(), extra...)...)
}

// Rules returns the table's rules.
func (t *Table) Rules() []Rule { return t.rules }

// Check returns the strictest matching rule for c, if any.
func (t *Table) Check(c Call) (Hit, bool) {
	var best *Rule
	for i := range t.rules {
		r := &t.rules[i]
		if r.Match == nil || !r.appliesTo(c.Tool) || !r.Match.MatchString(c.Text) {
			continue
		}
		if r.Unless != nil && r.Unless.MatchString(c.Text) {
			continue
		}
		if best == nil || r.Action.rank() > best.Action.rank() ||
			(r.Action.rank() == best.Action.rank() && r.Severity > best.Severity) {
			best = r
		}
	}
	if best == nil {
		return Hit{}, false
	}
	return Hit{Rule: best, Call: c}, true
}

// FromSpec compiles a RoleTemplate's wardenRules. Their severity is High for
// deny, Medium for escalate and Low for allow.
func FromSpec(specs []v1.WardenRule) ([]Rule, error) {
	var out []Rule
	for _, s := range specs {
		re, err := regexp.Compile(s.Match)
		if err != nil {
			return nil, fmt.Errorf("warden rule %s: %w", s.Name, err)
		}
		r := Rule{Name: s.Name, Tool: s.Tool, Match: re, Action: Action(s.Action), Reason: s.Reason}
		switch r.Action {
		case Deny:
			r.Severity = High
		case Escalate:
			r.Severity = Medium
		case Allow:
			r.Severity = Low
		default:
			return nil, fmt.Errorf("warden rule %s: unknown action %q", s.Name, s.Action)
		}
		out = append(out, r)
	}
	return out, nil
}

// protected are branch names treated as protected by the built-in rules.
const protected = `(main|master|trunk|develop|release[/-][^\s]*)`

// Builtin returns the built-in rules (ADR 0011, layer 1). Each call returns
// fresh copies.
func Builtin() []Rule {
	mk := func(name, tool, match string, a Action, s Severity, reason string) Rule {
		return Rule{Name: name, Tool: tool, Match: regexp.MustCompile(match), Action: a, Severity: s, Reason: reason}
	}
	upload := mk("upload-file", ShellTools,
		`\b(curl|wget)\b.*(\s(-d|--data[a-z-]*|-F|--form|--post-file|--body-file)[\s=]*['"]?([^\s'"=]*=)?@|\s(-T|--upload-file)\s)`,
		Escalate, High, "uploads a local file to a host outside the platform")
	upload.Unless = regexp.MustCompile(`https?://[^\s/'"]*(\.svc(\.cluster\.local)?|localhost|127\.0\.0\.1)([:/\s'"]|$)`)
	return []Rule{
		mk("pipe-to-shell", ShellTools,
			`\b(curl|wget|fetch)\b[^|;&]*\|\s*(sudo\s+)?(env\s+\S+\s+)?(ba|z|da|k|fi)?sh\b|\b(ba|z)?sh\s+(-c\s+)?["']?\s*<\(\s*(curl|wget)`,
			Escalate, High, "pipes a download into a shell"),
		mk("force-push-protected", ShellTools,
			`\bgit\b.*\bpush\b.*(\s(-f|--force|--force-with-lease(=\S+)?)\b.*\b`+protected+`\b|\s\+`+protected+`\b|\b`+protected+`\b.*\s(-f|--force|--force-with-lease)\b)`,
			Deny, High, "force-pushes a protected branch"),
		mk("force-push", ShellTools,
			`\bgit\b.*\bpush\b.*(\s(-f|--force|--force-with-lease)\b|\s\+\S)`,
			Escalate, Medium, "force-pushes a branch"),
		mk("delete-protected-branch", ShellTools,
			`\bgit\b.*\bpush\b.*(\s(-d|--delete)\b.*\b`+protected+`\b|\s:`+protected+`\b)|\bgit\s+branch\s+-[dD]\s+.*\b`+protected+`\b`,
			Deny, High, "deletes a protected branch"),
		mk("identity", "*", `/run/swarm/identity`,
			Deny, High, "touches the room's identity (keys and tokens)"),
		mk("managed-config-write", WriteTools, `^/etc/(claude-code|codex)(/|$)`,
			Deny, Critical, "changes the managed CLI settings"),
		mk("managed-config", ShellTools,
			`(\b(rm|mv|cp|tee|chmod|chown|truncate|ln|install|vi|vim|nano|emacs)\b|\bsed\s+-i|>).*/etc/(claude-code|codex)\b`,
			Deny, Critical, "changes the managed CLI settings"),
		mk("transcripts", ShellTools, `\b(rm|shred|truncate|mv)\b.*(\.claude/projects|\.(codex|ore)/(sessions|archived_sessions))`,
			Deny, High, "removes or changes the CLI's transcripts"),
		mk("transcripts-write", WriteTools, `\.claude/projects/|\.(codex|ore)/(sessions|archived_sessions)/`,
			Deny, High, "changes the CLI's transcripts"),
		mk("settings-files", WriteTools, `\.claude/settings(\.local)?\.json|\.(ore|codex)/config\.toml`,
			Escalate, Medium, "changes the CLI's own settings"),
		mk("kill-supervisor", ShellTools,
			`\b(pkill|killall)\b.*\b(supervisor|tini)\b|\bkill\b(\s+-\S+)*\s+1\b|\bkill\b.*\$\(\s*pidof\s+(supervisor|tini)|\bsystemctl\s+stop\b.*supervisor`,
			Deny, Critical, "kills the room supervisor"),
		mk("cloud-metadata", "*",
			`169\.254\.169\.254|\bfd00:ec2::254\b|metadata\.google\.internal|\b100\.100\.100\.200\b`,
			Deny, Critical, "reaches a cloud metadata service"),
		mk("cluster-scan", ShellTools,
			`\b(nmap|masscan|zmap|rustscan)\b|\bnc\b.*\s-[a-zA-Z]*z|kubernetes\.default\b|\bkubectl\b`,
			Deny, High, "scans or reaches the cluster"),
		upload,
		mk("merge-pr", ShellTools,
			`\b(tea\s+(pulls?|pr)\s+merge|gh\s+pr\s+merge)\b|/pulls/\d+/merge\b`,
			Escalate, Medium, "merges a pull request; one's own must not be merged"),
		mk("disable-hooks", "*",
			`disableAllHooks|--dangerously-skip-permissions|--dangerously-bypass-approvals-and-sandbox|\bcore\.hooksPath\b|CLAUDE_CODE_DISABLE_ADVISOR_TOOL|allowManagedHooksOnly`,
			Deny, High, "disables hooks or the CLI's guardrails"),
		mk("skip-verify", ShellTools, `\bgit\b.*\b(commit|push|merge)\b.*\s--no-verify\b`,
			Escalate, Medium, "skips git hooks"),
		mk("wipe-root", ShellTools, `\brm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rR][a-zA-Z]*\s+(-[a-zA-Z]+\s+)*(/|/\*|~|\$HOME|/home)(\s|$)`,
			Deny, Critical, "deletes the file system or home"),
	}
}
