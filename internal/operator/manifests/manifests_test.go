package manifests

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
)

var update = flag.Bool("update", false, "rewrite golden files")

func testValues() Values {
	cfg, provs := LiteLLMConfig([]v1alpha1.ModelProvider{
		{Name: "anthropic", Kind: v1alpha1.ProviderAnthropic, APIKeySecretRef: &corev1.LocalObjectReference{Name: "anthropic-key"}, Models: []string{"claude-opus-5-5"}},
		{Name: "local-vllm", Kind: v1alpha1.ProviderOpenAICompatible, BaseURL: "http://10.0.0.9:8000/v1", Models: []string{"qwen"}},
	})
	return Values{
		Namespace:       "swarmageddon-platform",
		SystemNamespace: "swarmageddon-system",
		Images:          images.Defaults("v0.1.0"),
		Postgres: PostgresValues{Storage: "20Gi", BootstrapHash: "abc123", Databases: []Database{
			{Name: "forgejo", Secret: "postgres-forgejo"},
			{Name: "litellm", Secret: "postgres-gateway"},
		}},
		Forgejo:   ForgejoValues{Storage: "50Gi", RootURL: "http://forgejo.swarmageddon-platform.svc:3000/", Domain: "forgejo.swarmageddon-platform.svc", SSHDomain: "forgejo-ssh.swarmageddon-platform.svc"},
		Messaging: MessagingValues{Replicas: 3, Storage: "10Gi", Routes: "nats://messaging-0.messaging-headless:6222"},
		Gateway:   GatewayValues{Config: cfg, ConfigHash: Hash(cfg), Providers: provs},
		Registry:  RegistryValues{Storage: "100Gi", RemoteURL: "https://registry-1.docker.io", TTL: "168h"},
		Ntfy:      NtfyValues{Storage: "1Gi", BaseURL: "https://ntfy.example.com"},
		Ingress:   IngressValues{ClassName: "traefik", ControllerPods: IngressControllerPods("traefik.io/ingress-controller")},
		Devices:   DevicesValues{Namespace: DevicesNamespace},
	}
}

func TestGolden(t *testing.T) {
	v := testValues()
	for _, name := range []string{Postgres, Forgejo, Messaging, Gateway, Egress, RegistryCache, Ntfy, NetworkPolicies, ControlCenterIngress, ControlCenterTraefik, ControlCenterNetworkPolicy, ControlCenterTLSStore, Devices, ForgejoIngress} {
		t.Run(name, func(t *testing.T) {
			v := v
			if name == ForgejoIngress {
				v.Ingress.Hostname = "forge.example.com"
			}
			objs, err := Render(name, v)
			if err != nil {
				t.Fatal(err)
			}
			if len(objs) == 0 {
				t.Fatal("no objects")
			}
			var buf bytes.Buffer
			for _, o := range objs {
				if o.GetKind() == "" || o.GetName() == "" {
					t.Fatalf("object without kind or name: %v", o.Object)
				}
				b, err := yaml.Marshal(o.Object)
				if err != nil {
					t.Fatal(err)
				}
				buf.WriteString("---\n")
				buf.Write(b)
			}
			path := filepath.Join("testdata", name+".golden.yaml")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if !bytes.Equal(want, buf.Bytes()) {
				t.Errorf("%s differs from golden; run go test ./internal/operator/manifests -update and review the diff", name)
			}
		})
	}
}

func TestForgejoConfig(t *testing.T) {
	objs, err := Render(Forgejo, testValues())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := yaml.Marshal(objs)
	s := string(b)
	for _, want := range []string{
		"FORGEJO__admin__DEFAULT_EMAIL_NOTIFICATIONS", "FORGEJO__mailer__ENABLED", "FORGEJO__actions__ENABLED",
		"FORGEJO__packages__ENABLED", "forgejo admin user create",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("forgejo manifest lacks %s", want)
		}
	}
	// The YAML anchor must have expanded into both containers.
	if strings.Count(s, "FORGEJO__database__PASSWD") != 2 {
		t.Error("env not shared by init and main container")
	}
	// Webhooks to the control center verify against the platform CA.
	if strings.Count(s, "/etc/ssl/certs:/etc/swarm/ca") != 2 || strings.Count(s, "mountPath: /etc/swarm/ca") != 2 ||
		!strings.Contains(s, "name: platform-ca-bundle") {
		t.Error("forgejo does not trust the platform CA")
	}
}

func TestControlCenterNetworkPolicyNodePort(t *testing.T) {
	v := testValues()
	v.Ingress = IngressValues{}
	objs, err := Render(ControlCenterNetworkPolicy, v)
	if err != nil || len(objs) != 1 {
		t.Fatal(objs, err)
	}
	b, _ := yaml.Marshal(objs[0].Object)
	if s := string(b); !strings.Contains(s, "cidr: 0.0.0.0/0") || strings.Contains(s, "DoesNotExist") {
		t.Fatalf("NodePort policy:\n%s", s)
	}
}

func TestIngressControllerPods(t *testing.T) {
	if IngressControllerPods("traefik.io/ingress-controller")["app.kubernetes.io/name"] != "traefik" ||
		IngressControllerPods("example.com/other") != nil {
		t.Fatal("ingress controller pods")
	}
}

func TestLiteLLMConfigKeepsKeysOut(t *testing.T) {
	cfg, provs := testValues().Gateway.Config, testValues().Gateway.Providers
	if !strings.Contains(cfg, "os.environ/PROVIDER_ANTHROPIC_API_KEY") || !strings.Contains(cfg, "anthropic/claude-opus-5-5") {
		t.Fatalf("config:\n%s", cfg)
	}
	if !strings.Contains(cfg, "api_base: http://10.0.0.9:8000/v1") || !strings.Contains(cfg, "openai/qwen") {
		t.Fatalf("config:\n%s", cfg)
	}
	if len(provs) != 2 || provs[0].SecretName != "anthropic-key" || provs[1].SecretName != "" {
		t.Fatalf("providers %+v", provs)
	}
}

func TestDecodeSkipsEmpty(t *testing.T) {
	objs, err := Decode([]byte("---\n# comment\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\n---\n"))
	if err != nil || len(objs) != 1 {
		t.Fatalf("objs=%d err=%v", len(objs), err)
	}
}

func TestGatewayIsTheWarden(t *testing.T) {
	objs, err := Render(Gateway, testValues())
	if err != nil {
		t.Fatal(err)
	}
	sel := map[string]map[string]any{}
	labels := map[string]map[string]any{}
	for _, o := range objs {
		switch o.GetKind() {
		case "Service":
			sel[o.GetName()] = o.Object["spec"].(map[string]any)["selector"].(map[string]any)
		case "Deployment":
			tmpl := o.Object["spec"].(map[string]any)["template"].(map[string]any)
			labels[o.GetName()] = tmpl["metadata"].(map[string]any)["labels"].(map[string]any)
		}
	}
	if sel["gateway"]["swarmageddon.dev/component"] != "warden" || labels["warden"]["app.kubernetes.io/name"] != "gateway" {
		t.Errorf("gateway selector %v, warden labels %v", sel["gateway"], labels["warden"])
	}
	if sel["litellm"]["swarmageddon.dev/component"] != "litellm" || labels["litellm"]["app.kubernetes.io/name"] != "litellm" {
		t.Errorf("litellm selector %v, labels %v", sel["litellm"], labels["litellm"])
	}
}
