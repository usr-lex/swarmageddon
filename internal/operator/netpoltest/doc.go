// Package netpoltest checks the NetworkPolicies the operator applies against
// the flows the design documents (docs/design.md §11.1, §11.2) and the
// components' actual clients. Its tests evaluate the policies in Go, the
// way the NetworkPolicy API defines them, over pods built from what the
// operator renders: the platform templates, the chart's Deployments, and
// the workspace, room and runner pool controllers' objects.
package netpoltest
