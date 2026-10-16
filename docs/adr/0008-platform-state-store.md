# 0008. Platform state: Kubernetes for bootstrap, one PostgreSQL for the rest

- Status: Proposed
- Date: 2026-10-16

## Context

Several components need a database:

- **The control center:** users, identity links, sessions, role
  assignments, its own audit log, approvals and their answers, notification
  subscriptions, wizard drafts.
- **Forgejo:** its own data. SQLite works for small installs; PostgreSQL is
  what its docs recommend for anything shared.
- **LiteLLM:** virtual keys, budgets and spend need PostgreSQL.

But the install creates only the operator and the control center, and the
control center must work *before* anything else exists: the first page
asks for the setup token, then runs preflight, which is where we find out
whether the cluster even has a usable StorageClass. A control center that
needed a volume to start would sit in `Pending` on exactly the clusters whose
problems preflight is meant to explain.

## Options

1. **SQLite on a volume, in the control center.** Simple, but the control
   center then needs storage at install time, can't run more than one
   replica, and Forgejo and LiteLLM still need their own databases.
2. **PostgreSQL deployed by the operator as a platform service, shared by the
   control center, Forgejo and LiteLLM (one database and one role each); the
   bootstrap state lives in Kubernetes objects.**
3. **An operator-managed PostgreSQL (CloudNativePG).** Highly available, but
   a second operator to install and upgrade, against "install only the
   operator and the control center".
4. **NATS JetStream KV (already in the messaging service) for the control
   center.** No SQL, no migrations tooling, and it couples the control
   center's data to the messaging service's lifecycle.

## Decision

Option 2.

- **Before PostgreSQL exists**, the control center is stateless. What it
  needs lives in Kubernetes: the setup token and the local admin's password
  hash in Secrets in the system namespace, the wizard's progress in the
  `Platform` resource's spec and status, and the session-signing key in a
  Secret. Audit events from this phase go to the control center's log and are
  copied into the audit table once the database is up.
- **The first thing the wizard deploys after preflight is PostgreSQL** (one
  StatefulSet, official `postgres` image, a pinned major version), then
  Forgejo, LiteLLM and the rest. Each gets its own database and role, with a
  generated password in a Secret only that service mounts.
- Migrations: embedded SQL files run at control center start
  (`pressly/goose`), forward-only.
- Backups: a nightly `pg_dump` CronJob to a volume, retention configurable in
  the `Platform` resource. Off-cluster backup is documented, not built.

## Consequences

- The control center can run two replicas once PostgreSQL is up (sessions
  are in the database, not in memory).
- One stateful component to back up instead of three.
- PostgreSQL is a single point of failure for the control center, Forgejo and
  LiteLLM. Forgejo is single-replica anyway, so the platform as a whole does
  not survive losing the node that holds them; **messaging does** (ADR 0002),
  so agents already running keep talking to each other, and rooms keep
  working, while the database's node is down. Rooms lose the forge and the
  model gateway until it is back. A highly available PostgreSQL can replace
  this later behind the same connection Secrets without changing any
  component.
