# 0004. Frontend stack: React, Vite and TypeScript, embedded in the Go binary

- Status: Proposed
- Date: 2026-10-16

## Context

The control center UI is an operations console: a setup wizard, a fleet view
that updates live, per-room tabs with terminals (xterm.js), a VNC view of the
room's display (noVNC), log and metric panes, an approvals inbox that must
work on a phone, and web push. It is served by the control center's Go
backend. A small team maintains it, and contributors will mostly be Go
developers.

## Options

1. **React + Vite + TypeScript**, with TanStack Query for server state and
   TanStack Router, and a plain component library (Radix primitives with our
   own styles). The most common stack, so the most libraries, examples and
   contributors. xterm.js and noVNC have React wrappers or are trivial to
   wrap.
2. **SvelteKit.** Smaller bundles and less code. Fewer libraries and fewer
   people who know it; SvelteKit's server side would be unused, since the Go
   backend is the server.
3. **Server-rendered HTML with htmx.** Least JavaScript, but the live views
   (terminals, streams, the wizard's progress) end up as custom JavaScript
   anyway.

## Decision

React, Vite and TypeScript, built to static files and embedded in the control
center binary with `go:embed`. No Node at runtime.

- Server state: TanStack Query over the generated Connect clients
  (`@connectrpc/connect-query`), so the API types come from the protobuf
  schema (ADR 0003).
- Routing: TanStack Router.
- Components: Radix primitives with Tailwind CSS. No heavyweight design
  system.
- Terminal: xterm.js. Display: noVNC.
- Push: the standard Push API with a service worker; the app is installable
  (PWA manifest), so approvals can be answered from a phone's home screen.
- Tests: Vitest for units, Playwright for end-to-end flows (Chromium is
  available in CI).

## Consequences

- The single control center image holds UI and API; upgrades can't skew
  them.
- Node is needed only to build the UI (`make ui`), and the dev loop uses
  Vite's dev server proxying to the Go backend.
- We accept React's larger bundle; the console is not a public site.
