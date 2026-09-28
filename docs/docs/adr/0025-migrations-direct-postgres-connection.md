---
id: 0025-migrations-direct-postgres-connection
slug: /adr/0025-migrations-direct-postgres-connection
title: 25. Run golang-migrate against a direct Postgres connection, not PgBouncer
sidebar_label: 25. Migrations bypass PgBouncer
description: "ADR 0025 — fleet-wide Phase 4 finding, ported from order-management's reference implementation (ADR-0029, PR #115): golang-migrate's postgres driver takes a session-scoped pg_advisory_lock to serialize concurrent migration runs, which is incompatible with PgBouncer's transaction-pooling mode (warehouse-infra PR #43). Two or more workforce-management replicas starting concurrently (HPA scale-out, or an ordinary rolling deploy) would crash-loop until one won the advisory-lock race. Fix: a second env var, MIGRATIONS_DATABASE_URL, carries a direct (non-pooled) connection string used ONLY for the migration step; DATABASE_URL/the runtime pgxpool is untouched and keeps going through PgBouncer. warehouse-infra PR #44 already provisions the secret key for this and all 9 OLTP services."
---

# 25. Run golang-migrate against a direct Postgres connection, not PgBouncer

## Status

Accepted — implemented in the same change that introduces this record.
This is a direct fan-out of **order-management PR #115** / **ADR-0029**,
the reference implementation for this fleet-wide fix (found during
order-management's own Phase 4 k6/HPA load-test validation cleanup, and
live-verified there). This record documents workforce-management's copy
of that fix; it is not a new investigation — see ADR-0029 for the full
incident narrative and live-verification evidence, both of which apply
here unchanged.

## Context

warehouse-infra's PgBouncer rollout (PR #43, Phase 3) repointed every
one of the fleet's 9 OLTP services' `DATABASE_URL` secret — including
workforce-management's — at PgBouncer (`terraform/pgbouncer.tf`), in
**transaction-pooling** mode (`pool_mode = "transaction"`). That is the
correct mode for this fleet's steady-state traffic: application code
never holds session state across statements.

What PR #43 did not carve out: **migrations**. Both of
workforce-management's Postgres-backed binaries run golang-migrate's
postgres driver (`github.com/golang-migrate/migrate/v4/database/postgres`)
against the same `DATABASE_URL` at process startup, before serving any
traffic:

- `cmd/workforce/main.go`'s `openPostgresPool`
- `cmd/mcp/main.go`'s `newRepos`

(`cmd/workforce-projector` and `cmd/workforce-reports` run migrations
against `ANALYTICS_DATABASE_URL` / `ANALYTICS_READER_DATABASE_URL`
respectively — the fleet's analytics DSNs, which PR #43 already pointed
directly at Postgres, not PgBouncer, so they are unaffected by this bug
and out of scope for this ADR.)

golang-migrate's postgres driver calls `SELECT pg_advisory_lock($1)` to
serialize concurrent migration runs — by design: if two processes start
at once and both try to run the same migration, whichever loses the
lock should block, not race.

`pg_advisory_lock` is **session-scoped**: the lock is held by whichever
physical backend connection issued it, and is expected to be released by
that same connection (or the session ending). PgBouncer's
transaction-pooling mode does not preserve that mapping — each statement
in a client's logical session can be routed to a different physical
backend connection, because the client's backend connection is returned
to the pool the instant its transaction commits. So:

- Pod A dials PgBouncer, gets backend connection #1, takes the advisory
  lock, runs migrations.
- Pod B dials PgBouncer *concurrently*, gets a **different** backend
  connection, and PgBouncer may freely reuse/rotate backend connections
  for either pod's subsequent statements mid-"session" from the
  application's point of view.
- The advisory lock never behaves as a real mutex across the two pods.
  Whichever pod's statements land on a backend connection with
  unexpected transaction/prepared-statement state gets errors like
  `pq: unnamed prepared statement does not exist` or `pq: canceling
  statement due to statement timeout`, and crash-loops for roughly 1-2
  minutes until the race resolves.

This is a **latent, fleet-wide, production-blocking bug**, not specific
to order-management: it fires on any ordinary rolling ArgoCD deploy with
more than 1 replica of `workforce` or `mcp`, and on any HPA scale-out
event for either. workforce-management's `api` and `analytics-reports`
Deployments already have per-workload HPAs defined (default-disabled,
ADR-0024); enabling them was blocked fleet-wide by exactly this bug
until each of the 9 OLTP services adopted order-management's fix — this
PR is workforce-management's adoption.

## Decision

Give workforce-management a **second** connection string,
`MIGRATIONS_DATABASE_URL` — a direct (non-pooled, session-mode) Postgres
connection string, same user/password/dbname as `DATABASE_URL`, pointed
at Postgres itself rather than PgBouncer — used **only** for the
golang-migrate startup step in `cmd/workforce/main.go`'s
`openPostgresPool` and `cmd/mcp/main.go`'s `newRepos`. `DATABASE_URL` and
the pgxpool built from it are completely unchanged: every request this
service serves still goes through PgBouncer in transaction-pooling mode,
exactly as PR #43 set up.

`warehouse-infra` PR #44 already provisions `MIGRATIONS_DATABASE_URL` as
a new key alongside the existing `DATABASE_URL` key in
`workforce-management-db` (and all 9 OLTP services' `<service>-db`
Secrets) — no further warehouse-infra work is needed for this task.
`cmd/workforce/main.go` and `cmd/mcp/main.go` (both run migrations) now
read `MIGRATIONS_DATABASE_URL` for the migration step:

```go
migrationsDatabaseURL := envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL)
...
pool, err := openPostgresPool(ctx, logger, databaseURL, migrationsDatabaseURL, migrationsPath)
...
postgres.Migrate(migrationsDatabaseURL, migrationsPath)  // migrations only
// the pgxpool opened right after this still uses databaseURL, unchanged
```

The fallback to `databaseURL` when `MIGRATIONS_DATABASE_URL` is unset
keeps every environment that doesn't provision the split — local dev,
CI integration tests, or any cluster whose Terraform predates this fix —
working exactly as before, byte-for-byte. Nothing about local dev or CI
changes as a result of this PR.

`charts/workforce-management`: a new `database.migrationsExistingSecretKey`
value (default `"MIGRATIONS_DATABASE_URL"`) renders the env var in both
the `api` (`deployment.yaml`) and `mcp` (`mcp-deployment.yaml`)
Deployments, sourced from the same `database.existingSecret`
(`workforce-management.databaseSecretName`), with `optional: true` on
the `secretKeyRef` — identical in shape to order-management's chart
diff — so a secret that predates this key still starts the pod.
`projector-deployment.yaml` and `reports-deployment.yaml` are untouched:
they migrate the analytics database, which was never behind PgBouncer.

### Why not just make PgBouncer's pool_mode session for this fleet?

Rejected — same reasoning as ADR-0029. Session pooling would fix the
advisory-lock problem but throws away the entire point of PgBouncer for
this fleet: transaction pooling is what lets many short-lived
HTTP-request-scoped OLTP connections share a small number of physical
Postgres backends.

### Why not just remove the advisory lock / skip migrations on non-leader replicas?

Rejected — same reasoning as ADR-0029. golang-migrate's advisory lock is
exactly the right mechanism *given a session-scoped connection*; the bug
is the mismatch with the pooling mode we run migrations through, not the
mechanism itself. An init-container/Job-based alternative is a bigger
architectural change for the same outcome this two-line env-var fallback
already achieves.

## Consequences

- **Fixes** the crash-loop bug for workforce-management's two
  Postgres-migrating binaries (`cmd/workforce`, `cmd/mcp`).
- **No runtime behavior change**: `DATABASE_URL` is untouched, so
  request-serving connection pooling, `pool_mode`, and PgBouncer's own
  configuration are all unaffected by this PR.
- **No behavior change for environments without the split**: the
  `envOrDefault("MIGRATIONS_DATABASE_URL", databaseURL)` fallback means
  local dev and CI integration tests keep using `DATABASE_URL` for
  everything, exactly as before.
- **Unblocks ADR-0024's `api` and `analytics-reports` HPAs**: an HPA
  scale-out is exactly the "2+ replicas start concurrently" trigger for
  this bug; this was a concrete blocker to safely enabling them.
- One more secret key to keep in sync going forward; already
  mechanically generated by warehouse-infra's Terraform (PR #44) from
  the same `local.services` map as `DATABASE_URL`, so there is no new
  per-service manual step.

## References

- [order-management ADR-0029](https://github.com/claudioed/order-management/blob/main/docs/docs/adr/0029-migrations-direct-postgres-connection.md)
  — the reference implementation this record and its code changes port,
  including the full incident narrative and live cluster verification
  (3-replica concurrent-start test, 0 restarts, no `pq:` errors).
- order-management PR [#115](https://github.com/claudioed/order-management/pull/115)
  — the code+chart+ADR change this PR mirrors.
- warehouse-infra PR [#43](https://github.com/claudioed/warehouse-infra/pull/43)
  — the PgBouncer rollout that introduced the transaction-pooling mode
  this fix routes migrations around.
- warehouse-infra PR [#44](https://github.com/claudioed/warehouse-infra/pull/44)
  — already provisions the `MIGRATIONS_DATABASE_URL` secret key for all
  9 OLTP services, workforce-management included; no further
  warehouse-infra work is needed for this task.
- [ADR-0024](./0024-horizontal-autoscaling-and-pgxpool-tuning.md) — the
  per-workload HPAs this fix unblocks safely enabling.
