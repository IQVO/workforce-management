package postgres

import (
	"testing"

	"github.com/claudioed/workforce-management/internal/idempotency"
)

// The sweeper's default TTL and the replay read's age guard must be the
// SAME value (ADR 0044 addendum): internal/idempotency.KeyTTL is the one
// definition both adapters share. A drift would either delete rows the
// read still answers or keep rows the read refuses — the exact gap between
// expiry and sweep that the read-side guard closes.
func TestDefaultIdempotencyKeyTLLEqualsTheReplayWindowConstant(t *testing.T) {
	if DefaultIdempotencyKeyTTL != idempotency.KeyTTL {
		t.Fatalf("DefaultIdempotencyKeyTTL = %s, want the shared replay-window constant %s",
			DefaultIdempotencyKeyTTL, idempotency.KeyTTL)
	}
}
