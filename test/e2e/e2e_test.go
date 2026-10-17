//go:build e2e

// Package e2e drives a whole installation on the `make dev` cluster (k3d) the
// way a person and the web UI do: through the control center's Connect API
// over HTTPS, with the platform CA pinned by its fingerprint. kubectl only
// observes, or does what a person does in a terminal (read the setup token,
// open a shell in a room). Design §14's E2E row, §3.3, §3.4, §7, §8.3, §9.
// Models are the in-cluster mock (hack/dev/extras.yaml).
//
//	make dev && make e2e
//
// Steps run in order; a step whose prerequisites failed is skipped, not
// failed. Environment: E2E_CONTEXT (default k3d-swarm-dev), KUBECTL, and
// E2E_URL (default https://localhost:8443/, the k3d load balancer; a
// port-forward is used if it doesn't answer).
package e2e

import "testing"

const (
	ws     = "acme"
	wsNS   = "ws-acme"
	mockLM = "http://mockmodel.swarmageddon-platform.svc:9999/v1"
	admin  = "admin" // the local admin's username and messaging handle
	runner = "swarm" // the workspace's runner label
)

func TestPlatform(t *testing.T) {
	s := newSuite(t)
	s.step("install", nil, s.install)
	s.step("ingress", []string{"install"}, s.ingress)
	s.step("claim admin", []string{"install"}, s.claimAdmin)
	s.step("preflight and isolation", []string{"claim admin"}, s.preflight)
	s.step("platform services", []string{"preflight and isolation"}, s.services)
	s.step("workspace wizard", []string{"platform services"}, s.workspace)
	s.step("rooms run", []string{"workspace wizard"}, s.roomsRun)

	rooms := []string{"rooms run"}
	s.step("message claude to ore", rooms, s.direct("athena", "hermes"))
	s.step("message ore to claude", rooms, s.direct("hermes", "athena"))
	s.step("message claude to claude", rooms, s.direct("athena", "artemis"))
	s.step("message ore to ore", rooms, s.direct("hermes", "apollo"))
	s.step("message person to agent", rooms, s.personToAgent)
	s.step("message agent to person", rooms, s.agentToPerson)
	s.step("mention in a channel", rooms, s.mention)
	s.step("crossing refused", rooms, s.crossingRefused)
	s.step("crossing over a mediated link", rooms, s.crossingMediated)

	s.step("forge signed push", rooms, s.signedPush)
	s.step("forge pull request", []string{"forge signed push"}, s.pullRequest)
	s.step("forge CI on an ephemeral runner", []string{"forge pull request"}, s.ciRunner)
	s.step("forge review", []string{"forge pull request"}, s.review)
	s.step("forge notification to the author", []string{"forge review"}, s.forgeNotice)
	s.step("forge merge by the mergers team", []string{"forge review"}, s.merge)

	s.step("add and remove a room", rooms, s.addRemoveRoom)
	s.step("quarantine and release", rooms, s.quarantine)
	s.step("observatory edges", []string{"message claude to ore", "message person to agent", "crossing over a mediated link"}, s.observatory)

	s.step("no secret in any log", []string{"install"}, s.noSecretsInLogs)
}
