---
id: 0042-idempotency-key-wire-pattern
slug: /adr/0042-idempotency-key-wire-pattern
title: 0042. The Idempotency-Key schema states the wire value; the middleware enforces the key exactly
sidebar_label: 0042. Idempotency-Key wire pattern
sidebar_position: 43
description: "ADR 0042: the IdempotencyKey parameter pattern describes the header as sent (the 1-128 character key with a visible first and last character, then optional trailing HTTP whitespace that the stack drops), and validIdempotencyKey enforces exactly that key, so the spec and the server agree on every request a client can send."
---

# 0042. The Idempotency-Key schema states the wire value; the middleware enforces the key exactly

## Status

Accepted (2026-11-09). Amends [ADR 0040](./0040-adopt-the-fleet-rest-api-guideline.md)
§2 (what a malformed key is) and [ADR 0027](./0027-idempotency-key-middleware.md)
(the header contract). Closes issue #254.

## Context

The `contract` CI job (Schemathesis over `apis/openapi.yaml`) failed on
about 3 of 4 `develop` runs: *API accepted schema-violating request*,
`Idempotency-Key` violates `pattern`, answered `204`. A local run with
output sanitization off showed the key: `8G'S` followed by a TAB.

Two facts, both checked:

1. **The middleware was looser than the spec.** The schema said
   `^[!-~]([ -~]*[!-~])?$` (visible first and last character).
   `validIdempotencyKey` accepted any 1-128 printable ASCII, so a value
   that reached it with an edge space (`" abc"`, `"abc "`, `" "`) passed.
2. **That was not what made the job red.** HTTP/1.1 frames a header line
   as `field-name ":" OWS field-value OWS` (RFC 9112 §5), and RFC 9110
   §5.5 requires the recipient to drop that optional whitespace before it
   evaluates the value. Go's `net/http` does that, so the key on the wire
   `8G'S<TAB>` reaches the middleware as `8G'S`, which is a valid key. No
   handler can see, or by RFC 9110 may act on, the whitespace. Schemathesis
   judges the raw string it sends, so it counts that request as invalid.
   It filters out leading whitespace (`requests` refuses to send it), but
   not trailing whitespace.

The spec had gone back and forth on this. `0b31738` and `74a851a` forbade
edge whitespace, `1677b3c` allowed edge spaces, and `a9c610e` allowed
whitespace on both sides. That last pattern led Schemathesis to generate
keys with leading whitespace, which the client refused to send
(`Network Error`). `7ebf51b` then forbade edge whitespace again and
described the pattern as the value after stripping. Its claim that "no HTTP
client can transmit edge whitespace" is wrong for trailing whitespace.

## Decision

1. **The middleware enforces the key exactly.** `validIdempotencyKey`
   accepts 1-128 printable ASCII characters (0x20-0x7E) whose first and last
   character are visible (0x21-0x7E). Anything else that reaches it is
   `400 idempotency-key-invalid`. `TestValidIdempotencyKey_SpecBoundary`
   pins the boundary: too short, too long, bad pattern, valid.
2. **The schema describes the header as sent:**
   `^[!-~]([ -~]{0,126}[!-~])?[ \t]*$`, `minLength: 1`. That is the key
   from point 1, then optional trailing SP/HTAB. The 128-character limit
   lives in the regex, not in `maxLength`, because `maxLength` would also
   count the trailing whitespace that the server never sees. Leading
   whitespace is not in the pattern because clients do not send it.
3. **The accepted keys do not change.** A request matches the pattern
   exactly when, after the HTTP stack drops OWS, its value passes the
   middleware. A request that does not match is rejected with the
   documented `400`. The only exception is leading whitespace, which
   clients do not send.
   `TestIdempotencyKeyWire_TrailingOWSIsStrippedBeforeTheMiddleware` pins
   the stripping. If a Go release stops doing it, that test fails, and
   this pattern and point 1 must be revisited together.

Rejected: checking raw header bytes at the connection layer to refuse a
key followed by OWS. That would need a framing-aware reader around
`net/http`, and it would break RFC 9110 §5.5. Also rejected: removing the
negative-data check or adding a Schemathesis hook, because that weakens the
sensor.

## Consequences

- The contract job can no longer report an accepted request as invalid
  because of the key's whitespace. This was verified by repeated local runs
  of `scripts/contract-test.sh` (the job is randomized).
- The 400 detail now says the key must "start and end with a visible
  character".
- Sibling contexts that declare `^[ -~]+$` (inventory-storage,
  labor-performance) have the same latent false positive for a key followed
  by a TAB. Each needs this change in its own repo.
