# 0002. Messaging storage and replication: embedded NATS JetStream

- Status: Proposed
- Date: 2026-10-16

## Context

The messaging service must:

- keep a durable inbox per recipient
- deliver at least once, with message IDs for deduplication
- keep each sender's messages in order
- hold messages for offline agents
- keep an audit log with configurable retention
- track presence
- run as at least three replicas and survive losing a node
- be ready for federation between clusters later

The brief recommends embedding NATS JetStream rather than writing consensus
ourselves.

## Options

1. **Embedded NATS JetStream.** Each messaging replica runs `nats-server`
   in-process (v2.15), clustered over routes. JetStream gives replicated
   streams with Raft, per-subject retention, `MaxAge` TTLs, duplicate
   detection by `Nats-Msg-Id`, durable consumers and KV buckets with TTLs.
   Leaf nodes and superclusters exist for federation.
2. **External NATS (its own StatefulSet) plus a stateless messaging service.**
   It separates concerns, but adds a second distributed system for the
   operator to install and upgrade, and a second network hop.
3. **PostgreSQL as the queue.** We already run one, but it is a single
   instance (ADR 0008), so messaging would not survive losing its node.
4. **Our own Raft (hashicorp/raft) plus a log store.** It is exactly the
   consensus code the brief tells us not to write.

## Decision

Option 1. The messaging service is a three-replica StatefulSet (one replica
on a single-node cluster, where losing the node is losing everything anyway),
with a volume per replica and stable names through a headless Service. Each
replica embeds a JetStream-enabled NATS server clustered with the others.
NATS ports are reachable only between messaging pods (NetworkPolicy). Clients
never speak NATS; they speak our Connect API (ADR 0003), so policy is always
applied by our code.

**Layout:**

| Stream / bucket | Subjects | Settings |
|---|---|---|
| `INBOX` stream | `inbox.<workspace>.<recipient>` | Replicas 3, file storage, `Duplicates` window 10 min, `MaxAge` = the workspace's message retention, discard old |
| `AUDIT` stream | `audit.<workspace>.<kind>` (`message`, `crossing`, `refusal`, `presence`) | Replicas 3, `MaxAge` = the workspace's audit retention |
| `presence` KV | `<workspace>.<agent>` | Replicas 3, per-key TTL 90 s, refreshed by the supervisor's heartbeat and on every state change |
| `approvals` stream | `approval.<workspace>.<room>` | Replicas 3; requests and answers, consumed by the control center and the room |

- **Delivery.** Each recipient has one durable pull consumer filtered to its
  inbox subject, with `AckExplicit` and `MaxAckPending: 1`. This keeps the
  whole inbox in order, which is stronger than per-sender order; agent
  message rates are low, so the throughput cost doesn't matter. The service
  holds the consumer for the connected supervisor and streams messages
  down `Subscribe`. The supervisor acks only after the CLI has accepted the
  message (see ADRs 0006 and 0007), so a crash in between redelivers it.
  Supervisors drop duplicates by message ID, using a small on-disk set of
  recently seen IDs.
- **IDs and order.** The sender's supervisor assigns a ULID to each message
  and retries `Send` with the same ID; JetStream's duplicate window and
  `Nats-Msg-Id` make retries safe. Each `Send` waits for the stream's
  acknowledgement before it returns, and a supervisor sends one message at a
  time, so a sender's messages enter the stream in the order it sent them.
- **Retention.** Retention TTLs come from the `Workspace` resource and are
  applied as each stream's `MaxAge`. Messages and audit can also be kept
  forever (`0`).
- **Scale.** A durable R3 consumer per recipient costs Raft state. At our
  scale (tens to hundreds of agents per cluster) that is fine; past a few
  thousand recipients we would switch to fewer, sharded consumers. This is
  noted, not built.

## Consequences

- No separate NATS deployment to install, upgrade or secure.
- Losing one of three nodes leaves a quorum. Rooms reconnect to another
  replica through the Service, and consumers resume where they acked.
- Federation later: clusters link with NATS leaf nodes or a supercluster, and
  inbox subjects gain a cluster segment (`inbox.<cluster>.<workspace>.<agent>`).
  Addresses already carry the workspace, and v1 reserves a third part
  (`agent@workspace.cluster`).
- We depend on NATS's embedding API, which is stable but less documented than
  running the server. Pin the version and cover leader loss in the e2e suite.
