package warden

import (
	"fmt"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
	"github.com/usr-lex/swarmageddon/internal/warden/rules"
)

// Rule actions.
const (
	ActionAllow    = string(rules.Allow)
	ActionDeny     = string(rules.Deny)
	ActionEscalate = string(rules.Escalate)
)

// RuleSet is layer 1 (ADR 0011).
type RuleSet interface {
	// Check returns the strictest rule matching the call.
	Check(call agent.ToolCall) (Rule, bool)
}

// Rule is a matching rule as the warden acts on it.
type Rule struct {
	Name   string
	Action string
	// Reason is fixed text shown to the agent on deny.
	Reason string
}

// Table is the shared rule table (internal/warden/rules), which the gateway
// guard runs too: the built-ins plus the persona's rules. The strictest
// matching rule wins (deny over escalate over allow), so a persona's allow
// can't undo a built-in deny.
type Table struct{ t *rules.Table }

// Check implements RuleSet.
func (t Table) Check(call agent.ToolCall) (Rule, bool) {
	text := call.Command
	if text == "" {
		text = rules.Subject([]byte(call.Input))
	}
	h, ok := t.t.Check(rules.Call{Tool: call.Tool, Text: text})
	if !ok {
		return Rule{}, false
	}
	reason := "The warden's rule " + h.Rule.Name + " stopped this call: it " + h.Rule.Reason + "."
	if h.Rule.Action == rules.Escalate {
		reason = "This call needs a person's approval: it " + h.Rule.Reason + "."
	}
	return Rule{Name: h.Rule.Name, Action: string(h.Rule.Action), Reason: reason}, true
}

// Compile builds the rule table: the built-ins and the persona's rules.
func Compile(extra []roomcfg.WardenRule) (Table, error) {
	specs := make([]v1.WardenRule, 0, len(extra))
	for _, r := range extra {
		reason := r.Reason
		if reason == "" {
			reason = "matches the room's rule " + r.Name
		}
		specs = append(specs, v1.WardenRule{Name: r.Name, Tool: r.Tool, Match: r.Match, Action: v1.WardenAction(r.Action), Reason: reason})
	}
	own, err := rules.FromSpec(specs)
	if err != nil {
		return Table{t: rules.Default()}, fmt.Errorf("persona rules: %w", err)
	}
	return Table{t: rules.Default(own...)}, nil
}
