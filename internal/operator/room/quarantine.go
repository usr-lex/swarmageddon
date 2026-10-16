package room

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/persona"
	"github.com/usr-lex/swarmageddon/internal/warden/keytable"
)

// noteQuarantine sets the Quarantined condition. On release it clears the
// warden's annotations, so a later quarantine by a person isn't attributed
// to an old verdict.
func (r *Reconciler) noteQuarantine(ctx context.Context, room *v1.Room, st *v1.RoomStatus) error {
	a := room.Annotations
	if room.Spec.Quarantined {
		reason, msg := "ByPerson", "quarantined by a person"
		if a[keytable.AnnotationQuarantinedBy] != "" {
			reason = "ByWarden"
			msg = fmt.Sprintf("quarantined by the %s at %s: %s", a[keytable.AnnotationQuarantinedBy], a[keytable.AnnotationQuarantinedAt], a[keytable.AnnotationQuarantineReason])
		}
		setCond(st, room.Generation, v1.ConditionQuarantined, true, reason,
			msg+". Model requests are refused, the network is cut to the gateway and the forge token is paused until a person releases the room.")
		return nil
	}
	if meta.FindStatusCondition(st.Conditions, v1.ConditionQuarantined) != nil {
		setCond(st, room.Generation, v1.ConditionQuarantined, false, "Released", "released by a person")
	}
	changed := false
	for _, k := range []string{keytable.AnnotationQuarantineReason, keytable.AnnotationQuarantinedBy, keytable.AnnotationQuarantinedAt} {
		if _, ok := a[k]; ok {
			delete(a, k)
			changed = true
		}
	}
	if changed {
		return r.Update(ctx, room)
	}
	return nil
}

// pauseForge revokes the room's forge tokens while it is quarantined. A new
// token is issued on release, by the usual forge sync.
func (r *Reconciler) pauseForge(ctx context.Context, room *v1.Room, sec *corev1.Secret, st *v1.RoomStatus) bool {
	gen := room.Generation
	if sec.Annotations[annForgePaused] == "true" {
		setCond(st, gen, v1.ConditionForgeAccountReady, false, "Quarantined", "the forge token is paused while the room is quarantined")
		return true
	}
	if room.Status.ForgeUser == "" && len(sec.Data[KeyForgeToken]) == 0 {
		sec.Annotations[annForgePaused] = "true"
		return true
	}
	if r.Deps.Forge == nil {
		setCond(st, gen, v1.ConditionForgeAccountReady, false, "ForgeUnavailable", "cannot pause the forge token: no forge client")
		return false
	}
	fc, err := r.Deps.Forge(ctx)
	if err == nil {
		err = fc.DeleteTokensExcept(ctx, room.Name, forgeTokenPrefix, "")
	}
	if err != nil {
		setCond(st, gen, v1.ConditionForgeAccountReady, false, "PauseFailed", err.Error())
		return false
	}
	sec.Data[KeyForgeToken] = []byte{}
	delete(sec.Annotations, annForgeSynced)
	sec.Annotations[annForgePaused] = "true"
	setCond(st, gen, v1.ConditionForgeAccountReady, false, "Quarantined", "the forge token is paused while the room is quarantined")
	return true
}

// purposeKey is a gateway key for one purpose: the advisor or the warden.
type purposeKey struct {
	secretKey string
	kind      string // advisor or warden
	models    []string
	budgetUSD int32
}

// purposeKeys are the room's advisor and warden keys it should have.
func purposeKeys(room *v1.Room, ws *v1.Workspace, role *v1.RoleTemplate) map[string]*purposeKey {
	out := map[string]*purposeKey{KeyAdvisorKey: nil, KeyWardenKey: nil}
	if a := persona.Advisor(room, role); a != nil {
		budget := ws.Spec.Models.Budgets.PerRoomMonthlyUSD
		spec := role.Spec.Advisor
		if room.Spec.Advisor != nil {
			spec = room.Spec.Advisor
		}
		if spec.MonthlyUSD > 0 {
			budget = spec.MonthlyUSD
		}
		out[KeyAdvisorKey] = &purposeKey{secretKey: KeyAdvisorKey, kind: keytable.KindAdvisor, models: []string{a.Model}, budgetUSD: budget}
	}
	if w := ws.Spec.Warden; w.Model != "" && persona.WardenMode(room, ws) != v1.WardenOff {
		budget := w.MonthlyUSD
		if budget <= 0 {
			budget = 20
		}
		out[KeyWardenKey] = &purposeKey{secretKey: KeyWardenKey, kind: keytable.KindWarden, models: []string{w.Model}, budgetUSD: budget}
	}
	return out
}

func (k *purposeKey) spec() string { return fmt.Sprintf("%v/%d", k.models, k.budgetUSD) }

// syncPurposeKeys issues, re-issues (on a new model or budget, or rotation)
// and deletes the room's advisor and warden keys (ADRs 0010, 0011).
func (r *Reconciler) syncPurposeKeys(ctx context.Context, room *v1.Room, ws *v1.Workspace, role *v1.RoleTemplate, sec *corev1.Secret, rotating bool, st *v1.RoomStatus, revoke *[]func() error) bool {
	var gc *gateway.Client
	client := func() (*gateway.Client, error) {
		if gc != nil {
			return gc, nil
		}
		if r.Deps.Gateway == nil {
			return nil, errors.New("no gateway client")
		}
		c, err := r.Deps.Gateway(ctx)
		gc = c
		return c, err
	}
	ok := true
	for secretKey, want := range purposeKeys(room, ws, role) {
		specAnn := "swarmageddon.dev/" + secretKey + "-spec"
		old := string(sec.Data[secretKey])
		if want == nil {
			if old != "" {
				c, err := client()
				if err != nil {
					ok = false
					continue
				}
				*revoke = append(*revoke, func() error { return c.DeleteKeys(ctx, old) })
			}
			delete(sec.Data, secretKey)
			delete(sec.Annotations, specAnn)
			continue
		}
		if !rotating && old != "" && sec.Annotations[specAnn] == want.spec() {
			continue
		}
		c, err := client()
		if err != nil {
			setCond(st, room.Generation, v1.ConditionGatewayKeyReady, false, "GatewayUnavailable", err.Error())
			ok = false
			continue
		}
		alias := fmt.Sprintf("room-%s-%s-%s-%d", ws.Name, room.Name, want.kind, r.now().Unix())
		req := gateway.RoomKeyRequest(ws.Name, room.Name, alias, want.budgetUSD, want.models)
		req.Metadata["purpose"] = want.kind
		k, err := c.GenerateKey(ctx, req)
		if err != nil {
			setCond(st, room.Generation, v1.ConditionGatewayKeyReady, false, "KeyFailed", want.kind+" key: "+err.Error())
			ok = false
			continue
		}
		if old != "" {
			*revoke = append(*revoke, func() error { return c.DeleteKeys(ctx, old) })
		}
		sec.Data[secretKey] = []byte(k.Key)
		sec.Annotations[specAnn] = want.spec()
	}
	return ok
}

