# Handoff 3: swarmageddon, session 3 (2026-10-19)

Read `handoff/HANDOFF-2.md` first for the environment traps; this file
says what changed since and what is open. Branch: `claude/adoring-wright-ab2awt`
(`main` still untouched at `ae3c040`; no pull request).

## End-to-end suite in CI: the cause, and the fix in flight

CI runs 25 and 28 failed at "rooms run": one room Running, four Pending for
20 minutes, no scheduling event in the dump. The cause: **the CI runner has
2 vCPUs and ~7 GiB** (preflight logged "free: 1465m CPU, 7Gi memory" before
the services). The services take ~950m, the first room 300m, and the next
room no longer fits; the CI step's runner pod (2 CPUs by default) never
could. The FailedScheduling events were cut from the dump because scheduler
events have no `lastTimestamp` (fixed: the dump lists them explicitly).

Fix (commit a5a82ab): operator flag `--dev-overcommit=N` divides rooms' and
runner pods' CPU and memory *requests* (never limits); `hack/dev/values.yaml`
sets 8. **Run 30 was dispatched on it; its result was not read yet.**
Memory may still be tight with six real rooms on 7 GiB. The robust fix is
a 4-vCPU runner (a public repository's standard runner, or a larger runner
for a private one): the owner's decision.

Also fixed on the way: the control-center image's Node stage didn't trust
`BUILD_CA` (npm failed behind the proxy); "VolumesReady: expanding" was
shown for claims that weren't bound yet; the failure dump now describes
nodes and room pods and shows the k3s scheduler's log lines.

## OpenShift first-install report: status

| Item | Status |
|---|---|
| 5.6 web UI in the image | Fixed earlier; image built here and the binary carries the Vite bundle. Not opened in a browser. |
| 5.5 provider key to LiteLLM | Fixed (`platform/providerkeys.go`), envtest |
| 3 ore swarm tool approvals | Fixed: config pre-approves swarm; driver answers `mcpServer/elicitation/request` (swarm: accept for session; others: approvals inbox). **Verified against real ore 1.161** in the canary (`TestOreAsksForSwarmTools`). |
| 6 VNC password | Fixed: x11vnc `-nopw`; operator drops `vnc_password`; UI has no password field |
| 5.7 control center TLS wait | Fixed: waits for the cert instead of exiting |
| 1 Ctrl+C ends the terminal | Fixed: tmux `remain-on-exit` + `respawn-pane -k`; UI reattaches the agent session |
| 4 terminal exit 1 at start | Fixed: attach waits up to 30 s for the session, says so |
| 2 messages steered into a blocked turn | Fixed: held while an approval is pending; 30 s bound on delivery calls. Not changed: "read" still means delivered to the turn. |
| 5.2 DNS policy | Fixed: `operator/clusterdns` finds the DNS Service behind the nameserver (OpenShift: openshift-dns:5353); all four policies use it |
| 5.3 SCCs | Fixed: chart `templates/openshift.yaml` (auto when the API exists, or `openshift.enabled=true`), operator binds per namespace (`operator/openshift`), device plugin SELinux type, probe uses the rooms' exact capabilities |
| 5.4 hidden admission failures | Fixed: conditions carry FailedCreate/FailedScheduling (`operator/stuck`); preflight names admission refusals |
| 5.1 no release | Not a code problem: nothing is tagged, ghcr packages are private. Added `CONTAINER_TOOL=podman`, fixed the chart push path, documented building from source. **Tagging a release and making packages public is the owner's call.** |
| 5.7 gateway env | Fixed: `spec.services.gateway.env` |
| 5.7 capacity warning | Exists already ("Doesn't fit" in the preview); not changed |
| 5.7 preflight noise on OpenShift | Fixed: pod/service ranges from `config.openshift.io` Network; Route hint |
| 7 per-room edit | Fixed: Edit on the room page (plan → preview → apply → update now), no new RPC |
| 8 bypass LiteLLM | **Not started: needs the owner's design decision** (what is lost: spend, warden gateway guard, advisor) |

None of the OpenShift fixes has run on OpenShift.

## New since session 2

- ADR 0012 (Proposed): importing a GitHub organization with `bot-<room>`
  accounts mapped to rooms. Forgejo's migration keeps original authors and
  re-attributes them through linked external logins; there's no API for the
  link. **Five questions for the owner at the end of the ADR.**
- Forgejo: turning `oidcLogin` off now deletes the auth source (Forgejo 17
  can't deactivate one; it refuses to delete one in use, which is reported).

## Not done

- Local `make e2e`: not possible in this container's disk quota (the room
  image build peaks ~20 GB).
- Smaller gaps from HANDOFF-2, now closed: cert-manager also issues
  forge.<hostname>'s certificate; the workspace diskLow follows the
  design's 15% node rule (pool condition NodeDiskLow); in-cluster model
  servers are documented (LiteLLM may reach any namespace; the platform
  namespace's default-deny only blocks servers deployed inside it).
