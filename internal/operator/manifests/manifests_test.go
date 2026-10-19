package manifests

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/clusterdns"
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

// TestSentry: with a DSN, messaging, the warden and egress get SENTRY_* (the
// DSN from Secret sentry) and a pod annotation that changes with it; without
// one (testValues, the goldens) nothing about Sentry is rendered.
func TestSentry(t *testing.T) {
	v := testValues()
	v.Sentry = SentryValues{Hash: "abc", Environment: "prod", TracesSampleRate: "0.25"}
	for _, name := range []string{Messaging, Gateway, Egress} {
		objs, err := Render(name, v)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, o := range objs {
			var pod corev1.PodTemplateSpec
			switch o.GetKind() {
			case "Deployment", "StatefulSet":
				tmpl, _, _ := unstructured.NestedMap(o.Object, "spec", "template")
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(tmpl, &pod); err != nil {
					t.Fatal(err)
				}
			default:
				continue
			}
			if o.GetName() == "litellm" {
				if strings.Contains(fmt.Sprint(pod.Spec.Containers[0].Env), "SENTRY") {
					t.Error("LiteLLM got SENTRY_*")
				}
				continue
			}
			found = true
			env := map[string]corev1.EnvVar{}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = e
			}
			ref := env["SENTRY_DSN"].ValueFrom
			if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != SentrySecret || ref.SecretKeyRef.Key != "dsn" ||
				ref.SecretKeyRef.Optional == nil || !*ref.SecretKeyRef.Optional {
				t.Errorf("%s: SENTRY_DSN %+v", o.GetName(), env["SENTRY_DSN"])
			}
			if env["SENTRY_ENVIRONMENT"].Value != "prod" || env["SENTRY_TRACES_SAMPLE_RATE"].Value != "0.25" ||
				pod.Annotations["swarmageddon.dev/sentry"] != "abc" {
				t.Errorf("%s: env %v, annotations %v", o.GetName(), env, pod.Annotations)
			}
			if name == Messaging && env["POD_NAME"].ValueFrom == nil {
				t.Error("messaging lost POD_NAME")
			}
		}
		if !found {
			t.Errorf("%s: no workload", name)
		}
		off, err := Render(name, testValues())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := yaml.Marshal(off)
		if s := string(b); strings.Contains(s, "SENTRY") || strings.Contains(s, "swarmageddon.dev/sentry") {
			t.Errorf("%s renders Sentry settings without a DSN", name)
		}
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

// On OpenShift the egress proxy resolves names through openshift-dns's pods
// on 5353 (OpenShift report, issue 5.2).
func TestEgressDNSOnOpenShift(t *testing.T) {
	v := testValues()
	v.DNS = clusterdns.Target{Namespace: "openshift-dns", Labels: map[string]string{"dns.operator.openshift.io/daemonset-dns": "default"}, Ports: []int32{53, 5353}}
	objs, err := Render(NetworkPolicies, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetName() != "egress" {
			continue
		}
		b, _ := yaml.Marshal(o.Object)
		s := string(b)
		for _, want := range []string{"kubernetes.io/metadata.name: openshift-dns", "dns.operator.openshift.io/daemonset-dns: default", "port: 5353"} {
			if !strings.Contains(s, want) {
				t.Errorf("egress policy lacks %q:\n%s", want, s)
			}
		}
		return
	}
	t.Fatal("no egress policy")
}

// On OpenShift the device plugin runs as container_device_plugin_t, or it
// can't reach the kubelet's socket (OpenShift report 5.3); elsewhere it
// keeps the runtime's default.
func TestDevicesSELinuxType(t *testing.T) {
	for _, typ := range []string{"", "container_device_plugin_t"} {
		v := testValues()
		v.Devices = DevicesValues{Namespace: DevicesNamespace, SELinuxType: typ}
		objs, err := Render(Devices, v)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, o := range objs {
			b, _ := yaml.Marshal(o.Object)
			found = found || strings.Contains(string(b), "type: container_device_plugin_t")
		}
		if found != (typ != "") {
			t.Errorf("SELinuxType %q: rendered type %v", typ, found)
		}
	}
}

// The admin's extra LiteLLM environment (OpenShift report 5.7: CRC on Apple
// silicon needs OPENSSL_armcap=0) reaches the container; names the template
// sets are left out.
func TestGatewayEnv(t *testing.T) {
	env, ignored := GatewayEnv(map[string]string{"OPENSSL_armcap": "0", "LITELLM_MASTER_KEY": "x", "PROVIDER_A_API_KEY": "y", "HTTP_TIMEOUT": "30"})
	if len(env) != 2 || env[0].Name != "HTTP_TIMEOUT" || env[1].Name != "OPENSSL_armcap" || strings.Join(ignored, ",") != "LITELLM_MASTER_KEY,PROVIDER_A_API_KEY" {
		t.Fatalf("env %v ignored %v", env, ignored)
	}
	v := testValues()
	v.Gateway.Env = env
	objs, err := Render(Gateway, v)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range objs {
		if o.GetKind() != "Deployment" || o.GetName() != "litellm" {
			continue
		}
		b, _ := yaml.Marshal(o.Object)
		found = strings.Contains(string(b), "name: OPENSSL_armcap") && strings.Contains(string(b), `value: "0"`)
	}
	if !found {
		t.Fatal("OPENSSL_armcap not in LiteLLM's env")
	}
}
