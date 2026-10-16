# 0009. Forgejo CLI for agents: `tea`

- Status: Proposed
- Date: 2026-10-16

## Context

Agents work through issues, pull requests and reviews. In remote-labs they
used `gh`; here they need a CLI for Forgejo, logged in with the room's token
and taught in the role templates. Forgejo 16 is our minimum version.

## Options

1. **`tea` (Gitea's CLI, v0.16).**
   - Issues and pull requests, including approve, reject, review comments,
     reply, resolve and merge.
   - Machine-readable output (`--output json`).
   - `tea api` for raw calls.
   - Logs in without prompts from `GITEA_INSTANCE_URL` and `GITEA_TOKEN`.
   - Works against Forgejo's Gitea-compatible API.
2. **`fj` (forgejo-cli, 0.7).** Forgejo-native, but its pull request review
   support only lists reviews and requests reviewers: an agent can't approve
   or request changes with it.
3. **Our own wrapper.** No.

## Decision

`tea`, pinned in the room image and built for both architectures (from source
if its releases don't ship arm64). The room supervisor writes its config from
the room's token on every start, so the token never sits in role text or an
environment variable. Role templates teach the commands each role uses (for
example, the reviewer template covers `tea pr review` and `tea pr approve`).

## Consequences

- A Gitea-compatible CLI against Forgejo: Forgejo-only features (for example,
  AGit flow, or newer endpoints) need `tea api`. The e2e suite runs each command
  the templates teach against our pinned Forgejo, so a break shows up when we
  bump either.
- If `fj` gains reviews, switching only changes the image and the templates.
