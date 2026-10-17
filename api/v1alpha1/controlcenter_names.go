package v1alpha1

import (
	"slices"
	"strings"
)

// The control center's Secrets and ConfigMaps in the system namespace
// (docs/design.md §11.3). It may create any Secret or ConfigMap there, but
// read and change only these. The chart's Role control-center grants the
// fixed names; the operator's Role ControlCenterOwnedRole grants, one by one,
// the existing objects whose names have one of the prefixes. The operator
// never gives its own Secrets these names or prefixes.
var (
	// ControlCenterSecrets are the fixed names.
	ControlCenterSecrets = []string{
		"setup-token", "local-admin", "control-center-session", "control-center-db", "vapid",
		"control-center-saml", "control-center-gateway", "forge-reader", "ntfy-publisher",
	}
	// ControlCenterSecretPrefixes prefix the Secrets made per model
	// provider, sign-in provider, notification channel and workspace
	// (forge-webhook-<ws>, which the operator makes for the control center).
	ControlCenterSecretPrefixes = []string{"gateway-provider-", "oidc-", "notify-", "forge-webhook-"}
	// ControlCenterConfigMaps are the fixed ConfigMap names it reads.
	ControlCenterConfigMaps = []string{"platform-ca-bundle"}
	// ControlCenterConfigMapPrefixes prefix the ConfigMaps it writes
	// (saml-<provider>: pasted IdP metadata).
	ControlCenterConfigMapPrefixes = []string{"saml-"}
)

// ControlCenterOwnedRole is the Role (and RoleBinding) in the system
// namespace that the operator keeps listing the control center's prefixed
// Secrets and ConfigMaps.
const ControlCenterOwnedRole = "control-center-owned"

// ControlCenterSecret reports whether the control center may read and
// change a Secret of this name in the system namespace.
func ControlCenterSecret(name string) bool {
	return slices.Contains(ControlCenterSecrets, name) || hasNamePrefix(name, ControlCenterSecretPrefixes)
}

// ControlCenterPrefixedSecret reports whether name is one of the control
// center's Secrets that the operator grants by name.
func ControlCenterPrefixedSecret(name string) bool {
	return hasNamePrefix(name, ControlCenterSecretPrefixes)
}

// ControlCenterConfigMap reports whether the control center may read a
// ConfigMap of this name in the system namespace.
func ControlCenterConfigMap(name string) bool {
	return slices.Contains(ControlCenterConfigMaps, name) || hasNamePrefix(name, ControlCenterConfigMapPrefixes)
}

// ControlCenterPrefixedConfigMap reports whether name is one of the control
// center's ConfigMaps that the operator grants by name.
func ControlCenterPrefixedConfigMap(name string) bool {
	return hasNamePrefix(name, ControlCenterConfigMapPrefixes)
}

func hasNamePrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	return false
}
