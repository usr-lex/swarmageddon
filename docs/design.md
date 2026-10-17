# swarmageddon: design

- Status: **Approved** by the owner on 2026-10-16, with the recommendations in
  §17 accepted. Implementation notes since approval:
  - The room image is based on Ubuntu 26.04 instead of Fedora (§6.1). Ubuntu's
    archive is reachable from our build environments and Fedora's mirrors
    were not, and Podman, crun and netavark are packaged there too.
- Date: 2026-10-16
- Decisions with real alternatives are recorded as ADRs in [`adr/`](adr/).

swarmageddon is a self-hosted workplace for teams of AI coding agents on
Kubernetes:

- Each agent (a **worker**) lives in its own **room**, a persistent Linux
  environment.
- Agents coordinate through a local Forgejo and talk to each other through our
  messaging service.
- People set everything up in a web **control center** and steer the team
  from there.

We host existing agent CLIs (Claude Code and ore). We do not write an agent
loop.

## Contents

1. [What we learned from remote-labs](#1-what-we-learned-from-remote-labs)
2. [Architecture at a glance](#2-architecture-at-a-glance)
3. [Installation and first run](#3-installation-and-first-run)
4. [Custom resources](#4-custom-resources)
5. [The operator](#5-the-operator)
6. [Rooms: image, pod and supervisor](#6-rooms-image-pod-and-supervisor)
7. [The messaging service](#7-the-messaging-service)
8. [The control center](#8-the-control-center)
9. [Forgejo integration](#9-forgejo-integration)
10. [Model gateway, spend and notifications](#10-model-gateway-spend-and-notifications)
11. [Security model](#11-security-model)
12. [Disk hygiene](#12-disk-hygiene)
13. [Repository layout and tooling](#13-repository-layout-and-tooling)
14. [Testing](#14-testing)
15. [Milestones](#15-milestones)
16. [Where the brief needs a change](#16-where-the-brief-needs-a-change)
17. [Open questions](#17-open-questions)
18. [What is verified and what is assumed](#18-what-is-verified-and-what-is-assumed)

---

## 1. What we learned from remote-labs

remote-labs runs about a dozen agents on one VM, with podman-compose and
GitHub. We take its behavior, not its architecture.

| remote-labs | What went wrong or was fragile | Here |
|---|---|---|
| One generated `docker-compose.yml`; any change recreated every room | Changing one room took the whole team down | One StatefulSet per room. The operator touches only the room whose spec changed (§5) |
| `entrypoint.sh` turns a generic image into a room: user, keys, git signing, `gh` login, instructions | Worked well | Same idea, done by the room supervisor from mounted Secrets and ConfigMaps (§6) |
| `agent-autostart` pre-trusts the folder, marks onboarding done, resumes the last session, gives ore a first turn | A dialog nobody answers silently blocks an agent; ore's `resume --last` found nothing | Same pre-answers, plus a test that fails if any dialog shows on start or resume (ADR 0006, 0007) |
| Rooms share `~/.claude/sessions` and the socket folder so `SendMessage` crosses rooms; PIDs offset per room | Relies on undocumented files; groups need separate volumes; not a security boundary | No shared folders. Each room's supervisor talks to a central messaging service over mTLS, and the server enforces policy (§7) |
| agent-bus: ore hooks, inbox files, a 20 s idle timer, then `ore queue` | Timing heuristics were the most fragile part; a session without a first turn couldn't be woken | Claude: the documented inbox socket. ore: app-server `turn/steer` or `turn/start`, driven by its events. No timers (ADR 0006, 0007) |
| `wall-relay`: two containers, a spool folder, a keeper, `to <name>:` prefixes, `crossings.jsonl` | Worked, but was a special case built from mounts | A **mediated link**, one policy kind in the messaging server (§7.4) |
| `github-org.sh`: teams, access per repo class, rulesets, signed commits | GitHub Free has no rulesets on private repos; agents had to accept invitations in a browser | The operator provisions Forgejo through its admin API, with no manual steps (§9) |
| `github-mute.sh` | Notification emails flooded the owner's inbox | Forgejo email notifications are off for agent accounts by default (§9) |
| `docker-janitor`, `hub-cache`, `ci-job-hook.sh` | A 126 GB disk filled quietly | Volume quotas, the janitor, a registry cache, ephemeral CI, and disk shown in the dashboard with early warnings (§12) |
| `IPQoS none` in the room's ssh config | SSH to the forge hung behind some NATs | Kept in the room image |
| Privileged rooms | "Not a security boundary" | User-namespaced pods by default (ADR 0001) |
| CI runners keep work folders and Docker caches between jobs, shared by clean and dirty repos | Jobs from different sides shared a disk | Ephemeral one-job runner pods, scoped per workspace (§9.4) |

Nothing project-specific is carried over: no role texts, no clean-room rulings,
no org or repository names.

## 2. Architecture at a glance

```
                         browser (people, phones)
                                  │ HTTPS (ingress)
                                  ▼
 ┌────────────────────── namespace swarmageddon-system ──────────────────────┐
 │  control-center (Go + embedded React UI)      operator (Go, controller-   │
 │   - sign-in (local, OIDC, SAML)                runtime, leader-elected)   │
 │   - wizards, fleet, approvals, notifications    - Platform, Workspace,    │
 │   - terminals via k8s exec (WebSocket)            Room, RoleTemplate,     │
 │                                                    RunnerPool             │
 └───────────────┬───────────────────────────────────────────┬──────────────┘
                 │ Connect/mTLS                              │ server-side apply
                 ▼                                           ▼
 ┌────────────────────── namespace swarmageddon-platform ────────────────────┐
 │  messaging ×3 (Go + embedded NATS JetStream)   postgres (1)               │
 │  forgejo (1)    litellm (1-2)    ntfy (opt.)   registry-cache (1)         │
 └───────────────▲────────────────▲──────────────▲───────────────────────────┘
                 │ mTLS           │ HTTPS+SSH    │ HTTPS (virtual key)
 ┌───────────────┴────────────────┴──────────────┴─── namespace ws-<name> ───┐
 │  room-athena (StatefulSet, 1 pod)    room-hermes    room-metis   ...      │
 │   ├ room supervisor (PID 1)          runner pods (ephemeral, per job)     │
 │   ├ Claude Code or ore (tmux)                                             │
 │   ├ Podman (Docker API), Xvfb + VNC                                       │
 │   └ volumes: home, engine                                                 │
 └───────────────────────────────────────────────────────────────────────────┘
```

**Binaries.** All of them are Go, built from one module.

| Binary | Runs | Talks to |
|---|---|---|
| `operator` | system namespace, 1 active replica | Kubernetes API; Forgejo and LiteLLM admin APIs; messaging admin API |
| `control-center` | system namespace, 1 replica, 2 once PostgreSQL is up | Kubernetes API (scoped, §11.3), PostgreSQL, messaging |
| `messaging` | platform namespace, 3 replicas (1 on a single node) | NATS between its own replicas; Kubernetes API (read Workspaces and Rooms) |
| `supervisor` | PID 1 of every room | messaging (mTLS), the local CLI, the local engine |
| `swarm` | in rooms: MCP server, hook helper, CLI for people in a shell | the local supervisor over a Unix socket |
| `warden` | platform namespace, 2 replicas, in front of LiteLLM (ADR 0011) | LiteLLM, messaging, Kubernetes API (patch rooms to quarantine) |
| `egress` | platform namespace, 2 replicas (§11.2) | the internet, for rooms in `limited` mode |
| `devices` | DaemonSet in `swarmageddon-devices`: a device plugin giving `userns` rooms `/dev/fuse` and `/dev/net/tun` (ADR 0001) | the kubelet |
| `mockmodel` | dev and tests only | none |

Preflight probes run as short-lived Jobs with stock images.

**Namespaces.**

| Namespace | Holds |
|---|---|
| `swarmageddon-system` | operator and control center |
| `swarmageddon-platform` | shared services |
| `ws-<workspace>` | one per workspace: its rooms and CI runners |

A workspace is a team; namespaces give each one its own NetworkPolicies,
ResourceQuota and RBAC.

**Platform services, all deployed by the operator from built-in manifests:**

| Service | What for | State |
|---|---|---|
| PostgreSQL | control center, Forgejo, LiteLLM (ADR 0008) | volume |
| Forgejo (≥ 16) | git, issues, PRs, reviews, Actions, packages, container registry | volume + PostgreSQL |
| messaging | agent and human messages, presence, approvals, audit (ADR 0002) | volume per replica |
| LiteLLM | one gateway for model access; a virtual key and budget per room | PostgreSQL |
| registry cache | pull-through cache for Docker Hub (CNCF `distribution`) | volume |
| ntfy | phone push, if chosen in the wizard | volume |
| forgejo-runner pods | CI, per workspace, one job each | `emptyDir` |

## 3. Installation and first run

### 3.1 The install

One command, two forms, the same objects:

```sh
helm install swarmageddon oci://ghcr.io/<org>/charts/swarmageddon -n swarmageddon-system --create-namespace
# or
kubectl apply -f https://github.com/<org>/swarmageddon/releases/latest/download/install.yaml
```

They create only:
- the `swarmageddon-system` namespace
- the CRDs
- the operator's and control center's ServiceAccounts and RBAC
- the operator Deployment
- the control center Deployment and its Service

Defaults work on a plain k3s cluster with no flags. `install.yaml` is
rendered from the chart in CI (`helm template`), so the two can't drift.

**Printing how to get in.** Helm prints `NOTES.txt` after
`helm install --wait`. `kubectl apply` can't print anything of ours, so the
manifest also contains a small `welcome` Job. It waits until the control
center is reachable, then prints the URL and the setup token. The documented
manifest one-liner is:

```sh
kubectl apply -f <url> && kubectl -n swarmageddon-system logs -f job/welcome
```

See §16 for why this differs slightly from the brief's wording.

**Machines without Kubernetes.** `curl -sfL https://<site>/install.sh | sh`
installs k3s with its defaults, waits for the node, then runs the `kubectl
apply` line above. It's a convenience, not a second install path.

### 3.2 Reaching the control center

The operator exposes the control center itself, since a static manifest can't
know the cluster:

1. If an IngressClass is marked default (k3s ships Traefik), it creates an
   Ingress with no host rule, so `https://<node-ip>/` works.
2. Otherwise it switches the Service to NodePort and reports the port.
3. Either way, the `welcome` Job and NOTES also print a
   `kubectl port-forward svc/control-center 8443:443` fallback.

TLS from the first request: the operator creates a self-signed CA and a
serving certificate (CA in a Secret in the system namespace). With Traefik,
a TLSStore `default` serves that certificate to clients that connect by IP
and send no SNI (unless another installation already set one). The welcome
output prints the CA's fingerprint, so the person can tell they reached the
right server before typing the token.

Later, the wizard's "hostname and TLS" step sets a real hostname. It uses
cert-manager if its CRDs are present (ACME or an existing issuer); otherwise
it keeps our CA and shows how to trust it on macOS, Windows, Linux, iOS and
Android.

### 3.3 Claiming admin

On first start, the operator creates `Secret/setup-token` with 32 random
bytes (base32), logs it once, and puts its location in `Platform` status. The
control center serves only the setup page until admin is claimed:

- The page asks for the token.
- It compares the token in constant time.
- It rate-limits attempts per client IP: 5 a minute, then a lockout that
  doubles each time.
- It audits every attempt to the control center log.

A correct token lets the person create the local admin (or go straight to
OIDC/SAML setup, §3.4). The operator then deletes the token Secret and
records `status.adminClaimed`. To recover a lost admin account, an admin
of the cluster runs `kubectl annotate platform default
swarmageddon.dev/reset-setup-token=now`, which issues a new token. Anyone who
can do that already owns the cluster.

### 3.4 The first-run wizard

The wizard is a page of the control center that edits the `Platform`
resource. The operator does the work and reports progress in `Platform`
status conditions, one per step and one per service. The wizard only reads
and writes the resource, so it can be closed and resumed at any step, and
re-running a step re-applies the same spec.

**1. Preflight.** The operator runs checks as short Jobs and reads the
cluster. Each check reports pass, warn or fail, with the fix:

| Check | How | Fails or warns with |
|---|---|---|
| Kubernetes version | discovery API | < 1.33 fails; 1.33 to 1.35 warns that user namespaces are beta |
| Architecture | nodes' `kubernetes.io/arch` | amd64 and arm64 only; mixed is fine |
| Default StorageClass | API | none: "install local-path" with the command; for k3s, how to re-enable it |
| Volume sharing | a Job pair mounting one RWX claim | warns that the shared image store and shared folders are off |
| Volume size enforcement | known provisioners (TopoLVM, OpenEBS LVM/ZFS, Longhorn, cloud CSI), or XFS project quota on local-path | warns that quotas are soft (§12) |
| idmap support for volumes | a `hostUsers: false` pod writing to a fresh claim | decides whether the `userns` tier is possible |
| NetworkPolicy enforcement | two probe pods, a deny policy, a connection that must fail | fails: "rooms would not be isolated" (k3s enforces by default) |
| Nested-container tier | try a `userns` pod running Podman; look for a `sysbox-runc` RuntimeClass; test whether privileged pods are admitted | picks the tier (ADR 0001); privileged needs explicit acceptance |
| Free CPU, memory, disk | node allocatable minus requests; node ephemeral storage; StorageClass capacity where reported | estimates "about N rooms at the default size" |
| Ingress and DNS | default IngressClass; resolve the chosen hostname; cluster DNS lookup from a probe pod | warns, with what to set up |
| Cluster CIDRs | node `podCIDR`s, the API server's endpoints, the `kubernetes` Service IP | needed for room egress policy (§11.2); asks if it can't detect them |

**2. Sign-in.**
- **Local admin:** done while claiming.
- **OIDC:** issuer URL, client ID and secret. Test it by signing in.
- **SAML:** IdP metadata URL or XML; shows our SP metadata to paste into the
  IdP. Test it by signing in.
- **Role mapping:** which claim or attribute carries groups, which groups
  map to admin, operator and viewer, and whether unmapped users get viewer
  or are refused.
- **Forgejo sign-in:** optionally, the same OIDC provider is configured
  for Forgejo, so people use one login (§9.6).

**3. Platform services.** Choose the optional ones (ntfy) and sizes. The
wizard then shows each service's progress, in order:

1. PostgreSQL
2. Forgejo, with its admin account generated and stored in a Secret
3. messaging
4. LiteLLM, with the provider API keys entered here: Anthropic, OpenAI, or
   any OpenAI-compatible endpoint
5. the registry cache
6. ntfy, if chosen

Runners are created per workspace.

**4. Hostname and TLS** (optional, can come later).

**5. The first workspace:** hands over to the workspace wizard (§8.3).

### 3.5 Upgrades

Upgrading is upgrading the operator: `helm upgrade`, or re-applying
`install.yaml`. Each operator version carries its own platform service
manifests, the room image tag and the CLI versions.

- **Platform services:** upgraded one at a time with the usual readiness
  gates. PostgreSQL major versions are not upgraded automatically; the
  wizard explains how.
- **Rooms:** never restarted by the upgrade itself. Each Room's status gets
  `updateAvailable`. The operator rolls rooms **one at a time**, and only
  when the room is idle (presence `idle`, no turn active, no pending
  approvals) or when someone clicks "update now". The workspace's
  `updatePolicy` can be `whenIdle` (default), `manual`, or a maintenance
  window.
- **Control center:** a normal Deployment rollout.

## 4. Custom resources

API group `swarmageddon.dev`, version `v1alpha1` (see §17 for the name).
Specs never hold secret values; they reference Secrets by name.

**`Platform`** (cluster-scoped, singleton `default`)

```yaml
spec:
  hostname: agents.example.com      # optional
  tls: { mode: selfSigned | certManager, issuerRef: {...} }
  isolationTier: auto | userns | sysbox | privileged
  privilegedAccepted: false         # must be true to use the privileged tier
  auth:
    local: { enabled: true }
    oidc: [{ name, issuer, clientID, clientSecretRef, groupsClaim, scopes }]
    saml: [{ name, metadataURL | metadataConfigMapRef, groupsAttribute }]
    roleMappings: [{ group: platform-admins, role: admin }, ...]
    defaultRole: viewer | none
  services:
    postgres:      { storage: 20Gi }
    forgejo:       { storage: 50Gi, oidcLogin: true }
    messaging:     { replicas: 3, storage: 10Gi }
    gateway:       { providers: [{ name: anthropic, kind: anthropic, apiKeySecretRef }] }
    registryCache: { storage: 100Gi, upstreams: [docker.io] }
    ntfy:          { enabled: false }
  egress: { extraDeniedCIDRs: [], clusterCIDRs: [] }   # detected values in status
status:
  adminClaimed: true
  preflight: { checks: [...], tier: userns, roomEstimate: 14 }
  conditions: [PostgresReady, ForgejoReady, MessagingReady, ...]
  versions: { operator, roomImage, claudeCode, ore, forgejo, ... }
```

**`Workspace`** (cluster-scoped; owns the namespace `ws-<name>`)

```yaml
spec:
  displayName: Acme
  forge: { org: acme, requiredApprovals: 1, signedCommits: true,
           teams: [{ name: mergers, members: [hermes] }] }
  groups: [{ name: clean }, { name: dirty }]
  messaging:
    defaultLinks: withinGroup         # rooms in a group reach each other
    links:
      - { from: clean, to: dirty, mode: mediated,
          senders: [hermes, proteus], recipients: [metis] }
      - { from: dirty, to: clean, mode: mediated,
          senders: [hecate], recipients: [hermes, proteus], requirePrefix: "to {recipient}:" }
    limits: { maxBytes: 65536, ratePerMinute: 30 }
    retention: { messages: 720h, audit: 8760h, transcripts: 2160h }   # 0 = keep
  models: { default: claude-opus-5-5, budgets: { perRoomMonthlyUSD: 200 } }
  notifications: { channels: [{ kind: webPush }, { kind: ntfy, topic: ... }],
                   events: [approvalWaiting, idleWithOpenWork, ciRed, budget, diskLow, roomCrashed] }
  isolationTier: inherit
  updatePolicy: whenIdle | manual | { window: "Sat 02:00-06:00" }
  owners: [alice@example.com]          # operators of this workspace
```

**`RoleTemplate`** (namespaced in `ws-<name>`; the wizard copies built-in
templates in)

```yaml
spec:
  title: Reviewer
  summary: Reviews every pull request against the task card.
  roleText: |                          # written to the agent
    You review ...
  claudeCode:
    permissions: { allow: [...], deny: [...] }
    hooks: {...}                       # added after ours; ours can't be removed
    permissionMode: default | acceptEdits | auto | bypassPermissions
  ore:
    approvalPolicy: on-request | never
    approvalsReviewer: user | auto_review
    rules: |                           # execpolicy prefix_rule(...) lines
      prefix_rule(pattern=["git", "push", "--force"], decision="forbidden")
    mcpTools: { swarm: { enabled: ["*"] } }
  autoApprove: [{ tool: Bash, match: "^(npm|pnpm) (test|run lint)" }]   # control-center rules (§8.5)
  forgeTeams: [reviewers]
```

**`Room`** (namespaced in `ws-<name>`)

```yaml
spec:
  displayName: Athena
  role: reviewer                      # RoleTemplate name
  cli: claude | ore
  model: claude-opus-5-5
  effort: high
  group: clean
  paused: false
  resources: { cpu: "2", memory: 6Gi }
  volumes: { home: 30Gi, engine: 60Gi }
  sharedDirs: [{ name: datasets, readOnly: true }]
  repos: [acme/api, acme/web]          # cloned into ~/work on first start
  workDir: work/api
  env: { FOO: bar }                    # non-secret only; validated
status:
  phase: Provisioning | Running | Paused | Updating | Failed
  forgeUser: athena
  imageVersion: ...
  updateAvailable: true
  conditions: [ForgeAccountReady, KeysReady, GatewayKeyReady, MessagingIdentityReady, PodReady]
```

Presence and usage are deliberately **not** in `Room` status. They change
every few seconds and live in the messaging service; writing them to etcd
would be pure churn.

**`RunnerPool`** (namespaced in `ws-<name>`)

```yaml
spec:
  labels: [swarm, swarm-dirty]
  scope: { org: acme } | { repos: [acme/api] }
  group: dirty                         # optional: only this group's repos
  maxConcurrent: 4
  resources: { cpu: "2", memory: 4Gi, ephemeralStorage: 40Gi }
  minFreeDiskGB: 5
```

## 5. The operator

Go, controller-runtime (v0.25), one binary, leader-elected. One controller
per kind:

- **Platform controller.** Runs preflight, applies the platform services, and
  manages the control center's exposure and certificates. Manifests are Go
  `embed`ded YAML templates with a few values, applied with **server-side
  apply** under the field manager `swarmageddon`. Helm is not used at runtime.
  Every object carries an owner reference to the `Platform` and a
  `swarmageddon.dev/version` label. Admin bootstrap per service:
  - **Forgejo:** a generated admin user, password and token in a Secret.
  - **LiteLLM:** a master key in a Secret.
  - **PostgreSQL:** a role and a database per service.
- **Workspace controller.** Owns the workspace namespace and everything in
  it:
  - default-deny NetworkPolicies and a ResourceQuota
  - the control center's RoleBinding (§11.3)
  - the Forgejo org, teams and branch protection (§9)
  - the messaging policy, which the messaging service reads from the CR
  - LiteLLM team budgets
  - the built-in RoleTemplates, if missing
- **Room controller.** For one Room, it reconciles exactly that room's
  objects:

| Object | Content |
|---|---|
| ServiceAccount `room-<name>` | `automountServiceAccountToken: false`; no RoleBindings |
| Secret `room-<name>-identity` | SSH key, GPG key, Forgejo token, messaging client certificate and key, gateway key, VNC password |
| ConfigMap `room-<name>-persona` | Claude managed settings and CLAUDE.md, or ore `/etc/codex` files; generated instructions |
| StatefulSet `room-<name>` | 1 replica; volumeClaimTemplates `home` and `engine` |
| Service `room-<name>` | headless; the VNC port reachable only from the control center |
| NetworkPolicy `room-<name>` | per-room egress (§11.2) |
| Forgejo user `<name>` | with keys and token (§9) |
| LiteLLM key | per room, with budget |
| Messaging identity | the certificate's subject (§7.5) |

- **RunnerPool controller.** Registers ephemeral runners and starts one pod
  per waiting job (§9.4).

**One room never restarts another.** Each Room has its own StatefulSet,
Secret, ConfigMap and policy. Nothing shared is rendered into a room's pod
template:
- the team list and teammates' summaries are fetched at runtime by the
  supervisor
- the CA bundle is a projected volume, which kubelet updates in place

The room controller computes the pod template only from that Room, its
RoleTemplate and the platform version. A test asserts that adding, changing,
pausing and deleting room B leaves room A's pod UID and its StatefulSet's
`generation` unchanged.

**Lifecycle actions.**

| Action | How |
|---|---|
| Pause | `spec.paused: true` sets replicas to 0; volumes stay |
| Resume | sets replicas back to 1 |
| Recreate | deletes the pod (and, if asked, the home volume, after typing the room name) |
| Remove | deletes the Room. Its Forgejo user is disabled, not deleted, so history keeps its author. Its volumes are kept for `retention.deletedRooms` (default 7 days) unless "delete data now" is chosen |
| Rotate | an annotation makes the controller issue new keys and tokens, revoke the old ones in Forgejo and LiteLLM, and update the Secret in place. The supervisor reloads; no restart |

## 6. Rooms: image, pod and supervisor

### 6.1 The image

One base image, `room`, built for amd64 and arm64 in CI (`docker buildx`
with native arm64 runners, no QEMU for the big layers). Ubuntu 26.04 (see
the note at the top). It contains:

- **Development tools:** git, Node (via a system-wide nvm under `/opt/nvm`,
  owned by the agent user), Python with uv, build tools, ripgrep, jq, tmux.
- **Containers:** Podman with its Docker-compatible service, and the Docker
  CLI and compose plugin pointed at it (ADR 0001).
- **Display:** Xvfb, openbox, x11vnc, Firefox. VS Code is optional (a
  build-arg variant).
- **Forge:** `tea` (ADR 0009) and git signing with GPG (§9.2).
- **Agent CLIs at pinned versions:** Claude Code and ore. Auto-update is off;
  version bumps come through image releases (Renovate PRs that must pass the
  canaries, §14).
- **Our code:** `supervisor` and `swarm`.

The image holds no identity, and the CI runner image is built from it (§9.4).

### 6.2 The pod

Pod fields:

| Field | Value | Why |
|---|---|---|
| `hostname` | the room name | |
| `hostUsers` | `false` | `userns` tier |
| `runtimeClassName` | `sysbox-runc` | `sysbox` tier only |
| `automountServiceAccountToken` | `false` | |
| `enableServiceLinks` | `false` | no other services' addresses in env |
| `terminationGracePeriodSeconds` | 120 | lets the supervisor stop the CLI cleanly and flush |

One container, `room`. Its supervisor is PID 1 (through `tini`).

| Mount | Contents | Access |
|---|---|---|
| `/home/<agent>` | `home` volume | agent |
| `/var/lib/containers` | `engine` volume | root in the room |
| `/run/swarm/identity` | the room's Secret | readable by root and the agent; it's the agent's own identity |
| `/etc/claude-code` or `/etc/codex` | persona ConfigMap | read-only |
| `/etc/swarm/ca` | projected platform CA bundle | read-only |
| `/srv/shared/<name>` | shared dirs, if any | per `sharedDirs` |

Resources are `requests` = `limits` for memory (no overcommit surprise inside
a room), and a CPU limit as given. A PID limit comes from the namespace's
`LimitRange` and kubelet's `podPidsLimit`, which preflight reports.

### 6.3 The room supervisor

The supervisor replaces remote-labs' `entrypoint.sh`, supervisord,
`agent-autostart`, `docker-janitor` and agent-bus. It's Go and kept small.

On start it:
1. Creates the agent user (uid 1000) and fixes ownership.
2. Installs the SSH key, GPG key and git config (signing on), `tea`'s
   config, and `~/.ssh/config` with `IPQoS none` and the forge's host key
   pinned.
3. Fetches the team list from messaging and writes the generated
   instructions.
4. Starts its services:
   - `podman system service` on `/var/run/docker.sock`
   - Xvfb `:99` with owner-only access (remote-labs' `room-xvfb`)
   - x11vnc on localhost only, reached through the control center
   - the disk janitor
5. Clones `spec.repos` that are missing.
6. Starts the agent (ADR 0006 for Claude Code, ADR 0007 for ore). The agent
   always gets a first turn after start, so other agents can wake it, and
   the CLI picks up open work.

While running it:
- Keeps one Connect stream to messaging (`Subscribe`) and delivers messages
  into the CLI without timers (ADR 0006, 0007). It acks after delivery and
  dedups by ID.
- Reports presence (`working`, `waitingOnHuman`, `idle`; `offline` is
  inferred by the server when the heartbeat lapses), the current task (the
  latest user prompt or `set_status` text), token usage, and disk use per
  volume.
- Routes approvals: CLI → supervisor → approvals stream → control center →
  back, and withdraws them when answered locally.
- Serves the `swarm` Unix socket for the in-room MCP server and hooks. Only
  the agent user and root can connect.
- Reloads rotated credentials from the Secret mount, which kubelet updates
  in place.
- Restarts the CLI if it exits unexpectedly, with backoff, and reports
  `roomCrashed` after repeated failures.

**The supervisor is not trusted by the platform.** The agent is root in its
room (through `sudo`) and can kill or replace the supervisor. Everything the
supervisor reports is treated as that room's self-report, and every policy
decision happens server-side (§11).

### 6.4 Personas

A `RoleTemplate` is role text plus CLI policy. The operator renders it into
the room's persona ConfigMap:

- **Claude Code** (ADR 0006): `managed-settings.json` holds permission allow
  and deny rules, our hooks plus the template's, `crossSessionInbound:
  accept` and `allowManagedHooksOnly`. `CLAUDE.md` holds the identity, the
  team, the role text and the tool guide.
- **ore** (ADR 0007):
  - `requirements.toml`: approval policy, `approvals_reviewer`, the model
    provider, and MCP servers with tool allowlists
  - `config.toml`: the profile defaults
  - `rules/*.rules`: execpolicy

Built-in templates: coordinator, implementer, reviewer, CI/infra, QA. They are
generic and teach the forge workflow with `tea`, messaging with the `swarm`
tools, and the branch rules. Personas are guardrails, not security. The hard
limits are forge permissions, branch protection, NetworkPolicy and what is
mounted (§11).

### 6.5 Advisors and the warden

Each room can have an **advisor**: a stronger model, or the same model at a
higher effort, that the agent consults through the `swarm advise` tool.

- **Inputs:** the question, the tail of the transcript, and files from the
  work folder.
- **Limits:** at most a read-only file reader; answers return only to the
  calling session.
- **Spend:** a separate budget-capped gateway key.
- **Claude rooms** also get a managed read-only `advisor` subagent, and
  Claude's native advisor tool as an opt-in. ADR 0010 has the details.

The **warden** is a small model that keeps agents from going rogue. It works
in four layers:

1. rules in the room
2. a synchronous judge for risky calls, in `gate` mode
3. trajectory review after each turn
4. a gateway guard outside the room, which a root agent can't tamper with

It can deny, escalate to a person, interrupt, or quarantine a room: refuse
its model requests and shrink its network and forge access until a person
releases it. Its verdicts land in the audit log, the room's Activity tab and
the observatory. ADR 0011 has the details.

### 6.6 Layered room settings and shared volumes

Some settings apply to everyone, some to a role, and some to one room. They
are written at three levels, applied in this order, with later levels
overriding earlier ones by key:

1. **Workspace** (`Workspace.spec.roomDefaults`): every room in the
   workspace.
2. **Role** (`RoleTemplate.spec.roomDefaults`): rooms of that role.
3. **Room** (`Room.spec`): the room itself.

| Setting | Merged by |
|---|---|
| `env` | variable name; non-secret values only |
| `secretEnv` | variable name; each value comes from a key of a Secret in the workspace namespace, copied into the room's identity Secret |
| `mcpServers` | server name, in the `.mcp.json` format (stdio, `http`). Header and env values may reference Secrets (`valueFrom`). A level can remove an inherited server with `disabled: true` |
| `network` | the mode as a whole (`none`, `limited` or `full`), with `domains` merged as a union and `packageManagers` taken from the latest level that sets it |

The operator renders the merged result into the room:

- **Variables:** in the room's environment, and for SSH-less shells in the
  supervisor's environment file.
- **MCP servers:** in Claude Code's managed MCP file, next to the `swarm`
  server, and in ore's `config.toml`. ore doesn't expand `${VAR}`
  placeholders, so the operator writes values in directly, inside the
  room's own files.
- **Network:** the room's NetworkPolicy and egress allowlist (§11.2).

Changing a workspace or role default rolls the rooms it affects, one at a
time, and only when each is idle (the same policy as image updates, §3.5).
It never rolls them all at once.

Secrets a room receives are readable by its agent, as everything mounted in a
room is (§11). Give MCP servers keys scoped to what the agents may do.

**Shared volumes.** `Workspace.spec.sharedVolumes` declares ReadWriteMany
volumes. Each one has:

- a name, a size and an optional StorageClass
- mounts by selector (`everyone`, `role:<t>`, `group:<g>`, `room:<name>`),
  each read-only or read-write

The operator creates the claim in the workspace namespace and mounts it at
`/srv/shared/<name>` in each matching room.

- **Walls:** a volume may not be mounted read-write on both sides of a
  wall. If a workspace asks for that, the operator mounts it read-only in
  every room and sets the rooms' `SettingsReady` condition to say why, so a
  wall can't be crossed through a disk and no room loses its other settings.
- **No ReadWriteMany:** preflight reports whether the cluster offers RWX
  (§3.4). Without it, the wizard hides shared volumes.
- **Identity mapping:** under the `userns` tier, a shared volume needs a
  filesystem that supports idmapped mounts. Files written by one room's
  agent (uid 1000 in each room) appear as the same uid in the others.

## 7. The messaging service

ADR 0002 covers storage, ADR 0003 the wire protocol.

### 7.1 Addressing

- **Agents:** `<name>@<workspace>`, for example `athena@acme`.
- **People:** `<handle>@<workspace>`, for example `alice@acme`, where the
  handle comes from their control center account.

Names are unique within a workspace across agents and people. The
directory knows which kind each one is. A bare `<name>` means the sender's
workspace. A third part, `@<workspace>.<cluster>`, is reserved for
federation. Two more kinds of address are covered in §7.8:

- `#<channel>`: a post to a channel.
- `@role:<role>`, `@team:<team>`, `@group:<group>`: a direct message to each
  member the sender may reach.

### 7.2 Delivery

- **API:** `Send(to, body, replyTo?, id)` returns once the message is
  stored, with at-least-once semantics.
- **Inbox:** a durable inbox per recipient (a JetStream consumer). Messages
  to an offline recipient wait until it is back or until the workspace's
  message retention expires. On expiry, the sender gets a "not delivered"
  notice, as remote-labs' wall relay did.
- **Order:** messages in each inbox are delivered in order (`MaxAckPending
  1`), which covers per-sender order.
- **Reply-to:** carried as an address. A reply is checked against policy like
  any message; a reply-to never grants permission.
- **People:** messages to people land in their control center inbox. A web
  push, and ntfy if configured, tell them.

### 7.3 Presence

States: `working`, `waitingOnHuman`, `idle`, `offline`. Reported by
supervisors on every change and refreshed every 30 s. A key in the presence
KV with a 90 s TTL turns into `offline` when it lapses. The control center
subscribes to changes for the fleet view and for "idle with open work"
notifications (§10.3).

### 7.4 Policy

The server evaluates every message against the workspace's policy. It never
trusts a room's word for who sent it, and reads the identity from the
connection (§7.5). Policy comes from the `Workspace` and `Room` resources,
watched read-only. In order:

1. **Identity.** The sender is the certificate's subject. Rooms can't set
   `from`. Only the control center's principal may send as a person, and only
   for a signed-in person of that workspace, named in the
   `x-swarm-on-behalf-of` header. Messaging trusts that header only on the
   control center's mTLS connection; rooms that send it are refused.
2. **Size and rate limits.** Per sender (token bucket; per replica, so
   approximate across replicas, which is fine for this purpose).
3. **Reachability.** Is there a link from the sender's group to the
   recipient's group?
   - **Within a group:** allowed by default (`defaultLinks: withinGroup`), or
     `none`.
   - **`open` link:** anyone in group A may message anyone in group B.
   - **`mediated` link:** only `senders` in A may send, only to `recipients`
     in B. The message must name exactly one recipient (no group addresses),
     and must start with `requirePrefix` if set (with `{recipient}`
     filled in). Every crossing is logged in full to `audit.<ws>.crossing`.
     One link per direction, so remote-labs' wall is two `mediated` links
     (example in §4).
   - **People** can message any agent in their workspace if they have the
     operator role, and agents can message any person of their workspace.
     Both are configurable.
4. **Required prefixes** on any link (for example, `[spec]`).

A refusal is returned to the sender as an error that says which rule refused
it and what would be allowed ("only hermes and proteus may message metis"),
and is logged to `audit.<ws>.refusal`.

### 7.5 Identity

The operator runs a small internal CA (its key is in a Secret in the system
namespace, which only the operator's ServiceAccount can read). For each room
it issues a client certificate:

- subject `CN=athena@acme`
- URI SAN `spiffe://swarmageddon/ws/acme/room/athena`
- 30-day lifetime, renewed at 2/3 of its life into the room's Secret

kubelet updates the mounted Secret in place, and the supervisor reloads it
without a restart. The control center and the messaging replicas get service
certificates from the same CA. Deleting a Room adds its serial to a deny list
the messaging service reads, until the certificate expires.

### 7.6 Audit

Every message (sender, recipients, IDs, size and body), every crossing,
every refusal and every presence change goes to the `AUDIT` stream, with
retention from the workspace. The control center shows it with filters and
exports JSONL. Bodies can be excluded from retention (`retention.auditBodies:
false`) for teams that only want metadata.

### 7.7 API sketch (`proto/swarm/messaging/v1`)

```proto
service Messaging {
  rpc Send(SendRequest) returns (SendResponse);                    // rooms, control center
  rpc Subscribe(SubscribeRequest) returns (stream Delivery);       // rooms, control center (people)
  rpc Ack(AckRequest) returns (AckResponse);
  rpc ReportPresence(PresenceReport) returns (PresenceAck);
  rpc ListPeers(ListPeersRequest) returns (ListPeersResponse);     // who may I reach, with presence
  rpc RequestApproval(ApprovalRequest) returns (ApprovalTicket);   // rooms
  rpc WatchApprovals(WatchApprovalsRequest) returns (stream ApprovalEvent);  // control center
  rpc AnswerApproval(ApprovalAnswer) returns (AnswerAck);          // control center
  rpc WatchPresence(WatchPresenceRequest) returns (stream PresenceEvent);    // control center
  rpc QueryAudit(AuditQuery) returns (stream AuditRecord);         // control center
}
// A Delivery is either a Message or an ApprovalAnswer for this room.
```

### 7.8 Channels, group sends and storm control

Direct messages suit two agents. A swarm also needs one-to-many: a
coordinator announcing to everyone, a question to whoever reviews, a
team's running conversation. It also needs protection from its own
chatter. An agent's context is its working memory, and a storm of
messages pushed into it can wreck a turn. So one-to-many communication is
mostly *pulled*, and what is pushed is budgeted.

**Channels.** A channel is a named, persistent conversation in a workspace,
defined in `Workspace.spec.messaging.channels`. Each one has:

- a name and description
- **members**, given by selectors, each with an access level:

| Selector | Matches |
|---|---|
| `everyone` | every room and person of the workspace |
| `people` | every person |
| `role:<template>` | rooms created from a role template |
| `team:<forge team>` | rooms whose forge user is in the team |
| `group:<group>` | rooms in a group |
| `room:<name>`, `person:<handle>` | one member |

| Access | Can |
|---|---|
| `read` | read posts (an announcement channel for most members) |
| `write` | post without reading (a report-in channel) |
| `readWrite` | both |

  A member's access is the highest of the entries that match it.
- a **default delivery** for its readers (below), and a post rate limit.

The workspace wizard creates three kinds of channel:

- `#announcements`: everyone reads; people and coordinators write; push.
- `#general`: everyone read-write; digest.
- One channel per role with two or more rooms, such as `#reviewers`: that
  role read-write, coordinators write; digest.

In a walled workspace, it creates them per group.

**Walls.** A channel may only have members from groups that open links
connect in both directions. The workspace controller rejects anything else
in the Workspace's status, and messaging resolves members only within that
scope. Mediated links never carry channel traffic: crossings stay direct,
named and logged (§7.4).

**Delivery modes.** Each reader has one mode per channel. It starts as the
channel's default, and the agent can change it with `set_delivery` among the
modes the channel allows.

| Mode | What reaches the agent's session |
|---|---|
| `push` | each post, as a message, within the push budget |
| `digest` | at most one notice per channel per digest interval (default 15 min): how many posts are new, who posted, their first lines, and how to read them |
| `pull` | nothing; posts wait for `read_messages` |

An **`@mention`** in a post (`@athena`) always pushes to that agent, whatever
its mode, within the budget. So "ask a specific reviewer in `#reviewers`" is
a post that mentions them: everyone in the channel sees it, and only the
mentioned agent is interrupted.

**Group sends.** `@role:reviewer`, `@team:mergers` and `@group:clean` send a
*direct* message to each matching member the sender may reach, policy
checked per recipient. This is for private requests to a set of agents. The
fan-out counts against the sender's fan-out budget.

**Storm control.** Mostly server-side, so a misbehaving room can't opt out:

- **Push budget per recipient:**
  - At most `pushPerMinute` pushed items (default 6) reach an agent's
    session per minute.
  - Beyond that, items wait unread, and the agent gets one notice: "N more
    messages are waiting; read them with read_messages".
  - Approval answers, warden notices and direct messages from people are
    never held back.
- **Coalescing:** when several items are waiting to be pushed, the supervisor
  injects them as one message, capped at `coalesceBytes` (default 4 KB).
  Long bodies are cut to their first lines, with "read the rest with
  get_message <id>". This works for both CLIs: one inbox frame for Claude
  Code, one `turn/steer` for ore.
- **Per sender:** a rate limit for messages and posts, a fan-out budget
  (recipients per minute, default 60), and duplicate suppression (the same
  body to the same target within 10 minutes is refused with a clear error).
- **Per channel:** a post rate limit.
- **Size:** the workspace's `maxBytes` applies to posts too.

**Reading.** All messages are kept for the workspace's message retention, so
an agent can always look back. Direct messages and channel posts appear in
one paginated view. Each agent has a read cursor per channel and for its
direct messages.

**Tools.** In the `swarm` MCP server, and as `swarm` subcommands:

| Tool | Does |
|---|---|
| `send_message(to, body)` | a direct message to a name or a group-send address |
| `post(channel, body)` | a post; `@name` mentions in the body push to those agents |
| `read_messages(scope, unread_only, limit, cursor, full)` | a page of messages, newest last. `scope` is `all`, `direct`, `mentions` or `#channel`. The default is unread only, 20 per page (at most 50), first 500 characters each unless `full`. Returns `next_cursor` and how many unread items remain. Returned items are marked read unless `mark_read: false` |
| `get_message(id)` | one message in full |
| `mark_read(scope, up_to)` | moves the read cursor without reading |
| `unread_summary()` | unread counts per channel, for direct messages and for mentions |
| `list_channels()` | channels the agent can see, with access, delivery mode and unread count |
| `set_delivery(channel, mode)` | changes the agent's mode |
| `list_agents()` | as before |

The generated instructions teach the etiquette:
- Use a direct message or a mention when one agent must act.
- Use a channel for things several agents should know.
- Keep posts short.
- Don't repeat on several channels.
- Check `unread_summary` at natural breaks.

**People** see channels in the control center, post where their role allows
(operators may post to any channel of their workspaces), and get the same
unread view.

**Storage.** Posts go to a `CHANNELS_<ws>` stream (`chan.<ws>.<channel>`),
retained like messages. Pushed items (direct messages, pushed posts,
mentions, digest notices) still go through the recipient's inbox consumer,
with acks and redelivery as in §7.2. Read cursors live in a `cursors` KV
bucket (`<ws>.<member>.<scope>`). Digest timers run on the inbox stream's
leader.

## 8. The control center

Go backend, React UI embedded (ADR 0004), Connect API (ADR 0003), PostgreSQL
once deployed (ADR 0008).

### 8.1 Sign-in and roles

- **Methods:** local accounts (argon2id), OIDC and SAML (ADR 0005). Keycloak
  is in the dev and e2e environments.
- **First admin:** from the setup token (§3.3) or from identity-provider
  group mapping.
- **Roles:**

| Role | Can |
|---|---|
| admin | everything, including the Platform resource, sign-in settings and all workspaces |
| operator | create workspaces (becoming their owner) and manage the ones they own or are assigned: rooms, lifecycle, terminals, approvals, policies |
| viewer | read-only fleet, activity, logs and transcripts of assigned workspaces; no terminals, no approvals |

- **Sessions:** server-side in PostgreSQL. HttpOnly, Secure, SameSite=Lax
  cookies. CSRF tokens on state-changing calls. Idle timeout 12 h, absolute
  7 d.
- **Audit:** every action (who, what, which object, before and after for
  spec changes, source IP), in PostgreSQL, with retention from `Platform`.

### 8.2 Talking to Kubernetes

The control center changes things only by writing custom resources. It
reads pods, logs and metrics, and opens `exec` streams for terminals. It
never creates pods or Secrets itself (§11.3).

### 8.3 The workspace wizard

Run at the end of first-run setup and from "New workspace". It asks for:

- **Name** and Forgejo org name.
- **Agents:** how many, and their names. The default pool is Greek gods,
  editable, and avoids names already used anywhere on the platform.
- **Role mix** from templates. Defaults per size: one coordinator, n
  implementers, one reviewer, plus CI/infra and QA past five agents.
- **CLI per agent:** Claude Code or ore.
- **Model per agent:** a model from the gateway's configured providers.
  Model access is through gateway keys in v1 (owner's decision, §17).
  Local model endpoints are added in the gateway as OpenAI-compatible
  providers. Users' own CLI logins come later.
- **Resource limits per agent,** with a live "fits / doesn't fit" estimate
  from preflight.
- **Repositories** to create or import (from a URL, with an optional
  credential used once), with optional push mirrors to GitHub (§9.5).
- **Groups and messaging policy:** groups, links, mediated links and their
  senders and recipients, with a diagram of who can reach whom.
- **Notifications:** browser push (default), ntfy, Slack, Discord, email
  webhooks.

It writes a `Workspace`, `RoleTemplate`s, `Room`s and a `RunnerPool`, then
shows provisioning progress per room from their conditions. Re-running it on
an existing workspace shows a diff and applies only changes. Nothing is
recreated that didn't change.

### 8.4 Fleet view and room tabs

**Fleet view:** every room with a presence badge, its CLI, model, current
task, spend this period, and disk. Rooms waiting on a human sort to the top,
then crashed, then working, then idle.

**Room tabs:**

| Tab | What it shows | Role needed |
|---|---|---|
| Agent | The live session. Viewers see a read-only rendering of the transcript stream (from the supervisor). Operators can attach the real TUI (tmux) through a terminal, read-only by default with a "take control" toggle | viewer / operator |
| Shell | A login shell as the agent user, through `exec`. Audited: open, close and duration. Keystrokes are not recorded by default; recording is a workspace option | operator |
| Display | noVNC through the control center to the room's x11vnc (localhost-only in the room, reached by `exec` port-forward). Never forwarded *into* a room | operator |
| Activity | Presence history, current task, recent messages in and out, pending approvals | viewer |
| Resources | CPU, memory and network from metrics-server; the process list from the supervisor | viewer |
| Storage | Volumes with size and use, the engine's images, containers and volumes, prune actions, shared directories | viewer (prune: operator) |
| Logs | Container logs, filterable by component (supervisor, CLI, engine, janitor) | viewer |

### 8.5 Approvals inbox

- **What it holds:** every pending approval from any room the person can
  see, for both CLIs. Each shows the tool, the command or diff, the agent's
  stated reason, and the persona's matching rules.
- **Answers:** allow once, allow for the session (both CLIs support it),
  deny with a message. Sent back through messaging.
- **Mobile:** the UI is a PWA. Web push notifications open the approval
  directly, and an answer takes one tap after sign-in.
- **Automatic rules** come from each persona's `autoApprove` and are
  evaluated by the control center when a request arrives. Every automatic
  answer is audited like a human one.
- **Timeouts:** after the workspace's approval timeout (default 24 h) the
  request expires, but the room stays `waitingOnHuman`, since the CLI's own
  dialog is still open in the TUI.

### 8.6 Notifications

See §10.3.

### 8.7 Swarm observatory

A dashboard for the question "what is the swarm doing?", so nobody has to
read a dozen transcripts to find out. It is the workspace's home page, and
everything on it is built from data the platform already has.

**Data.** The control center keeps an `activity_events` table in
PostgreSQL. Each row holds a time, workspace, actor, kind, target, repository
and a short summary. It is filled from three sources:

| Source | Events |
|---|---|
| Messaging (`QueryAudit` stream, followed live) | message sent (from, to; never the body), mediated-link crossing, refusal |
| Forgejo webhooks (§9.7) | commit pushed (per commit), PR opened, merged or closed, review submitted (approve, request changes, comment), issue opened or closed, comment, CI run failed or passed |
| Supervisor status (`WatchPresence`) | presence changes, current task, tokens used |

Retention follows the workspace's audit retention. The panels query
aggregates (counts per actor, pair, kind and day) through a new
`Observatory` Connect service. A live stream pushes new events to open
pages.

**Panels:**

| Panel | Shows |
|---|---|
| Swarm status | The latest report from the workspace's chronicler (below), with its time and a "request an update now" button |
| Who talks to whom | A chord diagram of message volume between agents (and people) over a chosen window. Edges are colored by kind: open link, mediated crossing, human. Clicking a pair lists those messages' metadata. Message bodies stay in the audit view, for roles allowed to see them |
| Handoff graph | A force-directed graph of the same edges, plus forge handoffs (review requested, assigned, mentioned) as dashed edges. Nodes are sized by activity and colored by presence |
| Now | One row per agent: presence, current task, the issue or PR it last touched, time in the current state. Waiting on a human sorts first, as in the fleet view |
| Contribution heatmap | A GitHub-style calendar per agent and for the whole swarm: events per day |
| Activity axes | Per agent, a four-axis chart (commits, pull requests, code review, issues), like GitHub's activity overview, with a fifth "messages" ring. It shows at a glance whether an agent leans to building, reviewing or coordinating, and whether that matches its role |
| Timeline | A merged, filterable feed of the events above |
| Throughput | Per day: PRs merged, review latency (PR opened to first review), CI failures, tokens and spend, stacked per agent |

The look is an instrument panel: a light theme first, with a dark one. Labels
are monospaced and quiet, there is one accent color for alerts, and charts
are thin-lined, dense and readable at a glance. Charts are drawn with d3
modules (`d3-chord`, `d3-force`, `d3-scale`, `d3-shape`) inside React, with
no charting framework.

**The chronicler.** A built-in role template, `chronicler`, whose whole job
is to report on the swarm. The workspace wizard offers it, on by default
from four agents up.

- **Inputs:** `swarm agents` (presence and current task of every agent it
  can reach), the forge through `tea` with read access to the workspace's
  repositories, and its own messages from agents and people.
- **Output:**
  - **Every few hours** (default 4) and on request, a short report sent to
    the system address `status@<workspace>`. It covers what shipped, what's
    in flight and who owns it, what's blocked or waiting on a human, and
    risks. The control center stores each report and shows the latest one
    on the observatory. Every report is kept, so the reports also read as
    a history.
  - **"Request an update now"** sends the chronicler a message from the
    person.
- **Limits:** it's a normal room under normal policy. It sees only what its
  group's links and forge permissions allow, so a wall stays a wall. In a
  workspace with a wall, each side can have its own chronicler. It writes
  no code; its persona denies edits outside its own notes.
- **Messaging:** `status@<workspace>` is a second system address (after
  `forge@`, §9.7). Agents may send to it; the control center consumes it.
  The peer list (`ListPeers`) includes each agent's current task, so the
  chronicler can see it without reading transcripts.

## 9. Forgejo integration

Facts and field names come from Forgejo's `swagger.v1.json` (versions 13 to
17) and the forgejo-runner, `tea` and `fj` sources. Minimum Forgejo: **16**.

### 9.1 Accounts

The operator uses the admin token from Forgejo's Secret.

- **Org:** `POST /orgs`, private by default.
- **Teams:** `POST /orgs/{org}/teams`, with `units_map`.
  `can_create_org_repo: false` on every team, so agents can't create org
  repositories. Default teams:
  - `implementers` (write)
  - `reviewers` (write)
  - `mergers`: the merge allowlist
  - `ci` (write, plus admin on workflow files through protected file
    patterns)
  - one team per group (`group-<name>`) for group-scoped repositories
- **One user per agent:** `POST /admin/users` with
  `must_change_password: false`, then `PATCH /admin/users/{u}` with
  `max_repo_creation: 0` and `allow_create_organization: false`.
- **Emails off:** `[admin] DEFAULT_EMAIL_NOTIFICATIONS = disabled` is set in
  Forgejo's config before any agent is created. The mailer itself is off
  unless the admin configures SMTP for people. Agents' addresses are
  `<name>@agents.<workspace>.invalid`, so nothing is deliverable even if
  mail is turned on.
- **SSH key:** `POST /admin/users/{u}/keys`.
- **Token:** `POST /admin/users/{u}/tokens` (Forgejo 16 and later), scoped to
  `write:repository, write:issue, read:organization, read:user,
  write:package, read:notification`, and stored in the room's Secret only.

### 9.2 Signing

**GPG, not SSH signing.** Forgejo verifies SSH signatures only against keys a
user has verified in the web UI, and has no API for that. A GPG key can be
added and verified entirely through the API:

1. `GET /user/gpg_key_token` with `Sudo: <agent>`.
2. Sign that token with the agent's key, in the operator.
3. `POST /user/gpg_keys` with `armored_signature`, again with `Sudo`.

The supervisor configures git with `commit.gpgsign` and `tag.gpgsign`. The
agent's email must be an activated address for commits to show as verified;
admin-created users' primary emails are activated (to confirm in M1).

### 9.3 Branch protection

Forgejo has no org-wide rules, so the operator applies protection to every
repository it manages:

- on creation through the wizard or a `Workspace` change
- on any repository that appears in the org, caught by a system webhook
  (`/admin/hooks`) to the operator plus a periodic resync

The rule is `POST /repos/{o}/{r}/branch_protections` with:

| Field | Value |
|---|---|
| `rule_name` | the default branch |
| `enable_push` | `false` (changes arrive as PRs) |
| `require_signed_commits` | `true` |
| `required_approvals` | from the Workspace, default 1 |
| `dismiss_stale_approvals` | `true` |
| `block_on_rejected_reviews` | `true` |
| `enable_merge_whitelist` | `true` |
| `merge_whitelist_teams` | `[mergers]` |
| `enable_status_check` | `true` |
| `status_check_contexts` | from the repo's workflows, editable |
| `apply_to_admins` | `true` |

A protected branch blocks force pushes and deletion. The swagger has no
separate switch for either, and the inherited Gitea code always blocks both;
an M1 test pushes `--force` and deletes the branch to prove it. Tags matching
`v*` are protected too.

### 9.4 CI

- **Runners:** Forgejo Actions with **ephemeral one-job runners.** The
  RunnerPool controller:
  1. Watches waiting jobs (`GET /orgs/{org}/actions/runners/jobs?labels=…`).
  2. For each job, registers an ephemeral runner (`POST
     /orgs/{org}/actions/runners` with `ephemeral: true`).
  3. Starts a pod running `forgejo-runner one-job` with the room image's CI
     variant, Podman as the job's Docker, and `emptyDir` storage capped by
     `ephemeralStorage`. The pod and its storage are deleted when the job
     ends.

  This replaces remote-labs' per-job cleanup hooks: there's nothing to clean.
  It also fixes the clean/dirty sharing problem, since no two jobs ever share
  a disk.
- **Scope:** per workspace (the org), optionally per group. A pool with
  `group: dirty` serves only repositories that `group-dirty` owns, under its
  own label.
- **Low disk:** before starting a job pod, the controller checks the node's
  free ephemeral storage. Below `minFreeDiskGB` it leaves the job waiting and
  raises `diskLow`, the same "refuse rather than fail halfway" rule as
  remote-labs' `ci-job-hook.sh`.
- **Isolation:** runner pods use the same tier as rooms. Their NetworkPolicy
  allows only Forgejo, the registry cache and the internet, never rooms.

### 9.5 Sharing and mirrors

- **Artifacts:** Forgejo's package registries (npm, PyPI, generic) and its
  OCI container registry, with agents' tokens (`write:package`). The role
  templates teach `podman push forgejo.<...>/acme/<image>`.
- **Push mirrors:** `POST /repos/{o}/{r}/push_mirrors` with a GitHub token
  the person enters in the wizard. The token is stored in a Secret and used
  only by Forgejo, never mounted in a room.

### 9.6 People in Forgejo

The same OIDC provider can sign people into Forgejo. There's no API for
authentication sources, so the operator runs `forgejo admin auth add-oauth`
(or `update-oauth`) in the Forgejo pod, with `ENABLE_AUTO_REGISTRATION` and
group-to-admin mapping. People see agents' PRs in Forgejo's own UI; the
control center links to them.

### 9.7 Forge notifications to agents

In remote-labs, agents sometimes "messaged" each other only by commenting on
an issue or PR, and never told the other room on the bus. The other agent
didn't see it until it happened to look. So forge activity that concerns an
agent is delivered to it as a message.

- **Source.** The workspace controller creates an org-level webhook in
  Forgejo, pointed at the control center (`/hooks/forgejo/<workspace>`) and
  signed with a per-workspace HMAC secret. It subscribes to `issues`,
  `issue_comment`, `pull_request`, `pull_request_review`,
  `pull_request_comment`, `workflow_run_failure` and `action_run_failure`
  (Forgejo 17 has no `workflow_run` hook event, and drops unknown names
  silently). The same receiver feeds the
  `ciRed` notification for people (§10.3).
- **Events delivered to an agent**, with the forge user mapped to the room of
  the same name:

| Event | Delivered to |
|---|---|
| Issue or PR assigned | the assignee |
| Review requested | the requested reviewer (or the members of a requested team) |
| `@mention` in an issue, PR, comment or review body | each mentioned agent |
| Review submitted (approved, changes requested, commented) | the PR's author |
| New comment on an issue or PR | its author and assignees |
| Workflow run failed on a PR | the PR's author |

- **Rules.**
  - The actor is never notified of their own action.
  - One delivery per agent per webhook delivery (Forgejo's delivery ID is the
    message ID, so retries deduplicate).
  - An agent is only notified about a repository its forge user can read,
    checked against the forge, so a wall made of repository permissions
    stays a wall.
- **Delivery.** The control center sends through messaging as the system
  sender `forge@<workspace>`. Only the control center's principal may send
  as it, and it bypasses group links (the forge's permissions already
  decided visibility), but it is rate-limited and audited like any message.
  The body is one line naming the event, the actor and the item, plus its
  link and a short excerpt (at most 500 characters). For example:
  `hermes requested your review on acme/api#42 "Retry uploads"`.
- **Settings.** `Workspace.spec.forgeNotifications` lists the events that
  are on; all are on by default. Role templates teach agents that forge
  notices arrive on the bus, and that a comment alone is not enough when
  something is urgent.

## 10. Model gateway, spend and notifications

### 10.1 Gateway

LiteLLM, pinned by image digest. Two of its PyPI releases were compromised
in March 2026, so version bumps go through review. It holds the providers'
API keys (entered in the wizard; Secrets only LiteLLM mounts). Each room gets
a virtual key (`/key/generate` with `max_budget`, `budget_duration`,
`models`, and `metadata.room`/`metadata.workspace`), mounted only in that
room:

- **Claude Code:** `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`, through
  LiteLLM's Anthropic-compatible `/v1/messages` endpoint.
- **ore:** a `model_providers.gateway` table with `env_key`, using the
  `responses`, `chat` or `anthropic` wire API as the model requires.

Provider keys never enter rooms.

### 10.2 Spend

Read from LiteLLM per key (`/key/info`, `/spend/logs`) with a view-only
key (Secret `control-center-gateway`), aggregated per room, workspace and
model. Budgets come from the Workspace:

- 80% of a budget raises `budget`.
- 100% makes LiteLLM refuse further calls, and the room shows "budget
  exhausted".

Live token counts and context size come from the CLIs' own telemetry, through
the supervisor.

### 10.3 Notifications

**Channels:**

| Channel | Notes |
|---|---|
| Browser push | first (owner's priority): the Push API with VAPID keys generated at install, through a service worker |
| ntfy | deployed when chosen |
| Slack, Discord, email | webhooks, and SMTP for email |

**Events:**

| Event | Source |
|---|---|
| `approvalWaiting` | approvals stream |
| `idleWithOpenWork` | presence went `idle`, and the agent has open issues or PRs assigned in Forgejo, or unread messages, for 10 min |
| `ciRed` | Forgejo webhook for failed workflow runs on default branches and open PRs |
| `budget` | §10.2 |
| `diskLow` | §12 |
| `roomCrashed` | the supervisor reports repeated CLI exits, or the pod restarts (Kubernetes events) |

Each person chooses channels per event and per workspace. Repeats are
collapsed: one notification per room and event until it changes.

## 11. Security model

### 11.1 Boundaries

| Boundary | Enforced by |
|---|---|
| Room ↔ node | `userns` or `sysbox` tier (ADR 0001); in the `privileged` tier, **nothing**: the admin accepted that explicitly |
| Room ↔ Kubernetes API | no ServiceAccount token; egress denied to the API server's endpoints and the `kubernetes` Service IP |
| Room ↔ other rooms and workspaces | egress denied to all cluster CIDRs except named platform services; ingress to rooms only from the control center (VNC) |
| Room ↔ control center API | egress denied; the control center's ingress allows only the ingress controller |
| Who a room may message | messaging server policy (§7.4), with identity from the certificate |
| What a room may change in git | Forgejo teams, branch protection, scoped tokens |
| Spend | gateway virtual key budgets |
| Personas | guardrails only; the agent may be root in its room |

**Running with permissions bypassed** is supported. Every row above except
"Personas" holds regardless of what the agent does inside the room.

### 11.2 Room network access

Each room has one of three network access modes. The mode is a layered
setting (§6.6): a workspace default, a role's, or a room's own.

| Mode | The room reaches |
|---|---|
| `none` | the platform services only: messaging, Forgejo (HTTP and SSH), the gateway, the registry cache, and DNS |
| `limited` | the platform services, plus the listed domains through the egress proxy; one switch adds the package managers |
| `full` | the platform services and the internet, minus private and cluster ranges |

**Enforcement.** Every mode starts from the same NetworkPolicy: DNS
(kube-dns, UDP and TCP 53) and the platform services, selected by namespace
and pod labels. The modes then differ:

- `none` adds nothing.
- `limited` adds the egress proxy (below) and nothing else. Tools that ignore
  the proxy settings fail to connect, so nothing gets out unchecked.
- `full` adds an `ipBlock` of `0.0.0.0/0` and `::/0` except:
  - the cluster's pod and service CIDRs, and node addresses
  - RFC 1918 ranges and CGNAT `100.64.0.0/10`
  - link-local `169.254.0.0/16`, which covers cloud metadata
  - the admin's `extraDeniedCIDRs`

`allowedCIDRs` (for example a model server on the LAN) adds direct ipBlocks
in `limited` and `full`.

**The egress proxy** (`egress`, two replicas in the platform namespace,
written in Go):

- **Protocols:** an HTTP proxy that handles `CONNECT` for HTTPS and plain
  HTTP requests.
- **Identity:** each room authenticates with `Proxy-Authorization`, using a
  credential from its identity Secret. The proxy looks up the room's
  allowlist.
- **What it checks:** the `CONNECT` host (and, for HTTPS, that the TLS SNI
  matches it) or the request host. It never terminates TLS.
- **Allowlist entries:** exact hosts or `*.suffix`, with an optional port
  (default 443 and 80).
- **The package-managers switch** adds a list maintained in the operator:
  - npm, Yarn and pnpm registries
  - PyPI and its files host
  - the Go module proxy and sum database
  - crates.io and its index
  - RubyGems
  - Maven Central
  - the Ubuntu archive and security mirrors
  - GitHub release downloads (`objects.githubusercontent.com`,
    `release-assets.githubusercontent.com`; not `github.com` itself, which
    a person adds when a room should clone from GitHub)

  Docker Hub is already covered by the registry cache.
- **Refusals:** a 403 with a plain-text body naming the room's mode and how
  to change it, such as "athena's network access is limited; this host is
  not on its list. A person can add it in the control center."
- **Audit:** every refusal, and (optionally) every connection's host, is
  logged to the room's audit stream.

The supervisor sets `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` (the
platform services and cluster domains) for the CLI and for shells, and
writes them into Podman's `containers.conf`, so nested containers use the
proxy too. In `full` mode no proxy is set.

Nested containers' traffic leaves through the pod's network namespace, so the
same NetworkPolicy applies to them. Preflight's NetworkPolicy check fails the
install if policies aren't enforced, rather than pretending they are.

### 11.3 RBAC

- **Operator:** a ClusterRole for our CRDs, namespaces, and the kinds it
  manages (StatefulSets, Services, ConfigMaps, Secrets, NetworkPolicies,
  ResourceQuotas, RoleBindings, Jobs, Pods) in namespaces it labels
  `swarmageddon.dev/managed=true`. It's the trusted component.
- **Control center:**
  - Cluster-wide: only get, list, watch, create, update and patch on
    `platforms` and `workspaces`, and read access to their status.
  - In each workspace namespace, a RoleBinding the operator creates:
    `rooms`, `roletemplates` and `runnerpools` (read and write), pods and
    pods/log (read), `pods/exec` and `pods/portforward` (create),
    `metrics.k8s.io` pods (read), and get, update and patch on the one
    Secret `room-secrets` by name, which the operator creates empty and
    which holds the values people enter for `secretEnv` and MCP `valueFrom`
    (§6.6).
  - In the system namespace: its own Secrets (session key, VAPID, database
    credentials) by name. The fixed names are in the chart's Role; Secrets
    and ConfigMaps it creates per provider, channel or workspace
    (`oidc-<p>`, `notify-<ws>-<kind>`, ...) the operator grants one by one
    in Role `control-center-owned`, since RBAC can't match a prefix. It may
    create any Secret there, but never read one it wasn't granted.

  No Secrets anywhere else, no cluster-wide pods.
- **Messaging:** get, list and watch `workspaces` and `rooms`; its own
  Secrets.
- **Rooms and runners:** nothing.

### 11.4 Secrets

- **Where they live:** only in Kubernetes Secrets. Encryption at rest is
  recommended; preflight warns if the API server doesn't encrypt Secrets,
  with the k3s flag `--secrets-encryption`.
- **Where they never appear:**
  - **custom resource specs:** references only, and validation rejects
    anything that looks like a key in `env`
  - **logs:** our loggers have a redaction list, and tests grep the e2e
    logs for every generated secret
  - **git, and messages:** the messaging service rejects bodies that
    contain one of the workspace's issued tokens; a cheap exact-match
    check against a hashed set
- **What a room gets:** only its own identity. No shared env file like
  remote-labs' `secrets/_shared/env`. Platform-wide settings that aren't
  secret go in the persona ConfigMap.
- **Never forwarded:** an SSH agent or a display. Terminals are `exec`
  sessions, and VNC flows only outward from the room to the browser.

### 11.5 What we don't defend against in v1

- A compromised node or cluster admin.
- Kernel exploits from a user-namespaced room. User namespaces shrink that
  surface but don't remove it.
- Data exfiltration to the internet by an agent with internet access. The
  egress switch is the control.
- Agents socially engineering each other within what policy allows. Mediated
  links and the audit log are the controls.

## 12. Disk hygiene

The lesson: a 126 GB disk filled quietly. So:

- **Quotas per volume.** Each room's `home` and `engine` claims have sizes.
  Where the StorageClass enforces them (TopoLVM, OpenEBS LVM/ZFS, Longhorn,
  cloud CSI; or local-path with XFS project quotas), they're hard limits.
  On plain local-path they're not enforced, and preflight and the Storage
  tab say so: quotas become soft, enforced by the janitor and warnings.
- **Janitor.** Runs in the supervisor, like remote-labs' `docker-janitor`.
  Every hour it removes stopped containers older than 24 h, dangling images,
  old build cache and unreferenced volumes. Above 70% of the engine
  volume, it also removes unused tagged images (they come back from the
  registry cache). All thresholds are per Room.
- **Early warnings.** The supervisor reports use per volume every 5 minutes:
  - 80%: `diskLow`
  - 90%: an aggressive prune, and the room shows red
  - node free space under 15%: a workspace-wide `diskLow`, from kubelet's
    stats
- **CI** can't accumulate: ephemeral pods with capped `emptyDir`.
- **Registry cache** expires blobs after 7 days (CNCF distribution's proxy
  TTL). The **shared Podman image store** (M5, where RWX exists) saves
  storing the same layers once per room.
- **Transcripts.** Claude Code and ore transcripts in homes are kept for the
  workspace's transcript retention (Claude's `cleanupPeriodDays`, and the
  janitor for ore's sessions).
- **The dashboard** shows disk for every room, the platform volumes and each
  node.

## 13. Repository layout and tooling

```
api/v1alpha1/            CRD types (kubebuilder markers) and generated deepcopy
cmd/
  operator/  control-center/  messaging/  supervisor/  swarm/
  warden/  egress/  devices/  mockmodel/
internal/
  operator/{platform,workspace,room,runnerpool,preflight,addons,persona,manifests}/
  roomsettings/          the one merge of layered room settings (§6.6)
  warden/  egress/  devices/  mockmodel/
  forgejo/               admin client (generated from swagger, trimmed)
  gateway/               LiteLLM client
  messaging/{server,policy,store}/
  supervisor/{claude,ore,engine,janitor,display}/
  controlcenter/{auth/{local,oidc,saml},api,terminal,approvals,notify,store}/
  pki/                   internal CA
proto/swarm/…/v1/        protobuf (buf)
web/                     React UI (Vite)
images/room/  images/runner/
charts/swarmageddon/     Helm chart; install.yaml is rendered from it in CI
hack/                    dev scripts (k3d cluster, Keycloak realm, dev extras)
test/e2e/  test/canary/
docs/                    design.md, adr/, user docs
```

- **Language and dependencies:** Go 1.26 (current at implementation start).
  Dependencies are kept boring: controller-runtime, connect-go, nats-server
  and nats.go, go-oidc, gosaml2, pgx and goose, client-go.
- **Make targets:**
  - `make dev`: creates a k3d cluster with a local registry, builds every
    image, installs with the dev values (Keycloak and the mock model
    included), and prints the URL and the setup token
  - `make test`: unit tests and envtest
  - `make e2e`: the k3d end-to-end suite
  - `make canary`: CLI protocol checks
- **CI** (GitHub Actions on this repository):
  - lint (golangci-lint, `buf lint` and `buf breaking`, eslint)
  - unit and envtest
  - multi-arch image builds
  - e2e on k3d
  - canaries on version bumps
  - `install.yaml` rendered and attached to releases

## 14. Testing

| Layer | What | Where |
|---|---|---|
| Unit | policy evaluation (table tests, including every mediated-link case from remote-labs' `wall-relay.test.mjs`), role mapping, SAML parsing (plus fuzzing), Forgejo request building, janitor thresholds | `go test` |
| Controller | envtest: reconcile each CRD; one room's change never touches another's objects; idempotent re-apply | `go test` with envtest |
| Messaging | 3 embedded replicas in-process: at-least-once with dedup, ordering, offline hold, retention expiry, a leader killed mid-stream | `go test` |
| Canary: Claude Code | pinned real binary + mock model: inbox frame mid-turn and from idle, hook payloads, managed settings, no dialogs on start and resume, approval round trip | `make canary` |
| Canary: ore | pinned real binary + mock model: schema diff, steer mid-turn, start from idle, approvals from two clients, resume, TUI `--remote` attach | `make canary` |
| E2E | k3d: install, claim admin, preflight, deploy services, workspace wizard via API, rooms of both CLIs messaging in every direction (Claude↔Claude, Claude↔ore, ore↔ore, person↔agent), a mediated link refusing and passing, a PR with signed commits, review and merge by the allowlisted team, CI on an ephemeral runner, add and remove a room while others keep their pod UIDs | `make e2e` |
| UI | Playwright for the wizards, the fleet view, approvals on a mobile viewport | `make e2e` |
| Acceptance (M1) | fresh single-node k3s VM: the one install command, then only a browser (driven by Playwright), until a terminal shows a working room | release checklist; nightly |

**The mock model** (`cmd/mockmodel`, `internal/mockmodel`) speaks Anthropic Messages and the
OpenAI Responses and Chat APIs, with streaming. It replays scripted turns:
text, tool calls (run a command, edit a file, call `swarm.send_message`) and
waits, keyed by a marker in the prompt. It's served through the real LiteLLM
in e2e, so the gateway path is tested too. Real-model smoke tests run
manually with a budgeted key.

## 15. Milestones

Each milestone ends with a recorded demo and passing automated tests.

**M1: foundations.**
- The repository, CI, and `make dev` on k3d.
- **First: the `userns` + Podman spike (ADR 0001).**
- The Helm chart and `install.yaml`, the `welcome` Job and `install.sh`.
- The operator with `Platform` and `Room` (and a minimal `Workspace`).
- The room image (both architectures) and supervisor, starting Claude Code
  with pre-answered dialogs and resume.
- The control center with the setup token, local admin, preflight, and
  deploying PostgreSQL and Forgejo.
- One Claude Code room with its Forgejo account (keys, GPG verified, token),
  reachable through a dashboard terminal.
- Tests: unit, envtest, the Claude canary (start, resume, no dialogs), e2e.
- **Acceptance:** on a fresh single-node k3s cluster, one install command,
  then only the browser, gives a working room.

**M2: messaging.**
- The messaging service (3 replicas, JetStream), PKI and room identities.
- Supervisor bridges for both CLIs: Claude through the inbox socket, ore
  through the app-server with `turn/steer`.
- The `swarm` MCP server.
- Policy including mediated links, and the audit log with retention.
- People's inboxes, minimal UI.
- Demo: Claude and ore rooms messaging in every direction; the wall
  scenario.

**M3: control center.**
- OIDC and SAML with Keycloak, and role mapping.
- The full first-run wizard (all platform services, hostname and TLS) and
  the workspace wizard producing a team of N agents.
- Upgrades through the operator (rolling rooms when idle).
- The fleet view and room tabs; adding and removing rooms live.

**M4: human in the loop.**
- Presence everywhere.
- The approvals inbox (both CLIs, automatic rules, mobile).
- Notifications: browser push first, then ntfy, Slack, Discord and email.
- Spend and budgets.

**M5: CI and sharing.**
- RunnerPools with ephemeral runners.
- Package and container registries, and push mirrors.
- Disk hygiene complete: quotas, janitor and warnings, plus the shared
  image store where RWX exists.

## 16. Where the brief needs a change

These are real problems with fixed decisions or wording. Each has a proposal;
none is applied silently.

1. **"Prefer Sysbox" (nested containers).** Sysbox doesn't support k3s (no
   plans to), supports only Kubernetes 1.32 to 1.35, and fails on containerd
   2, which every current k3s ships. Following the brief would make
   privileged rooms the default on our main target, and isolation would not
   hold under bypassed permissions. **Proposal:** user-namespaced pods with
   Podman as the default tier, Sysbox where it already exists, and
   privileged only on explicit acceptance (ADR 0001). The `userns` path is
   unproven on containerd, so it's the first spike of M1.
2. **"Its own Docker daemon".** Per the owner's answer, rooms run Podman with
   a Docker-compatible API instead of dockerd (ADR 0001). Agents still type
   `docker …`. A few Docker-internal workflows may differ.
3. **"The install prints how to open the control center"** with `kubectl
   apply -f <url>`. `kubectl apply` can't print our text. **Proposal:** the
   manifest one-liner chains `&& kubectl logs -f job/welcome` (§3.1). Helm
   prints NOTES as usual.
4. **"An SSH and signing key for each user"** with SSH signing. Forgejo
   can't verify SSH signing keys through its API, so signed-commit
   protection would reject agents' commits until someone clicks in the UI.
   **Proposal:** sign with GPG, which is fully automatable (§9.2).
5. **"Ore delivery through the app-server"** holds, but `app-server`
   doesn't accept profiles (`-p`). Personas go in through
   `/etc/codex/requirements.toml`, rules and `thread/start` options instead
   (ADR 0007). In this fork the config paths are still `codex`.

## 17. Open questions

The owner's answers so far, and what the design does with them:

| Question | Answer | In the design |
|---|---|---|
| Is Sysbox acceptable to require; may privileged be a fallback? | Yes; Podman also works and can share an image cache | Sysbox optional, privileged as an accepted fallback, Podman everywhere, a shared image store in M5 (§16.1, ADR 0001) |
| Model access | API keys through env variables for now | Provider keys in the gateway; each room gets its virtual key through env. Local servers work as OpenAI-compatible gateway providers. CLI logins later |
| Identity providers to test | Keycloak | Keycloak in dev and e2e, for OIDC and SAML |
| First notification channel | Browser notifications | Web push first, in M4 |
| Are mediated links a v1 requirement? | Yes | In M2 (§7.4) |
| Retention | Yes, with a TTL to choose | Messages, audit and transcripts each have a TTL per workspace (§4) |

New questions, each with a recommendation:

1. **The `userns` spike fails on containerd.** If rooms can't run nested
   containers under user namespaces on k3s, do we ship rootless Podman as
   the agent user (Che's approach; slower, some tools need tweaks) or accept
   privileged as the k3s default with a warning? **Recommend:** rootless
   Podman. Isolation matters more than speed for the bypass mode.
2. **API group and product names.** `swarmageddon.dev` is a placeholder; do
   you own a domain we should use for the API group, chart registry and
   image registry (`ghcr.io/usr-lex/...`)? **Recommend:** `ghcr.io/usr-lex`
   for images and charts, and the API group on a domain you control.
3. **Internet access for rooms by default.** Agents need npm, PyPI and
   GitHub to work, but internet access is also the exfiltration path.
   **Recommend:** on by default, a per-workspace switch, and a documented
   allowlist mode.
4. **`sudo` for agents.** remote-labs gives passwordless sudo; with
   user-namespaced rooms that's root inside a namespace only. **Recommend:**
   on by default, a per-room switch.
5. **PostgreSQL availability.** One instance is a single point of failure for
   the control center, Forgejo and the gateway (ADR 0008). **Recommend:**
   accept it for v1, with nightly dumps.
6. **Approval timeout.** **Recommend:** a 24 h default, after which the request
   leaves the inbox and the CLI's own dialog remains for someone attached.
   Should it rather auto-deny?
7. **Session recording for terminals.** **Recommend:** audit open, close and
   who; record keystrokes only when a workspace turns it on.
8. **Forgejo minimum version.** **Recommend:** 16 (needed for admin-issued
   tokens and the new runner registration API), shipping the latest stable
   (17).
9. **VS Code in the room image.** It adds roughly 400 MB. **Recommend:** a
   separate `room-desktop` variant, and a slim default.

## 18. What is verified and what is assumed

Research for this design (2026-10) read source and docs, and ran the ore
binary. The detail is in the ADRs.

**Verified**

- Claude Code (2.1.301) delivery and other behavior, from its docs:
  - inbox socket delivery: between tool calls when busy, a new turn when idle
  - `CLAUDE_CODE_MESSAGING_SOCKET` and `CLAUDE_CODE_MESSAGING_TOKEN` exported
    to hooks
  - `crossSessionInbound`
  - `PermissionRequest` and `Notification` hooks, and hook timeouts
  - managed settings paths and precedence
  - trust and bypass flags
  - gateway environment variables
  - OpenTelemetry metrics
- ore 1.161.0, by running it:
  - `app-server --listen unix://`
  - `turn/steer`
  - approvals going to all clients, with the first answer winning
  - the TUI attaching with `--remote` to the same thread
  - resume after a restart
  - `prompt` rules triggering approvals
  - the generated schema
- Forgejo 13 to 17 swagger:
  - the admin user, key and token endpoints (admin tokens in 16 and later)
  - GPG key verification through `Sudo`
  - teams with `can_create_org_repo`
  - branch protection fields
  - no org-wide rules
  - ephemeral runner registration (15 and later)
  - push mirrors
- forgejo-runner 13.2: `one-job` is required for ephemeral runners, and there
  are no job hooks.
- Forgejo 17.0.0, run against a real server (2026-10-16):
  - `POST /admin/users/{u}/tokens` issues an agent's token with the scopes in
    §9.1 (`write:repository`, `write:issue`, `read:organization`,
    `read:user`, `write:package`, `read:notification`)
  - admin-created users' emails are verified
  - a GPG key added through `Sudo` with a signed `gpg_key_token` is
    verified, and commits signed with it verify. git must be told
    `gpg.format=openpgp`: an inherited `gpg.format=ssh` produces SSH
    signatures that protection rejects
  - a team with `can_create_org_repo: false` stops agents creating org
    repositories (403)
  - a protected default branch refuses direct pushes, force pushes,
    deletion and unverified commits, and refuses merging a PR without the
    required approvals
- `tea` 0.16: PR review and approval. `fj` 0.7 can't approve.
- Sysbox v0.7.1: its supported versions, no k3s, the containerd 2 issue.
- User namespaces and `procMount` are GA in Kubernetes 1.36. k3s ships
  containerd 2.2 or 2.3 with runc 1.4.
- k3s enforces NetworkPolicy by default; local-path doesn't enforce size.
- LiteLLM virtual keys need PostgreSQL, and the key and budget endpoints
  exist.
- Library versions: connect-go 1.21, nats-server 2.15, go-oidc 3.21, gosaml2
  0.12, controller-runtime 0.25.

**Assumed, to be checked in the milestone that needs it**

| Assumption | Checked in |
|---|---|
| Podman as root in a user-namespaced pod on containerd 2 runs builds, compose and Testcontainers. `/dev/fuse` and `/dev/net/tun` through a device plugin are enough | M1 spike |
| Claude Code's inbox frame format (undocumented) stays stable within a pinned version. A canary guards it | M1 / M2 |
| Forgejo's OCI registry works with token auth (`write:package`) | M5 |
| LiteLLM keeps serving known keys while PostgreSQL is briefly down | M3 |
| `tea` arm64 builds, or that we can build them | M1 |
| Thousands of R3 JetStream consumers aren't needed at our scale | Measured in M2 |
| How long Claude Code's `PermissionRequest` hook can block (no documented maximum) | M4 |
