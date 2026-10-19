# Handoff 2: swarmageddon, end of session 2 (2026-10-19)

Read this top to bottom, then `docs/design.md` and `docs/conventions.md`.
The first handoff (`handoff/HANDOFF.md`) is history; its open items are
mostly done (below).

## Where things are

- **Working branch: `claude/adoring-wright-ab2awt`** in `usr-lex/swarmageddon`.
  Everything below is merged and pushed there. **`main` is untouched** at
  `ae3c040`; nothing has been merged to it this session. Merge the branch to
  `main` once the end-to-end suite is green (or earlier, if you want its
  fixes: the unit, envtest and lint checks were green at every push).
- `claude/compassionate-wozniak-y0fs0x` is the previous session's handoff
  branch; ignore it.
- No pull request exists. Don't open one unless asked.

## What this session did

1. **Install blockers from CI run 19, fixed:** the chart's operator
   ClusterRole is now generated from the code's `+kubebuilder:rbac` markers
   (`charts/swarmageddon/files/operator-rules.yaml`, copied by `make
   chart-crds`/`make manifests`; `make install-yaml-check` fails if stale).
   Plus: control center may patch `platforms/status` (claim admin);
   messaging may watch RoleTemplates; `get` on `pods/exec` and
   `pods/portforward` (client-go streams over WebSocket).
2. **Four agents' work, merged:** RBAC/NetworkPolicy fixes with impersonation
   and policy-flow tests (`internal/operator/rbactest`, `netpoltest`); control
   center and supervisor gaps (repo credentials, ntfy auth, log redaction in
   `internal/redact`, keystroke recording, open-work check, gateway key
   reload, UI); operator gaps (idle rollouts via `CN=operator`, room.json
   fields, cert-manager TLS, Forgejo OIDC, repo protection resync, messaging
   token refusal); the e2e suite rewritten to go through the control center
   API (`test/e2e`), plus Playwright in CI.
3. **Optional Sentry** (`internal/telemetry`, chart values `sentry.*`, off
   without a DSN, redacted, rooms never report; design §11.6).
4. **Bugs found by the new e2e suite on a live cluster, all fixed:**
   Traefik default cert for IP access; preflight rerun from a stale cache;
   add-ons spinning on not-ready; the operator ignoring the chart's image
   registry; welcome Job printing a NodePort URL before k3s's Traefik
   existed; single-replica messaging never ready; control center never
   retrying its messaging client certificate; wizard `internet` default;
   ore's allowed sandbox modes; the ore driver deadlock when the app-server
   exits at start; ore's socket directory; xhost in the room image;
   janitor's Podman-incompatible prune; transcript dropping peer messages;
   registry cache blocking later services; mock model missing `created_at`;
   room pods without `fsGroup`; wizard wiping default channels on an empty
   plan; supervisor-created `~/.config` owned by root (bug 17, below).

## State of verification (be precise)

- **Green locally at each push:** `go build`, `go vet`, golangci-lint
  (0 issues; use a build with Go 1.26: see Environment), `go test` for the
  packages touched, web `npm test`/lint/typecheck.
- **E2E on a local k3d cluster** (agent D, runs 10 and 12): install, welcome
  Job, ingress/TLS, claim admin, preflight and isolation, all platform
  services, workspace wizard and re-run, all rooms running (Claude and ore),
  presence, all four CLI-to-CLI message directions, person to agent, refused
  and mediated crossings, token refusal, signed push with protected main,
  add/remove room, quarantine and release, observatory edges, and the
  no-secret-in-logs scan **passed**. Not yet seen passing end to end in one
  run: the forge pull request / review / merge / CI-runner / forge-notification
  steps and everything that depends on the `tea` fix (bug 17).
- **CI on GitHub:** run 24 (older code) got through install, claim admin,
  preflight and all seven services, then failed on a test timing issue that is
  now fixed. **CI run 25 was started on an earlier head and its result was
  never read.** The full suite has not yet passed in CI. This is the main
  open item.

## Not done / open (in priority order)

1. **Get the e2e suite green in CI.** Dispatch the workflow on the branch
   (`ci.yaml` has `workflow_dispatch`; `push` only triggers on `main`), read
   the failing job log, fix, repeat. Each e2e cycle is ~45-75 minutes.
2. **Bug 17 needs a new room image to take effect** (supervisor now chowns
   the parent directories it creates: `~/.config` was root:root so `tea`
   failed). Fixed in code (3026666), not yet in any built image.
3. **Web UI in the control-center image:** `cmd/control-center/Dockerfile`
   now has a Node stage and fails if `web/dist/index.html` is missing. It was
   committed but **never built or run**. Build the image and check the UI
   loads. Until then every image built from before this has the placeholder
   page.
4. **OpenShift first-install report** (pasted by the owner, 13 issues, below).
   Only 5.6 was started (item 3 above).
5. Agent D (the e2e agent) was cut off twice by the org's monthly spend limit
   (resets Oct 20, 05:00 UTC). Its work is all committed and merged. If you
   resume it, it reports its bug list and may have more findings.

## OpenShift report (from the owner; tested on CRC 4.22, arm64, chart at 3026666)

Quick wins first. Verify each against the code before fixing; none has been
fixed except where noted.

- **5.6** control-center image had no web UI: Dockerfile fixed, unbuilt.
- **5.5** provider API key never copied to the gateway: the control center
  writes `gateway-provider-<name>` in `swarmageddon-system`; manifests
  reference it in `swarmageddon-platform`; nothing copies it. Implement the
  copy (with updates and deletion) in the platform controller, plus a test.
- **3** ore swarm tool calls need approval: `oreMCPServer` in
  `internal/operator/persona/persona.go` has no `default_tools_approval_mode`;
  `internal/supervisor/ore/config.go` sets it but only the canary uses that.
  Set `"approve"` for swarm in the production config (ideally build both from
  one function), have the supervisor answer `mcpServer/elicitation/request`
  for swarm, route others to the approvals inbox, make the canary fail on a
  swarm elicitation.
- **5.1** no published release/images (docs or release workflow).
- **5.2** DNS NetworkPolicy assumes k3s labels (`k8s-app=kube-dns`, port 53);
  OpenShift's DNS is `openshift-dns`, port 5353. Derive the rule from the
  cluster (endpoints behind the DNS Service).
- **5.3** no OpenShift SCCs (operator, services, probes, rooms, device
  plugin). Ship SCCs when `security.openshift.io` exists and bind per
  namespace; the report's appendix has the exact SCCs that worked. The
  preflight podman probe should use the exact room security context
  (rooms also need `AUDIT_WRITE` and `KILL`).
- **5.4** wizard/preflight hide admission failures: surface `FailedCreate`
  events and classify Forbidden as "refused by admission policy".
- **6** Display tab asks for a VNC password nobody has: simplest is x11vnc
  `-nopw` (it listens on 127.0.0.1, reached only through the audited
  port-forward), or have the control center do the RFB auth itself.
- **1** Ctrl+C under "Take control" ends the terminal (tmux session is
  recreated on TUI restart); restart inside the same session
  (`respawn-pane -k`) and reconnect in the UI.
- **2** messages steered into a turn blocked on an approval are acked unread;
  add a timeout on Deliver's RPCs and queue while blocked on a human.
- **4** terminal "exit code 1" while a room starts (no tmux session yet).
- **7** per-room CLI/model/role change without the whole wizard.
- **8** (feature) let rooms bypass LiteLLM: `direct-key` and `cli-login`
  modes (design decision needed: the warden/spend features depend on the
  gateway).
- **5.7** smaller items: pull policy for rebuilt dev tags; control center
  crash-restarts once waiting for its TLS cert (should wait, not exit);
  gateway env overrides; capacity warning in the wizard; OpenShift Route /
  OVN pod CIDR preflight noise.
- Environment notes from the report: CRC on Apple silicon advertises SVE2 and
  OpenSSL crashes (`OPENSSL_armcap=0` fixes LiteLLM); document for arm64.

## Other known gaps (smaller)

- The platform namespace's default-deny ingress blocks any in-cluster model
  provider (the dev mock needed an explicit policy in `hack/dev/extras.yaml`).
  Decide whether the operator should allow the gateway to reach providers it
  is configured with.
- Forgejo OIDC: turning `oidcLogin` off doesn't remove the auth source.
- cert-manager certificate covers the control center only, not `forge.<host>`.
- A secret registered after a log line is written isn't redacted in that line.
- `workspace diskLow` comes from the RunnerPool's condition, not the design's
  15% rule.
- Docker Hub rate limits (429) hit image builds; `mirror.gcr.io/library/<img>`
  works as a base-image mirror (tag it locally).

## Environment (this is what cost time; read before building)

- Cloud container, 15 GB RAM, ~40 GB disk quota. **The disk fills.** A room
  image build peaks ~20 GB. Run `docker builder prune -af` and
  `go clean -cache` between builds; don't build the room image with the dev
  cluster up if under ~20 GB free.
- Go: `GOTOOLCHAIN=go1.26.9`, `GOFLAGS=-p=2`. The installed golangci-lint is
  built with Go 1.25 and refuses; build one:
  `GOBIN=/tmp/glbin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.
- envtest: `KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.2-linux-amd64` (run
  `make envtest` once if missing). Tools (`helm`, `k3d`, `kubectl`) via
  `make tools` into `bin/tools`; `controller-gen v0.23.0`, `buf` and the
  protoc plugins via `go install` into `$(go env GOPATH)/bin`.
- Outbound traffic goes through a TLS-intercepting proxy whose port can
  change. Docker, buildx and containerd need it: a small relay
  (`/tmp/claude-0/k3stest/relay.py`, lost with the container; rewrite it:
  listen on 0.0.0.0:3128, forward to the proxy from
  `curl $HTTPS_PROXY/__agentproxy/status`, rediscover the port on refusal),
  `dockerd` started with `HTTPS_PROXY=http://127.0.0.1:3128`, image builds
  with `BUILD_CA=/root/.ccr/ca-bundle.crt`, and the k3d nodes with
  `DEV_K3D_ARGS="-e CONTAINERD_HTTPS_PROXY=http://172.17.0.1:3128@server:* -e CONTAINERD_NO_PROXY=... --volume /root/.ccr/ca-bundle.crt:/etc/ssl/certs/ca-certificates.crt@server:*"`.
  `hack/dev/up.sh` handles the oom_score_adj problem itself.
  Start `dockerd` detached (`setsid nohup dockerd ... & disown`); it dies
  with the shell otherwise. The worker restarts and takes it down.
- Pods run in k3d here only because of the `restrict_oom_score_adj` workaround.
- The GitHub MCP tools are the only GitHub access (no `gh`). Workflow runs on
  the branch are dispatched with `actions_run_trigger run_workflow`.
- Commit trailers: `Co-Authored-By: Claude <model> <noreply@anthropic.com>`
  and `Claude-Session: <url>` as the session reminder specifies.

## Suggested order

1. Read CI run 25's result (or dispatch a new run on the branch head), fix.
2. Build the control-center image and check the UI; rebuild the room image
   (bug 17); bring up the dev cluster; run `make e2e` locally.
3. OpenShift quick wins: 5.5, 3, then 6, 5.7's cert wait, 2/1/4.
4. OpenShift support proper (5.2, 5.3, 5.4), then release publishing (5.1).
5. Merge to `main` when the suite is green.
