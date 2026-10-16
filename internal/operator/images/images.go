// Package images pins every image the operator deploys (docs/conventions.md,
// "Images"). Third-party images are pinned by tag and digest; our own images
// follow the operator's version. A YAML or JSON file (--images-from) can
// override any of them, for air-gapped mirrors or development.
package images

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Registry is where our own images are published.
const Registry = "ghcr.io/usr-lex/swarmageddon"

// Images are the image references the operator uses. JSON names are the keys
// accepted in an override file.
type Images struct {
	Postgres      string `json:"postgres"`
	Forgejo       string `json:"forgejo"`
	LiteLLM       string `json:"litellm"`
	Registry      string `json:"registry"`
	Ntfy          string `json:"ntfy"`
	Busybox       string `json:"busybox"`
	ForgejoRunner string `json:"forgejoRunner"`

	Operator      string `json:"operator"`
	ControlCenter string `json:"controlCenter"`
	Messaging     string `json:"messaging"`
	Warden        string `json:"warden"`
	Egress        string `json:"egress"`
	Room          string `json:"room"`
	Runner        string `json:"runner"`
}

// Third-party pins. Digests were read from the registries on 2026-10-16.
const (
	postgres = "docker.io/library/postgres:18.6@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336"
	forgejo  = "codeberg.org/forgejo/forgejo:17.0.0-rootless@sha256:6554520f7574602100b7d6620c6ac5b18cec30325f823777497eac36234f5cf8"
	// LiteLLM is pinned by digest: two of its releases were compromised in 2026 (design §10.1).
	litellm  = "ghcr.io/berriai/litellm-database:v1.105.1@sha256:2bcadb9905b692a5ca4dadd51bccaaa23a95dc8a0f9322d9c9787f0ac2aeb50e"
	registry = "docker.io/library/registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8"
	ntfy     = "docker.io/binwiederhier/ntfy:v2.31.0@sha256:803b2aaeb94df76c8b32ecd91039dc80b0fddb7041dcaf082b33bcdfd4ba39ad"
	busybox  = "docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e"
	// forgejo-runner tag is assumed (code.forgejo.org was unreachable when pinned); no digest.
	forgejoRunner = "code.forgejo.org/forgejo/runner:13.2.0"
)

// Defaults returns the pinned images for operator version v.
func Defaults(v string) Images {
	tag := v
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	own := func(name string) string { return Registry + "/" + name + ":" + tag }
	return Images{
		Postgres:      postgres,
		Forgejo:       forgejo,
		LiteLLM:       litellm,
		Registry:      registry,
		Ntfy:          ntfy,
		Busybox:       busybox,
		ForgejoRunner: forgejoRunner,
		Operator:      own("operator"),
		ControlCenter: own("control-center"),
		Messaging:     own("messaging"),
		Warden:        own("warden"),
		Egress:        own("egress"),
		Room:          own("room"),
		Runner:        own("runner"),
	}
}

// Load returns Defaults(v) with the non-empty entries of the file at path
// applied. An empty path returns the defaults.
func Load(path, v string) (Images, error) {
	imgs := Defaults(v)
	if path == "" {
		return imgs, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return imgs, err
	}
	var over Images
	if err := yaml.UnmarshalStrict(data, &over); err != nil {
		return imgs, fmt.Errorf("%s: %w", path, err)
	}
	imgs.merge(over)
	return imgs, nil
}

func (i *Images) merge(o Images) {
	set := func(dst *string, src string) {
		if src != "" {
			*dst = src
		}
	}
	set(&i.Postgres, o.Postgres)
	set(&i.Forgejo, o.Forgejo)
	set(&i.LiteLLM, o.LiteLLM)
	set(&i.Registry, o.Registry)
	set(&i.Ntfy, o.Ntfy)
	set(&i.Busybox, o.Busybox)
	set(&i.ForgejoRunner, o.ForgejoRunner)
	set(&i.Operator, o.Operator)
	set(&i.ControlCenter, o.ControlCenter)
	set(&i.Messaging, o.Messaging)
	set(&i.Warden, o.Warden)
	set(&i.Egress, o.Egress)
	set(&i.Room, o.Room)
	set(&i.Runner, o.Runner)
}

// Versions is what Platform status reports under status.versions.
func (i Images) Versions(operator string) map[string]string {
	return map[string]string{
		"operator":      operator,
		"room":          i.Room,
		"postgres":      i.Postgres,
		"forgejo":       i.Forgejo,
		"litellm":       i.LiteLLM,
		"registryCache": i.Registry,
		"ntfy":          i.Ntfy,
		"messaging":     i.Messaging,
		"warden":        i.Warden,
		"egress":        i.Egress,
		"controlCenter": i.ControlCenter,
	}
}
