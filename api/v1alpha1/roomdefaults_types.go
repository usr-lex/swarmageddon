package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
)

// RoomDefaults are room settings written at the workspace or role level and
// merged into each room: workspace, then role, then the room's own spec, later
// levels overriding earlier ones by key (docs/design.md §6.6).
type RoomDefaults struct {
	// Env adds non-secret environment variables.
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// SecretEnv adds environment variables whose values come from Secrets in
	// the workspace namespace.
	// +optional
	SecretEnv []SecretEnvVar `json:"secretEnv,omitempty"`
	// MCPServers for both CLIs, by name, in the .mcp.json format.
	// +optional
	MCPServers map[string]MCPServer `json:"mcpServers,omitempty"`
	// Network is the room's network access (docs/design.md §11.2).
	// +optional
	Network *NetworkAccess `json:"network,omitempty"`
}

// SecretKeyRef names a key of a Secret in the workspace namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// SecretEnvVar is an environment variable whose value comes from a Secret.
type SecretEnvVar struct {
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	Name      string       `json:"name"`
	SecretRef SecretKeyRef `json:"secretRef"`
}

// MCPValue is a literal value, or one taken from a Secret.
type MCPValue struct {
	// +optional
	Value string `json:"value,omitempty"`
	// +optional
	ValueFrom *SecretKeyRef `json:"valueFrom,omitempty"`
}

// MCPServer is one MCP server, as in .mcp.json.
type MCPServer struct {
	// stdio (default when command is set) or http.
	// +kubebuilder:validation:Enum=stdio;http
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	Command string `json:"command,omitempty"`
	// +optional
	Args []string `json:"args,omitempty"`
	// +optional
	Env map[string]MCPValue `json:"env,omitempty"`
	// +optional
	URL string `json:"url,omitempty"`
	// +optional
	Headers map[string]MCPValue `json:"headers,omitempty"`
	// EnabledTools limits the server's tools; empty: all.
	// +optional
	EnabledTools []string `json:"enabledTools,omitempty"`
	// Disabled removes a server inherited from an earlier level.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
}

// NetworkMode is how much network a room gets.
// +kubebuilder:validation:Enum=none;limited;full
type NetworkMode string

const (
	NetworkNone    NetworkMode = "none"
	NetworkLimited NetworkMode = "limited"
	NetworkFull    NetworkMode = "full"
)

// NetworkAccess is a room's network access (docs/design.md §11.2).
type NetworkAccess struct {
	// +kubebuilder:default=full
	// +optional
	Mode NetworkMode `json:"mode,omitempty"`
	// Domains reachable in limited mode: exact hosts or *.suffix, with an
	// optional :port (default 443 and 80). Merged across levels.
	// +optional
	Domains []string `json:"domains,omitempty"`
	// PackageManagers adds the package registries' domains in limited mode.
	// +optional
	PackageManagers *bool `json:"packageManagers,omitempty"`
}

// SharedVolume is a ReadWriteMany volume mounted into the rooms its mounts
// select, at /srv/shared/<name> (docs/design.md §6.6).
type SharedVolume struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,62}$`
	Name string            `json:"name"`
	Size resource.Quantity `json:"size"`
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Mounts []VolumeMount `json:"mounts"`
}

// VolumeMount selects rooms for a shared volume.
type VolumeMount struct {
	// Selector: everyone, role:<template>, group:<group> or room:<name>.
	// +kubebuilder:validation:Pattern=`^(everyone|(role|group|room):[a-z0-9][a-z0-9_-]{0,62})$`
	Select string `json:"select"`
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}
