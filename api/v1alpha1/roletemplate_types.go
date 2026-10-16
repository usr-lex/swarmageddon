package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleTemplateSpec is a persona: role text plus CLI policy. Personas are
// guardrails, not security (docs/design.md §6.4).
type RoleTemplateSpec struct {
	Title string `json:"title"`
	// One line, shown in teammates' team lists.
	// +optional
	Summary string `json:"summary,omitempty"`
	// RoleText is written to the agent.
	RoleText string `json:"roleText"`
	// +optional
	ClaudeCode ClaudeCodePolicy `json:"claudeCode,omitempty"`
	// +optional
	Ore OrePolicy `json:"ore,omitempty"`
	// AutoApprove rules the control center applies to approval requests.
	// +optional
	AutoApprove []AutoApproveRule `json:"autoApprove,omitempty"`
	// ForgeTeams the room's forge user joins (implementers, reviewers, mergers, ci).
	// +optional
	ForgeTeams []string `json:"forgeTeams,omitempty"`
	// FirstTurn is the prompt a room gets when it starts. Empty: the built-in one.
	// +optional
	FirstTurn string `json:"firstTurn,omitempty"`
}

// PermissionMode is Claude Code's permission mode.
// +kubebuilder:validation:Enum=default;acceptEdits;auto;plan;bypassPermissions
type PermissionMode string

type ClaudeCodePolicy struct {
	// +optional
	PermissionMode PermissionMode `json:"permissionMode,omitempty"`
	// +optional
	Allow []string `json:"allow,omitempty"`
	// +optional
	Deny []string `json:"deny,omitempty"`
	// Hooks in Claude Code's settings.json format, added after the platform's own.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Hooks *runtime.RawExtension `json:"hooks,omitempty"`
	// ManagedRulesOnly sets allowManagedPermissionRulesOnly.
	// +optional
	ManagedRulesOnly bool `json:"managedRulesOnly,omitempty"`
}

// OreApprovalPolicy is ore's approval_policy.
// +kubebuilder:validation:Enum=on-request;on-failure;never
type OreApprovalPolicy string

type OrePolicy struct {
	// +kubebuilder:default=on-request
	// +optional
	ApprovalPolicy OreApprovalPolicy `json:"approvalPolicy,omitempty"`
	// +kubebuilder:validation:Enum=user;auto_review
	// +kubebuilder:default=user
	// +optional
	ApprovalsReviewer string `json:"approvalsReviewer,omitempty"`
	// SandboxMode: read-only, workspace-write or danger-full-access.
	// +optional
	SandboxMode string `json:"sandboxMode,omitempty"`
	// Rules in execpolicy syntax (prefix_rule(...)), one file.
	// +optional
	Rules string `json:"rules,omitempty"`
	// MCPTools: per server, the tools enabled. Empty: all.
	// +optional
	MCPTools map[string][]string `json:"mcpTools,omitempty"`
	// Bypass runs ore without approvals or sandbox.
	// +optional
	Bypass bool `json:"bypass,omitempty"`
}

// ApprovalDecision is what an automatic rule answers.
// +kubebuilder:validation:Enum=allow;deny
type ApprovalDecision string

type AutoApproveRule struct {
	// Tool name (Claude: Bash, Edit, ...; ore: exec, patch). "*" for any.
	Tool string `json:"tool"`
	// Match is a regular expression against the command or file path.
	// +optional
	Match string `json:"match,omitempty"`
	// +kubebuilder:default=allow
	// +optional
	Decision ApprovalDecision `json:"decision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`

// RoleTemplate is a persona rooms are created from.
type RoleTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RoleTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

type RoleTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RoleTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RoleTemplate{}, &RoleTemplateList{})
}
