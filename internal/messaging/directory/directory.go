// Package directory is the messaging service's read-only view of workspaces
// and rooms (docs/design.md §7.1, §7.4). It is fed from Workspace, Room and
// RoleTemplate resources in Kubernetes, from a file in development, or from
// static data in tests.
package directory

import (
	"sort"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Defaults for unset policy fields (the CRD defaults, for data that skipped
// the API server).
const (
	DefaultMaxBytes         = 65536
	DefaultRatePerMinute    = 30
	DefaultMessageRetention = 720 * time.Hour
	DefaultAuditRetention   = 8760 * time.Hour
	DefaultApprovalTimeout  = 24 * time.Hour
)

// Room is one agent as the messaging service sees it.
type Room struct {
	Name        string
	DisplayName string
	Group       string
	Title       string // from the room's RoleTemplate
	Summary     string
}

// Workspace is a snapshot of one workspace: its policy and its rooms.
type Workspace struct {
	Name            string
	Groups          []string
	Messaging       v1alpha1.MessagingPolicy
	ApprovalTimeout time.Duration
	Rooms           map[string]Room
}

// MaxBytes is the largest message body allowed.
func (w *Workspace) MaxBytes() int {
	if w.Messaging.Limits.MaxBytes > 0 {
		return int(w.Messaging.Limits.MaxBytes)
	}
	return DefaultMaxBytes
}

// RatePerMinute is the per-sender message rate.
func (w *Workspace) RatePerMinute() int {
	if w.Messaging.Limits.RatePerMinute > 0 {
		return int(w.Messaging.Limits.RatePerMinute)
	}
	return DefaultRatePerMinute
}

// MessageRetention is how long undelivered messages wait; 0 keeps them.
func (w *Workspace) MessageRetention() time.Duration {
	if r := w.Messaging.Retention.Messages; r != nil {
		return r.Duration
	}
	return DefaultMessageRetention
}

// AuditRetention is how long audit records are kept; 0 keeps them.
func (w *Workspace) AuditRetention() time.Duration {
	if r := w.Messaging.Retention.Audit; r != nil {
		return r.Duration
	}
	return DefaultAuditRetention
}

// AuditBodies reports whether message bodies go into the audit log.
func (w *Workspace) AuditBodies() bool {
	b := w.Messaging.Retention.AuditBodies
	return b == nil || *b
}

// HumansReachable reports whether people and agents may message each other.
func (w *Workspace) HumansReachable() bool {
	b := w.Messaging.HumansReachable
	return b == nil || *b
}

// WithinGroup reports whether rooms in one group reach each other by default.
func (w *Workspace) WithinGroup() bool {
	return w.Messaging.DefaultLinks != v1alpha1.DefaultNone
}

// HasGroup reports whether g is a declared group or some room's group.
func (w *Workspace) HasGroup(g string) bool {
	for _, n := range w.Groups {
		if n == g {
			return true
		}
	}
	for _, r := range w.Rooms {
		if r.Group == g {
			return true
		}
	}
	return false
}

// RoomNames returns the rooms' names, sorted.
func (w *Workspace) RoomNames() []string {
	names := make([]string, 0, len(w.Rooms))
	for n := range w.Rooms {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Directory looks up workspaces. Implementations are safe for concurrent use
// and return snapshots the caller must not modify.
type Directory interface {
	// Workspace returns the named workspace, or false if there is none.
	Workspace(name string) (*Workspace, bool)
	// Workspaces lists every workspace's name.
	Workspaces() []string
	// Ready reports whether the directory has loaded.
	Ready() bool
}

// FromObjects builds a snapshot from the resources of one workspace.
func FromObjects(ws *v1alpha1.Workspace, rooms []v1alpha1.Room, roles []v1alpha1.RoleTemplate) *Workspace {
	out := &Workspace{
		Name:            ws.Name,
		Messaging:       ws.Spec.Messaging,
		ApprovalTimeout: DefaultApprovalTimeout,
		Rooms:           map[string]Room{},
	}
	if ws.Spec.ApprovalTimeout != nil {
		out.ApprovalTimeout = ws.Spec.ApprovalTimeout.Duration
	}
	for _, g := range ws.Spec.Groups {
		out.Groups = append(out.Groups, g.Name)
	}
	byName := map[string]*v1alpha1.RoleTemplate{}
	for i := range roles {
		byName[roles[i].Name] = &roles[i]
	}
	for _, r := range rooms {
		if r.DeletionTimestamp != nil {
			continue
		}
		room := Room{Name: r.Name, DisplayName: r.Spec.DisplayName, Group: r.Spec.Group}
		if rt := byName[r.Spec.Role]; rt != nil {
			room.Title, room.Summary = rt.Spec.Title, rt.Spec.Summary
		}
		out.Rooms[r.Name] = room
	}
	return out
}

// Static is a Directory over fixed data, for tests and development.
type Static struct {
	mu sync.RWMutex
	ws map[string]*Workspace
}

// NewStatic returns a Static directory holding ws.
func NewStatic(ws ...*Workspace) *Static {
	s := &Static{ws: map[string]*Workspace{}}
	for _, w := range ws {
		s.Set(w)
	}
	return s
}

// Set adds or replaces a workspace.
func (s *Static) Set(w *Workspace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.Rooms == nil {
		w.Rooms = map[string]Room{}
	}
	s.ws[w.Name] = w
}

// Delete removes a workspace.
func (s *Static) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ws, name)
}

// Workspace implements Directory.
func (s *Static) Workspace(name string) (*Workspace, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.ws[name]
	return w, ok
}

// Workspaces implements Directory.
func (s *Static) Workspaces() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.ws))
	for n := range s.ws {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Ready implements Directory.
func (s *Static) Ready() bool { return true }
