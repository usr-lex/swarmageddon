package api

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/approvals"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
)

func retryConflict(fn func() error) error { return retry.RetryOnConflict(retry.DefaultRetry, fn) }

func tsMeta(t *metav1.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(t.Time)
}

// forgeDir implements forgehook.Directory.
type forgeDir struct{ s *Server }

func (d forgeDir) Workspace(ctx context.Context, name string) (*v1alpha1.Workspace, error) {
	return d.s.K.Workspace(ctx, name)
}

func (d forgeDir) IsRoom(ctx context.Context, ws, name string) (bool, error) {
	_, err := d.s.K.Room(ctx, ws, name)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (d forgeDir) Secret(ctx context.Context, ws string) ([]byte, error) {
	data, err := d.s.K.GetSecret(ctx, k8s.SecretForgeWebhookPrefix+ws)
	if err != nil {
		return nil, err
	}
	if len(data["secret"]) == 0 {
		return nil, errors.New("webhook secret is empty")
	}
	return data["secret"], nil
}

// policyLookup implements approvals.Lookup from Room, RoleTemplate and Workspace.
type policyLookup struct{ s *Server }

func (l policyLookup) RoomPolicy(ctx context.Context, ws, room string) (approvals.RoomPolicy, error) {
	var pol approvals.RoomPolicy
	w, err := l.s.K.Workspace(ctx, ws)
	if err != nil {
		return pol, err
	}
	if w.Spec.ApprovalTimeout != nil {
		pol.Timeout = w.Spec.ApprovalTimeout.Duration
	}
	r, err := l.s.K.Room(ctx, ws, room)
	if err != nil {
		return pol, err
	}
	pol.Persona = r.Spec.Role
	t, err := l.s.K.RoleTemplate(ctx, ws, r.Spec.Role)
	if err != nil {
		return pol, err
	}
	pol.Rules = t.Spec.AutoApprove
	return pol, nil
}

// ApprovalLookup returns the approvals policy lookup backed by this server.
func (s *Server) ApprovalLookup() approvals.Lookup { return policyLookup{s} }

// IsRoom tells agents from people for the observatory.
func (s *Server) IsRoom(ctx context.Context, ws, name string) bool {
	ok, _ := forgeDir{s}.IsRoom(ctx, ws, name)
	return ok
}

// WorkspaceNames lists every workspace's name.
func (s *Server) WorkspaceNames(ctx context.Context) []string {
	wss, err := s.K.Workspaces(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(wss))
	for _, w := range wss {
		out = append(out, w.Name)
	}
	return out
}

// Retentions returns each workspace's audit retention (0 keeps forever).
func (s *Server) Retentions(ctx context.Context) map[string]time.Duration {
	wss, err := s.K.Workspaces(ctx)
	if err != nil {
		return nil
	}
	out := map[string]time.Duration{}
	for _, w := range wss {
		if r := w.Spec.Messaging.Retention.Audit; r != nil {
			out[w.Name] = r.Duration
		}
	}
	return out
}

// AuditRetention is the Platform's control center audit retention.
func (s *Server) AuditRetention(ctx context.Context) time.Duration {
	p, err := s.K.Platform(ctx)
	if err != nil || p.Spec.AuditRetention == nil {
		return 0
	}
	return p.Spec.AuditRetention.Duration
}
