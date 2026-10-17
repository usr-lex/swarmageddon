# Claude Code 2.1.301: integration research for the K8s room supervisor

Sources: the official docs at https://code.claude.com/docs/en/*.md, fetched 2026-10-16 and saved under
`/tmp/claude-0/-home-user/e6902579-249c-5c06-9dd7-61017a37495c/scratchpad/docs/`, and the
`@anthropic-ai/claude-code@2.1.301` package installed into `/tmp/claude-code-research`. The npm package
ships a Bun-compiled native binary (`node_modules/@anthropic-ai/claude-code-linux-x64/claude`, 268 MB) with
the JS embedded. I extracted its strings to `scratchpad/bundle.txt` to inspect it.

Legend:
- **[DOC]** is quoted or paraphrased from the official docs. The URL is given.
- **[BUNDLE]** is inferred from the minified 2.1.301 code. It is undocumented and can change in any release.

---

## 1. Cross-session messaging and the inbox socket

Page: https://code.claude.com/docs/en/cross-session-messaging. It requires v2.1.224+ on Linux. "When a session meets the requirements, messaging is on with nothing to enable."

### Documented
- **Delivery semantics [DOC]:** "The receiving Claude reads the message between tool calls during an active turn, so a running tool is never interrupted. When the receiving session is idle, Claude Code starts a new turn with the message."
  - So a message to an **idle session starts a turn**.
  - A message to a **busy session arrives mid-turn, between tool calls**.
- **Socket [DOC]:** "Claude Code binds an inbox socket for each session with cross-session messaging enabled ... a Unix domain socket on macOS and Linux".
  - The path appears in `/status` under "Peer address" with a `uds:` prefix.
  - It is exported to hooks and Bash as `CLAUDE_CODE_MESSAGING_SOCKET`: "In a session that starts with messaging on, Claude Code exports the variable before any hook runs, including `SessionStart`."
  - Example address from https://code.claude.com/docs/en/errors: `uds:/tmp/cc-socks/13605.sock`.
  - Fallback directory: "refuses to create the socket in a directory it can't accept ... and uses a private per-user directory, `/tmp/cc-socks-<uid>`, instead."
- **Auth line [DOC]:** "Claude Code exports a per-session token as `CLAUDE_CODE_MESSAGING_TOKEN`. A script posting to its own session's socket can send `{"type":"auth","token":"<token>"}` as the first line of its connection".
  - On Linux "the line is optional. Claude Code accepts a connection with or without it."
  - "Claude Code closes a connection that hasn't sent a complete line within 30 seconds".
  - "On macOS and Linux, Claude Code restricts the socket to your operating-system user."
- **Own-child messages [DOC]:** "when no `crossSessionInbound` value applies, Claude Code delivers a message it verifies came from the session's own child processes".
  - Note: "in a container where Claude Code runs as process ID 1 it has no process evidence at all". In that case the token in the auth line is the proof.
  - "When Claude Code can verify neither way, it treats the message like any other that asserts no permission class, so a session that bypasses permission prompts holds it for your approval."
- **Inbound policy [DOC]:** `crossSessionInbound` takes `accept`, `hold` or `refuse` (https://code.claude.com/docs/en/settings-reference#crosssessioninbound).
  - "Claude Code reads managed settings first, then the `--settings` flag, then user settings, and applies the first value found." Project or local settings can only make it stricter.
  - Default with no value set: a prompting receiver delivers, unless the sender says it bypasses permissions. A `bypassPermissions` receiver holds each message for approval and opens an approval dialog, which expires after `dialogExpiry` (5m by default).
  - "To let a `-p` worker take messages unattended, start it with `crossSessionInbound` set to `accept`".
- **What a message can't do [DOC]:** it "can't approve anything", and "Commands don't run: a command in the message's text, such as `/compact`, arrives as plain text."
- **Limits [DOC]:**
  - Same-machine messages are capped at about 1M characters.
  - Bursts are refused at the sender.
  - The receiver "queues at most 50 accepted messages" and holds at most 100.
  - Messages are rate-limited per sender, and identical repeats are dropped.
- **Containers [DOC]:** "A session inside a container and a session on the host can't reach each other. Two sessions inside the same container can still message each other".
- **Bare mode [DOC]:** `--bare` doesn't bind the socket. `-p` sessions do bind it.
- **Feature flags [DOC]:** same-machine messaging works with feature-flag fetching off and on third-party providers from v2.1.248 (https://code.claude.com/docs/en/env-vars#features-that-need-feature-flag-fetching).
- **Turning it off [DOC]:** deny `SendMessage` and `ListAgents`, plus set `crossSessionInbound: "refuse"`, in managed settings.
- **Session records [DOC]:** `~/.claude/sessions/` "holds one small file per running session, used to detect concurrent sessions and crashes" (https://code.claude.com/docs/en/claude-directory). The directory moves with `CLAUDE_CONFIG_DIR`: "All settings, session history, and plugins are stored under this path."
- **Not documented:** the JSON frame format beyond the auth line, priority values, `from`, `from_mode`, and the `peer_message_status` frames.

### From the bundle [BUNDLE]
- **Socket path:**
  - The default is `${XDG_RUNTIME_DIR || tmpdir}/cc-socks/<pid>.sock`. tmpdir honours `CLAUDE_CODE_TMPDIR`.
  - If that path is longer than 103 bytes, it falls back to `/tmp/cc-socks-<uid>/<pid>.sock`.
  - A hidden flag, `--messaging-socket-path <path>`, sets an explicit path. Help text: "Cross-session messaging server path ... (defaults to an auto-generated path)".
  - Restricted sessions ignore it.
- **Session registry:** files are `<configDir>/sessions/<pid>.json`. Fields include:
  - `messagingSocketPath` (the value of `CLAUDE_CODE_MESSAGING_SOCKET`)
  - `cwd`, `startedAt`, `procStart`, `name`, `nameSource`, `kind`, `entrypoint`, `tmux`, `peerProtocol`, `peerFeatures`
- **Wire format:** newline-delimited JSON. One line may be at most 1,048,576 chars; a longer line drops the connection. The debug log prints this example:
  ```
  { echo '{"type":"auth","token":"'"$CLAUDE_CODE_MESSAGING_TOKEN"'"}'; echo '{"type":"user","message":{"role":"user","content":"hello"}}'; } | socat - UNIX-CONNECT:<sock>
  ```
- **User frame fields:**
  - `type:"user"` and `message.content`. The content must be a non-empty string.
  - `uuid`, optional.
  - `msg_id`, optional. It is echoed in receipts.
  - `from`, optional. A reply address such as `"uds:/tmp/cc-socks/123.sock"`; it defaults to `"unknown"`.
  - `priority`: `"now" | "next" | "later"`, default `"next"`.
  - `session_id`, optional. If present it must equal the receiver's session ID, or the frame is dropped.
  - `file_attachments` and `from_plugin`, optional.
  - The message is queued with `skipSlashCommands: true` and `isMeta: true`.
- **Priority semantics:**
  - `now`: the queue subscriber calls `abortController.abort("interrupt")`, so it **interrupts the running turn**.
  - `next`: delivered mid-turn between tool calls, or starts a turn when idle.
  - `later`: waits for the turn to end.
- **Envelope and from-mode:**
  - The content may be wrapped in an envelope: `<cross-session-message from="uds:..." from-session="..." hop-chain="..." from-name="..." from-mode="bypass|prompting" from-plugin="...">\nBODY\n</cross-session-message>`.
  - The envelope must re-serialise byte-for-byte to be recognised.
  - `from-mode` is the sender's claimed permission class (`bypass` or `prompting`), and it feeds the default inbound policy.
  - `notify_when_idle` and `peer_idle_notice` control frames also carry a `from_mode` field.
  - Practical advice: send no envelope, or `from-mode="prompting"`, and set `crossSessionInbound: "accept"`.
- **Control frames:** `{"type":"control","action":...}`. Actions:
  - `rename`, with `name`
  - `notify_when_idle`, with `from`, `msg_id` and `from_mode`
  - `peer_idle_notice`, with `orig_msg_id`, `state`, `finished_at`, `detail`, `from` and `from_mode`
  - `artifact_replies_yielded`
  - `peer_message_status`
- **Receipts:** the receiver sends `peer_message_status` back to the sender's `from` socket.
  - Status is one of `held`, `denied`, `expired`, `delivered`, `refused` or `dropped`.
  - Other fields: `orig_msg_id`, `reason`, `from`, `status_detail`, and `drop_reason` plus `dropped_msg_ids` for drops.
  - A reply address is used only if it is a `.sock` in the **same directory as the receiver's socket**, or a default `cc-socks` directory owned by a verified peer uid.
  - So for the supervisor to get receipts, it should bind its own `.sock` in the same `cc-socks` directory and set `from` to that address.
- **Inbound gate:** it applies the `crossSessionInbound` policy (accept, hold or refuse). Messages that arrive during shutdown are settled as "expired".

### Answer
Yes, an external non-Claude process can deliver a message to a running interactive session. Two conditions apply:
- It must run in the same container and as the same uid.
- It must write a JSON line to `<socket>` (found via `sessions/<pid>.json` or `/status`).

The behaviour then follows the [DOC] semantics above:
- Idle session: a new turn starts.
- Busy session: the message is read between tool calls.
- `priority:"now"` interrupts the turn [BUNDLE].

The supervisor is the parent, not a child of Claude, so it isn't "own-child". Set `crossSessionInbound: "accept"` in managed settings so delivery doesn't depend on the permission-mode heuristic. The frame format is undocumented apart from the auth line and the `{"type":"user","message":{...}}` example in the debug log, so pin the version.

---

## 2. Channels

Pages: https://code.claude.com/docs/en/channels and https://code.claude.com/docs/en/channels-reference.

### Documented
- **Status [DOC]:** "Channels are in research preview. They require Anthropic authentication through claude.ai or a Console API key, and are not available on Amazon Bedrock, Google Cloud's Agent Platform, or Microsoft Foundry. Team and Enterprise organizations must explicitly enable them."
- **Mechanism [DOC]:** a channel is an MCP server that declares `capabilities.experimental['claude/channel']: {}` and emits `notifications/claude/channel` with `content` and `meta`.
  - Claude sees the event as `<channel source="...">`.
  - The session must opt in with `--channels plugin:<name>@<marketplace>`. "Being in `.mcp.json` isn't enough".
- **Allowlist [DOC]:** custom channels aren't on the allowlist during the preview. You need `--dangerously-load-development-channels server:<name>`, which asks for confirmation, or an org `allowedChannelPlugins` list plus `channelsEnabled: true` in managed settings.
  - "Neither `--channels` nor `--dangerously-load-development-channels` appears in `claude --help`".
- **Permission relay [DOC]:** a server with `claude/channel/permission` receives `notifications/claude/channel/permission_request` with `request_id`, `tool_name`, `description` and `input_preview`.
  - It answers with `notifications/claude/channel/permission` carrying `{request_id, behavior: "allow"|"deny"}`.
  - "Both stay live: you can answer in the terminal or on your phone, and Claude Code applies whichever answer arrives first".
  - "Project trust and MCP server consent dialogs don't relay".

### From the bundle [BUNDLE]
- Channels are gated by the GrowthBook flag `tengu_harbor` (code default `false`) and an `allow_channels` policy verdict. Without feature-flag fetching, channels stay disabled.
- They are also disabled when the provider isn't firstParty ("Channels are not available on third-party providers").
- **Gateway caveat [DOC]:** with `ANTHROPIC_AUTH_TOKEN` against a non-Anthropic `ANTHROPIC_BASE_URL`, "Claude Code has no credential to fetch the flags with" (https://code.claude.com/docs/en/env-vars#features-that-need-feature-flag-fetching).
- Conclusion: **channels very likely won't work behind a LiteLLM gateway with a bearer token**, and they're fragile in general (preview, allowlist, dev flag prompt).
- Use the inbox socket for injecting messages, not channels. The permission relay protocol is a useful reference, but you can't rely on it here.

---

## 3. Hooks

Pages: https://code.claude.com/docs/en/hooks and https://code.claude.com/docs/en/hooks-guide.

### General [DOC]
- Hook types are `command`, `http`, `mcp_tool`, `prompt` and `agent`.
- `timeout` is in seconds: "Defaults: 600 for `command`, `http`, and `mcp_tool`; 30 for `prompt`; 60 for `agent`."
  - The default drops to 30 on `UserPromptSubmit`.
  - The docs state **no maximum** timeout.
- A timed-out hook's output is discarded: "on most events a timed-out hook renders no decision".
- `onFailure: "block"` (v2.1.295+) makes a failure or timeout block the action, for `command` and `http` hooks.
- An HTTP hook POSTs the hook input JSON. The response body uses the same JSON output format, so a Go supervisor can serve hooks over HTTP directly.

### PermissionRequest [DOC]
"Runs when Claude Code is about to ask you for permission to use a tool." It matches on the tool name.

**Input** (no `tool_use_id`):
```json
{"session_id":"abc123","transcript_path":"...jsonl","cwd":"...","permission_mode":"default",
 "hook_event_name":"PermissionRequest","tool_name":"Bash",
 "tool_input":{"command":"rm -rf node_modules","description":"Remove node_modules directory"},
 "permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"rm -rf node_modules"}],"behavior":"allow","destination":"localSettings"}]}
```
MCP tools also include `mcp_server`. The common fields `prompt_id`, `scratchpad_dir` and `effort` are present too.

**Output:**
```json
{"hookSpecificOutput":{"hookEventName":"PermissionRequest",
  "decision":{"behavior":"allow"|"deny","updatedInput":{...},"updatedPermissions":[...],"message":"(deny only)","interrupt":true}}}
```
- "Deny and ask rules are still evaluated, so a hook returning `"allow"` doesn't override a matching deny rule".
- In `-p` or headless sessions that can't prompt, "if no hook returns a decision, it denies the tool call".

**Blocking [BUNDLE]:**
- In the interactive TUI, the hooks run in an un-awaited async task **in parallel with the on-screen dialog**. Whichever decides first wins, and the other is torn down.
- So the supervisor's hook can block on an external decision for up to its `timeout` (default 600 s, settable higher; no documented maximum) while the human can still answer in tmux.
- The docs say the same for SDK hosts: "the hooks run alongside your host, and whichever decides first applies".
- If the hook times out, nothing happens and the dialog stays.

### Notification [DOC]
Matchers:
- `permission_prompt`: after about 6 s of no typing
- `idle_prompt`: about 60 s after the response, with no typing
- `auth_success`
- `elicitation_dialog`, `elicitation_url_dialog`, `elicitation_complete`, `elicitation_response`
- `agent_needs_input`, `agent_completed`
- `quota_auto_resume_fired`, `quota_auto_resume_stale`, `quota_auto_resume_disabled`

Input:
```json
{"session_id":"abc123","transcript_path":"...","cwd":"...","hook_event_name":"Notification",
 "message":"Claude needs your permission","title":"Permission needed","notification_type":"permission_prompt"}
```
- "Notification hooks can't block or modify notifications."
- The docs advise using PermissionRequest for an immediate signal.

### Stop [DOC]
- Runs when the main agent finishes. It doesn't run on a user interrupt; API errors fire `StopFailure` instead.
- Input: `stop_hook_active`, `last_assistant_message`, `background_tasks[]`, `session_crons[]`.
- Output: `{"decision":"block","reason":"..."}` keeps Claude working. There is a cap of 8 consecutive continuations (`CLAUDE_CODE_STOP_HOOK_BLOCK_CAP`).

### SessionStart [DOC]
- Matchers: `startup`, `resume`, `clear`, `compact`, `fork`.
- Input: `source`, `model?`, `agent_type?`, `session_title?`. On resume or fork it adds `seconds_since_last_response`, `context_tokens`, `prompt_cache_likely_expired` and `estimated_cache_write_usd`.
- Output: `additionalContext`, `initialUserMessage` (`-p` only), `sessionTitle`, `watchPaths`, `reloadSkills`.
- Only `command` and `mcp_tool` types are allowed, not `http`.
- `CLAUDE_CODE_MESSAGING_SOCKET` is already exported when SessionStart runs, so the hook can report the socket path to the supervisor.

### UserPromptSubmit [DOC]
- It fires on typed prompts and also on cross-session messages, scheduled tasks and background subagent reports.
- Input: `prompt`, `session_title?`.
- Output: `decision: "block"`, `reason`, `additionalContext`, `sessionTitle`, `suppressOriginalPrompt`.

---

## 4. Managed settings

Pages: https://code.claude.com/docs/en/managed-settings, https://code.claude.com/docs/en/settings and https://code.claude.com/docs/en/memory.

- **Paths [DOC]:**
  - Linux settings: `/etc/claude-code/managed-settings.json`, plus a `managed-settings.d/*.json` drop-in directory and `managed-mcp.json` in the same directory.
  - Managed CLAUDE.md: `/etc/claude-code/CLAUDE.md`. "Managed policy CLAUDE.md files cannot be excluded."
- **Precedence [DOC]:** "Managed settings ... Claude Code applies them above every other level, so no user, project, local, or `--settings` value overrides them".
  - The order is managed, then the `--settings` CLI flag, then local project, then shared project, then user.
  - "Lists merge instead of overriding", so a managed `permissions.deny` can't be removed by lower levels.
  - A few security keys accept a *stricter* lower-level value.
- **Lock-down keys [DOC]:**
  - `allowManagedPermissionRulesOnly: true` ignores allow, ask and deny rules from user, project, local and `--settings`, ignores `--allowedTools`, and hides the always-allow options.
  - `allowManagedHooksOnly: true` means only managed hooks run, plus SDK hooks and force-enabled plugin hooks.
  - `permissions.disableBypassPermissionsMode: "disable"`.
  - `crossSessionInbound`, `channelsEnabled` and `allowedChannelPlugins` are also enforceable.
- **Invalid files [DOC]:** an unparseable managed file makes Claude Code refuse to start.
- **Env [DOC]:** the `env` block can also be set in managed settings and wins over the same variable elsewhere, for example the OTEL variables, `ANTHROPIC_BASE_URL` and `CLAUDE_CONFIG_DIR`. Project and local settings can't set `CLAUDE_CONFIG_DIR` or the OTEL exporter variables.
- **Answer:** yes. Put the PermissionRequest/Stop/Notification hooks and the permission rules in `/etc/claude-code/managed-settings.json`, with `allowManagedHooksOnly` and `allowManagedPermissionRulesOnly`. The agent can't override them through its own settings files.
  - However, if the agent can write `/etc/claude-code` itself (root in the container), it can change them. Mount the file read-only and run Claude as non-root.

---

## 5. Pre-accepting onboarding and trust dialogs

- **Workspace trust [DOC]:** set `projects["<abs path>"].hasTrustDialogAccepted: true` in `~/.claude.json`, or `$CLAUDE_CONFIG_DIR/.claude.json` when the config dir is moved. The errors page documents this repeatedly, for example "set projects["/Users/you/project"].hasTrustDialogAccepted: true in /Users/you/.claude.json".
  - Trust is never saved for the home directory, so use a project dir as the cwd.
- **Bypass-mode confirmation [DOC]:** `skipDangerousModePermissionPrompt: true` in user, local or managed settings. "An untrusted repository can't skip the dialog for you."
- **Root [DOC]:** "The CLI rejects this flag [`--dangerously-skip-permissions`] when launched as root". [BUNDLE] The check is bypassed when `IS_SANDBOX=1` is set or `CLAUDE_CODE_BUBBLEWRAP` is set.
- **First-run onboarding [BUNDLE]:** the key is `hasCompletedOnboarding: true` in `~/.claude.json`; it isn't documented on any page. `IS_DEMO=1` also "skips onboarding" [DOC, env-vars] but hides the email and org.
- **API key approval [DOC]:** `ANTHROPIC_API_KEY` triggers a one-time approval prompt in interactive mode. [BUNDLE] The answer is stored as `customApiKeyResponses` in `~/.claude.json`.
  - `ANTHROPIC_AUTH_TOKEN` has no prompt: "With `ANTHROPIC_AUTH_TOKEN`, the variable takes precedence immediately."
  - **So use `ANTHROPIC_AUTH_TOKEN` for LiteLLM.**
- **Resume-from-summary dialog [DOC]** (https://code.claude.com/docs/en/sessions#resume-from-a-summary): "On a Pro or Max plan, when you resume a session that has been inactive for more than about an hour and is over 100,000 tokens". It offers "Don't ask me again".
  - With a gateway token (no subscription) it shouldn't appear.
  - [BUNDLE] "Don't ask me again" persists `resumeReturnDismissed: true` in `~/.claude.json`, and setting that suppresses the dialog.
  - [BUNDLE] The thresholds come from `CLAUDE_CODE_RESUME_THRESHOLD_MINUTES` (default 70) and `CLAUDE_CODE_RESUME_TOKEN_THRESHOLD` (default 100000), and the dialog is also gated by GrowthBook flags.
- **Other dialogs:**
  - The held cross-session message dialog: avoid it with `crossSessionInbound: "accept"`.
  - MCP server approval: `enableAllProjectMcpServers`, or use `--mcp-config --strict-mcp-config`.

---

## 6. Resuming

Pages: https://code.claude.com/docs/en/sessions and https://code.claude.com/docs/en/cli-reference.

- **Flags [DOC]:**
  - `--continue`/`-c`: the most recent session in the cwd. It skips `-p` and SDK sessions.
  - `--resume`/`-r <id|name|abs .jsonl path>`.
  - `--name`/`-n <name>`: the display name. It is resumable via `--resume <name>` and is also the cross-session address name.
  - `--session-id <uuid>`: pins the ID.
  - `--fork-session`.
- **What isn't restored [DOC]:** "Not every configuration flag ... If the session depended on `--mcp-config`, `--settings`, `--plugin-dir`, ... pass them again when you resume".
- **Permission mode [DOC]:** it is restored on a terminal resume, except `bypassPermissions`, which needs a launch flag again.
- **Transcripts [DOC]:** `~/.claude/projects/<cwd with non-alphanumerics replaced by ->/<session-id>.jsonl`.
  - Move them with `CLAUDE_CONFIG_DIR`. Pin the project dir name with `CLAUDE_CODE_PROJECT_DIR_NAME` (v2.1.234+, needs `CLAUDE_CONFIG_DIR`).
  - Default retention is 30 days (`cleanupPeriodDays`). "The entry format is internal". Put the config dir on a PVC.
- **Supervisor pattern:** launch with `--session-id <uuid> --name <room>`, persist the UUID, and relaunch with `--resume <uuid>` plus the same flags.

---

## 7. Usage and spend reporting

Page: https://code.claude.com/docs/en/monitoring-usage.

- **Environment variables [DOC]:**
  - `CLAUDE_CODE_ENABLE_TELEMETRY=1`
  - `OTEL_METRICS_EXPORTER=otlp|prometheus|console|none` and `OTEL_LOGS_EXPORTER=otlp|console|none`
  - `OTEL_EXPORTER_OTLP_PROTOCOL` ("Claude Code has no default protocol")
  - `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS`
  - `OTEL_METRIC_EXPORT_INTERVAL` (60000) and `OTEL_LOGS_EXPORT_INTERVAL` (5000)
  - `OTEL_RESOURCE_ATTRIBUTES`, for tags such as room.id with no spaces
  - `OTEL_METRICS_INCLUDE_SESSION_ID`
  - Opt-in content capture: `OTEL_LOG_USER_PROMPTS` and `OTEL_LOG_TOOL_DETAILS`
- **Locking [DOC]:** set these in the managed `env` to lock them. "Claude Code doesn't pass `OTEL_*` environment variables to the subprocesses it spawns".
- **Metrics [DOC]:**
  - `claude_code.session.count`
  - `claude_code.cost.usage` (USD)
  - `claude_code.token.usage` (attr `type` = input|output|cacheRead|cacheCreation, plus `model`)
  - `claude_code.lines_of_code.count`, `claude_code.commit.count`, `claude_code.pull_request.count`
  - `claude_code.code_edit_tool.decision`, `claude_code.active_time.total`
- **Events [DOC]:** `claude_code.api_request` has `cost_usd`, `cost_usd_micros`, the token counts, `model` and `request_id`. Other events include `user_prompt`, `tool_result`, `tool_decision`, hook events and `permission_mode_changed`.
- **Cost accuracy [DOC]:** cost is an estimate at list price. For real spend, use LiteLLM's per-key spend tracking.
- **Statusline JSON [DOC]** (https://code.claude.com/docs/en/statusline): the script gets these on stdin:
  - `session_id`, `session_name`, `transcript_path`, `model{id,display_name}`, `workspace{...}`
  - `cost{total_cost_usd,total_duration_ms,total_api_duration_ms,total_lines_added,total_lines_removed}`
  - `context_window{total_input_tokens,total_output_tokens,context_window_size,used_percentage,remaining_percentage,current_usage{...}}`
  - `rate_limits{...}`, `prompt_cache{...}`, `effort`, `version`
  - The statusline command could also push these to the supervisor.

---

## 8. LiteLLM gateway, autoupdate and egress

Pages: https://code.claude.com/docs/en/llm-gateway-connect and https://code.claude.com/docs/en/env-vars.

- **Base URL and credential [DOC]:**
  - `ANTHROPIC_BASE_URL=https://litellm...` and `ANTHROPIC_AUTH_TOKEN=<key>`, sent as `Authorization: Bearer`.
  - `ANTHROPIC_API_KEY` is sent as `x-api-key` instead and causes an interactive approval prompt.
  - `apiKeyHelper` (a settings key that names a command) sends the value in both headers. Its output is cached for 5 min (`CLAUDE_CODE_API_KEY_HELPER_TTL_MS`).
- **Extra options [DOC]:**
  - `ANTHROPIC_CUSTOM_HEADERS` (newline-separated `Name: Value`).
  - `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` reads `/v1/models`.
  - Model mapping: `ANTHROPIC_MODEL` and `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU,FABLE}_MODEL`.
- **Egress [DOC]:**
  - `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` disables "auto-updates, telemetry, error reporting, /feedback, release notes ..." and also feature-flag fetching. It doesn't affect your own OTEL exporter.
  - `DISABLE_AUTOUPDATER=1` stops background updates. `DISABLE_UPDATES=1` also blocks `claude update` and `claude install`.
  - `DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING` and `DISABLE_GROWTHBOOK` are also available.
  - WebFetch still calls api.anthropic.com unless `skipWebFetchPreflight: true`.
- **Consequences of gateway mode [DOC]:**
  - No feature flags: "A gateway token with no API key ... Claude Code has no credential to fetch the flags with".
  - So Remote Control, cross-machine messaging and (by inference) channels are unavailable.
  - Same-machine cross-session messaging still works (v2.1.248+).
