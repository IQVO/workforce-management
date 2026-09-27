---
id: 0021-optimistic-concurrency-version-column
slug: /adr/0021-optimistic-concurrency-version-column
title: 0021. Optimistic concurrency (version column) on AssociateShift and LaborAssignment
sidebar_label: 0021. Optimistic concurrency (version column)
description: ADR 0021 — a version column closes a blind-overwrite lost-update race on AssociateRepo.Save and AssignmentRepo.Save; ShiftPlanRepo.Save is deliberately NOT protected because CommitShiftPlan is a full-replace write with no stale-field hazard.
---

# 0021. Optimistic concurrency (version column) on AssociateShift and LaborAssignment

## Status

Accepted — implemented in the same change that introduced this record.
Mirrors the fleet's reference implementation on inventory-storage
(`stock_repo.go`, `location_repo.go`, `reservation_repo.go`), adapted to
this service's aggregates.

## Context

Before this change, `AssociateRepo.Save` and `AssignmentRepo.Save` were
blind full-column upserts:

```go
// AssociateRepo.Save, before
INSERT INTO associate_shift (associate_id, certifications, on_break, hours_logged, ended)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (associate_id) DO UPDATE SET
    certifications = EXCLUDED.certifications, on_break = EXCLUDED.on_break,
    hours_logged = EXCLUDED.hours_logged, ended = EXCLUDED.ended
```

Every use case follows a read-modify-write shape: `FindByID`/
`FindByAssociateID`, mutate the in-memory aggregate, `Save`. Two callers
that both load the same row, each apply their own mutation, and Save one
after the other will have the second Save silently overwrite every column
the first Save touched — not just the field the second caller intended to
change. Neither caller is told anything went wrong.

### Where this is a genuine risk in this domain

The three mutable aggregates were assessed for concurrent-write risk
against how this service's own use cases actually call them, not
in the abstract:

- **`AssociateShift`** (`associate_shift` table). `StartBreak`, `EndBreak`,
  `CertifyAssociate`, and `EndAssociateShift` (via `AssignLabor`'s
  `LogHours` path too) all read-modify-write the SAME row for the SAME
  associate. These are triggered by independent HTTP calls with no
  ordering guarantee between them — a break-start and a certification for
  the same associate, or a break-end racing an end-shift, are two
  DIFFERENT operations a caller could genuinely fire close together (a
  supervisor certifying someone at the exact moment that associate hits
  "end break" on a handheld, for instance). **Protected.**
- **`LaborAssignment`** (`labor_assignment` table). `AssignLabor` and
  `EndAssociateShift` both read-modify-write the same associate's
  assignment row — a scheduler reassigning someone to a new path at the
  same moment their shift is being ended by a supervisor is a realistic
  double-writer scenario, not a contrived one. **Protected.**
- **`ShiftPlan`** (`shift_plan` + `path_plan` tables). `CommitShiftPlan`
  is the ONLY write path, and it is not a read-modify-write at all:
  `shiftplan.CommitShiftPlan(...)` constructs a brand-new `ShiftPlan`
  purely from the caller-supplied `lines`/`installedStations`/
  `installedCapacity` arguments — it never loads the PREVIOUS plan and
  mutates it. `ShiftPlanRepo.Save` reflects this: it deletes every
  existing `path_plan` row for the building+shift and inserts the new
  set wholesale (see `TestShiftPlanRepo_Save_UpdatesExistingPlanLines`,
  already existing before this change). There is no stale-field-clobber
  hazard for a full-replace write: whichever commit lands last simply IS
  the plan, by design, and that is the intended "last commit wins"
  semantics for a human-decided headcount split — not a bug this ADR
  needs to fix. **Deliberately NOT protected.** Adding a version column
  here would only add friction (rejecting a legitimate re-commit) without
  closing any real defect.

## Decision

**Add a `version` column to `associate_shift` and `labor_assignment`
only.** `shift_plan`/`path_plan` are untouched.

1. **Domain**: `AssociateShift` and `LaborAssignment` each gain an
   unexported `version int` field and a `Version() int` accessor —
   inert infrastructure metadata, exactly like each aggregate's own id,
   never read by domain business logic. `NewAssociateShift`/
   `NewLaborAssignment` (fresh aggregates) start at `version: 1`.
   `Rehydrate` gains a `version` parameter, populated by the adapter
   from the row it read.

2. **Migration** (`migrations/000003_version.up.sql` /
   `.down.sql`):

   ```sql
   ALTER TABLE associate_shift ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
   ALTER TABLE labor_assignment ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
   ```

3. **Repo `Save`**: a single version-guarded `INSERT ... ON CONFLICT
   DO UPDATE ... WHERE <table>.version = $loaded_version`, checking
   `RowsAffected()`:

   ```go
   tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
       INSERT INTO associate_shift (associate_id, certifications, on_break, hours_logged, ended, version)
       VALUES ($1, $2, $3, $4, $5, 1)
       ON CONFLICT (associate_id) DO UPDATE SET
           certifications = EXCLUDED.certifications, on_break = EXCLUDED.on_break,
           hours_logged = EXCLUDED.hours_logged, ended = EXCLUDED.ended,
           version = associate_shift.version + 1
       WHERE associate_shift.version = $6
   `, ..., a.Version())
   if tag.RowsAffected() == 0 {
       return ports.ErrConcurrentModification
   }
   ```

   **Verified against a real testcontainers Postgres, not assumed**: a
   fresh INSERT (no conflict) always reports `RowsAffected() == 1`; an
   `ON CONFLICT DO UPDATE` whose `WHERE` clause does NOT match the
   existing row correctly reports `RowsAffected() == 0` rather than
   silently no-op-succeeding (confirmed with a throwaway table and
   `psql` before writing the Go code, then reconfirmed by the new
   integration tests). This makes the single-statement form safe to use
   here — no need for the fleet reference's fallback two-branch
   INSERT/UPDATE form. `AssignmentRepo.Save`'s active-assignment upsert
   is guarded the same way; the history-table writes inside the same
   multi-statement transaction are unconditional inserts, which is
   correct — history is append-only and never subject to the same
   caller's own concurrent overwrite.

4. **`ports.ErrConcurrentModification`** (new sentinel in
   `internal/application/ports/errors.go`, alongside the existing
   `ErrNotFound`) is returned by both repos' `Save` on a version
   mismatch against an existing row.

5. **HTTP mapping**: `statusFor`/`categoryFor` in
   `internal/adapters/inbound/http/errors.go` map
   `ports.ErrConcurrentModification` to `409 Conflict` with its own
   `concurrent-modification` RFC 7807 category, distinct from the
   existing domain-rule 409s (`associate-already-on-break`, etc.) so a
   caller can tell "re-fetch and retry" apart from "this request was
   rejected on its merits."

6. **Use cases are unchanged.** Every use case already does
   `FindByID`/`FindByAssociateID` → mutate → `Save` inside `atomically`;
   the version flows through transparently because `Rehydrate` populates
   it from what was read and `Save` reads it back off the aggregate. No
   use-case file in `internal/application/usecases` needed a code
   change — confirmed by the full existing test suite passing unmodified
   (only call-site signature updates for the new `Rehydrate` parameter
   in tests and the in-memory adapters).

## Consequences

**Positive**

- The lost-update race on `AssociateShift` and `LaborAssignment` — two
  concurrent read-modify-write requests on the same associate silently
  clobbering each other — is closed and proven closed by a real
  two-goroutine concurrent test against testcontainers Postgres, not
  just a sequential simulation.
- Single-writer flows are completely unaffected: every pre-existing test
  passes unmodified, and coverage is unchanged (99.5% before and after
  on `./internal/domain/...,./internal/application/...`).
- The 409 contract is additive and narrowly scoped: it only fires when a
  genuine stale-version Save is attempted, which cannot happen on any
  existing single-writer-per-request flow.

**Negative / accepted**

- Callers that receive `409 concurrent-modification` must re-fetch and
  retry; this repo does not implement automatic retry (matches the
  fleet's existing pattern — the idempotency-key middleware rollout is a
  distinct concern from this one, see that ADR/reference for retries on
  the SAME request rather than lost updates from DIFFERENT requests).
- `ShiftPlan` remains unprotected by design (see Context above) — a
  future change that makes `CommitShiftPlan` a genuine read-modify-write
  (e.g. an incremental per-path amend endpoint, rather than "resubmit the
  whole plan") would need to revisit this and add a version column then,
  not retrofit one onto the current full-replace semantics.
- One more column, one more migration, one more error to document per
  protected aggregate.

## Alternatives considered

- **Explicit two-branch INSERT/UPDATE instead of one guarded upsert.**
  The fleet reference flagged this as the fallback if
  `RowsAffected()` didn't report the WHERE-mismatch correctly on a real
  Postgres. It does report correctly (verified), so the simpler
  single-statement form was kept — less code, same guarantee.
- **Protecting `ShiftPlan` anyway, "for consistency."** Rejected: adding
  a version column to a full-replace write has no bug to close and would
  actively break the "last full commit wins" semantics `CommitShiftPlan`
  already implements by design (see `TestShiftPlanRepo_Save_UpdatesExistingPlanLines`).
  Force-fitting the pattern onto every aggregate regardless of its real
  write shape was explicitly the wrong call here.
- **A `SELECT ... FOR UPDATE` row lock instead of a version column.**
  Would require holding a transaction across the read AND the caller's
  in-memory mutation, which does not fit this codebase's
  load-then-later-Save use-case shape (the mutation happens in Go code
  between two separate repo calls, not inside one DB round trip).
  Optimistic concurrency needs no lock held across that gap.

## Verification

- Unit/domain (`labor_assignment_test.go`, `associate_shift_test.go`):
  `Rehydrate` preserves the passed-in version; every existing test
  updated for the new parameter, no behavior change.
- Integration (`-tags=integration`, testcontainers Postgres,
  `internal/adapters/outbound/postgres/version_integration_test.go`):
  - `TestAssociateRepo_Save_StaleVersionFails` /
    `TestAssignmentRepo_Save_StaleVersionFails`: two sequential loads of
    the same row, first Save succeeds, second (stale) Save returns
    `ErrConcurrentModification` and its change is verified absent on
    reload — the first writer's change is never clobbered.
  - `TestAssociateRepo_Save_CurrentVersionSucceedsAndIncrements` /
    `TestAssignmentRepo_Save_CurrentVersionSucceedsAndIncrements`: a
    Save at the current version succeeds and the row's version advances
    by exactly one.
  - `TestAssociateRepo_ConcurrentSaves_ExactlyOneWinner`: the real proof
    — two goroutines each load the SAME row, mutate independently, and
    call `Save` concurrently (synchronized start via a channel so both
    genuinely race). Exactly one succeeds, the other gets
    `ErrConcurrentModification`, the row's version advances by exactly
    one (never two), and exactly one of the two mutations is visible on
    reload — never both, never neither.
  - Full existing integration suite (`go test -tags=integration ./...
    -race -count=1`, all testcontainers-backed) passes unmodified.
- `go build`/`go vet` plain and `-tags=integration`, `gofmt -l .` clean,
  `golangci-lint run ./...` (0 issues), `make check` and `make arch-test`
  pass, `make coverage` unchanged at 99.5% (gate 90%).
