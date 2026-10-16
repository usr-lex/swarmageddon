// Package manifests renders the platform services from embedded YAML
// templates (docs/design.md §5). Each template is a text/template producing
// one or more YAML documents; Render turns them into unstructured objects
// for server-side apply.
package manifests

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
)

//go:embed templates/*.yaml
var files embed.FS

// Template names.
const (
	Postgres             = "postgres"
	Forgejo              = "forgejo"
	Messaging            = "messaging"
	Gateway              = "gateway"
	RegistryCache        = "registry-cache"
	Ntfy                 = "ntfy"
	NetworkPolicies      = "networkpolicies"
	Egress               = "egress"
	ControlCenterIngress = "control-center-ingress"
	ControlCenterTraefik = "control-center-traefik"
)

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"quote": func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	},
	"indent": func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i, l := range lines {
			if l != "" {
				lines[i] = pad + l
			}
		}
		return strings.Join(lines, "\n")
	},
}).ParseFS(files, "templates/*.yaml"))

// Values fill the templates.
type Values struct {
	Namespace       string
	SystemNamespace string
	Images          images.Images

	Postgres  PostgresValues
	Forgejo   ForgejoValues
	Messaging MessagingValues
	Gateway   GatewayValues
	Registry  RegistryValues
	Ntfy      NtfyValues
	Ingress   IngressValues
}

// Database is one service's role and database in PostgreSQL.
type Database struct {
	// Name of the role and the database (also an env var suffix: [a-z_]).
	Name string
	// Secret holding key password (postgres-<service>).
	Secret string
}

type PostgresValues struct {
	Storage   string
	Databases []Database
	// BootstrapHash names the bootstrap Job, so a new database list runs a new Job.
	BootstrapHash string
}

type ForgejoValues struct {
	Storage   string
	RootURL   string
	Domain    string
	SSHDomain string
}

type MessagingValues struct {
	Replicas int32
	Storage  string
	// Routes is the comma-separated NATS route URL list.
	Routes string
}

// Provider is a model provider as the gateway template needs it.
type Provider struct {
	// EnvVar carrying the API key, e.g. PROVIDER_ANTHROPIC_API_KEY.
	EnvVar string
	// SecretName holding key apiKey; empty for keyless local servers.
	SecretName string
}

type GatewayValues struct {
	// Config is LiteLLM's config.yaml.
	Config     string
	ConfigHash string
	Providers  []Provider
}

type RegistryValues struct {
	Storage   string
	RemoteURL string
	TTL       string
}

type NtfyValues struct {
	Storage string
	BaseURL string
}

type IngressValues struct {
	ClassName string
	Hostname  string
}

// Render executes template name and returns its objects.
func Render(name string, v Values) ([]*unstructured.Unstructured, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name+".yaml", v); err != nil {
		return nil, err
	}
	return Decode(buf.Bytes())
}

// Decode splits multi-document YAML into objects, skipping empty documents.
func Decode(data []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for i, doc := range splitDocs(string(data)) {
		j, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if string(j) == "null" {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(j); err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		out = append(out, u)
	}
	return out, nil
}

func splitDocs(s string) []string {
	var docs []string
	var cur []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimRight(line, " ") == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return append(docs, strings.Join(cur, "\n"))
}

// LiteLLMConfig renders LiteLLM's config.yaml from the providers, and the
// env vars and Secrets carrying their keys. Provider keys stay in Secrets:
// the config names env vars (os.environ/...).
func LiteLLMConfig(providers []v1alpha1.ModelProvider) (string, []Provider) {
	type params struct {
		Model   string `json:"model"`
		APIKey  string `json:"api_key,omitempty"`
		APIBase string `json:"api_base,omitempty"`
	}
	type entry struct {
		ModelName string `json:"model_name"`
		Params    params `json:"litellm_params"`
	}
	var list []entry
	var envs []Provider
	for _, p := range providers {
		env := "PROVIDER_" + strings.ToUpper(strings.ReplaceAll(p.Name, "-", "_")) + "_API_KEY"
		prov := Provider{EnvVar: env}
		if p.APIKeySecretRef != nil {
			prov.SecretName = p.APIKeySecretRef.Name
		}
		envs = append(envs, prov)
		prefix := "openai/"
		if p.Kind == v1alpha1.ProviderAnthropic {
			prefix = "anthropic/"
		}
		for _, m := range p.Models {
			e := entry{ModelName: m, Params: params{Model: prefix + m, APIBase: p.BaseURL}}
			if prov.SecretName != "" {
				e.Params.APIKey = "os.environ/" + env
			} else {
				e.Params.APIKey = "none"
			}
			list = append(list, e)
		}
	}
	cfg := map[string]any{
		"model_list": list,
		"general_settings": map[string]any{
			"master_key":   "os.environ/LITELLM_MASTER_KEY",
			"database_url": "os.environ/DATABASE_URL",
		},
		"litellm_settings": map[string]any{
			"drop_params":              true,
			"telemetry":                false,
			"turn_off_message_logging": true,
		},
	}
	if list == nil {
		cfg["model_list"] = []entry{}
	}
	b, _ := yaml.Marshal(cfg)
	return string(b), envs
}

// Hash is a short stable hash of s, for names and annotations.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:10]
}
