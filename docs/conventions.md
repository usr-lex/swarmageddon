# Conventions between components

These are the names, paths and ports that more than one component depends on.
Change them here first.

## Namespaces and services

| What | Value |
|---|---|
| System namespace | `swarmageddon-system` (operator, control center) |
| Platform namespace | `swarmageddon-platform` |
| Workspace namespace | `ws-<workspace>` |
| Messaging | Service `messaging.swarmageddon-platform.svc`, port 8443 (Connect over HTTPS, mTLS); headless `messaging-headless` for NATS routes (port 6222, cluster-internal only); StatefulSet `messaging` |
| Messaging admin/health | port 8080 (`/healthz`, `/readyz`), cluster-internal |
| Forgejo | Service `forgejo.swarmageddon-platform.svc`, HTTP port 3000; SSH Service `forgejo-ssh`, port 22 |
| PostgreSQL | Service `postgres.swarmageddon-platform.svc:5432` |
| LiteLLM | Service `gateway.swarmageddon-platform.svc:4000` |
| Registry cache | Service `registry-cache.swarmageddon-platform.svc:5000` (HTTP) |
| ntfy | Service `ntfy.swarmageddon-platform.svc:80` |
| Control center | Deployment and Service `control-center` in the system namespace; HTTPS port 8443 (Service port 443); metrics/health on 8080 |
| Operator | Deployment `swarmageddon-operator`; health on 8081, metrics 8080 |

## Secrets (names; keys in parentheses)

| Secret | Namespace | Contents |
|---|---|---|
| `setup-token` | system | `token`; deleted once admin is claimed |
| `platform-ca` | system | `ca.crt`, `ca.key`: the internal CA (operator only) |
| `platform-ca-bundle` | every managed namespace (ConfigMap, not Secret) | `ca.crt` |
| `control-center-tls` | system | `tls.crt`, `tls.key`: serving cert for the control center |
| `control-center-client` | system | `tls.crt`, `tls.key`: client cert, CN `control-center` |
| `control-center-session` | system | `key`: 32 random bytes for signing session cookies |
| `local-admin` | system | `username`, `passwordHash` (argon2id PHC string), `email` |
| `control-center-db` | system | `url` (postgres URL), created when PostgreSQL is deployed |
| `vapid` | system | `publicKey`, `privateKey` |
| `messaging-tls` | platform | serving cert for messaging (`tls.crt`, `tls.key`, `ca.crt`), SANs for the Service and every pod |
| `forgejo-admin` | platform | `username`, `password`, `token` |
| `gateway-master` | platform | `masterKey` |
| `postgres-<service>` | platform | `username`, `password`, `database`, `url` |
| `room-<name>-identity` | `ws-<ws>` | `id_ed25519`, `id_ed25519.pub`, `gpg_secret.asc`, `forge_token`, `tls.crt`, `tls.key`, `ca.crt`, `gateway_key`, `vnc_password`, `forge_known_hosts` |

## Identities (internal CA)

- Room client certificate: `CN=<room>@<workspace>`, URI SAN
  `spiffe://swarmageddon/ws/<workspace>/room/<room>`, 30-day lifetime.
- Control center client certificate: `CN=control-center`, URI SAN
  `spiffe://swarmageddon/system/control-center`.
- Messaging serving certificate: DNS SANs `messaging`,
  `messaging.swarmageddon-platform.svc`,
  `messaging.swarmageddon-platform.svc.cluster.local`, and
  `*.messaging-headless.swarmageddon-platform.svc.cluster.local`.
- The control center sends `x-swarm-on-behalf-of: <handle>@<workspace>` to act
  for a person.

## Inside a room

| Path | What |
|---|---|
| `/run/swarm/identity/` | the room's Secret (mode 0440, group = agent) |
| `/etc/swarm/ca/ca.crt` | platform CA bundle |
| `/etc/swarm/room.json` | from the persona ConfigMap: room, workspace, group, role title and summary, cli, model, effort, forge URL, messaging URL, gateway URL, workDir, repos, firstTurn |
| `/etc/claude-code/managed-settings.json`, `/etc/claude-code/CLAUDE.md` | persona (Claude rooms) |
| `/etc/codex/requirements.toml`, `/etc/codex/config.toml`, `/etc/codex/rules/persona.rules` | persona (ore rooms) |
| `/etc/swarm/role.md` | the role text (both CLIs) |
| `/run/swarm/supervisor.sock` | room-local API (proto `swarm.room.v1`), Connect over HTTP on a Unix socket |
| `/run/swarm/ore.sock` | ore app-server (ore rooms) |
| `/var/run/docker.sock` | Podman's Docker-compatible API |
| `/home/<room>` | home volume; the agent user is the room name, uid 1000 |
| `/var/lib/containers` | engine volume |

Persona ConfigMap `room-<name>-persona` keys and where the supervisor puts them:
`room.json` → `/etc/swarm/room.json`; `role.md` → `/etc/swarm/role.md`;
`claude-managed-settings.json`, `claude-CLAUDE.md`; `ore-requirements.toml`,
`ore-config.toml`, `ore-persona.rules`. The pod mounts the ConfigMap at
`/etc/swarm/persona/`, and the supervisor copies the files into place at start
(root-owned, read-only to the agent).

Environment of the room container: `SWARM_ROOM`, `SWARM_WORKSPACE`,
`ANTHROPIC_BASE_URL` (gateway), plus the room's `spec.env`. Keys are read from
files, never env, except `ANTHROPIC_AUTH_TOKEN` and `SWARM_GATEWAY_KEY`, which
the supervisor sets for the CLI process only, from `gateway_key`.

## Binaries and flags

| Binary | Flags |
|---|---|
| `operator` | `--metrics-bind-address`, `--health-probe-bind-address`, `--leader-elect`, `--images-from` (file with image refs), `--room-image`, `--version` |
| `control-center` | `--listen :8443`, `--health :8080`, `--tls-cert`, `--tls-key`, `--messaging-url`, `--client-cert`, `--client-key`, `--ca`, `--dev` (plain HTTP, no TLS) |
| `messaging` | `--listen :8443`, `--health :8080`, `--tls-cert`, `--tls-key`, `--ca`, `--data /data`, `--nats-routes`, `--server-name`, `--cluster-size`, `--namespace` |
| `supervisor` | runs as PID 1 under tini; reads `/etc/swarm/room.json` |
| `swarm` | `swarm mcp`, `swarm hook <event>`, `swarm send <to> <msg>`, `swarm agents`, `swarm whoami`, `swarm status <text>` |
| `mockmodel` | `--listen :9999`, `--script <file>`; serves Anthropic `/v1/messages`, OpenAI `/v1/responses` and `/v1/chat/completions` |

## Images

`ghcr.io/usr-lex/swarmageddon/<name>:<version>`: `operator`, `control-center`,
`messaging`, `room`, `runner`, `mockmodel`. Third-party images are pinned by
the operator in `internal/operator/images/images.go`.
