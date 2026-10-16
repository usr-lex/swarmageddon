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
| Pod labels | platform pods carry `app.kubernetes.io/name` = `messaging`, `forgejo`, `gateway`, `registry-cache`, `control-center`, `postgres`, `ntfy`; room NetworkPolicies select on them |
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
| `room-<name>-identity` | `ws-<ws>` | `id_ed25519`, `id_ed25519.pub`, `gpg_secret.asc`, `forge_token`, `tls.crt`, `tls.key`, `ca.crt`, `gateway_key`, `vnc_password`, `forge_known_hosts`; when configured: `advisor_key` (ADR 0010), `warden_key` (ADR 0011), `egress_auth` (`user:password` for the egress proxy, limited network mode), and `env.<NAME>` per secret variable (value of `NAME`, design §6.6) |

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
| `/etc/swarm/room.json` | from the persona ConfigMap; the struct is `persona.RoomConfig` in `internal/operator/persona` (room, address, workspace, group, email, role title and summary, cli, model, effort, permission mode, sudo, forge URL, SSH host and org, messaging and gateway URLs, workDir, repos, firstTurn, approval timeout, env) |
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
`claude-managed-settings.json`, `claude-CLAUDE.md`, `claude-managed-mcp.json`
(→ `/etc/claude-code/managed-mcp.json`, which holds the `swarm` MCP server and is
exclusive); `ore-requirements.toml`,
`ore-config.toml`, `ore-persona.rules`. The pod mounts the ConfigMap at
`/etc/swarm/persona/`, and the supervisor copies the files into place at start
(root-owned, read-only to the agent).

room.json also carries, when set: `network` (`mode` none/limited/full,
`proxy` the egress proxy URL, `noProxy` extra direct hosts), `storm`
(`coalesceBytes`, default 4096), `advisor` (`model`, `effort`,
`maxCallsPerHour`), `warden` (`mode` off/monitor/gate, `model`,
`interruptOnHigh`, `reviewEveryToolCalls`, `rules`: `[{name, tool, match,
action, reason}]`, where `tool` is a name, `a|b`, or empty for all, `match` a
regular expression on the command or path, `action` allow/deny/escalate) and
`janitor`. The Go form is `roomcfg.Room` in `internal/supervisor/roomcfg`.

The supervisor sets room.json `env` plus the secret variables (`env.<NAME>`
keys) for the CLI, writes them to `~/.swarm/env` (0600) for the agent's shells
(sourced from `/etc/profile.d/swarm-env.sh` and `/etc/bash.bashrc`), and in
`limited` mode adds `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (and lowercase)
with `egress_auth` in the proxy URL, also in
`/etc/containers/containers.conf.d/60-swarm-proxy.conf` for nested
containers. Names the supervisor sets itself (`HOME`, `PATH`, the gateway and
proxy variables) can't be overridden.

Environment of the room container: `SWARM_ROOM`, `SWARM_WORKSPACE`,
`ANTHROPIC_BASE_URL` (gateway), plus the room's `spec.env`. Keys are read from
files, never env, except `ANTHROPIC_AUTH_TOKEN` and `SWARM_GATEWAY_KEY`, which
the supervisor sets for the CLI process only, from `gateway_key`.

## Binaries and flags

| Binary | Flags |
|---|---|
| `operator` | `--metrics-bind-address`, `--health-probe-bind-address`, `--leader-elect`, `--images-from` (file with image refs), `--room-image`, `--version`; env `POD_NAMESPACE`. `operator welcome` (the chart's welcome Job): waits for Platform `default` `status.url` and Secret `setup-token`, prints the URL, the token, `status.caFingerprint` and the port-forward fallback, exits 0 |
| `control-center` | `--listen :8443`, `--health :8080`, `--tls-cert`, `--tls-key`, `--messaging-url`, `--client-cert`, `--client-key`, `--ca`, `--dev` (plain HTTP, no TLS) |
| `messaging` | `--listen :8443`, `--health :8080`, `--tls-cert`, `--tls-key`, `--ca` (defaults under `/etc/messaging/tls/`), `--data /data`, `--nats-routes` (comma-separated `host:6222` or `nats://` URLs; may include itself), `--server-name` (default hostname), `--cluster-size` (0: number of routes, or 1), `--namespace`, `--dev-insecure` (h2c; identity from `x-swarm-dev-identity`), `--directory-file` (Workspace/Room/RoleTemplate YAML instead of Kubernetes), `--version`. Needs get/list/watch on `workspaces`, `rooms`, `roletemplates` |
| `supervisor` | runs as PID 1 under tini; reads `/etc/swarm/room.json` |
| `swarm` | `swarm mcp`, `swarm hook <event>`, `swarm send <to> <msg>`, `swarm agents`, `swarm whoami`, `swarm status <text>`, `swarm post`, `swarm read`, `swarm get`, `swarm mark-read`, `swarm unread`, `swarm channels`, `swarm delivery`, `swarm advise` (see `swarm help`) |
| `mockmodel` | `--listen :9999`, `--script <file>`; serves Anthropic `/v1/messages`, OpenAI `/v1/responses` and `/v1/chat/completions` |

## Images

`ghcr.io/usr-lex/swarmageddon/<name>:<version>`: `operator`, `control-center`,
`messaging`, `room`, `runner`, `mockmodel`. Third-party images are pinned by
the operator in `internal/operator/images/images.go`.

Each image's ENTRYPOINT is its binary (`images/service/Dockerfile` is the
shared default; a `cmd/<name>/Dockerfile` overrides it), so manifests pass only
`args`. The chart runs the operator, control center and welcome Job as uid
65532 with a read-only root filesystem and a writable `/tmp`. The room
supervisor runs `/usr/local/libexec/podman-ulimits` before starting Podman.

## Messaging storage

One JetStream stream per workspace and kind, so retention is that stream's
`MaxAge`: `INBOX_<ws>` (`inbox.<ws>.<name>`), `AUDIT_<ws>`
(`audit.<ws>.{message,crossing,refusal,presence,verdict}`) and `APPROVALS_<ws>`
(`approval.<ws>.<room>`); KV buckets `presence`, `status` and `people`. Each
recipient has a durable consumer named after it. `Nats-Msg-Id` is
`<id>/<recipient>`. Names match `^[a-z0-9][a-z0-9_-]{0,62}$`; `forge` and
`status` are reserved system addresses.
