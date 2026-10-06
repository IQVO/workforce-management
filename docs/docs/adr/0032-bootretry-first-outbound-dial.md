---
id: 0032-bootretry-first-outbound-dial
slug: /adr/0032-bootretry-first-outbound-dial
title: 0032. Boot-time retry for the first outbound dial (Istio sidecar warm-up)
sidebar_label: 0032. Boot retry (sidecar warm-up)
sidebar_position: 33
description: "ADR 0032 — every composition root wraps its first outbound dial (Postgres, Kafka) in internal/adapters/outbound/bootretry's exponential backoff (~31s budget): the fleet's Istio native sidecars reset the first dial ~10s after start, and one attempt turns that known transient into CrashLoopBackOff. The retry never weakens a fail-closed rule. Recorded retroactively by the 2026-10 ADR-conformance pass."
---

# 0032. Boot-time retry for the first outbound dial (Istio sidecar warm-up)

## Status

Accepted — recorded retroactively (2026-10 ADR-conformance pass); the
helper is `internal/adapters/outbound/bootretry` (originally extracted
from network-fulfillment's `retryWithDelay`).

## Context

In this fleet, EVERY injected pod's FIRST outbound TCP dial (Postgres,
Kafka) is reset ~10 seconds after the application starts. The cause is
the Istio native sidecars' start-up ordering —
`holdApplicationUntilProxyStarts` is a no-op for them — so the
application's first dial can land before the sidecar's envoy is
listening, and envoy resets the connection.

A single dial attempt turns that known, transient condition into
CrashLoopBackOff: the dial fails with "read: connection reset by peer",
the composition root returns the error, the process exits, and the pod
never gets far enough to serve its own health probe. Kubernetes then
restarts it into the same race.

## Decision

Every composition root in this repo (cmd/workforce,
cmd/workforce-projector, cmd/workforce-reports, cmd/mcp) wraps each
boot-time outbound connection attempt in `bootretry.Retry`: 5 attempts
with exponential backoff from 1s (1+2+4+8+16 ≈ 31s total), returning the
LAST error so a permanent failure still reports its real cause.

The retry is NOT a weakening of any fail-closed rule. Once the budget is
exhausted the caller still refuses to boot; the retry only stops treating
sidecar warm-up — a condition that always resolves within the budget —
as a permanent failure.

## Consequences

- Pods survive the sidecar warm-up window; a genuinely unreachable
  dependency still fails the pod within ~31s, well inside the liveness
  probe's tolerance.
- The pattern is one shared, tested package instead of four per-binary
  copies (bootretry.RetryWithDelay exists so tests exercise the give-up
  path without sleeping out the real budget).
- The same helper now also guards later-added boot steps (the Kafka
  catalogue consumer's construction dial), keeping the contract uniform.
