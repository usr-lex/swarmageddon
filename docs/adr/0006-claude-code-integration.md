# 0006. Claude Code integration

- Status: Proposed
- Date: 2026-10-16
- Verified against: Claude Code 2.1.301 (docs at code.claude.com, and the
  shipped bundle where the docs are silent)

## Context

The room supervisor (Go) must, for a Claude Code room:

- start the CLI with no one attached, and resume the last session after a
  restart
- pre-answer every dialog that would otherwise block it
- deliver messages so that one to a busy agent arrives within its current
  turn, and one to an idle agent starts a turn, with no timing heuristics
- report status (working, waiting on a human, idle) and usage
- route approval requests to the control center
- let a person watch and take over the live session

remote-labs did messaging by sharing `~/.claude/sessions` and the socket
folder between containers, so that Claude's own `SendMessage` and `ListAgents`
crossed rooms. Rooms can't share folders here (each is isolated), and
delivery has to go through our server's policy.

## What is documented and what is not

Documented (code.claude.com/docs/en):

- **Cross-session messaging.** "The receiving Claude reads the message between
  tool calls during an active turn … When the receiving session is idle, Claude
  Code starts a new turn with the message." The session's inbox socket is
  exported to hooks as `CLAUDE_CODE_MESSAGING_SOCKET`, along with a per-session
  `CLAUDE_CODE_MESSAGING_TOKEN`. A process can authenticate with
  `{"type":"auth","token":…}` as its first line. `crossSessionInbound`
  (`accept`, `hold` or `refuse`) is a setting that managed settings can
  enforce.
- **Hooks**
  - `PermissionRequest` can answer `allow` or `deny`. The `timeout` defaults
    to 600 s, can be set higher, and has no documented maximum.
  - `Notification` (with `notification_type`, including
    `permission_prompt` and `idle_prompt`), `SessionStart`,
    `UserPromptSubmit`, `PreToolUse`, `PostToolUse` and `Stop`.
  - HTTP hooks exist.
- **Managed settings and instructions.** `/etc/claude-code/managed-settings.json`
  (plus `managed-settings.d/`) and `/etc/claude-code/CLAUDE.md` take
  precedence over everything else. `allowManagedHooksOnly`,
  `allowManagedPermissionRulesOnly` and
  `permissions.disableBypassPermissionsMode` exist.
- **Trust dialogs.** `projects[<dir>].hasTrustDialogAccepted` in
  `~/.claude.json`, and `skipDangerousModePermissionPrompt` for bypass mode.
- **Resuming and naming.** `--continue`, `--resume`, `--name`, `--session-id`.
  Transcripts live under `~/.claude/projects/`.
- **Gateways.** `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`.
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` turns off auto-update, telemetry
  and feature flags. Same-machine messaging works in gateway mode (2.1.248
  and later).
- **OpenTelemetry metrics.** `claude_code.cost.usage` and
  `claude_code.token.usage`.

Not documented (read from the 2.1.301 bundle, can change in any release):

- **The message frame.** One JSON object per line,
  `{"type":"user","message":{"role":"user","content":…},"msg_id":…,"priority":"next"}`,
  where `next` means between tool calls, `now` interrupts, and `later` waits
  for the turn to end.
- **The cross-session envelope.** `<cross-session-message from=… from-name=…
  from-mode=…>`.
- **More onboarding flags.** `hasCompletedOnboarding`, and the
  `resumeReturnDismissed` flag that skips the resume-from-summary dialog.
- **The permission dialog.** In the TUI, the `PermissionRequest` hook runs
  alongside the on-screen dialog, and the first answer wins.

Channels (MCP servers pushing into a session) would be the cleaner, documented
way in, but they are a research preview gated on Anthropic sign-in and a
server-side flag. Behind our model gateway they are not available. Not used.

## Decision

**Process layout.** The supervisor runs Claude Code as the agent user in a
tmux session named `agent`, with `--name <room>`, the room's permission mode,
and `--continue` when a transcript exists for the work folder (resume by
explicit `--resume <id>` when the supervisor recorded one). The CLI version is
pinned in the image; auto-update is off
(`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_AUTOUPDATER=1`).

**Pre-answered dialogs.** Before each start the supervisor sets, in
`~/.claude.json` and `~/.claude/settings.json`:

- `hasCompletedOnboarding`
- `hasTrustDialogAccepted` for the work folder's repository root
- `resumeReturnDismissed`
- `skipDangerousModePermissionPrompt`, only when the room runs with bypassed
  permissions

The e2e suite starts a fresh room and fails if the TUI shows any dialog (it
reads the tmux pane).

**Managed settings, written by the operator.** The operator renders
`/etc/claude-code/managed-settings.json` and `/etc/claude-code/CLAUDE.md` from
the room's `RoleTemplate` into a ConfigMap, mounted read-only. They contain:

- The persona's permission allow and deny rules.
- Our hooks, all pointing at `swarm hook <event>` (a small binary that talks
  to the supervisor over a Unix socket only the agent user can reach):
  - `SessionStart`: reports the session ID, transcript path, and
    `CLAUDE_CODE_MESSAGING_SOCKET` and `CLAUDE_CODE_MESSAGING_TOKEN`.
  - `UserPromptSubmit`, `PreToolUse`, `PostToolUse` and `Stop`: report
    presence (working or idle).
  - `Notification`: `permission_prompt` reports waiting on a human.
  - `PermissionRequest`: routes the approval (below), with `timeout` set to
    the workspace's approval timeout (default 24 h).
- `crossSessionInbound: "accept"`, so our messages are never held behind a
  dialog. Delivery is then gated by our server's policy instead.
- `allowManagedHooksOnly: true`, so an agent can't add hooks that silence
  status. The persona decides whether to also set
  `allowManagedPermissionRulesOnly`.
- `ANTHROPIC_BASE_URL` pointing at the model gateway. The room's gateway key
  arrives as `ANTHROPIC_AUTH_TOKEN` from the room's Secret.

The shared instructions (the room's identity, its team, how to message, `tea`
usage and the role text) go in the managed `CLAUDE.md`, as remote-labs did,
leaving `~/.claude/CLAUDE.md` to the agent.

**Messages in.** The supervisor receives a message from the messaging service,
wraps it with a header naming the sender and reply address (in the same
`<cross-session-message from-name=…>` form Claude uses), and writes one frame
with `priority: "next"` to the session's inbox socket, authenticated with
the session's token. Claude Code's own documented behavior then does the
rest: between tool calls when busy, a new turn when idle. No polling, no
"idle after N seconds". The supervisor acks the message to the server once
the socket write succeeds. If the socket is gone (Claude restarting), the
message stays unacked and is redelivered.

**Messages out, and seeing other agents.** Agents use an MCP server built
into the room, `swarm mcp`, registered in the managed settings. Its tools
are `send_message`, `list_agents`, `whoami` and `set_status`. They go through
the supervisor to the messaging service, which applies policy. Claude's own
`SendMessage` and `ListAgents` only see sessions in the same room; the role
text says to use the `swarm` tools.

**Approvals.** The `PermissionRequest` hook sends the request (tool, input,
suggestions) to the supervisor, which publishes it on the workspace's
approvals stream and blocks until an answer comes back. The answer can come
from the control center, a phone, or an automatic rule. Meanwhile the dialog
is also on screen in tmux, so a person attached to the terminal can answer
there; whichever comes first wins, and the supervisor withdraws the other.
If the hook times out, the dialog stays in the TUI, the room stays "waiting
on a human", and the approval can still be answered from the terminal.

**Usage.** Spend comes from the model gateway per room key (authoritative,
same for both CLIs). The CLI's own OpenTelemetry metrics are exported to the
supervisor (OTLP on localhost) for live token counts and context size in the
Activity tab.

**Pinned versions and a canary.** A protocol test runs the pinned Claude
Code against a mock model endpoint and checks:

- the inbox frame is accepted, both mid-turn and from idle
- the hook payloads match our structs
- managed settings load
- no dialog appears on first start or on resume

Renovate's version bumps run it first; the image does not move to a new
version until it passes.

## Consequences

- Message delivery relies on a documented behavior (when a message arrives)
  over an undocumented frame format. The canary is what keeps that safe.
- Cross-room messaging no longer depends on shared folders or PID ranges.
  remote-labs' `ROOM_INDEX` PID offsetting and shared session volumes go
  away.
- A person can always attach to the real TUI (`tmux attach`). Nothing about
  the session is hidden behind our own UI.
- Claude Code refuses `--dangerously-skip-permissions` as root, so the agent
  runs as a normal user (uid 1000) with optional `sudo` inside the room.
