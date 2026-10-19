// Package openshift lets the operator's pods past OpenShift's security
// context constraints (OpenShift report, issue 5.3). On OpenShift the default
// SCC (restricted-v2) refuses fixed UIDs, user-namespaced pods with added
// capabilities and hostPath. The chart ships two SCCs and a ClusterRole to
// use each (charts/swarmageddon/templates/openshift.yaml); the operator binds
// them, or OpenShift's nonroot-v2, to the service accounts of each namespace
// it creates.
package openshift

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
)

// Group is the API group whose presence means OpenShift.
const Group = "security.openshift.io"

// ClusterRoles that grant "use" on an SCC.
const (
	// Nonroot is OpenShift's own role for SCC nonroot-v2: any non-root UID,
	// for the platform services' fixed UIDs (Postgres 999, ...).
	Nonroot = "system:openshift:scc:nonroot-v2"
	// Rooms is the chart's role for SCC swarmageddon-rooms: user-namespaced
	// pods with the rooms' capabilities (rooms, runners, preflight probes).
	Rooms = "swarmageddon-scc-rooms"
	// Devices is the chart's role for SCC swarmageddon-devices: hostPath and
	// SELinux type container_device_plugin_t for the device plugin.
	Devices = "swarmageddon-scc-devices"
)

// BindingName is the RoleBinding the operator writes in each namespace.
const BindingName = "swarmageddon-scc"

// DevicePluginSELinuxType lets the device plugin reach the kubelet's socket.
const DevicePluginSELinuxType = "container_device_plugin_t"

// Detect reports whether the cluster serves security.openshift.io.
func Detect(d discovery.ServerGroupsInterface) (bool, error) {
	groups, err := d.ServerGroups()
	if err != nil {
		return false, err
	}
	for _, g := range groups.Groups {
		if g.Name == Group {
			return true, nil
		}
	}
	return false, nil
}

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=system:openshift:scc:nonroot-v2;swarmageddon-scc-rooms;swarmageddon-scc-devices

// Binding lets every service account in ns use the SCC behind clusterRole.
func Binding(ns, clusterRole string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: BindingName, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
		Subjects: []rbacv1.Subject{{
			APIGroup: rbacv1.GroupName, Kind: rbacv1.GroupKind, Name: "system:serviceaccounts:" + ns,
		}},
	}
}
