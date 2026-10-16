# 0011. The warden: a small model that keeps agents from going rogue

- Status: Accepted
- Date: 2026-10-16
- Prior art: turnstone (turnstonelabs/turnstone), Claude Code's auto-mode
  classifier, ore's guardian (`approvals_reviewer = "auto_review"`)

## Context

The owner wants a small model watching each agent, to stop it going rogue:
for example force-pushing, wandering outside its role, looping, or burning
budget. We call it the **warden**, to keep it apart from the room
supervisor, which is the room's process manager.

Facts that shape the design:

- **Inside the room, nothing is a hard stop.** The agent may be root in its
  room, and running with permissions bypassed is supported. It can kill the
  supervisor, start a CLI without our hooks, or read any key the room
  holds. Anything that runs in the room is a guardrail against an honest
  but mistaken agent, which is the common "rogue" case, not against a
  hostile one.
- **The gateway is the one vantage point the agent can't tamper with.**
  Every model request carries the whole transcript, tool calls and tool
  results included, and the room can only reach models through it.
- **Existing judges:**
  - Claude Code's auto-mode classifier judges each action from user messages
    and tool calls, falling back to prompting after 3 consecutive or 20
    total blocks.
  - ore's guardian reviews approval requests (allow or deny with risk and a
    reason), and interrupts after 3 consecutive denials. It only runs when
    approvals are on, so never in bypass rooms.
- **Turnstone's rules we adopt:**
  - Rules first, then a model judge.
  - **A judge only tightens:** it can deny or escalate, never allow
    something the rules would stop.
  - **Errors and timeouts go to a human,** never to "allow".
  - The agent never sees text the judge wrote, except a fixed notice.
  - Every verdict is stored next to the human's decision, so rules can be
    tuned.

## Decision

The warden has four layers. A workspace chooses the mode (`off`, `monitor`
or `gate`, default `monitor`) and the model (any small model on the
gateway); a Room can override both.

1. **Rules, in the room** (both CLIs, microseconds). The supervisor checks
   each side-effecting tool call against a fixed rule table:
   - pipe-to-shell, and `--force` pushes or deletion of protected branches
   - touching `/run/swarm/identity`, `/etc/claude-code`, `/etc/codex` or
     transcripts, or killing the supervisor
   - cloud metadata addresses, scanning the cluster's ranges, and
     exfiltration-shaped uploads (`curl -d @file` to unknown hosts)
   - merging one's own PR, and disabling hooks or settings

   Each rule says allow, deny or escalate. A persona can add rules.
2. **A synchronous judge, in the room, in `gate` mode only.** It looks at
   side-effecting calls the rules didn't decide (shell commands outside the
   work folder's usual build/test commands, MCP writes, network tools).
   - The supervisor asks the warden model on a dedicated, budget-capped key
     (`room-<name>-warden`).
   - **Claude:** through the `PreToolUse` hook. `deny` returns the reason to
     the agent; `escalate` turns into `ask`, which reaches our
     `PermissionRequest` route and the approvals inbox.
   - **ore:** through the approval requests the supervisor already answers
     as the app-server's client; execpolicy `prompt` rules make sure risky
     commands reach it.
   - The judge can only deny or escalate. Timeouts and errors escalate.
   - Parallel tool calls are judged as one batch.
3. **Trajectory review, in the room** (both CLIs, asynchronous, `monitor`
   and `gate`). After each turn, and every N tool calls, the supervisor
   sends the warden model a compact summary of what happened:
   - the task
   - recent tool calls
   - tool-call rate and repeated failures
   - diff size and paths
   - pushes, PRs and messages sent
   - tokens against budget

   It asks whether the trajectory is drifting from the task, overreaching
   its role, looping or escalating. On `high`, the supervisor:
   - switches the room to `gate` mode
   - reports the verdict
   - with `interruptOnHigh`, interrupts the turn (Claude: an inbox frame
     with `priority: "now"`; ore: `turn/interrupt`) and sets presence to
     waiting on a human
4. **The gateway guard, outside the room** (tamper-proof). A small Go
   service, `warden`, sits in front of LiteLLM: the `gateway` Service points
   at it, and it proxies to LiteLLM unchanged, streaming included.
   - **Identity:** each request's virtual key maps to a room (the operator
     keeps a table of key hashes).
   - **Scoring:** asynchronously, the same trajectory review as layer 3,
     but on the transcript the request actually carries. So a room that
     killed its supervisor, or runs a CLI without our hooks, is still
     watched.
   - **Quarantine:** it refuses the room's model requests with a clear error
     (the agent stops thinking, its tools stop being called), and asks the
     operator to quarantine the room. The operator applies a NetworkPolicy
     that leaves only the gateway, and pauses its forge token, until a
     person releases it from the control center.
   - **Reporting:** it reports verdicts to messaging.

   It quarantines on `critical` verdicts, or on `high` when the workspace
   says so.

**Verdicts** (layers 2 to 4) go to messaging through a new `ReportVerdict`
call. Each records the room, layer, severity, rule or reason, the evidence,
and whether it came from the room (a self-report) or the gateway. They land
in the audit stream (`audit.<ws>.verdict`). The control center shows them in
the room's Activity tab and on the observatory, notifies people (new event
`wardenFlag`), and records each human decision next to the verdict.

**Claude's auto mode** stays available as a persona choice
(`permissionMode: auto`); its fallbacks reach the same approvals route. ore's
guardian can be turned on per persona (`approvalsReviewer: auto_review`).
Both are optional extras, not the warden.

## Consequences

- An honest agent that drifts is caught early in the room. A hostile or
  broken one is caught at the gateway, where it can't interfere.
- Synchronous judging costs about 1–5 s per gated call, so `gate` mode
  judges only side-effecting calls the rules didn't decide.
- Warden spend is separate (its own keys), and small models keep it cheap.
- The gateway guard makes LiteLLM's availability depend on one more hop. The
  proxy is stateless and runs two replicas. If scoring fails, requests still
  pass (monitoring degrades, it doesn't block); only quarantine blocks.
- Thresholds will need tuning; the stored verdict/decision pairs are there
  for that.
