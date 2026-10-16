# Architecture decision records

Each record states one decision, the options we weighed, and what it costs us.
A record is never edited after it is accepted; a later record supersedes it.

| # | Decision | Status |
|---|---|---|
| [0001](0001-kubernetes-distribution-and-nested-containers.md) | Kubernetes distribution and nested containers | Accepted |
| [0002](0002-messaging-storage-and-replication.md) | Messaging storage and replication: embedded NATS JetStream | Accepted |
| [0003](0003-wire-protocol.md) | Wire protocol: Connect (protobuf) over HTTP/2 with mTLS | Accepted |
| [0004](0004-frontend-stack.md) | Frontend stack: React, Vite and TypeScript, embedded in the Go binary | Accepted |
| [0005](0005-oidc-and-saml-libraries.md) | OIDC and SAML libraries | Accepted |
| [0006](0006-claude-code-integration.md) | Claude Code integration | Accepted |
| [0007](0007-ore-integration.md) | ore integration through its app-server | Accepted |
| [0008](0008-platform-state-store.md) | Platform state: Kubernetes for bootstrap, one PostgreSQL for the rest | Accepted |
| [0009](0009-forgejo-cli-for-agents.md) | Forgejo CLI for agents: `tea` | Accepted |
| [0010](0010-advisors.md) | Advisors: a `swarm advise` tool for both CLIs | Accepted |
| [0011](0011-warden.md) | The warden: a small model that keeps agents from going rogue | Accepted |
