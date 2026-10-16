package room

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/egress"
	"github.com/usr-lex/swarmageddon/internal/operator/persona"
	"github.com/usr-lex/swarmageddon/internal/warden/keytable"
)

func (e *env) workspace(mut func(*v1.Workspace)) {
	var ws v1.Workspace
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Name: "acme"}, &ws))
	mut(&ws)
	must(e.t, e.c.Update(context.Background(), &ws))
}

func (e *env) role(mut func(*v1.RoleTemplate)) {
	var r v1.RoleTemplate
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "reviewer"}, &r))
	mut(&r)
	must(e.t, e.c.Update(context.Background(), &r))
}

func (e *env) netpol(name string) *networkingv1.NetworkPolicy {
	var np networkingv1.NetworkPolicy
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: Name(name)}, &np))
	return &np
}

func (e *env) table(name string) map[string]string {
	var cm corev1.ConfigMap
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: v1.PlatformNamespace, Name: name}, &cm))
	return cm.Data
}

func (e *env) roomJSON(name string) persona.RoomJSON {
	var cm corev1.ConfigMap
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: PersonaConfigMapName(name)}, &cm))
	var rj persona.RoomJSON
	must(e.t, json.Unmarshal([]byte(cm.Data[persona.KeyRoomJSON]), &rj))
	return rj
}

// peers lists the app names and ipBlocks a policy lets the room reach.
func peers(np *networkingv1.NetworkPolicy) (apps []string, blocks []string) {
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.PodSelector != nil {
				if a := p.PodSelector.MatchLabels[LabelAppName]; a != "" {
					apps = append(apps, a)
				} else {
					apps = append(apps, p.PodSelector.MatchLabels["k8s-app"])
				}
			}
			if p.IPBlock != nil {
				blocks = append(blocks, p.IPBlock.CIDR)
			}
		}
	}
	return
}

func TestNetworkModes(t *testing.T) {
	e := newEnv(t)
	e.workspace(func(w *v1.Workspace) {
		w.Spec.AllowedCIDRs = []string{"192.168.1.10/32"}
		w.Spec.RoomDefaults.Network = &v1.NetworkAccess{Mode: v1.NetworkLimited, Domains: []string{"api.example.com"}}
	})
	e.role(func(r *v1.RoleTemplate) {
		r.Spec.RoomDefaults.Network = &v1.NetworkAccess{Mode: v1.NetworkLimited, Domains: []string{"*.docs.example"}, PackageManagers: ptr.To(true)}
	})
	e.addRoom("athena", nil)
	e.addRoom("hermes", func(r *v1.Room) { r.Spec.Network = &v1.NetworkAccess{Mode: v1.NetworkNone} })
	e.addRoom("metis", func(r *v1.Room) { r.Spec.Network = &v1.NetworkAccess{Mode: v1.NetworkFull} })
	e.reconcile("athena", "hermes", "metis")

	apps, blocks := peers(e.netpol("athena"))
	if !contains(apps, "egress") || !contains(apps, "gateway") || len(blocks) != 1 || blocks[0] != "192.168.1.10/32" {
		t.Errorf("limited: apps %v blocks %v", apps, blocks)
	}
	apps, blocks = peers(e.netpol("hermes"))
	if contains(apps, "egress") || len(blocks) != 0 || !contains(apps, "messaging") || !contains(apps, "kube-dns") {
		t.Errorf("none: apps %v blocks %v", apps, blocks)
	}
	apps, blocks = peers(e.netpol("metis"))
	if contains(apps, "egress") || !contains(blocks, "0.0.0.0/0") || !contains(blocks, "192.168.1.10/32") {
		t.Errorf("full: apps %v blocks %v", apps, blocks)
	}

	rj := e.roomJSON("athena")
	if rj.Network.Mode != "limited" || rj.Network.ProxyURL != EgressProxyURL || !contains(rj.Network.Domains, "pypi.org") ||
		!contains(rj.Network.Domains, "api.example.com") || !contains(rj.Network.Domains, "*.docs.example") || !contains(rj.Network.NoProxy, "192.168.1.10/32") {
		t.Errorf("room.json network = %+v", rj.Network)
	}
	if rj := e.roomJSON("metis"); rj.Network.ProxyURL != "" {
		t.Errorf("full mode has a proxy: %+v", rj.Network)
	}

	// The egress proxy's row: mode, credential hash, domains.
	var row egress.Entry
	must(t, json.Unmarshal([]byte(e.table(egress.ConfigMapName)[egress.DataKey("acme", "athena")]), &row))
	sec := e.secret("athena")
	if row.Mode != "limited" || row.Auth != egress.Hash(string(sec.Data[KeyEgressAuth])) || len(sec.Data[KeyEgressAuth]) < 20 || !row.PackageManagers || !contains(row.Domains, "crates.io") {
		t.Errorf("egress row = %+v", row)
	}
	row = egress.Entry{}
	must(t, json.Unmarshal([]byte(e.table(egress.ConfigMapName)[egress.DataKey("acme", "hermes")]), &row))
	if row.Mode != "none" || len(row.Domains) != 0 {
		t.Errorf("none row = %+v", row)
	}

	// Removing a room removes its rows.
	must(t, e.c.Delete(context.Background(), e.room("hermes")))
	e.reconcile("hermes")
	if _, ok := e.table(egress.ConfigMapName)[egress.DataKey("acme", "hermes")]; ok {
		t.Error("egress row kept after deletion")
	}
	if _, ok := e.table(keytable.ConfigMapName)[keytable.DataKey("acme", "hermes")]; ok {
		t.Error("key row kept after deletion")
	}
}

func TestLayeredSettings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must(t, e.c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "room-secrets", Namespace: ns}, Data: map[string][]byte{
		"npm": []byte("npm-token"), "jira": []byte("jira-token"),
	}}))
	e.workspace(func(w *v1.Workspace) {
		w.Spec.RoomDefaults = v1.RoomDefaults{
			Env:        map[string]string{"TEAM": "acme", "LEVEL": "ws"},
			SecretEnv:  []v1.SecretEnvVar{{Name: "NPM_TOKEN", SecretRef: v1.SecretKeyRef{Name: "room-secrets", Key: "npm"}}},
			MCPServers: map[string]v1.MCPServer{"jira": {Command: "jira-mcp", Env: map[string]v1.MCPValue{"JIRA_TOKEN": {ValueFrom: &v1.SecretKeyRef{Name: "room-secrets", Key: "jira"}}}}},
		}
	})
	e.role(func(r *v1.RoleTemplate) { r.Spec.RoomDefaults.Env = map[string]string{"LEVEL": "role"} })
	e.addRoom("athena", func(r *v1.Room) {
		r.Spec.Env = map[string]string{"OWN": "1"}
		r.Spec.SecretEnv = []v1.SecretEnvVar{{Name: "MISSING", SecretRef: v1.SecretKeyRef{Name: "nope", Key: "k"}}}
	})
	e.reconcile("athena")

	env := map[string]string{}
	for _, v := range e.sts("athena").Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	if env["TEAM"] != "acme" || env["LEVEL"] != "role" || env["OWN"] != "1" || env["NPM_TOKEN"] != "" {
		t.Errorf("pod env = %v", env)
	}
	sec := e.secret("athena")
	if string(sec.Data["env_NPM_TOKEN"]) != "npm-token" {
		t.Errorf("secret env not copied: %v", sec.Data["env_NPM_TOKEN"])
	}
	rj := e.roomJSON("athena")
	if len(rj.SecretEnv) != 1 || rj.SecretEnv[0] != "NPM_TOKEN" || len(rj.MCPServers) != 1 {
		t.Errorf("room.json = %+v", rj)
	}
	var cm corev1.ConfigMap
	must(t, e.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: PersonaConfigMapName("athena")}, &cm))
	if !strings.Contains(cm.Data[persona.KeyClaudeManagedMCP], `"JIRA_TOKEN": "jira-token"`) {
		t.Errorf("managed-mcp = %s", cm.Data[persona.KeyClaudeManagedMCP])
	}
	c := meta.FindStatusCondition(e.room("athena").Status.Conditions, ConditionSettingsReady)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "MISSING") {
		t.Errorf("settings condition = %+v", c)
	}

	// A workspace default change waits for the room to be idle, then rolls.
	before := e.snap("athena")
	e.workspace(func(w *v1.Workspace) { w.Spec.RoomDefaults.Env["TEAM"] = "acme2" })
	e.reconcile("athena")
	if got := e.snap("athena"); got.hash != before.hash {
		t.Fatal("a workspace default rolled a busy room")
	}
	if !e.room("athena").Status.UpdateAvailable {
		t.Error("updateAvailable not set for a pending default")
	}
	e.r.Idle = idleFunc(func() bool { return true })
	e.now = e.now.Add(5 * time.Minute)
	e.reconcile("athena")
	if got := e.snap("athena"); got.hash == before.hash {
		t.Fatal("idle room not rolled to the new default")
	}
	// The room's own change applies at once.
	e.r.Idle = NeverIdle{}
	before = e.snap("athena")
	e.update("athena", func(r *v1.Room) { r.Spec.Env["OWN"] = "2" })
	e.reconcile("athena")
	if got := e.snap("athena"); got.hash == before.hash {
		t.Fatal("the room's own change waited")
	}
}

func TestSharedVolumes(t *testing.T) {
	e := newEnv(t)
	e.workspace(func(w *v1.Workspace) {
		w.Spec.Groups = []v1.Group{{Name: "clean"}, {Name: "dirty"}}
		w.Spec.Messaging.Links = []v1.GroupLink{{From: "clean", To: "dirty", Mode: v1.LinkMediated}}
		w.Spec.SharedVolumes = []v1.SharedVolume{
			{Name: "specs", Size: resource.MustParse("1Gi"), Mounts: []v1.VolumeMount{{Select: "group:clean"}, {Select: "everyone", ReadOnly: true}}},
			{Name: "scratch", Size: resource.MustParse("1Gi"), Mounts: []v1.VolumeMount{{Select: "everyone"}}},
		}
	})
	e.addRoom("athena", func(r *v1.Room) { r.Spec.Group = "clean" })
	e.addRoom("hermes", func(r *v1.Room) { r.Spec.Group = "dirty" })
	e.reconcile("athena", "hermes")

	mounts := func(name string) map[string]bool {
		out := map[string]bool{}
		for _, m := range e.sts(name).Spec.Template.Spec.Containers[0].VolumeMounts {
			if strings.HasPrefix(m.MountPath, "/srv/shared/") {
				out[strings.TrimPrefix(m.MountPath, "/srv/shared/")] = m.ReadOnly
			}
		}
		return out
	}
	a, h := mounts("athena"), mounts("hermes")
	if ro, ok := a["specs"]; !ok || ro {
		t.Errorf("athena specs = %v", a)
	}
	if ro, ok := h["specs"]; !ok || !ro {
		t.Errorf("hermes specs = %v", h)
	}
	// scratch is read-write on both sides of the wall: read-only everywhere.
	if !a["scratch"] || !h["scratch"] {
		t.Errorf("scratch: athena %v hermes %v", a, h)
	}
	c := meta.FindStatusCondition(e.room("athena").Status.Conditions, ConditionSettingsReady)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "scratch") {
		t.Errorf("condition = %+v", c)
	}
	var pvc corev1.PersistentVolumeClaim
	must(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: SharedVolumeClaim("specs")}, &pvc))
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("pvc = %+v", pvc.Spec)
	}
}

func TestQuarantine(t *testing.T) {
	e := newEnv(t)
	e.workspace(func(w *v1.Workspace) { w.Spec.Warden = v1.WardenSpec{Mode: v1.WardenMonitor, Model: "small", MonthlyUSD: 5} })
	e.role(func(r *v1.RoleTemplate) {
		r.Spec.Advisor = &v1.AdvisorSpec{Model: "big", MonthlyUSD: 30}
		r.Spec.WardenRules = []v1.WardenRule{{Name: "x", Tool: "Bash", Match: "prod", Action: v1.WardenDeny}}
	})
	e.addRoom("athena", nil)
	e.reconcile("athena")

	sec := e.secret("athena")
	for _, k := range []string{KeyAdvisorKey, KeyWardenKey, KeyGatewayKey} {
		if len(sec.Data[k]) == 0 {
			t.Fatalf("%s missing", k)
		}
	}
	var advisor, warden map[string]any
	for _, c := range e.gw.Calls("POST", "/key/generate") {
		switch c.Body["metadata"].(map[string]any)["purpose"] {
		case "advisor":
			advisor = c.Body
		case "warden":
			warden = c.Body
		}
	}
	if advisor["max_budget"] != 30.0 || advisor["models"].([]any)[0] != "big" || warden["max_budget"] != 5.0 || warden["models"].([]any)[0] != "small" {
		t.Errorf("advisor %v warden %v", advisor, warden)
	}
	var row keytable.Entry
	must(t, json.Unmarshal([]byte(e.table(keytable.ConfigMapName)["acme.athena"]), &row))
	if row.Keys[keytable.Hash(string(sec.Data[KeyAdvisorKey]))] != keytable.KindAdvisor || row.Keys[keytable.Hash(string(sec.Data[KeyGatewayKey]))] != keytable.KindMain ||
		row.Model != "small" || len(row.Rules) != 1 || row.Quarantined {
		t.Errorf("key row = %+v", row)
	}
	before := e.snap("athena")

	// The warden quarantines.
	e.update("athena", func(r *v1.Room) {
		r.Spec.Quarantined = true
		r.Annotations = map[string]string{keytable.AnnotationQuarantinedBy: "warden", keytable.AnnotationQuarantineReason: "critical verdict (cloud-metadata)"}
	})
	e.reconcile("athena")
	apps, blocks := peers(e.netpol("athena"))
	if len(blocks) != 0 || len(apps) != 2 || !contains(apps, "gateway") || !contains(apps, "kube-dns") {
		t.Errorf("quarantine policy: apps %v blocks %v", apps, blocks)
	}
	if len(e.secret("athena").Data[KeyForgeToken]) != 0 || len(e.forge.Calls("GET", "/admin/users/athena/tokens")) == 0 {
		t.Error("forge token not paused")
	}
	must(t, json.Unmarshal([]byte(e.table(keytable.ConfigMapName)["acme.athena"]), &row))
	if !row.Quarantined {
		t.Error("key table not quarantined")
	}
	c := meta.FindStatusCondition(e.room("athena").Status.Conditions, v1.ConditionQuarantined)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ByWarden" || !strings.Contains(c.Message, "cloud-metadata") {
		t.Errorf("condition = %+v", c)
	}
	if got := e.snap("athena"); got.hash != before.hash || got.uid != before.uid {
		t.Error("quarantine restarted the pod")
	}

	// A person releases it.
	tokens := len(e.forge.Calls("POST", "/admin/users/athena/tokens"))
	e.update("athena", func(r *v1.Room) { r.Spec.Quarantined = false })
	e.reconcile("athena")
	if len(e.secret("athena").Data[KeyForgeToken]) == 0 || len(e.forge.Calls("POST", "/admin/users/athena/tokens")) != tokens+1 {
		t.Error("forge token not restored")
	}
	apps, _ = peers(e.netpol("athena"))
	if !contains(apps, "messaging") {
		t.Error("network not restored")
	}
	r := e.room("athena")
	if c := meta.FindStatusCondition(r.Status.Conditions, v1.ConditionQuarantined); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("condition after release = %+v", c)
	}
	if r.Annotations[keytable.AnnotationQuarantinedBy] != "" {
		t.Error("warden annotations kept after release")
	}

	// No advisor any more: its key is deleted.
	e.role(func(r *v1.RoleTemplate) { r.Spec.Advisor = nil })
	e.reconcile("athena")
	if len(e.secret("athena").Data[KeyAdvisorKey]) != 0 {
		t.Error("advisor key kept")
	}
}
