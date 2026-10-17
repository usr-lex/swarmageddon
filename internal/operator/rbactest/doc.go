// Package rbactest checks the RBAC that the chart installs and the operator
// grants against the Kubernetes API calls each component makes (docs/design.md
// §11.3). Its envtest test runs the operator's reconcilers as the operator's
// ServiceAccount with the chart's rendered RBAC (deploy/install.yaml, which
// CI checks against the chart), then makes the control center's,
// messaging's, the warden's, the egress proxy's and the welcome Job's calls
// as theirs, including calls that must be refused. A unit test checks that
// the operator holds every permission it grants, which RBAC requires.
package rbactest
