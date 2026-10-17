package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CLI is the agent CLI a room runs.
// +kubebuilder:validation:Enum=claude;ore
type CLI string

const (
	CLIClaude CLI = "claude"
	CLIOre    CLI = "ore"
)

// RoomSpec describes one worker's room. It lives in its workspace's namespace;
// the room's name is the agent's name.
type RoomSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Role names a RoleTemplate in the same namespace.
	Role string `json:"role"`
	// +kubebuilder:default=claude
	CLI CLI `json:"cli"`
	// Model, as the gateway names it. Empty: the workspace default.
	// +optional
	Model string `json:"model,omitempty"`
	// Effort, e.g. low, medium, high, xhigh, max.
	// +optional
	Effort string `json:"effort,omitempty"`
	// +optional
	Group string `json:"group,omitempty"`
	// Paused scales the room to zero; its volumes stay.
	// +optional
	Paused bool `json:"paused,omitempty"`
	// +optional
	Resources RoomResources `json:"resources,omitempty"`
	// +optional
	Volumes RoomVolumes `json:"volumes,omitempty"`
	// Deprecated: use Workspace.spec.sharedVolumes.
	// +optional
	SharedDirs []SharedDir `json:"sharedDirs,omitempty"`
	// Repos (owner/name in the forge) cloned into ~/work on first start.
	// +optional
	Repos []string `json:"repos,omitempty"`
	// WorkDir the agent starts in, relative to its home. Default "work".
	// +optional
	WorkDir string `json:"workDir,omitempty"`
	// Env adds non-secret environment variables, over the role's and workspace's.
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// SecretEnv adds variables from Secrets in the workspace namespace.
	// +optional
	SecretEnv []SecretEnvVar `json:"secretEnv,omitempty"`
	// MCPServers for this room, over the role's and workspace's.
	// +optional
	MCPServers map[string]MCPServer `json:"mcpServers,omitempty"`
	// Network access for this room, over the role's and workspace's.
	// +optional
	Network *NetworkAccess `json:"network,omitempty"`
	// Sudo gives the agent passwordless sudo inside the room.
	// +kubebuilder:default=true
	// +optional
	Sudo *bool `json:"sudo,omitempty"`
	// Advisor overrides the role template's advisor.
	// +optional
	Advisor *AdvisorSpec `json:"advisor,omitempty"`
	// WardenMode overrides the workspace's warden mode for this room.
	// +optional
	WardenMode WardenMode `json:"wardenMode,omitempty"`
	// Quarantined cuts the room off: model requests are refused, its network
	// shrinks to the gateway, its forge token is paused. Set by the warden or a
	// person; only a person clears it.
	// +optional
	Quarantined bool `json:"quarantined,omitempty"`
	// Image overrides the room image (for development).
	// +optional
	Image string `json:"image,omitempty"`
	// Janitor tunes the room's disk janitor (docs/design.md §12).
	// +optional
	Janitor *JanitorSpec `json:"janitor,omitempty"`
}

// JanitorSpec tunes a room's disk janitor (docs/design.md §12). Zero values
// keep the defaults.
type JanitorSpec struct {
	// Disabled turns the janitor off.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// IntervalMinutes between runs. Default 60.
	// +kubebuilder:validation:Minimum=1
	// +optional
	IntervalMinutes int32 `json:"intervalMinutes,omitempty"`
	// PruneAfterHours: stopped containers and dangling images older than this are removed. Default 24.
	// +kubebuilder:validation:Minimum=1
	// +optional
	PruneAfterHours int32 `json:"pruneAfterHours,omitempty"`
	// ImagesAbovePercent of the engine volume also removes unused tagged images. Default 70.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	ImagesAbovePercent int32 `json:"imagesAbovePercent,omitempty"`
	// WarnPercent of a volume raises diskLow. Default 80.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	WarnPercent int32 `json:"warnPercent,omitempty"`
	// AggressivePercent of a volume prunes everything unused. Default 90.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	AggressivePercent int32 `json:"aggressivePercent,omitempty"`
}

type RoomResources struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

type RoomVolumes struct {
	// +optional
	Home *resource.Quantity `json:"home,omitempty"`
	// +optional
	Engine *resource.Quantity `json:"engine,omitempty"`
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

type SharedDir struct {
	// Name of a ReadWriteMany PersistentVolumeClaim in the workspace namespace; mounted at /srv/shared/<name>.
	Name string `json:"name"`
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// RoomPhase summarizes a room's lifecycle.
// +kubebuilder:validation:Enum=Provisioning;Running;Paused;Updating;Failed
type RoomPhase string

const (
	RoomProvisioning RoomPhase = "Provisioning"
	RoomRunning      RoomPhase = "Running"
	RoomPaused       RoomPhase = "Paused"
	RoomUpdating     RoomPhase = "Updating"
	RoomFailed       RoomPhase = "Failed"
)

type RoomStatus struct {
	// +optional
	Phase RoomPhase `json:"phase,omitempty"`
	// +optional
	ForgeUser string `json:"forgeUser,omitempty"`
	// Address on the messaging service, name@workspace.
	// +optional
	Address string `json:"address,omitempty"`
	// +optional
	ImageVersion string `json:"imageVersion,omitempty"`
	// +optional
	UpdateAvailable bool `json:"updateAvailable,omitempty"`
	// +optional
	CredentialsRotatedAt *metav1.Time `json:"credentialsRotatedAt,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Room condition types.
const (
	ConditionForgeAccountReady      = "ForgeAccountReady"
	ConditionKeysReady              = "KeysReady"
	ConditionGatewayKeyReady        = "GatewayKeyReady"
	ConditionMessagingIdentityReady = "MessagingIdentityReady"
	ConditionPodReady               = "PodReady"
	ConditionQuarantined            = "Quarantined"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="CLI",type=string,JSONPath=`.spec.cli`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.group`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Room is one agent's persistent environment.
type Room struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RoomSpec   `json:"spec,omitempty"`
	Status RoomStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type RoomList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Room `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Room{}, &RoomList{})
}
