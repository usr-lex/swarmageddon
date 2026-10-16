// Package v1alpha1 holds the swarmageddon custom resources: Platform,
// Workspace, Room, RoleTemplate and RunnerPool (docs/design.md §4).
//
// +kubebuilder:object:generate=true
// +groupName=swarmageddon.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version of every swarmageddon resource.
	GroupVersion = schema.GroupVersion{Group: "swarmageddon.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

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
