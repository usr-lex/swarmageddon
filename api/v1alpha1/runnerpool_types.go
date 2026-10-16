package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunnerPoolSpec runs ephemeral one-job Forgejo Actions runners for a workspace.
type RunnerPoolSpec struct {
	// Labels jobs select with runs-on.
	// +kubebuilder:validation:MinItems=1
	Labels []string `json:"labels"`
	// Repos limits the pool to these repositories of the workspace's org; empty: the whole org.
	// +optional
	Repos []string `json:"repos,omitempty"`
	// Group limits the pool to repositories owned by this group's team.
	// +optional
	Group string `json:"group,omitempty"`
	// +kubebuilder:default=4
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
	// +optional
	Resources RunnerResources `json:"resources,omitempty"`
	// MinFreeDiskGB on the node below which jobs wait instead of starting.
	// +kubebuilder:default=5
	// +optional
	MinFreeDiskGB int32 `json:"minFreeDiskGB,omitempty"`
	// Image overrides the runner image.
	// +optional
	Image string `json:"image,omitempty"`
}

type RunnerResources struct {
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
	// +optional
	EphemeralStorage *resource.Quantity `json:"ephemeralStorage,omitempty"`
}

type RunnerPoolStatus struct {
	// +optional
	Running int32 `json:"running,omitempty"`
	// +optional
	Waiting int32 `json:"waiting,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Running",type=integer,JSONPath=`.status.running`
// +kubebuilder:printcolumn:name="Waiting",type=integer,JSONPath=`.status.waiting`

// RunnerPool is a workspace's CI capacity.
type RunnerPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunnerPoolSpec   `json:"spec,omitempty"`
	Status RunnerPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type RunnerPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RunnerPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RunnerPool{}, &RunnerPoolList{})
}
