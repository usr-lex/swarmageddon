# ore (Codex fork) research for the Go room supervisor

## Sources and method

- Source: `git clone --depth 1 https://github.com/ore-cli/ore /tmp/ore-src`. It worked, so I did not need upstream. HEAD is `c47ec83 sync: rust-v0.155.1 -> rust-v0.161.0`. `fork/UPSTREAM` says tag `rust-v0.161.0`, commit `979011409d`. `fork/VERSION` says `1.161.0`. The `main` branch is generated (upstream tag + `delta` series + rebrand substitution).
- npm: `@ore-cli/ore@1.161.0` is only a JS launcher (`bin/codex.js`). The real binary ships in the optional dependency `@ore-cli/ore@1.161.0-linux-x64`, at `vendor/x86_64-unknown-linux-musl/bin/ore`. That is a static musl build, and it runs in this sandbox. `ore --version` prints `ore 1.161.0` / `codex-base: rust-v0.161.0 (979011409d)`.
- I ran the binary against a mock Responses API server (`wire_api="responses"`, `env_key="MOCK_KEY"`). Test scripts are in `scratchpad/scripts/`: `ws_uds.py` (a minimal WebSocket-over-UDS client), `multi_test2.py`, `resume_test.py` and `drive_turn.py`. The generated schema is in `scratchpad/schema/` and `scratchpad/schema-exp/`, and the TS types are in `scratchpad/ts/`.
- **V** = verified by running the binary. **S** = verified by reading the source. **I** = inferred.

---

## 1. App-server

### Transports (V, S)

The command is `ore app-server [--listen URL]`. The parser is `codex-rs/app-server-transport/src/transport/mod.rs:118-150`.

| `--listen` value | What it does |
|---|---|
| `stdio://` (default) | Newline-delimited JSON-RPC on stdin/stdout. Messages omit `"jsonrpc":"2.0"`. Verified with `stdio_test.py`. |
| `unix://` | Listens on `$ORE_HOME/app-server-control/app-server-control.sock` (`transport/mod.rs:53-71`). The path is a symlink to a rendezvous socket under `/tmp/codex-daemon-<uid>/<hash>`, with mode 0600. The framing is **WebSocket over the unix socket**, not raw JSONL (`transport/unix_socket.rs:1,160-203`). Verified: an HTTP Upgrade handshake on the UDS followed by text frames, one JSON-RPC message per frame. |
| `unix://PATH` | Same, at an explicit path. |
| `ws://IP:PORT` | TCP WebSocket. Optional auth: `--ws-auth capability-token\|signed-bearer-token`, `--ws-token-file`, `--ws-token-sha256`, `--ws-shared-secret-file`, `--ws-issuer`, `--ws-audience`. |
| `off` | No listener. |

- `ore app-server proxy [--sock PATH]` pipes raw stdio bytes to the control socket (`cli/src/main.rs:1395-1403`, crate `codex-stdio-to-uds`). It does **not** convert to JSONL, so a client of the proxy still has to speak WebSocket framing.
- **One process serves many clients.** One app-server process accepts many connections, and threads, approvals and the queue are shared between them. Verified with two clients A and B on one socket; details in §3.

### The daemon (V for `--help`, S for the rest)

- `ore app-server daemon {bootstrap,start,restart,update,enable-remote-control,disable-remote-control,stop,version}`. It is a pidfile-backed detached app-server on the same `unix://` default socket. State lives in `$ORE_HOME/app-server-daemon/` (`settings.json`, `app-server.pid`, `daemon.lock`, `loaded-threads.json` for thread recovery after a restart). See `codex-rs/app-server-daemon/README.md`.
- The managed daemon runs from `$ORE_HOME/packages/app-server-daemon/current/bin/codex` and has an auto-updater. **In a pod, skip the daemon commands.** Run `ore app-server --listen unix://` (or `ws://`) as a child process of the supervisor instead (I).
- Feature `daemon_auto_start` is stable and on by default (`ore features list`, V). A plain `ore` TUI launch tries to start or attach to the daemon. The TUI skips it when you pass `--no-daemon`, any `-c`/`--enable`, `--profile`, `--strict-config`, `--oss` or `--dangerously-bypass-hook-trust`, or when `CODEX_EXEC_SERVER_URL` is set (`tui/src/daemon_startup.rs:29-93`, `tui/src/startup_orchestration.rs:189-196,509-560`).

### Protocol schema (V)

- Rust definitions live in the `codex-rs/app-server-protocol` crate:
  - the method tables are in `src/protocol/common.rs` (client requests from about line 547, server requests from about line 1774, server notifications from about line 1950);
  - the types are in `src/protocol/v2/{thread,turn,item,shared}.rs`.
- Generators:
  - `ore app-server generate-json-schema --out DIR [--experimental]`
  - `ore app-server generate-ts --out DIR [--experimental]`

  The stable bundle has 104 client methods, 10 server requests and 84 notifications. Experimental-only methods appear only with `--experimental`. At runtime they also require `initialize.params.capabilities.experimentalApi = true`. I tested with it set to true. That experimental methods fail without it is inferred from the `#[experimental]` gating.
- Handshake: request `initialize {clientInfo:{name,version}, capabilities:{experimentalApi:true}}`, then send the notification `initialized`.

### Methods relevant to the supervisor

All paths below are in `common.rs` unless another file is named.

**Start or resume a thread**

- `thread/start` (`common.rs:551`). The params live in `v2/thread.rs:62` and include:
  - `cwd`, `model`, `modelProvider`
  - `approvalPolicy`, `approvalsReviewer`, `sandbox`
  - `config` (a map of per-thread config overrides)
  - `baseInstructions`, `developerInstructions`
  - `ephemeral`
  - `dynamicTools` (experimental)

  Response: `{thread:{id,…}, model, …}`. The notification `thread/started` follows. (V)
- `thread/resume` (`common.rs:557`, `thread.rs:354`) takes `{threadId, [overrides…], excludeTurns}`. If the thread is already loaded, the call joins it and subscribes this connection. If it is not loaded, the call loads it from disk. (V)
  - Gotcha (V): a thread that has had no turn yet has no rollout file, so resuming it from a second client fails with `no rollout found for thread id`.
  - Pending approvals are replayed to a connection that resumes the thread (`outgoing_message.rs` `replay_requests_to_connection_for_thread`, S).
- Related methods: `thread/fork`, `thread/read`, `thread/turns/list`, `thread/items/list`, `thread/name/set`, `thread/archive`, `thread/unsubscribe`, `thread/loaded/list`, `thread/compact/start`, `thread/metadata/update`.

**Start a turn**

- `turn/start` (`common.rs:1038`, `v2/turn.rs:167`) takes `{threadId, input:[{type:"text",text}], …per-turn overrides…}` and returns `{turn:{id,status:"inProgress"}}`.
- **If a turn is already running, `turn/start` steers it.** It calls `start_or_steer_turn`, it does not queue (`app-server/src/request_processors/turn_processor.rs:652-675`, `core/src/codex_thread.rs:397`). Verified: the response carried the active turn id, and the text landed in that same turn's next model request.

**Inject input into a running turn**

- **`turn/steer` exists and is stable** (`common.rs:1050`, `turn.rs:293`). It takes `{threadId, input, expectedTurnId}` and returns `{turnId}`.
- If no turn is running it returns error -32600 `"no active turn to steer"` (V). Review and compact turns cannot be steered.
- Verified: the steer text appeared in the model input right after the pending tool output.

**Append to history without starting a turn**

- `thread/inject_items {threadId, items:[raw Responses API items]}` (`common.rs:870`). Verified: it returned `{}` on an idle thread.

**Durable queue (experimental)**

- `thread/queue/{add,list,update,delete,reorder,start}` (`common.rs:629-663`).
- `add` takes `{threadId, input, clientUserMessageId}`.
- Items dispatch as a **new turn when the thread is idle**. Verified: an add on an idle thread dispatched immediately and the list came back empty. The notification `thread/queue/changed` fires on changes.

**Interrupt**

- `turn/interrupt {threadId, turnId}` (`common.rs:1056`, `turn.rs:327`).

**List threads**

- `thread/list` (`common.rs:766`, `thread.rs:1394`) takes:
  - `cursor`, `limit`
  - `sortKey` (`created_at` or `updated_at`), `sortDirection`
  - `sourceKinds` (omitted or empty means interactive sources only)
  - `cwd`, `archived`, `searchTerm`, `modelProviders`, `useStateDbOnly`
- Also `thread/loaded/list` (V).

**Notifications** (all V unless marked)

- `thread/started`
- `thread/status/changed` (`{type:"active",activeFlags:["waitingOnApproval"]}` or `idle`)
- `turn/started`, `turn/completed` (S for `turn/completed`), `turn/diff/updated`, `turn/plan/updated`
- `item/started`, `item/completed`, `item/agentMessage/delta`, `item/commandExecution/outputDelta`, `item/fileChange/outputDelta`, `item/reasoning/*`
- `thread/tokenUsage/updated` with `{threadId, turnId, tokenUsage:{total:{…},last:{…}}}`
- `serverRequest/resolved` with `{threadId, requestId}`
- `hook/started`, `hook/completed`
- `item/autoApprovalReview/started`, `item/autoApprovalReview/completed`
- `error`, `warning`, `configWarning`

---

## 2. Approvals over the app-server

### Server-to-client requests (S, and V for command execution)

- `item/commandExecution/requestApproval` (`common.rs:1778`). Params (`v2/item.rs:1549`):
  - `kind` (`command` or `writeStdin`), `threadId`, `turnId`, `itemId`, `startedAtMs`, `approvalId?`, `environmentId`
  - `reason`, `command`, `cwd`, `commandActions`
  - `proposedExecpolicyAmendment?`, `proposedNetworkPolicyAmendments?`
  - `availableDecisions?` (experimental)

  The response is `{decision}`, where `decision` is one of:
  - `"accept"`
  - `"acceptForSession"`
  - `{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":[…]}}`
  - `{"applyNetworkPolicyAmendment":{…}}`
  - `"decline"` (the turn continues)
  - `"cancel"` (also interrupts the turn)

  See `item.rs:66-86`.
- `item/fileChange/requestApproval` (`common.rs:1785`, `item.rs:1632`). Params: `threadId`, `turnId`, `itemId`, `startedAtMs`, `reason?`, `grantRoot?`. Decisions: `accept`, `acceptForSession`, `decline`, `cancel` (`item.rs:115`).
- Other server requests:
  - `item/permissions/requestApproval`
  - `item/tool/requestUserInput`
  - `mcpServer/elicitation/request` (MCP tool approvals go through elicitation)
  - `item/tool/call` (dynamic tools)
  - legacy v1 `execCommandApproval` and `applyPatchApproval`

### Fan-out (S, V)

- A thread-scoped request goes to every connection subscribed to the thread (`app-server/src/outgoing_message.rs:325-430`). The first response wins, and the server then broadcasts `serverRequest/resolved`.
- Verified: A and B both received request id 2. B answered `decline`, and A got `serverRequest/resolved {requestId:2}`.

### Approval policy

- `approval_policy` values (`protocol/src/protocol.rs:986`): `on-request`, `never`, `granular{sandbox_approval,rules,skill_approval,request_permissions,mcp_elicitations}`, and the internal `untrusted`.
- **Setting `approval_policy="untrusted"` in config is rejected in 1.161** (V). The error is `approval_policy = "untrusted" is no longer supported`.

### `approvals_reviewer`

- Defined in `v2/shared.rs:274` and `config/src/config_toml.rs:197-200`.
- Values: `"user"` (the default) and `"auto_review"`. The alias `"guardian_subagent"` is also accepted.
- It sets **who reviews escalated approval requests**: sandbox escapes, blocked network, MCP approval prompts and ARC escalations. `auto_review` sends them to a "guardian" subagent that gathers context and approves or denies based on risk (`core/src/guardian/`), and the client sees `item/autoApprovalReview/*` notifications.
- It can be set in config, per thread (`thread/start`, `thread/resume`) or per turn (`turn/start`).
- `--approve-for-me` (alias `--not-so-yolo`) sets `approvals_reviewer="auto_review"`, `approval_policy="on-request"` and `sandbox_mode="workspace-write"` (`cli/src/main.rs:3185-3225`).
- Admins can constrain it with `allowed_approvals_reviewers` in requirements.toml.
- For our web UI we want `"user"`. I infer that with `auto_review` the client is not asked.

---

## 3. TUI and supervisor on the same app-server: yes (V)

- The `--remote <ADDR>` flag on `ore`, `ore resume`, `ore fork`, `ore agents` and `ore queue` accepts `ws://host:port`, `wss://host:port`, `unix://` (the default socket) or `unix://PATH`. `--remote-auth-token-env VAR` supplies a bearer token for ws/wss. The resolver is `tui/src/lib.rs:449-485`.
- **Verified:**
  - The supervisor ran `ore app-server --listen unix://`.
  - `ore resume <threadId> --remote unix:// --no-alt-screen` under a pty rendered the full history.
  - While the TUI was attached, the supervisor started a turn over the socket. The TUI showed the supervisor's user message and the live approval prompt ("Would you like to run the following command? … 1. Yes, proceed (y)"). When the supervisor answered the request, the TUI's prompt was resolved.
- So the **supervisor is one client and the TUI is another on the same thread**. The TUI identifies as `client_name:"codex-tui"` with `experimental_api:true` (`tui/src/lib.rs:497-509`).
- Without `--remote`, the TUI probes `$ORE_HOME/app-server-control/app-server-control.sock` itself and attaches if it is listening (`tui/src/lib.rs:514-550,1014-1040`). That probe is skipped under the exclusions in §1. `--no-daemon` forces an embedded in-process server.
- For a human in a browser, run the TUI in a pty (for example ttyd or `kubectl exec -it … ore resume <id> --remote unix://`) (I). Several TUIs can attach (I, following from the fan-out).
- Caveat (S): a TUI connected through `--remote` uses the **server's** config and environment. A shared daemon does not isolate environment per client (daemon README).

---

## 4. Config for persona policy

### Home directory and layering (S, `config/src/loader/mod.rs:107-138`)

- **Requirements** (admin-enforced; the agent cannot override them):
  - `/etc/codex/requirements.toml`
  - the cloud bundle
  - `/etc/codex/managed_config.toml` (legacy; mapped to requirements)
  - macOS MDM
- **Config layers, low to high precedence:**
  1. package defaults
  2. MDM
  3. **system** `/etc/codex/config.toml`
  4. cloud
  5. legacy `~/.codex/config.toml` (fork-only; see §6)
  6. user `$ORE_HOME/config.toml`
  7. profile `$ORE_HOME/<name>.config.toml`
  8. project `.codex/config.toml`, only when the project is trusted
  9. runtime `-c` and the per-thread `config` map
- **The paths are still `/etc/codex/…`, not `/etc/ore/`.** Verified with `strings` on the binary.
- The system `config.toml` is only a low-precedence default. Anything that must be enforced belongs in **requirements.toml**. Fields there (`config_requirements.rs:1031-1078`) include:
  - `allowed_approval_policies`, `allowed_approvals_reviewers`, `allowed_sandbox_modes`, `allowed_permission_profiles`, `default_permissions`
  - `model_provider`, `model_providers`
  - `mcp_servers` (identity, command or url matchers)
  - `rules` (`[[rules.prefix_rules]]` with `pattern=[{token=…}|{any_of=[…]}]`, `decision="allow|prompt|forbidden"`, `justification`)
  - `hooks` (managed hooks), `allow_managed_hooks_only`
  - `network`, `filesystem`, `additional_developer_instructions`, `features`, `apps`, `plugins`
- Project-local config cannot set `model_provider(s)`, `profiles`, `notify`, `otel` or the base URLs (`loader/mod.rs:88-101`).

### Profiles (V, S)

- **Profile-v2 replaces `[profiles.x]`.** `-p NAME` layers `$ORE_HOME/NAME.config.toml` on top of `config.toml`.
- A legacy `profile = "x"` key is a hard error: "legacy `profile = \"x\"` config is no longer supported" (`core/src/config/mod.rs:3394-3401`). The `[profiles.x]` table still parses, but using it together with `-p x` errors (`loader/mod.rs:303-321`).
- **`--profile` is not accepted by `app-server`.** It is rejected for every subcommand outside the list at `cli/src/main.rs:1874-1900`, and `ore app-server -p foo` gives "unexpected argument". For the app-server, express a persona through a per-pod `ORE_HOME/config.toml`, `-c` overrides, or the `thread/start` params `config`, `developerInstructions` and `approvalPolicy`.

### Execpolicy rules (V, S)

- Starlark `.rules` files:

  ```
  prefix_rule(pattern=["git", ["push","reset"]], decision="allow"|"prompt"|"forbidden",
              justification="…", match=[…], not_match=[…])
  host_executable(name="git", paths=[…])
  ```

  See `codex-rs/execpolicy/README.md`.
- Loading: every `*.rules` file in `<config_folder>/rules/` for each layer, low to high precedence (`core/src/exec_policy.rs:54-56,661-704`). That means:
  - `/etc/codex/rules/` (system layer)
  - `$ORE_HOME/rules/`
  - trusted project `.codex/rules/`
  - then requirements `[rules]` overlaid on top
- `acceptWithExecpolicyAmendment` writes to `$ORE_HOME/rules/default.rules` (`exec_policy.rs:867`).
- `ore exec --ignore-rules` skips user and project rules.
- Verified: `$ORE_HOME/rules/persona.rules` containing `prefix_rule(pattern=["touch"], decision="prompt", …)` made `bash -lc 'touch … && echo hi'` raise an approval request whose reason was "requires approval: persona: file creation needs human ok".

### MCP servers (S, `config/src/mcp_types.rs:240-300`)

- Per server: `[mcp_servers.NAME]` accepts `enabled_tools` (an allow-list), `disabled_tools` (applied after `enabled_tools`), `default_tools_approval_mode` and `required`.
- Per tool: `[mcp_servers.NAME.tools.TOOL] approval_mode`.
- `approval_mode` values are `auto`, `prompt`, `writes` or `approve` (`mcp_types.rs:28`).
- requirements.toml `mcp_servers` can restrict which servers are allowed at all.

### Hooks (S)

- Events (`protocol/src/protocol.rs:1579`): `PreToolUse`, `PermissionRequest`, `PostToolUse`, `PreCompact`, `PostCompact`, `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `SubagentStart`, `SubagentStop`, `Stop`, `Interrupt`.
- Handler types: `command` and `mcp_tool`. `prompt` and `agent` are skipped.
- Where hooks come from:
  - `[hooks]` in any config layer: `[[hooks.PreToolUse]] matcher=…, hooks=[{type="command",command=…}]`
  - `<config_folder>/hooks.json`
  - plugins
- Trust (`hooks/src/engine/discovery.rs:666-822`):
  - Each handler gets a key, `"<source>:<event>:<group>:<handler>"`, and a hash, `"sha256:…"` computed over a normalized TOML identity.
  - Non-managed hooks run only if `[hooks.state."<key>"] trusted_hash` matches the current hash.
  - Hooks from the **system layer (`/etc/codex/config.toml`) and requirements count as managed and are trusted automatically.**
  - `--dangerously-bypass-hook-trust` skips the check, but it is not offered on `app-server`.
  - The app-server method `hooks/list` reports trust status.

### `model_providers` (S)

- `[model_providers.ID]` fields (`model-provider-info/src/lib.rs:154-206`):
  - `name`, `base_url`, `env_key` (the env var holding the API key), `env_key_instructions`, `experimental_bearer_token`
  - `auth` (command-backed token), `wire_api`
  - `http_headers`, `env_http_headers`, `query_params`
  - retry and timeout settings, `requires_openai_auth`
- `wire_api` (`lib.rs:103`) is `responses` (the default), `chat`, `anthropic` or `gemini`. The last three are additions in this fork.
- Verified: `env_key="MOCK_KEY"` with `wire_api="responses"` against the mock.

---

## 5. Sessions, resume and the queue

### Session storage (V)

- Rollouts are written to `$ORE_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<threadId>.jsonl`. The first line is `session_meta` (id, cwd, originator=clientInfo.name, `source:"vscode"` for app-server clients, model_provider, …).
- New threads use `historyMode:"paginated"`, so history is also kept in `$ORE_HOME/thread_history_1.sqlite`.
- Other databases in `$ORE_HOME`, or in `$CODEX_SQLITE_HOME` / `sqlite_home` when set: `state_5.sqlite`, `logs_2.sqlite`, `goals_1.sqlite`, `memories_1.sqlite`, `queue_1.sqlite` (`state/src/sqlite.rs:34-39`).

### Resume after a restart (V)

1. Kill the server and start a new one. `thread/loaded/list` is now empty.
2. `thread/list {sortKey:"updated_at", limit:1, sourceKinds:[]}` returns the last thread with `status:notLoaded`.
3. `thread/resume {threadId}` returns 2 turns and status `idle`.

A turn that was in progress when the server died, together with its pending approval, is lost; the TUI showed it as "Conversation interrupted". A managed daemon additionally records loaded threads in `app-server-daemon/loaded-threads.json` for recovery (S).

### CLI resume

- `ore resume <UUID|name>`, `ore resume --last [--all] [--include-non-interactive]`
- `ore exec resume <id>` or `ore exec resume --last`

### `ore queue` (V for `--help`, S for the rest)

- `ore queue --thread <UUID|exact name> --message TEXT [--remote ADDR]` (`cli/src/queue_cmd.rs`, `tui/src/session_queue_commands.rs:27-110`).
- It connects to the running shared daemon, or to `--remote`, and calls the experimental `thread/queue/add`.
- It refuses `--no-daemon`. It also refuses to go through an embedded server while a daemon is running, so that two servers never write the same thread.

### Queue storage (S)

- The queue is stored in `$ORE_HOME/queue_1.sqlite`.
- Tables: `queued_items(id, thread_id, payload_json, queue_order, created_at_ms, updated_at_ms)` and `queued_thread_revisions` (kept up to date by triggers) (`state/queue_migrations/0001_*.sql`, `0002_*.sql`).
- The queue service (`ext/queue/src/service.rs:89-140`) checks the SQLite data version every **10 s**. It dispatches queued items as new turns when a thread is idle (`start_turn_if_idle`), so external writers to the database get picked up.
- It is "next turn" delivery, not a steer.

### Delivery options for room messages

- `turn/steer`, or `turn/start` while a turn is running: injects into the current turn.
- `turn/start` on an idle thread: starts a new turn.
- `thread/queue/add`: durable; delivered when the thread is idle.
- `thread/inject_items`: adds context only, without waking the agent.

---

## 6. Home directory and the legacy layer (S, `utils/home-dir/src/lib.rs:13-47`)

- `find_codex_home()` checks `ORE_HOME` first, then `CODEX_HOME` (a full alias), then defaults to `~/.ore`. The directory must already exist; otherwise startup errors.
- Fork-only: `~/.codex/config.toml` is layered **underneath** `$ORE_HOME/config.toml` as a read-only user layer (`config/src/loader/mod.rs:545-617`). Its `profile`/`profiles` keys are stripped, and a parse error disables the layer rather than aborting startup.
- The legacy layer is turned off when:
  - `CODEX_HOME` is set (setting only `ORE_HOME` does not turn it off), or
  - `~/.codex` does not exist, or
  - the resolved home was injected (it is not the process's own home).
- Because its config folder is `~/.codex`, I infer that `~/.codex/rules/*.rules` and `~/.codex/hooks.json` would also be picked up.
- **Pod recommendation:** set `CODEX_HOME` (or make sure no `~/.codex` exists) for a deterministic config.
- Other environment variables:
  - `CODEX_SQLITE_HOME` relocates the databases.
  - The `CODEX_INTERNAL_*` names are kept unrebranded on purpose (`fork/verify/strings.toml`).

---

## Pod blueprint (I)

1. Use `/etc/codex/requirements.toml` and `/etc/codex/rules/*.rules`, owned by root and read-only, for policy the agent cannot change. Put trusted hooks in `/etc/codex/config.toml`.
2. Use `CODEX_HOME=/data/ore` on a PVC holding `config.toml` (the persona), the sessions and the SQLite databases.
3. The supervisor spawns `ore app-server --listen unix://` and speaks WebSocket over the UDS (Go: `gorilla/websocket` or `nhooyr` with a custom unix dialer). The alternative is `--listen stdio://` with JSONL, but then the TUI cannot attach.
4. On boot, the supervisor calls `thread/list` (sorted by `updated_at`, filtered on `cwd`), then `thread/resume`, then streams notifications.
5. Room messages go through `turn/steer` when a turn is active, otherwise `turn/start`; or through `thread/queue/add`.
6. Approval requests are forwarded to the web UI, and the supervisor replies with a `{decision}`.
7. A human attaches with `ore resume <id> --remote unix://` in a pty. Approvals are first-responder-wins between the web UI and the TUI.
