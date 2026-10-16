// Package auth holds the control center's roles, role mapping, the signed-in
// principal, workspace access rules and the setup-token rate limiter
// (docs/design.md §3.3, §8.1). The sign-in mechanisms live in subpackages.
package auth

import (
	"context"
	"slices"
	"strings"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Role is a control center role; higher includes lower.
type Role int

const (
	RoleNone Role = iota
	RoleViewer
	RoleOperator
	RoleAdmin
)

// ParseRole parses admin, operator or viewer; anything else is RoleNone.
func ParseRole(s string) Role {
	switch strings.ToLower(s) {
	case string(v1alpha1.RoleAdmin):
		return RoleAdmin
	case string(v1alpha1.RoleOperator):
		return RoleOperator
	case string(v1alpha1.RoleViewer):
		return RoleViewer
	}
	return RoleNone
}

func (r Role) String() string {
	switch r {
	case RoleAdmin:
		return "admin"
	case RoleOperator:
		return "operator"
	case RoleViewer:
		return "viewer"
	}
	return ""
}

// MapRole returns the highest role the groups map to, else the default role.
// RoleNone means the person is refused.
func MapRole(groups []string, mappings []v1alpha1.RoleMapping, def v1alpha1.Role) Role {
	best := RoleNone
	for _, m := range mappings {
		if slices.Contains(groups, m.Group) {
			if r := ParseRole(string(m.Role)); r > best {
				best = r
			}
		}
	}
	if best == RoleNone {
		return ParseRole(string(def))
	}
	return best
}

// Principal is the signed-in person behind a request.
type Principal struct {
	UserID      string
	Handle      string
	Email       string
	DisplayName string
	Role        Role
	Provider    string // local, oidc:<name>, saml:<name>, setup
	// Workspaces assigned or owned, from the database. Ownership through a
	// Workspace's spec.owners is checked separately.
	Workspaces []string
}

// Name is how the principal appears in the audit log.
func (p *Principal) Name() string {
	if p == nil {
		return "anonymous"
	}
	if p.Email != "" {
		return p.Email
	}
	return p.Handle
}

// IsOwner reports whether owners (a Workspace's spec.owners) names the principal.
func (p *Principal) IsOwner(owners []string) bool {
	for _, o := range owners {
		if o == "" {
			continue
		}
		if strings.EqualFold(o, p.Email) || strings.EqualFold(o, p.Handle) {
			return true
		}
	}
	return false
}

// CanView reports whether the principal may see a workspace: admins see all,
// others what they own or are assigned.
func (p *Principal) CanView(workspace string, owners []string) bool {
	if p == nil || p.Role == RoleNone {
		return false
	}
	if p.Role == RoleAdmin {
		return true
	}
	return slices.Contains(p.Workspaces, workspace) || p.IsOwner(owners)
}

// CanOperate reports whether the principal may change a workspace: rooms,
// lifecycle, terminals, approvals.
func (p *Principal) CanOperate(workspace string, owners []string) bool {
	if p == nil {
		return false
	}
	if p.Role == RoleAdmin {
		return true
	}
	return p.Role >= RoleOperator && p.CanView(workspace, owners)
}

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal, or nil.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}
