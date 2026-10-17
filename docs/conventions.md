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
| Model gateway | Service `gateway.swarmageddon-platform.svc:4000`: the warden (Deployment `warden`, 2 replicas, ADR 0011), a pass-through proxy in front of LiteLLM; health on 8080 |
| LiteLLM | Service `litellm.swarmageddon-platform.svc:4000` (Deployment `litellm`); reachable only from the warden and the system namespace (admin API) |
| Egress proxy | Service `egress.swarmageddon-platform.svc:3128` (Deployment `egress`, 2 replicas; design §11.2); HTTP proxy with CONNECT, `Proxy-Authorization: Basic <room>:<egress_auth>`; health on 8080 |
| Warden and egress tables | ConfigMaps `warden-keys` and `egress-allowlist` in the platform namespace, one data key `<workspace>.<room>` per room, written by the room controller (JSON: `keytable.Entry` in `internal/warden/keytable`, `egress.Entry` in `internal/egress`; keys and credentials as SHA-256 hex only) |
| Registry cache | Service `registry-cache.swarmageddon-platform.svc:5000` (HTTP) |
| ntfy | Service `ntfy.swarmageddon-platform.svc:80`; `auth-default-access: deny-all`: anonymous clients can neither publish nor subscribe. The operator provisions user `swarm-admin` (role admin) with a token (Secrets `ntfy-auth`, `ntfy-publisher`). Publish with `Authorization: Bearer <token>`; phones subscribe with the same user (or a user an admin adds with `ntfy user add` in the pod) |
| Forgejo web UI | with `Platform.spec.hostname`: Ingress `forgejo` at `forge.<hostname>` behind the default IngressClass, TLS from the platform CA (Secret `forgejo-tls`), plus NetworkPolicy `forgejo-ingress` opening port 3000 to the ingress controller |
| Device plugin | DaemonSet `devices` in namespace `swarmageddon-devices` (Pod Security `privileged`: it needs hostPath), only in the `userns` tier. Resources `swarmageddon.dev/fuse` and `swarmageddon.dev/tun` (250 units per node each; any number of units mounts `/dev/fuse` or `/dev/net/tun`); userns-tier room and runner containers request 1 of each |
| Messaging deny list | ConfigMap `messaging-revoked` in the platform namespace: one key per revoked client certificate, its serial in lowercase hex; value `<notAfter RFC 3339> <CN>`. The room controller adds removed rooms' and rotated certificates and drops expired entries; messaging mounts it at `/etc/swarm/revoked` (`--revoked`) and refuses those certificates at the TLS handshake until they expire |
| Messaging token digests | ConfigMap `messaging-token-hashes` in each `ws-<ws>` namespace, data key `sha256`: the lowercase hex SHA-256 of every token issued for the workspace's rooms (`forge_token`, `gateway_key`, `advisor_key`, `warden_key`, `egress_auth`), sorted, one per line; never the tokens. Written by the room controller; messaging watches it by name (`internal/messaging/tokenhash`) and refuses sends and posts containing one (design §11.4) |
| Runner pools | a pool with `spec.group: <g>` serves only repositories team `group-<g>` has (narrowed by `spec.repos`); its runners carry the extra label `group-<g>` (so workflows can ask `runs-on: group-<g>`) and are registered on the job's repository, so Forgejo can't give them another repository's job |
| Pod labels | platform pods carry `app.kubernetes.io/name` = `messaging`, `forgejo`, `gateway` (the warden's pods), `litellm`, `egress`, `registry-cache`, `control-center`, `postgres`, `ntfy`; room NetworkPolicies select on them |
| Control center | Deployment and Service `control-center` in the system namespace; HTTPS port 8443 (Service port 443); metrics/health on 8080. NetworkPolicy `control-center` admits port 8443 only from Forgejo (webhooks) and the default IngressClass's controller (pods `app.kubernetes.io/name: traefik` or `ingress-nginx` outside workspace and platform namespaces; any pod there for other controllers), or from any address when it's a NodePort. With Traefik's CRDs, TLSStore `default` in the system namespace makes `control-center-tls` the certificate for clients without SNI (`https://<node-ip>/`), unless another namespace already has one |
| Operator | Deployment `swarmageddon-operator`; health on 8081, metrics 8080 |

## Secrets (names; keys in parentheses)

| Secret | Namespace | Contents |
|---|---|---|
| `setup-token` | system | `token`; deleted once admin is claimed |
| `platform-ca` | system | `ca.crt`, `ca.key`: the internal CA (operator only) |
| `platform-ca-bundle` | every managed namespace (ConfigMap, not Secret) | `ca.crt` |
| `control-center-tls` | system | `tls.crt`, `tls.key`: serving cert for the control center |
| `control-center-public-tls` | system | in TLS mode `certManager`: cert-manager's certificate for `spec.hostname` (Certificate `control-center`), served by the control center's Ingress once ready |
| `control-center-client` | system | `tls.crt`, `tls.key`: client cert, CN `control-center` |
| `control-center-session` | system | `key`: 32 random bytes for signing session cookies |
| `local-admin` | system | `username`, `passwordHash` (argon2id PHC string), `email` |
| `control-center-db` | system | `url` (postgres URL), created when PostgreSQL is deployed |
| `vapid` | system | `publicKey`, `privateKey` |
| `control-center-gateway` | system | `masterKey`: despite the name, a view-only LiteLLM key (user `control-center`, role `proxy_admin_viewer`) the control center reads spend with; made by the operator once the gateway is up |
| the control center's other Secrets | system | fixed names and prefixes in `api/v1alpha1/controlcenter_names.go` (`gateway-provider-<p>`, `oidc-<p>`, `notify-<ws>-<kind>`, `forge-webhook-<ws>`, ConfigMaps `saml-<p>`). It may create any Secret or ConfigMap there but read and change only these: the chart's Role `control-center` names the fixed ones; the operator's Role `control-center-owned` lists, one by one, the existing prefixed ones labelled `swarmageddon.dev/component=control-center` (or `forge-webhook`). The control center creates first and retries a few seconds while a new one is still Forbidden. The operator never gives its own Secrets these names |
| `messaging-tls` | platform | serving cert for messaging (`tls.crt`, `tls.key`, `ca.crt`), SANs for the Service and every pod |
| `forgejo-admin` | platform | `username`, `password`, `token` |
| `gateway-master` | platform | `masterKey` |
| `warden-client` | platform | `tls.crt`, `tls.key`, `ca.crt`: client cert, CN `warden`, for ReportVerdict |
| `warden-review` | platform | `key`: the gateway warden's own LiteLLM key for model reviews (monthly budget 100 USD) |
| `postgres-<service>` | platform | `username`, `password`, `database`, `url` |
| `room-secrets` | `ws-<ws>` | label `swarmageddon.dev/room-secret=true`; keys people enter in the control center for `secretEnv` and MCP `valueFrom` (design §6.6). The operator creates it empty and owns only its metadata; the control center writes the data and may get, update and patch this one Secret by name, nothing else in the namespace |
| `ntfy-auth` | platform | `users` (`swarm-admin:<bcrypt>:admin`, ntfy's `NTFY_AUTH_USERS`), `tokens` (`swarm-admin:tk_…:control-center`, `NTFY_AUTH_TOKENS`), `username`, `password`, `token`; made once by the operator when ntfy is enabled |
| `ntfy-publisher` | system | `token`, `username`, `password`, `url` (`http://ntfy.swarmageddon-platform.svc`): what the control center publishes to ntfy with |
| `forgejo-tls` | platform | `tls.crt`, `tls.key`, `ca.crt` for `forge.<hostname>` |
| `sentry` | system, platform | `dsn`, only when error reporting is on (below). System: made by the chart from `sentry.dsn` (or the admin's own, named by `sentry.existingSecret`); platform: the operator's copy of its `SENTRY_DSN`, owned by the Platform and deleted when the DSN is unset |
| repository credentials | `ws-<ws>` | named by `RepositorySpec.mirrorSecretName` (push mirror) and `importSecretName` (import): `token`, optional `username` (default `oauth2`). Read only by the operator and handed to Forgejo; never mounted in a room. The control center creates each as a new Secret `repo-<repo>-<kind>-<random>` (kind `import` or `mirror`) (label `swarmageddon.dev/component=repo-credential`) and can't read it back |
| `room-<name>-identity` | `ws-<ws>` | `id_ed25519`, `id_ed25519.pub`, `gpg_secret.asc`, `forge_token`, `tls.crt`, `tls.key`, `ca.crt`, `gateway_key`, `vnc_password`, `forge_known_hosts`; when configured: `advisor_key` (ADR 0010), `warden_key` (ADR 0011), `egress_auth` (the egress proxy password; the user is the room name), and `env_<NAME>` per secretEnv variable (design §6.6) |

## Identities (internal CA)

- Room client certificate: `CN=<room>@<workspace>`, URI SAN
  `spiffe://swarmageddon/ws/<workspace>/room/<room>`, 30-day lifetime.
- Control center client certificate: `CN=control-center`, URI SAN
  `spiffe://swarmageddon/system/control-center`.
- Operator client certificate: `CN=operator`, URI SAN
  `spiffe://swarmageddon/system/operator`, issued in the operator's memory
  (24 h, renewed at 2/3). Messaging lets it only `WatchPresence` and
  `WatchApprovals`, to roll idle rooms (design §3.5).
- Messaging serving certificate: DNS SANs `messaging`,
  `messaging.swarmageddon-platform.svc`,
  `messaging.swarmageddon-platform.svc.cluster.local`, and
  `*.messaging-headless.swarmageddon-platform.svc.cluster.local`.
- The control center sends `x-swarm-on-behalf-of: <handle>@<workspace>` to act
  for a person, and `x-swarm-operator-post: 1` on a channel post by a person
  who may operate the workspace: messaging then skips channel membership (limits
  and audit still apply; the audit record's reason is `operator post`). The
  header is ignored from anyone but the control center acting for a person.

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
`claude-managed-settings.json`, `claude-CLAUDE.md`, `claude-agent-advisor.md`
(→ `/etc/claude-code/.claude/agents/advisor.md`), `claude-managed-mcp.json`
(→ `/etc/claude-code/managed-mcp.json`, which holds the `swarm` MCP server and is
exclusive); `ore-requirements.toml`,
`ore-config.toml`, `ore-persona.rules`. The pod mounts the ConfigMap at
`/etc/swarm/persona/`, and the supervisor copies the files into place at start
(root-owned, read-only to the agent).

room.json also carries, when set: `secretEnv` (variable names), `mcpServers`
(names), `network` (`mode` none/limited/full, `proxyURL` without credentials,
`noProxy` extra direct hosts, `domains`), `storm`
(`coalesceBytes`, default 4096), `advisor` (`model`, `effort`,
`maxCallsPerHour`, `native`), `warden` (`mode` off/monitor/gate, `model`,
`interruptOnHigh`, `reviewEveryToolCalls`, `rules`: `[{name, tool, match,
action, reason}]`, where `tool` is a name, `a|b`, or `*`, `match` a regular
expression on the command or path, `action` allow/deny/escalate; with the
built-ins, the strictest match wins) and
`janitor`. The Go form is `roomcfg.Room` in `internal/supervisor/roomcfg`.

The supervisor sets room.json `env` plus the secret variables (`env_<NAME>`
keys) for the CLI, writes them to `~/.swarm/env` (0600) for the agent's shells
(sourced from `/etc/profile.d/swarm-env.sh` and `/etc/bash.bashrc`), and in
`limited` mode adds `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (and lowercase)
with `<room>:<egress_auth>` in the proxy URL, also in
`/etc/containers/containers.conf.d/60-swarm-proxy.conf` for nested
containers. Names the supervisor sets itself (`HOME`, `PATH`, the gateway and
proxy variables) can't be overridden.

Environment of the room container: `SWARM_ROOM`, `SWARM_WORKSPACE`,
`ANTHROPIC_BASE_URL` (gateway), plus the merged layered `env` (design §6.6;
`internal/roomsettings` is the one merge, used by the operator and the control
center). Shared volumes are claims `shared-<name>`, mounted at `/srv/shared/<name>`. Keys are read from
files, never env, except `ANTHROPIC_AUTH_TOKEN` and `SWARM_GATEWAY_KEY`, which
the supervisor sets for the CLI process only, from `gateway_key`.

## Binaries and flags

| Binary | Flags |
|---|---|
| `operator` | `--metrics-bind-address`, `--health-probe-bind-address`, `--leader-elect`, `--images-from` (file with image refs), `--room-image`, `--version`; env `POD_NAMESPACE`. `operator welcome` (the chart's welcome Job): waits for Platform `default` `status.url` and Secret `setup-token`, prints the URL, the token, `status.caFingerprint` and the port-forward fallback, exits 0 |
| `control-center` | `--listen :8443`, `--health :8080`, `--tls-cert`, `--tls-key`, `--messaging-url`, `--client-cert`, `--client-key`, `--ca`, `--dev` (plain HTTP, no TLS) |
| `messaging` | `--listen :8443`, `--health :8080`, `--tls-cert`, `--tls-key`, `--ca` (defaults under `/etc/messaging/tls/`), `--revoked /etc/messaging/revoked` (the deny list directory; missing means empty), `--data /data`, `--nats-routes` (comma-separated `host:6222` or `nats://` URLs; may include itself), `--server-name` (default hostname), `--cluster-size` (0: number of routes, or 1), `--namespace`, `--dev-insecure` (h2c; identity from `x-swarm-dev-identity`), `--directory-file` (Workspace/Room/RoleTemplate YAML instead of Kubernetes), `--version`. Needs get/list/watch on `workspaces`, `rooms`, `roletemplates`, and on ConfigMaps named `messaging-token-hashes` |
| `warden` | `--listen :4000`, `--health :8080`, `--upstream` (LiteLLM URL), `--namespace` (of `warden-keys`), `--messaging-url`, `--client-cert`, `--client-key`, `--ca` (defaults under `/etc/warden/tls/`), `--review-key-file /etc/warden/review/key`, `--review-every 1m`, `--max-body 8388608`. Needs get/list/watch on ConfigMap `warden-keys`, get/patch on `rooms` (quarantine: `spec.quarantined` and annotations `swarmageddon.dev/quarantine-reason`, `-quarantined-by`, `-quarantined-at`) |
| `egress` | `--listen :3128`, `--health :8080`, `--namespace` (of `egress-allowlist`), `--log-connections`. Needs get/list/watch on ConfigMap `egress-allowlist` |
| `supervisor` | runs as PID 1 under tini; reads `/etc/swarm/room.json` |
| `swarm` | `swarm mcp`, `swarm hook <event>`, `swarm send <to> <msg>`, `swarm agents`, `swarm whoami`, `swarm status <text>`, `swarm post`, `swarm read`, `swarm get`, `swarm mark-read`, `swarm unread`, `swarm channels`, `swarm delivery`, `swarm advise` (see `swarm help`) |
| `devices` | `--dir /var/lib/kubelet/device-plugins/`, `--host-root` (where the host's `/dev` is mounted as `<root>/dev`), `--units 250`. Runs as root (writes its sockets in kubelet's directory), no capabilities |
| `mockmodel` | `--listen :9999`, `--script <file>`; serves Anthropic `/v1/messages`, OpenAI `/v1/responses` and `/v1/chat/completions` |

## Error reporting (Sentry)

Off unless a DSN is configured; `internal/telemetry` is the one integration.

| What | Value |
|---|---|
| Chart values | `sentry.dsn` (the chart makes Secret `sentry`, key `dsn`), or `sentry.existingSecret` (a Secret in the system namespace with key `dsn`); `sentry.environment`; `sentry.tracesSampleRate` (default 0.1; 0 turns tracing off) |
| Environment | `SENTRY_DSN` (`secretKeyRef` to Secret `sentry` or the existing one, key `dsn`, `optional: true`), `SENTRY_ENVIRONMENT`, `SENTRY_TRACES_SAMPLE_RATE`. The chart sets them on the operator and the control center; the operator sets them, with its own values, on messaging, the warden and egress. `devices` and `mockmodel` read them too, but nothing sets them yet. Unset, the manifests are as before |
| Pod annotation | `swarmageddon.dev/sentry`: a short hash of the DSN, so a new DSN rolls the pods (chart-made Secret and operator-deployed services) |
| What is sent | errors and panics; slog (the operator: zap) records at Error and above as events, Info and Warn as breadcrumbs, Info and above as Sentry Logs; HTTP and Connect server transactions and outgoing `http.DefaultTransport` spans. Release `swarmageddon@<version>`, server name the pod name (`POD_NAME` or the hostname), tag and log attribute `component` (the binary's name) |
| Never sent | request bodies, cookies, users, and the headers `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `x-swarm-*`. Everything else passes through the redact registry first (design §11.4) |
| Sampling | a fixed rate per service: incoming `sentry-trace` headers don't decide it. Outgoing trace headers go only to `messaging`, `gateway` and `control-center` |
| Web UI | `GET /client-config.json` on the control center (no sign-in): `{"sentry": {"dsn", "environment", "release", "tracesSampleRate"}}`, or `{}` when off. The UI then loads the SDK; no session replay. The control center sends no Content-Security-Policy, so there is no `connect-src` to extend |
| Network | the reporting pods already reach port 443 outside the cluster: the system and devices namespaces have no egress policies, and in the platform namespace only egress has one, which allows 443 (`netpoltest` `TestSentryReachable`) |
| Rooms | never: the supervisor and `swarm` don't use `internal/telemetry`. A room holds only its own identity (design §11.4), and usually has no internet |

## Images

`ghcr.io/usr-lex/swarmageddon/<name>:<version>`: `operator`, `control-center`,
`messaging`, `warden`, `egress`, `room`, `runner`, `mockmodel`, `devices`. Third-party images are pinned by
the operator in `internal/operator/images/images.go`.

Each image's ENTRYPOINT is its binary (`images/service/Dockerfile` is the
shared default; a `cmd/<name>/Dockerfile` overrides it), so manifests pass only
`args`. The chart runs the operator, control center and welcome Job as uid
65532 with a read-only root filesystem and a writable `/tmp`. The room
supervisor runs `/usr/local/libexec/podman-ulimits` before starting Podman.

## Messaging storage

One JetStream stream per workspace and kind, so retention is that stream's
`MaxAge`: `INBOX_<ws>` (`inbox.<ws>.<name>`), `CHANNELS_<ws>`
(`chan.<ws>.<channel>`, retained like messages), `AUDIT_<ws>`
(`audit.<ws>.{message,post,crossing,refusal,presence,verdict}`) and
`APPROVALS_<ws>` (`approval.<ws>.<room>`). Each recipient has a durable
consumer named after it. `Nats-Msg-Id` is `<id>/<recipient>` for direct
messages, `<id>` for posts, `post/<id>/<reader>` for a pushed post or
mention, `digest/<channel>/<reader>/<last seq>` for digests and
`overflow/<notice id>` for push-budget notices. A pushed post in an inbox
carries its `CHANNELS` sequence in the `Swarm-Chan-Seq` header.

KV buckets:

| Bucket | Key | Value |
|---|---|---|
| `presence`, `status`, `people` | `<ws>.<name>` | as before |
| `cursors` | `<ws>.<member>.<scope>` (scope `direct` or a channel) | JSON `{"upto": seq, "extra": [seq...]}`: read up to `upto` plus `extra`, in the inbox (direct) or `CHANNELS` (channel) sequence space |
| `delivery` | `<ws>.<member>.<channel>` | `push`, `digest` or `pull`; unset means the channel default |
| `digests` | `<ws>.<member>.<channel>` | JSON `{"seq", "sent"}`: last post covered, when the last digest went |
| `dupes` | `<ws>.<sender>.<hash of target and body>` | first message id; bucket TTL 10 min (duplicate-body suppression) |

`cursors`, `delivery`, `digests` and `dupes` are read through the stream
leader (`$JS.API.STREAM.MSG.GET`), not direct gets, so a replica reads its
own writes. Digests run only on the `INBOX_<ws>` stream leader. Names match
`^[a-z0-9][a-z0-9_-]{0,62}$`; `forge` and `status` are reserved system
addresses; `all`, `direct` and `mentions` can't be channel names.
