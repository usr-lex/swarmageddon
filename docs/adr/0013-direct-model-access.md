# 0013. Direct model access: rooms without the gateway

- Status: Accepted
- Date: 2026-10-19
- Context: OpenShift first-install report, feature request 8

## Context

Every room reached its model through the platform's LiteLLM gateway with a
per-room virtual key. The owner wants rooms that talk to their provider
directly: with a provider API key, or with the CLI's own login (a Claude
subscription is meant to be used through Claude Code's own login, not a
third-party proxy). The owner accepted what is lost without the gateway.

## Decision

`Workspace.spec.models.access` is `gateway` (default) or `direct`;
`Room.spec.modelAccess` overrides it per room. One `direct` mode covers both
the report's `direct-key` and `cli-login`: the CLI uses whichever it has.

For a direct room:

- **Claude Code** gets no `ANTHROPIC_BASE_URL` and no `ANTHROPIC_AUTH_TOKEN`;
  it uses `ANTHROPIC_API_KEY` from the room's secretEnv, or its own login
  (`/login` in the Agent tab, kept in the room's home).
- **ore** gets no forced gateway provider (requirements and config), and the
  supervisor drops a gateway provider left in `~/.ore/config.toml`; it uses
  `OPENAI_API_KEY` from secretEnv or `ore login`.
- **No gateway key** is issued; one left from gateway access is revoked.
- **Egress:** the provider's domains are opened. A room with network `none`
  gets `limited` with exactly those; a `limited` room gets them added; a
  `full` room needs nothing. Claude: api.anthropic.com, claude.ai,
  console.anthropic.com, platform.claude.com. ore: api.openai.com,
  auth.openai.com, chatgpt.com.
- The **advisor** and the **warden's model reviews** still go through the
  gateway with their own keys, so they keep working.
- The control center shows "Direct model access" on the room and how to log
  in; the wizard and the room's Edit dialog set the mode.

## Consequences

Lost for direct rooms, as accepted: per-room spend limits and the Spend page,
the warden's gateway guard (ADR 0011, layer 4: it only sees gateway traffic),
and key rotation by the operator. Quarantine still stops a direct room's
model use: it shrinks the room's network to DNS and the gateway, so the
provider is out of reach.

Not verified against the real providers' login flows from inside a room.
