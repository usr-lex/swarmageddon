# 0007. ore integration through its app-server

- Status: Proposed
- Date: 2026-10-16
- Verified against: ore 1.161.0 (Codex rust-v0.161.0 base), by reading
  github.com/ore-cli/ore and by running the linux-x64 binary against a mock
  Responses API server

## Context

ore needs the same things as Claude Code (ADR 0006): unattended start and
resume, no blocking dialogs, timely message delivery without heuristics,
status, approvals, usage, and a live session a person can take over.

remote-labs bridged ore with agent-bus: an MCP server and hooks inside each ore
process, an on-disk inbox handed over in `PostToolUse` hooks, and a nudge
through `ore queue` when the session had looked idle for 20 seconds. Its own
notes say the idle detection was the most fragile part, and that a session
which had never had a turn could not be woken.

## What we verified

- **Transports.** `ore app-server --listen unix://` serves the JSON-RPC
  app-server over WebSocket framing on a Unix socket (also `stdio://`, and
  `ws://` with token auth). `generate-json-schema` and `generate-ts` emit the
  protocol schema.
- **Methods.**
  - `initialize`, `thread/start`, `thread/resume`, `thread/list`,
    `turn/start`.
  - **`turn/steer {threadId, input, expectedTurnId}`** injects input into
    the running turn, and fails with "no active turn to steer" when idle.
  - `turn/interrupt`.
- **Events.** `turn/started`, `turn/completed`, `item/*`,
  `thread/status/changed` (including `waitingOnApproval`),
  `thread/tokenUsage/updated` and `serverRequest/resolved`.
- **Approvals arrive as requests to clients.**
  `item/commandExecution/requestApproval` and `item/fileChange/requestApproval`
  take `accept`, `acceptForSession`, `decline` or `cancel`. Every client
  subscribed to the thread gets the request, and the first answer wins.
- **The TUI can attach to the same thread.** `ore resume <id> --remote unix://…`
  in a terminal showed the history, then the supervisor's turn live, including
  the approval prompt, which closed when the supervisor answered.
- **Policy files.**
  - Policy that can't be overridden lives in `/etc/codex/requirements.toml`.
    In this fork the path is still `codex`. It holds allowed approval
    policies, reviewers and sandbox modes, model providers, MCP servers,
    rules and hooks.
  - Exec policy rules (`prefix_rule(pattern, decision=allow|prompt|forbidden)`)
    load from `rules/*.rules` next to each config layer.
  - MCP servers take `enabled_tools` and `disabled_tools`.
  - `approvals_reviewer` is `user` or `auto_review`.
- **Gotchas.**
  - `approval_policy = "untrusted"` is rejected in 1.161; use `on-request`
    with `prompt` rules.
  - `app-server` does not accept `-p`, so profiles go in through the config
    layers or `thread/start`'s `config` and `developerInstructions`.
  - A thread with no turns has no rollout file and can't be resumed by a
    second client.
  - Unless `CODEX_HOME` is set, this fork also layers `~/.codex/config.toml`.
  - A turn in flight when the app-server dies is lost, along with its
    pending approval.

## Decision

**The supervisor owns the app-server.** It runs `ore app-server --listen
unix:///run/swarm/ore.sock` as the agent user, with `CODEX_HOME=~/.ore` (so no
stray `~/.codex` layer), and is its first client.

- **Start and resume.** On start, `thread/list` finds the latest thread for
  the work folder and `thread/resume` reopens it; otherwise `thread/start`.
  Either way the supervisor gives the agent a first turn (the role's
  "you've just started: check messages and pick up your open work" prompt).
  That first turn also gives the thread its rollout file, so the TUI can
  attach. Trust for the work folder is written to `~/.ore/config.toml` first,
  as remote-labs did.
- **The person's view.** tmux runs `ore resume <thread> --remote
  unix:///run/swarm/ore.sock`, so attaching to the room's Agent tab shows the
  real ore TUI on the same thread. If the TUI exits, the supervisor restarts
  it; the session itself lives in the app-server.
- **Messages in, event-driven.** The supervisor tracks the thread's state
  from app-server events.
  - When a turn is active, it sends `turn/steer` with that turn's ID.
  - When idle, it sends `turn/start`.
  - If a steer races the end of a turn ("no active turn"), it retries
    as `turn/start`.

  The message is acked to the server when the call succeeds. This replaces
  agent-bus's inbox files, its `PostToolUse` handoff, its 20-second idle
  timer and `ore queue`.
- **Messages out.** The same `swarm mcp` server as for Claude Code (ADR
  0006), declared in `/etc/codex/requirements.toml` so the agent can't remove
  it.
- **Approvals.** The supervisor answers `requestApproval` requests from the
  approvals stream (control center, phone or automatic rule). A person in the
  TUI can answer first, and `serverRequest/resolved` tells the supervisor to
  withdraw its copy.
- **Persona.** The operator renders the `RoleTemplate`'s ore section into a
  ConfigMap mounted read-only at `/etc/codex`. It holds:
  - `requirements.toml`: the allowed approval policy and reviewer
    (`approvals_reviewer = "user"` by default), the model provider (the
    gateway), and the MCP servers with their tool allowlists.
  - `config.toml`: the persona's defaults.
  - `rules/*.rules`: the execpolicy rules.

  The role text goes in as `developerInstructions` on `thread/start` and in
  the generated block of `~/.ore/AGENTS.md`, as remote-labs did.
- **Status and usage.**
  - Presence:
    - working when a turn is active
    - waiting on a human while `waitingOnApproval` is set or an approval is
      outstanding
    - idle otherwise
  - Tokens come from `thread/tokenUsage/updated`; spend from the gateway.
- **Model access.** A `[model_providers.gateway]` table with `env_key`
  pointing at the room's gateway key and `base_url` at LiteLLM. The model
  comes from the `Room` spec.
- **Pinned version and a canary.** The supervisor's protocol structs are
  generated from `ore app-server generate-json-schema` for the pinned
  version. CI regenerates the schema for each new version and fails on any
  change to the methods and events we use. An e2e canary runs the real binary
  against the mock model and covers:
  - steering mid-turn
  - starting a turn from idle
  - an approval round trip from both clients
  - resume after a restart
  - the TUI attaching with `--remote`

## Consequences

- No timing heuristics remain for ore: the app-server tells us exactly when
  a turn starts and ends.
- The supervisor is a hard dependency of an ore room's session. If it
  crashes, the app-server (its child) goes with it and the in-flight turn is
  lost. The thread resumes on restart. To keep this rare, the supervisor
  stays small, and its restart is what resumes the thread.
- We depend on the app-server protocol, which is internal and changes between
  versions; the generated schema diff is our early warning.
