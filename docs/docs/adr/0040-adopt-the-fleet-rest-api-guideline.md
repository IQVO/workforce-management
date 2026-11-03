---
id: 0040-adopt-the-fleet-rest-api-guideline
slug: /adr/0040-adopt-the-fleet-rest-api-guideline
title: 0040. Adopt the fleet REST API guideline
sidebar_label: 0040. Fleet REST API guideline
sidebar_position: 41
description: "ADR 0040: workforce-management adopts the fleet REST API guideline: noun routes instead of command verbs, every POST idempotent with a global Idempotency-Key (409 idempotency-key-reused, Idempotency-Replayed), and keyset-paged {items,nextCursor} collections."
---

# 0040. Adopt the fleet REST API guideline

## Status

Accepted (2026-11-01). Amends [ADR 0027](./0027-idempotency-key-middleware.md)
(its v1 scope and its 422 answer are superseded). Hard cutover: no aliases, no
dual routes, no toggles. It must deploy together with the rest of the fleet's
REST cutover set.

## Context

The fleet REST API guideline (warehouse-docs) removes divergences measured
across the services: three command-URL styles, two spellings of the replay
header, key reuse answered 422, unkeyed writes and unpaginated lists. The
guideline is enforced by `.spectral.yaml` + `.spectral/functions/restGuideline.js`
(rules R1-R8) in the `api-lint` job.

## Decision

1. **Noun routes.** URLs name things; a command is a transition resource.

   | Old | New |
   | --- | --- |
   | `POST /associates/{id}/start-shift` | `POST /associates/{id}/shift-starts` |
   | `POST /associates/{id}/end-shift` | `POST /associates/{id}/shift-ends` |
   | `POST /associates/{id}/break/start` | `POST /associates/{id}/break-starts` |
   | `POST /associates/{id}/break/end` | `POST /associates/{id}/break-ends` |
   | `POST /paths/{pathId}/plan/propose` | `POST /paths/{pathId}/plan-proposals` |

   `operationId`s, status codes and bodies are unchanged. `TestRetiredRoutesAreGone`
   asserts the old routes answer 404/405.
2. **Every POST is idempotent.** `RequireIdempotencyKey` wraps all eight POST
   routes (route-scoped). The key is global: the row is found by key alone and
   compared by fingerprint (method + path + SHA-256 of the body). Same key and
   fingerprint replays the stored answer with `Idempotency-Replayed: true`; a
   different fingerprint is `409` problem `idempotency-key-reused` (was 422); a
   missing key is `400 idempotency-key-required`; a key that is not 1-128
   printable ASCII is `400 idempotency-key-invalid`. `Idempotent-Replayed` is
   retired. The key row, the aggregate change and the outbox rows still commit
   in one transaction (every use case already runs in the `UnitOfWork`, which
   joins the middleware's transaction through `internal/pgtx`); 2xx and 4xx are
   stored, a 5xx or panic rolls back and is never stored. `PUT`/`DELETE` do not
   exist on this service, so no keyless-by-method exception applies.
3. **Keyset pagination.** `GET /sites/{siteCode}/shifts/{shiftId}/staffing-gap`
   and the deprecated `GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap`
   take `limit` (1-500, default 100) and `cursor` and answer
   `{items,nextCursor}`, ordered by `pathId` (unique within a plan) via the
   shared `page` helper (`internal/adapters/inbound/http/page`). The gaps are
   computed from the committed plan's lines (a bounded in-memory set, not a SQL
   query), so the keyset is applied to that sorted slice. Bad `limit` is
   `400 invalid-limit`, bad or repeated `cursor` `400 invalid-cursor`, any other
   query parameter `400 unknown-query-parameter`.

## Exemptions

None. `GET /paths/{pathId}/staffing-gap` is a single-path lookup (an item, not a
collection) and `/reports/labor` belongs to the separate reports API.

## Consumers changed

`web/` (staffing-gap screen follows `nextCursor`, `fetchAllPages`), the BDD
steps and e2e helpers in this repo. Outside this repo: `e2e-tests`
(`cmd/warehouse-day/floor.go`, `sim.go`, `e2e_test.go`) call the renamed routes.

## Consequences

Breaking for every client of the five renamed routes, every POST caller (a key
is now required) and every reader of the staffing-gap lists. The deprecated
`/buildings/...` staffing-gap alias predates this ADR and is untouched apart
from paging.
