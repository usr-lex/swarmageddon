# 0003. Wire protocol: Connect (protobuf) over HTTP/2 with mTLS

- Status: Proposed
- Date: 2026-10-16

## Context

Four kinds of client talk to our services:

- **Room supervisors** (Go, one per room) talk to the messaging service. They
  need a long-lived server stream (incoming messages, approval answers) and
  unary calls (send, ack, report status). They authenticate with a per-room
  identity the operator issues.
- **The control center backend** (Go) talks to the messaging service as a
  privileged client: it sends as humans, subscribes to a workspace's
  presence, approvals and audit events.
- **The browser** talks to the control center backend: unary calls for most
  things, server streams for the fleet view and inboxes, and a raw
  bidirectional byte stream for terminals.
- **Later, other clusters** (federation, not built in v1) talk to the
  messaging service.

The brief allows gRPC or Connect.

## Options

1. **gRPC (grpc-go).** The standard. Browsers cannot speak it without a proxy
   (gRPC-Web through Envoy or a Go shim). Its Go library is large and brings
   its own HTTP/2 server, separate from `net/http`.
2. **Connect (connectrpc.com/connect).** Same protobuf schema and code
   generation (buf). One `net/http` handler serves the Connect protocol, gRPC
   and gRPC-Web at once, so Go clients can use gRPC framing and browsers can
   call the same services directly with `@connectrpc/connect-web`. Server
   streaming works from browsers over HTTP/1.1 and HTTP/2; bidirectional
   streaming needs HTTP/2 end to end, which browsers can't do with fetch.
3. **JSON over HTTP plus WebSockets, hand-rolled.** No schema, no generated
   clients, and every stream reinvented.

## Decision

Connect, with protobuf schemas in `proto/` managed by buf.

- Room supervisor ↔ messaging: Connect's gRPC protocol over HTTP/2 with
  mutual TLS. The client certificate is the room's identity
  (`agent@workspace`, see the design's "Identity"). One server stream per
  supervisor (`Subscribe`) plus unary `Send`, `Ack` and `ReportPresence`.
- Control center ↔ messaging: the same, with a service certificate the
  messaging service maps to the "control center" principal, which may act for
  human members of a workspace.
- Browser ↔ control center: Connect protocol (JSON or binary) through
  `connect-web`; server streams for live views. Session cookie auth, CSRF
  protection on unary calls.
- Terminals are the one exception: a WebSocket carrying raw bytes between
  xterm.js and the control center, which bridges it to a Kubernetes `exec`
  stream. Browsers can't do bidirectional Connect streams, and a terminal is
  just bytes.
- Every message carries a schema version in its envelope; fields are only
  ever added. Breaking changes get a new package (`swarm.messaging.v2`).

## Consequences

- One schema serves Go and TypeScript; `buf lint` and `buf breaking` run in CI
  and stop accidental wire breaks between a new control center and older
  supervisors still running in rooms that haven't been rolled yet.
- No Envoy or gRPC-Web proxy anywhere.
- Kubernetes Services and NetworkPolicies see plain HTTPS on one port per
  service.
- Federation can reuse the same services over mTLS between clusters.
