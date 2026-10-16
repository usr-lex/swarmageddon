# 0010. Advisors: a `swarm advise` tool for both CLIs

- Status: Accepted
- Date: 2026-10-16
- Research: Claude Code 2.1.301 docs and bundle, ore 1.161.0 source,
  LiteLLM 1.105.1 (notes in the design's research folder)

## Context

The owner wants each room to be able to consult an **advisor**: a stronger
model, or the same one at higher effort, that the agent can ask for a second
opinion. It has read access at most, and its answers stay inside the room.
It must work for both Claude Code and ore.

What exists:

- **Claude Code's `--advisor`** (setting `advisorModel`) is a server-side
  tool (`advisor_20260301`).
  - The main model decides when to call it, and the advisor sees the whole
    transcript, with no tools of its own.
  - It needs feature flags fetched from Anthropic. Our rooms turn those off
    (`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, gateway token), so it is
    off in our rooms. An undocumented variable
    (`CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL=1`) skips the flag check.
  - LiteLLM passes the advisor through to Anthropic, and emulates it for
    other upstreams. Whether that emulation is billed to the room's key is
    unverified.
  - Advice from 5.x advisors comes back encrypted, so we can't audit or show
    it.
- **Claude Code subagents** (`.claude/agents/*.md` with `model`, `effort`,
  `tools`) are genuinely limited to their tool list, even under bypassed
  permissions. But they start from a fresh context, not the transcript.
- **ore** has no advisor feature. Its agent roles can set model and effort,
  but can't be made read-only (`apply_patch` stays).

## Decision

1. **Baseline for both CLIs: a `swarm advise` tool**, served by the room
   supervisor through the `swarm` MCP server.
   - **Input:** the agent's question, optional file paths, and a flag to
     include recent context.
   - **What the advisor sees:**
     - the role text
     - the question
     - the tail of the transcript (Claude: the JSONL path reported by
       `SessionStart`; ore: the thread's items from the app-server),
       trimmed to a budget
     - the named files, read as the agent user from inside the work folder
       only, size-capped
   - **Tools:** at most a read-only file reader (`read_file`, `grep`,
     `list_dir`) confined to the work folder, in a loop of up to 5 turns.
     No shell, no writes, no messaging, no network beyond the gateway call.
   - **Model access:** a dedicated, budget-capped gateway key per room
     (`room-<name>-advisor`), allowed only the advisor model, so advice
     spend shows separately. The key is in the room (the supervisor needs
     it), so a root agent could read it; the budget and model allowlist
     bound what that's worth.
   - **Answers** go back only to the calling session. They are logged by the
     supervisor and shown in the room's Activity tab. A rate limit
     (`maxCallsPerHour`, default 10) stops an agent from leaning on it for
     everything.
2. **Complement for Claude rooms: a managed `advisor` subagent** in
   `/etc/claude-code/.claude/agents/advisor.md`, with the advisor model,
   high effort, and `tools: Read, Grep, Glob`. It's cheap, needs no new
   code, and is useful for "look at these files and tell me what you think".
   It uses the room's normal key.
3. **Opt-in only: Claude's native advisor** (`advisor.native: true`). It
   adds `--advisor <model>` and the experimental variable. It is enabled
   only after the canary proves it works through our pinned LiteLLM and
   bills to the room's key. Its advice can't be audited.

Configuration lives on the `RoleTemplate` (`spec.advisor`: model, effort,
`maxCallsPerHour`, `native`), and a `Room` can override it. No advisor model
means no advisor.

## Consequences

- One tool, one prompt and one audit path for both CLIs. Any model on the
  gateway can be an advisor, including a local one.
- The advisor sees a trimmed tail of the transcript, not all of it. For long
  sessions the supervisor sends the tail plus the agent's own summary of its
  task (its `set_status` text).
- Advice is never forwarded to other rooms, and the advisor has no tools that
  could reach them.
