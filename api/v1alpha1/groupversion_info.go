// Package v1alpha1 holds the swarmageddon custom resources: Platform,
// Workspace, Room, RoleTemplate and RunnerPool (docs/design.md §4).
//
// +kubebuilder:object:generate=true
// +groupName=swarmageddon.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version of every swarmageddon resource.
	GroupVersion = schema.GroupVersion{Group: "swarmageddon.dev", Version: "v1alpha1"}

	// SchemeBuilder collects the types each file registers in its init.
	SchemeBuilder = &schemeBuilder{}

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// schemeBuilder is the small part of controller-runtime's scheme.Builder this
// package needs, without depending on controller-runtime: API packages should
// import little beyond apimachinery.
type schemeBuilder struct{ objects []runtime.Object }

// Register adds types to register with a scheme.
func (b *schemeBuilder) Register(objects ...runtime.Object) {
	b.objects = append(b.objects, objects...)
}

// AddToScheme adds the registered types to s.
func (b *schemeBuilder) AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, b.objects...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Well-known names and labels shared by every component.
const (
	// PlatformName is the name of the singleton Platform.
	PlatformName = "default"
	// SystemNamespace holds the operator and the control center.
	SystemNamespace = "swarmageddon-system"
	// PlatformNamespace holds the shared platform services.
	PlatformNamespace = "swarmageddon-platform"
	// WorkspaceNamespacePrefix prefixes each workspace's namespace: ws-<name>.
	WorkspaceNamespacePrefix = "ws-"

	LabelManaged   = "swarmageddon.dev/managed"
	LabelWorkspace = "swarmageddon.dev/workspace"
	LabelRoom      = "swarmageddon.dev/room"
	LabelComponent = "swarmageddon.dev/component"
	LabelGroup     = "swarmageddon.dev/group"
	LabelVersion   = "swarmageddon.dev/version"

	// AnnotationRotate asks the room controller to rotate a room's credentials.
	AnnotationRotate = "swarmageddon.dev/rotate"
	// AnnotationRecreate asks the room controller to delete the room's pod.
	AnnotationRecreate = "swarmageddon.dev/recreate"
	// AnnotationUpdateNow lets the room controller roll the room to the new image now.
	AnnotationUpdateNow = "swarmageddon.dev/update-now"
	// AnnotationResetSetupToken asks the operator to issue a new setup token.
	AnnotationResetSetupToken = "swarmageddon.dev/reset-setup-token"
)

// WorkspaceNamespace returns the namespace of a workspace.
func WorkspaceNamespace(workspace string) string { return WorkspaceNamespacePrefix + workspace }
