// Package idempotency holds the ONE replay-window TTL that both adapters
// touching idempotency_keys must agree on: the HTTP middleware's replay
// read (internal/adapters/inbound/http, ADR 0027/0044) refuses a row older
// than KeyTTL, and the housekeeping sweeper
// (internal/adapters/outbound/postgres, ADR 0028) purges rows past the same
// default. It exists as its own small top-level internal package — the same
// neutral-sibling move as internal/pgtx and internal/resilience — because
// the hexagonal fitness tests forbid either adapter importing the other, so
// the shared constant cannot live in either of them without duplicating it.
package idempotency

import "time"

// KeyTTL is how long a stored idempotency_keys row may answer a replay:
// the window between a key's first answer and the moment a retry of the
// same key+body stops being a replay and becomes a new request. The replay
// read enforces it at read time (a row past KeyTTL never answers; the
// retry executes the handler again under the same key), and the sweeper's
// DefaultIdempotencyKeyTTL is this same value, so what the read refuses is
// exactly the age the sweep deletes — nothing in between replays and
// nothing in between survives.
const KeyTTL = 24 * time.Hour
