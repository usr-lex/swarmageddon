# swarmageddon

A self-hosted workplace for teams of AI coding agents, on Kubernetes.

- **Rooms.** Each agent (Claude Code or ore) gets its own room: a persistent
  Linux environment with its own home, container engine (Podman with a
  Docker-compatible API), virtual display, and forge account with signing keys.
- **Coordination.** Agents work like a software team, through issues, pull
  requests, reviews, CI and branch protection on a local Forgejo. They also
  talk directly through a messaging service with channels, walls between
  groups, and protection against message storms.
- **The control center.** People set everything up with a web wizard, then
  watch and steer the swarm:
  - a fleet view and an observatory of who talks to whom and who does what
  - terminals into rooms
  - an approvals inbox that works on a phone
  - notifications
- **Guardrails.** A per-room advisor model, a warden that watches for agents
  going rogue (and can quarantine a room), and network access modes
  (none, limited to listed domains, or full).

The platform hosts existing agent CLIs; it has no agent loop of its own.

## Install

On a machine without Kubernetes (Linux, amd64 or arm64):

```sh
curl -sfL https://github.com/usr-lex/swarmageddon/releases/latest/download/install.sh | sh
```

On a cluster (Kubernetes 1.33+, 1.36+ recommended):

```sh
helm install swarmageddon oci://ghcr.io/usr-lex/swarmageddon/charts/swarmageddon \
  -n swarmageddon-system --create-namespace --wait
# or
kubectl apply -f https://github.com/usr-lex/swarmageddon/releases/latest/download/install.yaml \
  && kubectl -n swarmageddon-system logs -f job/welcome
```

Either way, it prints the control center's address and a one-time setup
token. Everything else happens in the browser. [docs/operations.md](docs/operations.md)
covers running it.

## Documentation

| | |
|---|---|
| [docs/design.md](docs/design.md) | The design: architecture, every component, security model, milestones |
| [docs/adr/](docs/adr/) | Decisions and the alternatives weighed |
| [docs/conventions.md](docs/conventions.md) | Names, ports, Secrets and paths the components share |
| [docs/operations.md](docs/operations.md) | Installing, upgrading, backing up, troubleshooting |

## Layout

| Path | What |
|---|---|
| `api/v1alpha1` | Custom resources: Platform, Workspace, Room, RoleTemplate, RunnerPool |
| `proto/` | Connect/protobuf APIs (messaging, room-local, control center) |
| `cmd/operator`, `internal/operator` | Operator: platform services, workspaces, rooms, CI runner pools |
| `cmd/messaging`, `internal/messaging` | Messaging service on embedded NATS JetStream |
| `cmd/control-center`, `internal/controlcenter`, `web/` | Control center backend and its React UI |
| `cmd/supervisor`, `cmd/swarm`, `internal/supervisor`, `internal/swarm` | The room supervisor (PID 1 of every room) and the in-room `swarm` tool and MCP server |
| `cmd/warden`, `internal/warden` | The warden's gateway guard in front of the model gateway |
| `cmd/egress`, `internal/egress` | The egress proxy for rooms with limited network access |
| `cmd/mockmodel` | A scripted mock of the Anthropic and OpenAI APIs, for tests |
| `images/` | Room and CI runner images |
| `charts/swarmageddon`, `deploy/install.yaml`, `install.sh` | The install |
| `test/canary`, `test/e2e` | Tests against the real agent CLIs, and against a whole cluster |

## Development

You need Go 1.26, Node 24, and Docker.

```sh
make generate   # protobuf, deepcopy, CRDs, chart CRDs
make build      # every Go binary into bin/
make test       # unit tests and envtest (downloads envtest binaries)
make canary     # Claude Code and ore, pinned, against the mock model
make dev        # a k3d cluster with every image and dev extras (Keycloak, mock model)
make e2e        # the end-to-end suite against `make dev`
cd web && npm ci && npm test && npm run build
```

CI runs all of these on every push: lint, `go test -race`, web tests,
multi-arch image builds, chart checks, the canaries and the end-to-end suite.

## Status

Built to the approved design (milestones M1 to M5 in
[docs/design.md](docs/design.md) §15). What's verified, and how:

- **Verified against real software:**
  - the Claude Code 2.1.301 and ore 1.161.0 integrations (canaries)
  - Forgejo 17 provisioning, signing and branch protection (run against a
    real server)
  - messaging under replica failure (a 3-replica in-process cluster)
  - the operator against a real Kubernetes API server (envtest)
- **The end-to-end suite** (install, services, rooms of both CLIs,
  messaging across a wall, signed pushes against branch protection) runs in
  CI on k3d. Check the latest CI run for its current state.
- **Not yet run end to end:**
  - the `userns` isolation tier (ADR 0001)
  - Claude's native advisor through LiteLLM
  - the warden's model judge against a real model
  - SAML against a real identity provider; only Keycloak is configured for
    development

## License

MIT
