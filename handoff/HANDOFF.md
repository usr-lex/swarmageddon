# Handoff: swarmageddon, end of session 1 (2026-10-17)

This is everything the next session needs to continue. Read it top to bottom,
then `docs/design.md` (the approved design) and `docs/conventions.md`.

## Where things are

- **`main` = `ae3c040`.** Every component is built and merged. `go build`,
  `go vet`, golangci-lint (0 issues), `go test -race ./...`, web tests, image
  builds and chart checks pass. The end-to-end suite does **not** pass yet
  (see "Install blockers").
- **The working branch `claude/compassionate-wozniak-y0fs0x`** = `main` plus
  this `handoff/` folder.
- **Unfinished work** is in `handoff/patches/` as `git format-patch` series
  based on `ae3c040`. Apply with:

  ```sh
  git checkout -b cc-gaps ae3c040 && git am handoff/patches/control-center-gaps/*.patch
  git checkout -b op-gaps ae3c040 && git am handoff/patches/operator-gaps/*.patch
  ```

  - `control-center-gaps/0001`: ntfy publishes with the `ntfy-publisher`
    bearer token (audit item 5). Finished and tested by its agent.
  - `control-center-gaps/0002`: **unfinished**. Repository credentials
    (audit item 10) in proto, wizard and k8s helper, plus new tests.
    Committed as-is when the session ended; build and test it before use.
  - `operator-gaps/0001`: a messaging principal `CN=operator` that may only
    watch presence. The first step of idle rollouts (audit item 8). The
    operator side (IdleChecker using it) was not written yet.
- The same work is also in `handoff/agents-wip.bundle` (`git fetch handoff/agents-wip.bundle worktree-agent-a66138e4d3f9e72da worktree-agent-ad3a1cfdb97d03c3c`).
- **Lost work:** two agents (RBAC/NetworkPolicy fixes, and the e2e rewrite)
  were cut off while still reading code and left nothing. Redo them from the
  briefs below.

## Install blockers (CI run 19, end-to-end job)

Found by CI after this session's last push. All four stop the M1 flow, and
all four were invisible to unit tests:

1. **Ingress:** the operator can't write Traefik `ServersTransport`
   objects. Add RBAC for `traefik.io` `serverstransports` (and the
   `traefik.containo.us` group for older Traefik), or drop
   ServersTransport. The control center is never exposed.
2. **Workspace Role escalation:** the workspace controller grants the control
   center permissions the operator doesn't hold itself (`events`,
   `rooms/status`, `runnerpools/status`). Kubernetes refuses that. Either give
   the operator those verbs (RBAC markers → `make manifests install-yaml`) or
   trim the Role.
3. **RoleTemplates:** no RoleTemplate named `implementer` exists when the e2e
   creates rooms. Check the built-in template names in
   `internal/operator/workspace` against `test/e2e` (`implementer`,
   `coordinator`, `reviewer`) and when they are copied in. Rooms never start.
4. **Postgres never comes up:** the add-ons controller waits on a preflight
   tier that was never chosen. The e2e sets `isolationTier: privileged` in
   the spec; check that `PlatformTier`/add-ons honor the spec tier when
   preflight hasn't picked one.

Nightly run 20 failed for an unrelated reason: the arm64 runner couldn't
resolve `ports.ubuntu.com` during the image build. Rerun it.

## The audit (open items)

`handoff/notes/audit-f7cb407.md` is the full read-only audit of code against
design at `f7cb407`. Status of its items:

| Item | Status |
|---|---|
| 1 Claim admin (platforms/status patch RBAC) | **open** (RBAC agent lost) |
| 2 Control center Secret access (gateway-provider-*, oidc-*, notify-*, forge-webhook-*, forge-reader, control-center-gateway, control-center-saml; saml ConfigMaps) | **open**; decide fixed names vs label-scoped access |
| 3 Messaging ClusterRole lacks roletemplates (informer never syncs; /readyz fails) | **open**, one-line fix in `internal/operator/manifests/templates/messaging.yaml` |
| 4 Warden → messaging blocked by NetworkPolicy | **open** |
| 5 ntfy publisher auth | done in patch `control-center-gaps/0001` |
| 6 Forgejo doesn't trust the platform CA (webhooks) | **open** |
| 7 Secret `control-center-gateway` never created | **open** |
| 8 Idle rollouts (NeverIdle) | started: `operator-gaps/0001` (messaging side) |
| 9 e2e through the real API, Playwright in CI | **open** (agent lost) |
| 10 Repository credentials dropped | partly done in `control-center-gaps/0002` (unfinished) |
| Missing: cert-manager TLS, Forgejo OIDC (add-oauth), system webhook or repo protection resync, control center ingress policy, §11.4 log redaction and token-in-body refusal, keystroke recording, roomCrashed from pod restarts, workspace diskLow | **open** |
| Partial: coalesceBytes/janitor/reviewEveryToolCalls not written to room.json; gateway_key rotation needs restart; forge open-work TODO (`cmd/control-center/main.go`); UI text "agents can create repositories later"; 3 unused RPCs in UI | **open** |
| Inconsistent design text | fixed in `ae3c040` |

Suggested order: install blockers 1–4 → audit items 3, 1, 2, 4, 6, 7 →
re-run e2e → the rest.

## The four briefs that were in flight

These are the scopes given to the agents that didn't finish. They're good
starting points (paths and owners are still accurate):

1. **RBAC, NetworkPolicy, platform wiring.** Audit items 1, 2, 3, 4, 6, 7, the
   control center ingress NetworkPolicy, and tests:
   - an envtest that impersonates each ServiceAccount (control center,
     messaging, warden) and makes every API call the code makes
   - a pure-Go evaluation of the rendered NetworkPolicies against the
     documented flows

   Files: `charts/`, `config/rbac`, `internal/operator/manifests`,
   `internal/operator/addons`, `internal/operator/platform`.
2. **Control center and supervisor gaps.** Audit items 5 and 10, §11.4
   (`internal/redact` slog wrapper; messaging refusing bodies with issued
   tokens via a `messaging-token-hashes` ConfigMap the operator writes),
   keystroke recording, roomCrashed from pod restarts, workspace diskLow,
   the forge open-work check, gateway_key reload, and the UI fixes.
3. **Operator gaps:**
   - Idle rollouts: an IdleChecker over messaging presence with the operator
     cert `CN=operator`, idle ≥ 2 minutes, one room at a time.
   - room.json fields: coalesceBytes, janitor, reviewEveryToolCalls.
   - cert-manager Certificate (unstructured) when `tls.mode=certManager`.
   - Forgejo `admin auth add-oauth` via pods/exec.
   - A periodic org repo protection resync.
4. **e2e through the real API.** Claim admin with the setup token, preflight
   and SetIsolation, provider and services via the Platform API, the workspace
   wizard API (preview → apply), then:
   - **Messaging proof of delivery:** Claude↔ore, person↔agent, a refused
     crossing, a mention.
   - **Forge:** a signed push, a PR, review and merge by the mergers team, a
     forge@ notification, a CI runner job.
   - **Lifecycle and observatory:** add and remove a room without touching
     another; quarantine and release with the warden's 403; observatory
     edges.

   Also add Playwright (mock mode) to CI.

`handoff/notes/agent-preamble.md` and `resume-preamble.md` are the standing
instructions every agent got: environment notes, memory limits, commit
rules.

## Environment notes (cloud container)

- **Go:** use `GOTOOLCHAIN=go1.26.9`, and add `GOFLAGS=-p=2 GOMAXPROCS=2`
  when several builds run (the container has 15 GB RAM and OOM-killed itself
  once). The disk quota is about 40 GB: prune Docker and `go clean -cache`
  when it runs low.
- **Pods can't run** in k3d/k3s in this container (runc can't write
  `oom_score_adj`). The end-to-end suite only runs in GitHub CI (`make dev`,
  then `make e2e`), unless the next environment allows it.
- **Docker** pulls go through `mirror.gcr.io` (`/etc/docker/daemon.json`).
  Codeberg is allow-listed; Forgejo 17 runs fine in Docker for live tests
  (`FORGEJO_TEST_URL`, `FORGEJO_TEST_TOKEN`).
- **envtest:** `KUBEBUILDER_ASSETS=$(make -s envtest)`.
- **golangci-lint:** build with `GOBIN=/tmp/glbin go install
  github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest` (the
  released binaries are built with an older Go).

## Research notes

`handoff/notes/research-*.md` hold verified facts with sources:
- Claude Code 2.1.301: the inbox socket, hooks, managed settings, the
  advisor.
- ore 1.161.0: the app-server protocol, turn/steer, approvals, the TUI's
  `--remote` attach.
- Forgejo 13–17: the API.
- Kubernetes, user namespaces, Sysbox, NATS and the libraries.
- Advisors and the warden: turnstone, ore's guardian, Claude's auto mode.

Design §18 lists what was verified against real software.

## Session transcript

The full transcript of session 1 is at
`/root/.claude/projects/-home-user/e6902579-249c-5c06-9dd7-61017a37495c.jsonl`
(8 MB), with its subagents in the folder of the same name (42 MB), in the old
container. It isn't committed, because it's large and contains local test
credentials.
