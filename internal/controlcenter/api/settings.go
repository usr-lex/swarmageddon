package api

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"connectrpc.com/connect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/wizard"
)

// roomLevels reads the three levels of a room's settings (docs/design.md §6.6).
func (s *Server) roomLevels(ctx context.Context, ref *ccv1.RoomRef) (*v1alpha1.Workspace, *v1alpha1.Room, v1alpha1.RoomDefaults, error) {
	ws, err := s.K.Workspace(ctx, ref.GetWorkspace())
	if err != nil {
		return nil, nil, v1alpha1.RoomDefaults{}, err
	}
	r, err := s.K.Room(ctx, ref.GetWorkspace(), ref.GetName())
	if err != nil {
		return nil, nil, v1alpha1.RoomDefaults{}, err
	}
	var role v1alpha1.RoomDefaults
	if t, err := s.K.RoleTemplate(ctx, ref.GetWorkspace(), r.Spec.Role); err == nil {
		role = t.Spec.RoomDefaults
	} else if !apierrors.IsNotFound(err) {
		return nil, nil, role, err
	}
	return ws, r, role, nil
}

func (s roomSvc) GetRoomSettings(ctx context.Context, req *connect.Request[ccv1.GetRoomSettingsRequest]) (*connect.Response[ccv1.GetRoomSettingsResponse], error) {
	ref := req.Msg.GetRoom()
	if _, err := s.canView(ctx, ref.GetWorkspace()); err != nil {
		return nil, err
	}
	ws, r, role, err := s.roomLevels(ctx, ref)
	if err != nil {
		return nil, kerr(err)
	}
	wd := wizard.WorkspaceDefaults(ws)
	return connect.NewResponse(&ccv1.GetRoomSettingsResponse{
		Workspace:     wizard.SettingsToPlan(wd),
		Role:          wizard.SettingsToPlan(role),
		Room:          wizard.SettingsToPlan(wizard.RoomSettings(r.Spec)),
		Effective:     wizard.Effective(r.Name, r.Spec.Role, wd, role, wizard.RoomSettings(r.Spec)),
		SharedVolumes: wizard.RoomVolumes(ws.Spec.SharedVolumes, r.Name, r.Spec.Role, r.Spec.Group),
	}), nil
}

// UpdateRoomSettings replaces the room's own level of settings. The operator
// rolls the room when it is idle.
func (s roomSvc) UpdateRoomSettings(ctx context.Context, req *connect.Request[ccv1.UpdateRoomSettingsRequest]) (*connect.Response[ccv1.UpdateRoomSettingsResponse], error) {
	ref := req.Msg.GetRoom()
	if probs := wizard.ValidateSettings("room "+ref.GetName(), req.Msg.GetSettings()); len(probs) > 0 {
		return nil, errInvalid("%s", strings.Join(probs, "; "))
	}
	err := s.lifecycle(ctx, req, ref, "settings", func(r *v1alpha1.Room) error {
		wizard.SetRoomSettings(&r.Spec, wizard.SettingsFromPlan(req.Msg.GetSettings()))
		return nil
	})
	if err != nil {
		return nil, err
	}
	ws, r, role, err := s.roomLevels(ctx, ref)
	if err != nil {
		return nil, kerr(err)
	}
	return connect.NewResponse(&ccv1.UpdateRoomSettingsResponse{
		Effective: wizard.Effective(r.Name, r.Spec.Role, wizard.WorkspaceDefaults(ws), role, wizard.RoomSettings(r.Spec)),
	}), nil
}

var secretKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

func (s workspaceSvc) ListRoomSecrets(ctx context.Context, req *connect.Request[ccv1.ListRoomSecretsRequest]) (*connect.Response[ccv1.ListRoomSecretsResponse], error) {
	if _, err := s.canOperate(ctx, req.Msg.GetWorkspace()); err != nil {
		return nil, err
	}
	keys, err := s.K.RoomSecretKeys(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, kerr(err)
	}
	slices.Sort(keys)
	return connect.NewResponse(&ccv1.ListRoomSecretsResponse{SecretName: k8s.RoomSecretsName, Keys: keys}), nil
}

func (s workspaceSvc) SetRoomSecret(ctx context.Context, req *connect.Request[ccv1.SetRoomSecretRequest]) (*connect.Response[ccv1.SetRoomSecretResponse], error) {
	m := req.Msg
	if _, err := s.canOperate(ctx, m.GetWorkspace()); err != nil {
		return nil, err
	}
	if !secretKeyRE.MatchString(m.GetKey()) {
		return nil, errInvalid("the key must be letters, digits, '-', '_' or '.'")
	}
	if m.GetValue() == "" {
		return nil, errInvalid("the value is empty")
	}
	err := s.K.SetRoomSecretKey(ctx, m.GetWorkspace(), m.GetKey(), []byte(m.GetValue()))
	// The value never reaches the audit log.
	s.audit(ctx, req, "roomsecret.set", "secret/"+k8s.RoomSecretsName+"/"+m.GetKey(), m.GetWorkspace(), nil, nil, err)
	if errors.Is(err, k8s.ErrRoomSecretsMissing) {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if err != nil {
		return nil, kerr(err)
	}
	return connect.NewResponse(&ccv1.SetRoomSecretResponse{Ref: &ccv1.SecretKeyPlan{Name: k8s.RoomSecretsName, Key: m.GetKey()}}), nil
}

func (s workspaceSvc) DeleteRoomSecret(ctx context.Context, req *connect.Request[ccv1.DeleteRoomSecretRequest]) (*connect.Response[ccv1.DeleteRoomSecretResponse], error) {
	m := req.Msg
	if _, err := s.canOperate(ctx, m.GetWorkspace()); err != nil {
		return nil, err
	}
	if !secretKeyRE.MatchString(m.GetKey()) {
		return nil, errInvalid("the key is not valid")
	}
	err := s.K.SetRoomSecretKey(ctx, m.GetWorkspace(), m.GetKey(), nil)
	s.audit(ctx, req, "roomsecret.delete", "secret/"+k8s.RoomSecretsName+"/"+m.GetKey(), m.GetWorkspace(), nil, nil, err)
	if errors.Is(err, k8s.ErrRoomSecretsMissing) {
		return connect.NewResponse(&ccv1.DeleteRoomSecretResponse{}), nil
	}
	if err != nil {
		return nil, kerr(err)
	}
	return connect.NewResponse(&ccv1.DeleteRoomSecretResponse{}), nil
}
