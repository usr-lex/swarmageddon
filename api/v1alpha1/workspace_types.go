package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkspaceSpec is a team: its forge org, groups, messaging policy, models
// and notifications. The workspace owns the namespace ws-<name>.
type WorkspaceSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	Forge       ForgeSpec `json:"forge"`
	// +optional
	Groups []Group `json:"groups,omitempty"`
	// +optional
	Messaging MessagingPolicy `json:"messaging,omitempty"`
	// +optional
	Models ModelSettings `json:"models,omitempty"`
	// +optional
	Notifications NotificationSettings `json:"notifications,omitempty"`
	// +optional
	Repositories []RepositorySpec `json:"repositories,omitempty"`
	// +kubebuilder:default=inherit
	// +optional
	IsolationTier string `json:"isolationTier,omitempty"`
	// +optional
	UpdatePolicy UpdatePolicy `json:"updatePolicy,omitempty"`
	// Owners are control center users (by email or handle) who operate this workspace.
	// +optional
	Owners []string `json:"owners,omitempty"`
	// RoomDefaults apply to every room of the workspace (docs/design.md §6.6).
	// +optional
	RoomDefaults RoomDefaults `json:"roomDefaults,omitempty"`
	// SharedVolumes are ReadWriteMany volumes shared between rooms.
	// +optional
	SharedVolumes []SharedVolume `json:"sharedVolumes,omitempty"`
	// Deprecated: use roomDefaults.network. false is the same as network mode none.
	// +kubebuilder:default=true
	// +optional
	Internet *bool `json:"internet,omitempty"`
	// AllowedCIDRs rooms may reach even inside denied ranges (e.g. a model server on the LAN).
	// +optional
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`
	// ApprovalTimeout after which an unanswered approval leaves the inbox. Default 24h.
	// +optional
	ApprovalTimeout *metav1.Duration `json:"approvalTimeout,omitempty"`
	// ForgeNotifications lists the forge events delivered to agents as messages
	// (docs/design.md §9.7). Empty: all of them.
	// +optional
	ForgeNotifications []ForgeEvent `json:"forgeNotifications,omitempty"`
	// Warden watches agents for drift and risky actions (ADR 0011).
	// +optional
	Warden WardenSpec `json:"warden,omitempty"`
	// DeletedRoomRetention keeps a removed room's volumes this long. Default 168h.
	// +optional
	DeletedRoomRetention *metav1.Duration `json:"deletedRoomRetention,omitempty"`
}

type ForgeSpec struct {
	// Org is the Forgejo organization the workspace's agents work in.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
	Org string `json:"org"`
	// +kubebuilder:default=1
	// +optional
	RequiredApprovals int32 `json:"requiredApprovals,omitempty"`
	// +kubebuilder:default=true
	// +optional
	SignedCommits *bool `json:"signedCommits,omitempty"`
	// Extra teams and their members (room names). Built-in teams come from role templates.
	// +optional
	Teams []TeamSpec `json:"teams,omitempty"`
}

// TeamPermission is a Forgejo team permission.
// +kubebuilder:validation:Enum=read;write;admin
type TeamPermission string

type TeamSpec struct {
	Name string `json:"name"`
	// +kubebuilder:default=write
	// +optional
	Permission TeamPermission `json:"permission,omitempty"`
	// +optional
	Members []string `json:"members,omitempty"`
}

type Group struct {
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	Name string `json:"name"`
	// +optional
	Description string `json:"description,omitempty"`
}

// LinkMode is how one group may reach another.
// +kubebuilder:validation:Enum=open;mediated;none
type LinkMode string

const (
	LinkOpen     LinkMode = "open"
	LinkMediated LinkMode = "mediated"
	LinkNone     LinkMode = "none"
)

// DefaultLinks is how rooms in the same group reach each other.
// +kubebuilder:validation:Enum=withinGroup;none
type DefaultLinks string

const (
	DefaultWithinGroup DefaultLinks = "withinGroup"
	DefaultNone        DefaultLinks = "none"
)

type MessagingPolicy struct {
	// +kubebuilder:default=withinGroup
	// +optional
	DefaultLinks DefaultLinks `json:"defaultLinks,omitempty"`
	// Links between groups. "" names the default group (rooms without a group).
	// +optional
	Links []GroupLink `json:"links,omitempty"`
	// +optional
	Limits MessageLimits `json:"limits,omitempty"`
	// +optional
	Retention RetentionSettings `json:"retention,omitempty"`
	// Channels are the workspace's one-to-many conversations (docs/design.md §7.8).
	// +optional
	Channels []ChannelSpec `json:"channels,omitempty"`
	// Storm keeps agents' sessions from being flooded (docs/design.md §7.8).
	// +optional
	Storm StormControl `json:"storm,omitempty"`
	// People with the operator role may message agents; agents may message people.
	// +kubebuilder:default=true
	// +optional
	HumansReachable *bool `json:"humansReachable,omitempty"`
}

// GroupLink lets messages flow one way, from one group to another.
type GroupLink struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Mode LinkMode `json:"mode"`
	// Mediated: only these rooms in From may send. Empty: anyone.
	// +optional
	Senders []string `json:"senders,omitempty"`
	// Mediated: only these rooms in To may receive. Empty: anyone.
	// +optional
	Recipients []string `json:"recipients,omitempty"`
	// RequirePrefix the message must start with; {recipient} is replaced by the recipient's name.
	// +optional
	RequirePrefix string `json:"requirePrefix,omitempty"`
}

// ChannelAccess is what a channel member may do.
// +kubebuilder:validation:Enum=read;write;readWrite
type ChannelAccess string

const (
	AccessRead      ChannelAccess = "read"
	AccessWrite     ChannelAccess = "write"
	AccessReadWrite ChannelAccess = "readWrite"
)

// DeliveryMode is how a channel's posts reach a reader's session.
// +kubebuilder:validation:Enum=push;digest;pull
type DeliveryMode string

const (
	DeliveryPush   DeliveryMode = "push"
	DeliveryDigest DeliveryMode = "digest"
	DeliveryPull   DeliveryMode = "pull"
)

// ChannelSpec is a named conversation and who may read and post in it.
type ChannelSpec struct {
	// Name without the leading #.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9_-]{0,62}$`
	Name string `json:"name"`
	// +optional
	Description string `json:"description,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Members []ChannelMember `json:"members"`
	// +kubebuilder:default=digest
	// +optional
	DefaultDelivery DeliveryMode `json:"defaultDelivery,omitempty"`
	// AllowedDelivery lists the modes readers may switch to. Empty: all.
	// +optional
	AllowedDelivery []DeliveryMode `json:"allowedDelivery,omitempty"`
	// PostsPerMinute across all posters. Default 20.
	// +optional
	PostsPerMinute int32 `json:"postsPerMinute,omitempty"`
}

// ChannelMember selects members and gives them access.
type ChannelMember struct {
	// Selector: everyone, people, role:<template>, team:<forge team>,
	// group:<group>, room:<name> or person:<handle>.
	// +kubebuilder:validation:Pattern=`^(everyone|people|(role|team|group|room|person):[a-z0-9][a-z0-9_-]{0,62})$`
	Select string        `json:"select"`
	Access ChannelAccess `json:"access"`
}

// StormControl limits what reaches an agent's session (docs/design.md §7.8).
type StormControl struct {
	// PushPerMinute is how many items may be pushed into a session per minute. Default 6.
	// +optional
	PushPerMinute int32 `json:"pushPerMinute,omitempty"`
	// CoalesceBytes caps one coalesced injection. Default 4096.
	// +optional
	CoalesceBytes int32 `json:"coalesceBytes,omitempty"`
	// DigestInterval between digest notices per channel. Default 15m.
	// +optional
	DigestInterval *metav1.Duration `json:"digestInterval,omitempty"`
	// FanoutPerMinute caps recipients a sender reaches per minute. Default 60.
	// +optional
	FanoutPerMinute int32 `json:"fanoutPerMinute,omitempty"`
}

type MessageLimits struct {
	// +kubebuilder:default=65536
	// +optional
	MaxBytes int32 `json:"maxBytes,omitempty"`
	// +kubebuilder:default=30
	// +optional
	RatePerMinute int32 `json:"ratePerMinute,omitempty"`
}

// RetentionSettings are TTLs; 0 keeps forever.
type RetentionSettings struct {
	// +optional
	Messages *metav1.Duration `json:"messages,omitempty"`
	// +optional
	Audit *metav1.Duration `json:"audit,omitempty"`
	// +optional
	Transcripts *metav1.Duration `json:"transcripts,omitempty"`
	// AuditBodies keeps message bodies in the audit log; false keeps metadata only.
	// +kubebuilder:default=true
	// +optional
	AuditBodies *bool `json:"auditBodies,omitempty"`
}

type ModelSettings struct {
	// +optional
	Default string `json:"default,omitempty"`
	// +optional
	Budgets BudgetSettings `json:"budgets,omitempty"`
}

type BudgetSettings struct {
	// Monthly budget per room in USD; 0 is unlimited.
	// +optional
	PerRoomMonthlyUSD int32 `json:"perRoomMonthlyUSD,omitempty"`
}

// ChannelKind is a notification channel.
// +kubebuilder:validation:Enum=webPush;ntfy;slack;discord;email
type ChannelKind string

// NotificationEvent is something people get notified about.
// +kubebuilder:validation:Enum=approvalWaiting;idleWithOpenWork;ciRed;budget;diskLow;roomCrashed;wardenFlag
type NotificationEvent string

const (
	EventApprovalWaiting  NotificationEvent = "approvalWaiting"
	EventIdleWithOpenWork NotificationEvent = "idleWithOpenWork"
	EventCIRed            NotificationEvent = "ciRed"
	EventBudget           NotificationEvent = "budget"
	EventDiskLow          NotificationEvent = "diskLow"
	EventRoomCrashed      NotificationEvent = "roomCrashed"
	EventWardenFlag       NotificationEvent = "wardenFlag"
)

type NotificationSettings struct {
	// +optional
	Channels []NotificationChannel `json:"channels,omitempty"`
	// +optional
	Events []NotificationEvent `json:"events,omitempty"`
}

type NotificationChannel struct {
	Kind ChannelKind `json:"kind"`
	// ntfy topic.
	// +optional
	Topic string `json:"topic,omitempty"`
	// Secret in the system namespace holding key url (Slack, Discord) or smtp settings (email).
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

type RepositorySpec struct {
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
	Name string `json:"name"`
	// ImportURL to clone from instead of creating an empty repository.
	// +optional
	ImportURL string `json:"importURL,omitempty"`
	// ImportSecretName is a Secret in the workspace namespace with the
	// credential for ImportURL (keys token, and optionally username), used
	// for the one import only.
	// +optional
	ImportSecretName string `json:"importSecretName,omitempty"`
	// Group whose team owns it; empty: the default teams.
	// +optional
	Group string `json:"group,omitempty"`
	// PushMirror to GitHub (or any git URL); credentials from MirrorSecretName (key token).
	// +optional
	PushMirror string `json:"pushMirror,omitempty"`
	// MirrorSecretName is a Secret in the workspace namespace (key token,
	// optionally username), used only by Forgejo, never mounted in a room.
	// +optional
	MirrorSecretName string `json:"mirrorSecretName,omitempty"`
	// StatusChecks required on the default branch.
	// +optional
	StatusChecks []string `json:"statusChecks,omitempty"`
}

// WardenMode is how closely the warden watches.
// +kubebuilder:validation:Enum=off;monitor;gate
type WardenMode string

const (
	WardenOff     WardenMode = "off"
	WardenMonitor WardenMode = "monitor"
	WardenGate    WardenMode = "gate"
)

// WardenSpec configures the warden (ADR 0011).
type WardenSpec struct {
	// +kubebuilder:default=monitor
	// +optional
	Mode WardenMode `json:"mode,omitempty"`
	// Model the warden judges with: a small, fast model on the gateway. Empty: rules only.
	// +optional
	Model string `json:"model,omitempty"`
	// InterruptOnHigh interrupts the agent's turn on a high-severity verdict.
	// +kubebuilder:default=true
	// +optional
	InterruptOnHigh *bool `json:"interruptOnHigh,omitempty"`
	// QuarantineOnHigh quarantines on high verdicts from the gateway guard, not only critical ones.
	// +optional
	QuarantineOnHigh bool `json:"quarantineOnHigh,omitempty"`
	// Monthly budget of each room's warden key in USD. Default 20.
	// +optional
	MonthlyUSD int32 `json:"monthlyUSD,omitempty"`
	// ReviewEveryToolCalls also reviews a room's trajectory every N tool
	// calls, besides after each turn (ADR 0011, layer 3). Default 20.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ReviewEveryToolCalls int32 `json:"reviewEveryToolCalls,omitempty"`
}

// ForgeEvent is a kind of forge activity delivered to agents.
// +kubebuilder:validation:Enum=assigned;reviewRequested;mentioned;reviewSubmitted;commented;ciFailed
type ForgeEvent string

const (
	ForgeAssigned        ForgeEvent = "assigned"
	ForgeReviewRequested ForgeEvent = "reviewRequested"
	ForgeMentioned       ForgeEvent = "mentioned"
	ForgeReviewSubmitted ForgeEvent = "reviewSubmitted"
	ForgeCommented       ForgeEvent = "commented"
	ForgeCIFailed        ForgeEvent = "ciFailed"
)

// UpdateMode is when rooms move to a new image.
// +kubebuilder:validation:Enum=whenIdle;manual;window
type UpdateMode string

const (
	UpdateWhenIdle UpdateMode = "whenIdle"
	UpdateManual   UpdateMode = "manual"
	UpdateWindow   UpdateMode = "window"
)

type UpdatePolicy struct {
	// +kubebuilder:default=whenIdle
	// +optional
	Mode UpdateMode `json:"mode,omitempty"`
	// Window, for mode window, e.g. "Sat 02:00-06:00" (UTC).
	// +optional
	Window string `json:"window,omitempty"`
}

type WorkspaceStatus struct {
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Workspace condition types.
const (
	ConditionNamespaceReady = "NamespaceReady"
	ConditionForgeOrgReady  = "ForgeOrgReady"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ws
// +kubebuilder:printcolumn:name="Org",type=string,JSONPath=`.spec.forge.org`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workspace is a team of agents.
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec,omitempty"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workspace{}, &WorkspaceList{})
}
