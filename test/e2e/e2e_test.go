//go:build e2e

// Package e2e drives a whole installation on the `make dev` cluster (k3d):
// the install, the platform services, a workspace with rooms of both CLIs,
// messaging across a wall, and the forge. It runs in CI after `make dev`
// (docs/design.md §14). Models are the in-cluster mock (hack/dev/extras.yaml).
//
//	make dev && make e2e
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

const (
	ws     = "acme"
	wsNS   = "ws-acme"
	mockLM = "http://mockmodel.swarmageddon-platform.svc:9999"
)

type env struct {
	c       client.Client
	context string
	kubectl string
}

func setup(t *testing.T) *env {
	t.Helper()
	context := os.Getenv("E2E_CONTEXT")
	if context == "" {
		context = "k3d-swarm-dev"
	}
	kubectl := os.Getenv("KUBECTL")
	if kubectl == "" {
		kubectl = "../../bin/tools/kubectl"
		if _, err := os.Stat(kubectl); err != nil {
			kubectl = "kubectl"
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: context}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig context %s: %v", context, err)
	}
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	return &env{c: c, context: context, kubectl: kubectl}
}

// eventually retries f until it returns nil or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = f(); last == nil {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%s: not done after %s: %v", what, timeout, last)
}

func (e *env) platform(t *testing.T) *v1.Platform {
	t.Helper()
	var p v1.Platform
	if err := e.c.Get(context.Background(), types.NamespacedName{Name: v1.PlatformName}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// exec runs a command in a room's container as its agent user.
func (e *env) exec(room string, args ...string) (string, error) {
	cmd := append([]string{"--context", e.context, "-n", wsNS, "exec", "room-" + room + "-0", "-c", "room", "--",
		"runuser", "-u", room, "--"}, args...)
	var out bytes.Buffer
	c := exec.Command(e.kubectl, cmd...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}

func TestPlatform(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	t.Run("install", func(t *testing.T) {
		for _, name := range []string{"swarmageddon-operator", "control-center"} {
			eventually(t, 5*time.Minute, name+" available", func() error {
				var d appsv1.Deployment
				if err := e.c.Get(ctx, types.NamespacedName{Namespace: v1.SystemNamespace, Name: name}, &d); err != nil {
					return err
				}
				if d.Status.AvailableReplicas < 1 {
					return fmt.Errorf("%d available", d.Status.AvailableReplicas)
				}
				return nil
			})
		}
		eventually(t, 3*time.Minute, "URL, CA fingerprint and setup token", func() error {
			p := e.platform(t)
			if p.Status.URL == "" || p.Status.CAFingerprint == "" {
				return fmt.Errorf("status %+v", p.Status)
			}
			if p.Status.AdminClaimed {
				return nil
			}
			var s corev1.Secret
			return e.c.Get(ctx, types.NamespacedName{Namespace: v1.SystemNamespace, Name: "setup-token"}, &s)
		})
	})

	t.Run("services", func(t *testing.T) {
		// What the first-run wizard does after preflight: accept the tier
		// (k3d on a CI runner has no user namespaces for pods), add the mock
		// model as a provider, and approve the services.
		p := e.platform(t)
		patch := client.MergeFrom(p.DeepCopy())
		p.Spec.IsolationTier = v1.TierPrivileged
		p.Spec.PrivilegedAccepted = true
		p.Spec.Services.Gateway.Providers = []v1.ModelProvider{{
			Name: "mock", Kind: v1.ProviderOpenAICompatible, BaseURL: mockLM + "/v1",
			Models: []string{"mock-claude", "mock-ore", "mock-small"},
		}}
		p.Spec.Wizard.ServicesApproved = true
		if err := e.c.Patch(ctx, p, patch); err != nil {
			t.Fatal(err)
		}
		for _, cond := range []string{v1.ConditionPostgresReady, v1.ConditionForgejoReady, v1.ConditionMessagingReady,
			v1.ConditionGatewayReady, v1.ConditionRegistryReady} {
			eventually(t, 15*time.Minute, cond, func() error {
				if !meta.IsStatusConditionTrue(e.platform(t).Status.Conditions, cond) {
					c := meta.FindStatusCondition(e.platform(t).Status.Conditions, cond)
					return fmt.Errorf("condition %v", c)
				}
				return nil
			})
		}
	})

	t.Run("workspace", func(t *testing.T) {
		w := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ws}, Spec: v1.WorkspaceSpec{
			Forge:  v1.ForgeSpec{Org: ws},
			Groups: []v1.Group{{Name: "clean"}, {Name: "dirty"}},
			Messaging: v1.MessagingPolicy{Links: []v1.GroupLink{
				{From: "clean", To: "dirty", Mode: v1.LinkMediated, Senders: []string{"hermes"}, Recipients: []string{"metis"}},
				{From: "dirty", To: "clean", Mode: v1.LinkMediated, Senders: []string{"metis"}, Recipients: []string{"hermes"}},
			}},
			Models:       v1.ModelSettings{Default: "mock-claude"},
			Repositories: []v1.RepositorySpec{{Name: "api"}},
		}}
		if err := e.c.Create(ctx, w); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		eventually(t, 5*time.Minute, "workspace namespace and forge org", func() error {
			var got v1.Workspace
			if err := e.c.Get(ctx, types.NamespacedName{Name: ws}, &got); err != nil {
				return err
			}
			for _, c := range []string{v1.ConditionNamespaceReady, v1.ConditionForgeOrgReady} {
				if !meta.IsStatusConditionTrue(got.Status.Conditions, c) {
					return fmt.Errorf("%s: %v", c, meta.FindStatusCondition(got.Status.Conditions, c))
				}
			}
			return nil
		})
		eventually(t, time.Minute, "built-in role templates", func() error {
			var l v1.RoleTemplateList
			if err := e.c.List(ctx, &l, client.InNamespace(wsNS)); err != nil {
				return err
			}
			if len(l.Items) < 5 {
				return fmt.Errorf("%d role templates", len(l.Items))
			}
			return nil
		})
	})

	rooms := []struct {
		name, role, group string
		cli               v1.CLI
		model             string
	}{
		{"athena", "implementer", "clean", v1.CLIClaude, "mock-claude"},
		{"hermes", "coordinator", "clean", v1.CLIOre, "mock-ore"},
		{"metis", "implementer", "dirty", v1.CLIClaude, "mock-claude"},
	}

	t.Run("rooms", func(t *testing.T) {
		for _, r := range rooms {
			room := &v1.Room{ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: wsNS}, Spec: v1.RoomSpec{
				Role: r.role, CLI: r.cli, Model: r.model, Group: r.group, Repos: []string{ws + "/api"},
			}}
			if err := e.c.Create(ctx, room); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatal(err)
			}
		}
		for _, r := range rooms {
			eventually(t, 15*time.Minute, r.name+" running", func() error {
				var got v1.Room
				if err := e.c.Get(ctx, types.NamespacedName{Namespace: wsNS, Name: r.name}, &got); err != nil {
					return err
				}
				if got.Status.Phase != v1.RoomRunning {
					return fmt.Errorf("phase %s, conditions %v", got.Status.Phase, got.Status.Conditions)
				}
				return nil
			})
		}
		for _, r := range rooms {
			eventually(t, 3*time.Minute, r.name+" answers whoami", func() error {
				out, err := e.exec(r.name, "swarm", "whoami")
				if err != nil || !strings.Contains(out, r.name+"@"+ws) {
					return fmt.Errorf("%v: %s", err, out)
				}
				return nil
			})
		}
	})

	t.Run("adding and removing a room touches no other room", func(t *testing.T) {
		before := e.podUID(t, "athena")
		temp := &v1.Room{ObjectMeta: metav1.ObjectMeta{Name: "iris", Namespace: wsNS},
			Spec: v1.RoomSpec{Role: "reviewer", CLI: v1.CLIClaude, Model: "mock-claude", Group: "clean"}}
		if err := e.c.Create(ctx, temp); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Minute, "iris running", func() error {
			var got v1.Room
			if err := e.c.Get(ctx, client.ObjectKeyFromObject(temp), &got); err != nil {
				return err
			}
			if got.Status.Phase != v1.RoomRunning {
				return fmt.Errorf("phase %s", got.Status.Phase)
			}
			return nil
		})
		if err := e.c.Delete(ctx, temp); err != nil {
			t.Fatal(err)
		}
		eventually(t, 5*time.Minute, "iris gone", func() error {
			err := e.c.Get(ctx, client.ObjectKeyFromObject(temp), &v1.Room{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("still there: %v", err)
		})
		if after := e.podUID(t, "athena"); after != before {
			t.Errorf("athena's pod was replaced (%s -> %s) when another room came and went", before, after)
		}
	})

	t.Run("messaging", func(t *testing.T) {
		// Within a group, both CLIs.
		if out, err := e.exec("athena", "swarm", "send", "hermes", "e2e: hello from athena"); err != nil {
			t.Fatalf("athena -> hermes: %v: %s", err, out)
		}
		if out, err := e.exec("hermes", "swarm", "send", "athena", "e2e: hello from hermes"); err != nil {
			t.Fatalf("hermes -> athena: %v: %s", err, out)
		}
		// Across the wall: only hermes may write to metis.
		out, err := e.exec("athena", "swarm", "send", "metis", "e2e: should be refused")
		if err == nil || !strings.Contains(out, "hermes") {
			t.Errorf("athena -> metis across the wall: want a refusal naming hermes, got %v: %s", err, out)
		}
		if out, err := e.exec("hermes", "swarm", "send", "metis", "e2e: allowed crossing"); err != nil {
			t.Errorf("hermes -> metis (mediated): %v: %s", err, out)
		}
		// Peers list only who each room may reach.
		out, err = e.exec("athena", "swarm", "agents")
		if err != nil || !strings.Contains(out, "hermes") || strings.Contains(out, "metis") {
			t.Errorf("athena's peers: %v: %s", err, out)
		}
	})

	t.Run("forge", func(t *testing.T) {
		repo := "/home/athena/work/api"
		script := strings.Join([]string{
			"set -e",
			"cd " + repo,
			"git switch -c e2e-branch",
			"echo e2e > e2e.txt && git add e2e.txt && git commit -qm 'e2e: signed commit'",
			"git log -1 --format=%G? | grep -q G", // signed with a good key
			"git push -q origin e2e-branch",
		}, " && ")
		if out, err := e.exec("athena", "bash", "-lc", script); err != nil {
			t.Fatalf("signed commit on a branch: %v: %s", err, out)
		}
		out, err := e.exec("athena", "bash", "-lc", "cd "+repo+" && git push -q origin e2e-branch:main")
		if err == nil || !strings.Contains(out, "protected") {
			t.Errorf("a direct push to main must be refused: %v: %s", err, out)
		}
		if out, err := e.exec("athena", "bash", "-lc", "cd "+repo+" && tea pr create --head e2e-branch --base main --title e2e"); err != nil {
			t.Errorf("tea pr create: %v: %s", err, out)
		}
	})
}

func (e *env) podUID(t *testing.T, room string) types.UID {
	t.Helper()
	var p corev1.Pod
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: wsNS, Name: "room-" + room + "-0"}, &p); err != nil {
		t.Fatal(err)
	}
	return p.UID
}
