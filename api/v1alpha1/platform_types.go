package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsolationTier is how rooms nest containers (ADR 0001).
// +kubebuilder:validation:Enum=auto;userns;sysbox;privileged
type IsolationTier string

const (
	TierAuto       IsolationTier = "auto"
	TierUserns     IsolationTier = "userns"
	TierSysbox     IsolationTier = "sysbox"
	TierPrivileged IsolationTier = "privileged"
)

// PlatformSpec is the installation: services, sign-in, hostname and TLS.
type PlatformSpec struct {
	// Hostname the control center (and Forgejo, under forge.<hostname>) is served on.
	// Empty: reach it by node IP, NodePort or port-forward.
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// +optional
	TLS TLSSpec `json:"tls,omitempty"`
	// +kubebuilder:default=auto
	// +optional
	IsolationTier IsolationTier `json:"isolationTier,omitempty"`
	// PrivilegedAccepted must be true for rooms to run privileged; isolation does not hold then.
	// +optional
	PrivilegedAccepted bool `json:"privilegedAccepted,omitempty"`
	// +optional
	Auth AuthSpec `json:"auth,omitempty"`
	// Services lists the platform services and their sizes. Deployed once Wizard.ServicesApproved is set.
	// +optional
	Services ServicesSpec `json:"services,omitempty"`
	// +optional
	Egress EgressSpec `json:"egress,omitempty"`
	// Retention for the control center's audit log. 0 keeps it forever.
	// +optional
	AuditRetention *metav1.Duration `json:"auditRetention,omitempty"`
	// Wizard records how far the first-run wizard got, so it can be resumed.
	// +optional
	Wizard WizardSpec `json:"wizard,omitempty"`
}

// TLSMode chooses where the control center's certificate comes from.
// +kubebuilder:validation:Enum=selfSigned;certManager
type TLSMode string

const (
	TLSSelfSigned  TLSMode = "selfSigned"
	TLSCertManager TLSMode = "certManager"
)

type TLSSpec struct {
	// +kubebuilder:default=selfSigned
	// +optional
	Mode TLSMode `json:"mode,omitempty"`
	// cert-manager issuer, when Mode is certManager.
	// +optional
	IssuerRef *IssuerRef `json:"issuerRef,omitempty"`
}

type IssuerRef struct {
	Name string `json:"name"`
	// +kubebuilder:default=ClusterIssuer
	// +optional
	Kind string `json:"kind,omitempty"`
}

// Role is a control center role.
// +kubebuilder:validation:Enum=admin;operator;viewer
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

type AuthSpec struct {
	// +optional
	Local LocalAuthSpec `json:"local,omitempty"`
	// +optional
	OIDC []OIDCProvider `json:"oidc,omitempty"`
	// +optional
	SAML []SAMLProvider `json:"saml,omitempty"`
	// RoleMappings map identity-provider groups to roles. The highest matching role wins.
	// +optional
	RoleMappings []RoleMapping `json:"roleMappings,omitempty"`
	// DefaultRole for signed-in users no mapping matches. Empty refuses them.
	// +optional
	DefaultRole Role `json:"defaultRole,omitempty"`
}

type LocalAuthSpec struct {
	// Disabled turns off local accounts once an identity provider works.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
}

type OIDCProvider struct {
	// Name shown on the sign-in page; also the provider's ID in URLs.
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Name string `json:"name"`
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"clientID"`
	// Secret in the system namespace with key clientSecret.
	ClientSecretRef corev1.LocalObjectReference `json:"clientSecretRef"`
	// +kubebuilder:default=groups
	// +optional
	GroupsClaim string `json:"groupsClaim,omitempty"`
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

type SAMLProvider struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Name string `json:"name"`
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// +optional
	MetadataURL string `json:"metadataURL,omitempty"`
	// ConfigMap in the system namespace with key metadata.xml.
	// +optional
	MetadataConfigMapRef *corev1.LocalObjectReference `json:"metadataConfigMapRef,omitempty"`
	// +kubebuilder:default=groups
	// +optional
	GroupsAttribute string `json:"groupsAttribute,omitempty"`
}

type RoleMapping struct {
	Group string `json:"group"`
	Role  Role   `json:"role"`
}

type ServicesSpec struct {
	// +optional
	Postgres StorageService `json:"postgres,omitempty"`
	// +optional
	Forgejo ForgejoService `json:"forgejo,omitempty"`
	// +optional
	Messaging MessagingService `json:"messaging,omitempty"`
	// +optional
	Gateway GatewayService `json:"gateway,omitempty"`
	// +optional
	RegistryCache RegistryCacheService `json:"registryCache,omitempty"`
	// +optional
	Ntfy NtfyService `json:"ntfy,omitempty"`
}

type StorageService struct {
	// +optional
	Storage *resource.Quantity `json:"storage,omitempty"`
}

type ForgejoService struct {
	StorageService `json:",inline"`
	// OIDCLogin lets people sign in to Forgejo with the first OIDC provider.
	// +optional
	OIDCLogin bool `json:"oidcLogin,omitempty"`
}

type MessagingService struct {
	StorageService `json:",inline"`
	// Replicas; 0 picks 3, or 1 on a single-node cluster.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
}

// ProviderKind is a model provider's API.
// +kubebuilder:validation:Enum=anthropic;openai;openaiCompatible
type ProviderKind string

const (
	ProviderAnthropic        ProviderKind = "anthropic"
	ProviderOpenAI           ProviderKind = "openai"
	ProviderOpenAICompatible ProviderKind = "openaiCompatible"
)

type GatewayService struct {
	// +optional
	Providers []ModelProvider `json:"providers,omitempty"`
	// Env adds environment variables to LiteLLM, e.g. OPENSSL_armcap=0 where
	// OpenSSL crashes on the node's CPU. Names it sets itself are refused.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

type ModelProvider struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Name string       `json:"name"`
	Kind ProviderKind `json:"kind"`
	// Base URL, for openaiCompatible (e.g. a local vLLM) or a proxy.
	// +optional
	BaseURL string `json:"baseURL,omitempty"`
	// Secret with key apiKey. One in the system namespace (where the control
	// center writes it) is copied to the platform namespace for the gateway;
	// otherwise it is read from the platform namespace. Optional for local servers.
	// +optional
	APIKeySecretRef *corev1.LocalObjectReference `json:"apiKeySecretRef,omitempty"`
	// Models this provider serves, as the gateway exposes them.
	Models []string `json:"models"`
}

type RegistryCacheService struct {
	StorageService `json:",inline"`
	// +optional
	Upstreams []string `json:"upstreams,omitempty"`
}

type NtfyService struct {
	StorageService `json:",inline"`
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

type EgressSpec struct {
	// ClusterCIDRs overrides detection of the pod, service and node ranges rooms may not reach.
	// +optional
	ClusterCIDRs []string `json:"clusterCIDRs,omitempty"`
	// ExtraDeniedCIDRs are also kept from rooms.
	// +optional
	ExtraDeniedCIDRs []string `json:"extraDeniedCIDRs,omitempty"`
}

type WizardSpec struct {
	// ServicesApproved starts deploying the platform services.
	// +optional
	ServicesApproved bool `json:"servicesApproved,omitempty"`
	// Step the wizard was on, for resuming.
	// +optional
	Step string `json:"step,omitempty"`
}

// PlatformStatus is the operator's view of the installation.
type PlatformStatus struct {
	// +optional
	AdminClaimed bool `json:"adminClaimed,omitempty"`
	// URL the control center is reachable on, as far as the operator can tell.
	// +optional
	URL string `json:"url,omitempty"`
	// SHA-256 fingerprint of the control center's CA, to check before typing the setup token.
	// +optional
	CAFingerprint string `json:"caFingerprint,omitempty"`
	// +optional
	Preflight *PreflightStatus `json:"preflight,omitempty"`
	// PreflightRerun is the swarmageddon.dev/rerun-preflight annotation value
	// the operator last acted on; a different value runs preflight again.
	// +optional
	PreflightRerun string `json:"preflightRerun,omitempty"`
	// +optional
	Versions map[string]string `json:"versions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CheckResult is the outcome of one preflight check.
// +kubebuilder:validation:Enum=pass;warn;fail;pending
type CheckResult string

const (
	CheckPass    CheckResult = "pass"
	CheckWarn    CheckResult = "warn"
	CheckFail    CheckResult = "fail"
	CheckPending CheckResult = "pending"
)

type PreflightStatus struct {
	// +optional
	Checks []PreflightCheck `json:"checks,omitempty"`
	// The isolation tier preflight chose (or the spec forced).
	// +optional
	Tier IsolationTier `json:"tier,omitempty"`
	// About how many rooms of the default size fit.
	// +optional
	RoomEstimate int32 `json:"roomEstimate,omitempty"`
	// Detected ranges rooms may not reach.
	// +optional
	ClusterCIDRs []string `json:"clusterCIDRs,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

type PreflightCheck struct {
	Name   string      `json:"name"`
	Result CheckResult `json:"result"`
	// +optional
	Message string `json:"message,omitempty"`
	// What to do about a warning or failure.
	// +optional
	Fix string `json:"fix,omitempty"`
}

// Platform condition types.
const (
	ConditionPreflightDone     = "PreflightDone"
	ConditionPostgresReady     = "PostgresReady"
	ConditionForgejoReady      = "ForgejoReady"
	ConditionMessagingReady    = "MessagingReady"
	ConditionGatewayReady      = "GatewayReady"
	ConditionRegistryReady     = "RegistryCacheReady"
	ConditionNtfyReady         = "NtfyReady"
	ConditionControlCenterTLS  = "ControlCenterTLSReady"
	ConditionReady             = "Ready"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Admin",type=boolean,JSONPath=`.status.adminClaimed`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Platform is the installation. There is one, named "default".
type Platform struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformSpec   `json:"spec,omitempty"`
	Status PlatformStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type PlatformList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Platform `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Platform{}, &PlatformList{})
}
